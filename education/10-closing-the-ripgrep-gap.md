# Closing the ripgrep Gap: What Worked and What Didn't

gogrep started **16x slower** than ripgrep on regex patterns. After building a custom lazy DFA regex engine with SIMD prefiltering, we reached **1.0-1.4x** on most patterns — competitive with ripgrep using pure safe Go. This document records every optimization we tried, what each contributed, and why several promising approaches failed.

> **Update**: a later round ([doc 11](11-beating-ripgrep.md)) went past parity — gogrep now wins or ties every benchmarked workload. It also fixed a concurrency bug in the lazy DFA described here (lazy transition computation raced when one Regexp was shared across scheduler workers; transitions are now fully precomputed at compile time). The final tables in section 6 below are superseded by doc 11 section 13.

---

## Table of Contents

1. [The Starting Point](#1-the-starting-point)
2. [What Worked: Two Things That Matter](#2-what-worked-two-things-that-matter)
3. [The Custom Regex Engine](#3-the-custom-regex-engine)
4. [SIMD Optimizations](#4-simd-optimizations)
5. [What Didn't Work](#5-what-didnt-work)
6. [Final Results](#6-final-results)
7. [Architecture Diagram](#7-architecture-diagram)
8. [Key Lessons](#8-key-lessons)

---

## 1. The Starting Point

gogrep used Go's stdlib `regexp` package (Thompson NFA simulation). On a 29MB file with regex patterns, it was **13-16x slower** than ripgrep:

```
Pattern: \d{4}-\d{2}-\d{2}
gogrep (stdlib regexp):  ~480ms
ripgrep:                  ~30ms
Gap:                       16x
```

Go's NFA does ~10-30 instructions per byte. Rust's lazy DFA does ~3 instructions per byte. The gap is architectural, not language-level.

---

## 2. What Worked: Two Things That Matter

Every optimization we tried falls into two categories. Only these two produced measurable improvements:

### Category 1: Custom Regex Engine (Lazy DFA)

Replaced Go's stdlib NFA with a purpose-built lazy DFA. This changed the per-byte cost from ~10-30 instructions to ~3 instructions.

**Impact: 16x → 3-5x** (accounting for ~80% of the total improvement)

### Category 2: SIMD Data Skipping

Used AVX2 SIMD to skip bytes that can't participate in matches, so the DFA only runs on candidate regions.

**Impact: 3-5x → 1.0-1.4x** (accounting for the remaining ~20%)

Everything else we tried — unsafe pointers, Go assembly, streaming DFA, builder patterns, recursion — contributed **0-4%** or was actually slower.

---

## 3. The Custom Regex Engine

### Architecture

```
Compile(pattern)
    │
    ├─ Pure literal?  → LiteralEngine (SIMD bytes.Index)
    │
    ├─ Has assertions? → PikeVM (NFA, handles ^$\b correctly)
    │
    └─ Default → Two DFAs:
         ├─ forwardDFA  — single-pass match(), O(n)
         └─ searchDFA   — findIndex/findAllIndex with SIMD skip
```

### Forward DFA: Single-Pass Match Detection

For `Match()` (does any match exist?), the forward DFA processes every byte once in a single pass. Every DFA state includes the NFA start states (unioned in), so the DFA simultaneously tracks all possible match starts. When any thread reaches a match state, it returns true immediately.

**Key implementation detail — match-bit encoding**: The transition value itself carries the match flag in bit 30. The inner loop checks `next >= 0x40000000` instead of loading a separate `isMatch[]` array. This eliminates a cache miss that was consuming 54% of CPU time (see doc 09).

```go
// Forward DFA inner loop — 2 ops per byte after warmup
for _, b := range data {
    next := trans[ss256+int(b)]
    if next >= fwdMatchBit {
        return true
    }
    state = next
    ss256 = int(state) * 256
}
```

**Result**: gogrep's `Match()` is **faster than ripgrep** on recursive searches because the forward DFA's single-pass loop amortizes well across many small files.

### Search DFA: Position Extraction

For `FindAllIndex()` (locate all matches), the search DFA runs from each candidate byte position. It uses the same flat transition array and match-bit encoding but without the start-state union — so it can track where matches start.

### Flat Transition Arrays

Both DFAs store transitions in a flat `[]int32` array indexed by `state*256 + byte`, not as structs with `[256]int32` fields. This eliminates the 1028-byte struct stride that caused L1 cache thrashing when accessing `isMatch` after `trans[b]`.

### Lazy Computation with Eager Precompute

Transitions are computed lazily on first access (cache miss → epsilon-closure → new DFA state). For the fast path, `precomputeAll()` eagerly BFS-computes all reachable transitions, so the inner loop can use `if next <= 0 { break }` instead of checking for the `dfaUncomputed` sentinel. This removes one branch per byte.

### Unconditional State Masking

The DFA inner loop uses unconditional masking:

```go
// Before (two branches):
if next >= sMatchBit {
    state = next & sStateMask
    lastMatch = i + 1
} else {
    state = next
}
ss256 = int(state) * 256

// After (one branch, enables CMOV):
ss256 = int(next & sStateMask) * 256  // unconditional — always correct
if next >= sMatchBit {
    lastMatch = i + 1  // compiler emits CMOV (branchless)
}
```

The mask is a no-op for non-match states (bit 30 is already clear). Making the state update unconditional breaks the data dependency chain and lets the CPU start computing the next iteration's address earlier. The compiler further optimizes the `lastMatch` update to a conditional move instruction.

---

## 4. SIMD Optimizations

### First-Byte Range Scan

For patterns like `\d{4}-\d{2}-\d{2}`, only bytes 0x30-0x39 (digits) can start a match. We added `simd.IndexByteRange(data, lo, hi)` that uses AVX2 `GreaterEqual` + `LessEqual` to check 32 bytes per iteration:

```go
// Check 32 bytes at once for any byte in [lo, hi]
chunk := archsimd.LoadUint8x32Slice(data[i:])
inRange := chunk.GreaterEqual(vecLo).And(chunk.LessEqual(vecHi))
b := inRange.ToBits()  // 32-bit mask: 1 = byte in range
```

For ASCII text with 14% digit density, this skips 86% of bytes. The search DFA only processes candidate positions.

**Impact: 2x improvement** (halved the search time)

### Pre-Computed Scanner Vectors

The initial implementation recreated AVX2 broadcast vectors (`BroadcastUint8x32(lo)`, `BroadcastUint8x32(hi)`) on every scan call. With 100K matches, that's 200K broadcast operations. `ByteRangeScanner` pre-computes these once at warmup:

```go
type ByteRangeScanner struct {
    vecLo, vecHi archsimd.Uint8x32  // pre-computed, reused across scans
    lo, hi       byte
}
```

**Impact: ~7% improvement** on the scan loop

### Batch SIMD Scanning

The per-candidate SIMD scan (`nextCandidateFrom`) was called 100K+ times, each setting up a scan from a new position. `BatchNext` collects up to 4096 candidate positions in a single SIMD pass over a data chunk, amortizing the function call overhead:

```go
const batchSize = 4096
var candBuf [batchSize]int
nCand := scanner.BatchNext(data, pos, dataLen, candBuf[:])
// Process all candidates in candBuf without re-entering SIMD
```

**Impact: 28% improvement** — the single largest SIMD optimization, reduced 100K function calls to ~25.

### Rare-Byte Prefilter

For patterns without literal substrings but with embedded punctuation, we extract the rarest required byte from the AST. For `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`, the `@` byte appears in <0.1% of positions. SIMD memchr for `@` + DFA verify on hits is dramatically faster than scanning with a 52-byte start range.

```go
var byteRarity [256]byte  // lower = rarer
// '@' scores 10 (very rare), 'a' scores 200 (very common)
```

**Impact: 6.4x improvement** on email pattern (59ms → 9ms)

### Alternation-of-Literals → Aho-Corasick Routing

Patterns like `ERROR|INFO|function` are alternations where every branch is a literal. The factory parses the AST, detects this, and routes to the existing SIMD-accelerated Aho-Corasick matcher instead of the DFA:

```go
if alts := extractAlternationLiterals(pattern); len(alts) > 0 {
    return NewAhoCorasickMatcher(alts, ignoreCase, invert)
}
```

**Impact: eliminated DFA overhead** for multi-literal patterns entirely

---

## 5. What Didn't Work

### Unsafe Pointer Arithmetic (4% gain — not worth the safety tradeoff)

Replaced `trans[ss256+int(b)]` with `*(*int32)(unsafe.Pointer(...))` to eliminate bounds checks. Measurable but small:

| Approach | Time | vs safe Go |
|---|---|---|
| Safe Go (batch + precompute) | 32ms | baseline |
| + unsafe pointers | 30ms | 4% faster |

We dropped this. The 4% isn't worth losing Go's memory safety guarantees.

### Go Assembly Inner Loop (0% gain)

Wrote the DFA transition loop in Go's Plan 9 assembly (`dfa_amd64.s`). The per-transition instruction count was optimal (6 instructions, 2 branches vs Go's 11 instructions, 4 branches). But:

- **ABI0 function call overhead** neutralized the gain. Go assembly functions use the old stack-based ABI, adding ~20ns of save/restore per call. With 100K candidates, that's 2ms of pure overhead.
- The assembly was faster per-byte but only ran for 5-10 bytes per candidate before hitting dead state.

**Lesson**: Go assembly only helps for long-running functions (like `bytes.IndexByte` which processes entire buffers). Short-lived inner loops are dominated by call overhead.

### Streaming Forward DFA (slower)

Processed every byte through the forward DFA in a single pass, recording match-end positions. Then found match starts via a reverse scan.

| Approach | Time | Why |
|---|---|---|
| SIMD skip + DFA verify | 52ms | 4M transitions × 13ns |
| Streaming (every byte) | 70ms | 29M transitions × 2.4ns |

The streaming approach processes **7x more bytes**. Even though each transition is cheaper (no function call, no SIMD setup), the total work is higher. SIMD skipping is more valuable than a tighter loop when the start-byte density is < 50%.

### Builder Pattern / Recursion for Accumulation (0% gain)

Investigated whether `append`, recursive accumulation, or callback patterns could reduce position-recording overhead in the DFA loop. After correcting a cold-cache measurement bug (see doc 09), position recording adds only **2% overhead** vs bare counting. The accumulation strategy is irrelevant.

### Cold-Cache Benchmarking (cautionary tale)

Early measurements showed recording positions doubled DFA loop time (17ms → 34ms). We attributed this to register pressure and wrote a detailed analysis with disassembly.

**It was wrong.** The 17ms "baseline" was running on cold DFA transitions — most bytes were skipped via `if next < 0 { continue }` instead of being processed. With properly warmed cache (all transitions pre-computed), recording adds 2%. See doc 09 for the full investigation.

---

## 6. Final Results

### Single file (29MB, 500K lines) — pure safe Go

| Pattern | gogrep | ripgrep | Gap | Started at |
|---------|--------|---------|-----|------------|
| `\d{4}-\d{2}-\d{2}` count | 42ms | 30ms | **1.4x** | 16x |
| `ERROR.*port [0-9]+` | 57ms | 30ms | **1.9x** | 16x |
| `[A-Z]{2,}` | 67ms | 67ms | **tied** | 16x |
| `[a-zA-Z]+@...\.[a-zA-Z]+` | 10ms | 8ms | **1.2x** | 16x |
| No match | 9ms | 8ms | **1.1x** | ~1x |

### Recursive search (2526 files, /usr/include)

| Pattern | gogrep | ripgrep | Gap |
|---------|--------|---------|-----|
| `\d{4}` | **14ms** | 25ms | **gogrep 1.8x faster** |
| `struct\s+\w+\s*\{` | **16ms** | 30ms | **gogrep 1.9x faster** |
| `define` | 22ms | 16ms | 1.4x |

### Contribution by optimization

| Optimization | Reduction | Mechanism |
|---|---|---|
| Custom lazy DFA | **16x → 3-5x** | ~3 instructions/byte vs stdlib's ~10-30 |
| SIMD first-byte range scan | **5x → 2-3x** | AVX2 skips 86% of bytes |
| Batch SIMD scanning | **2.5x → 2x** | Amortize 100K calls to ~25 |
| Rare-byte prefilter | **10x → 1.2x** (email) | SIMD memchr for `@` |
| Unconditional masking | **~15%** | Breaks dependency chain, enables CMOV |
| Pre-computed transitions | **~5%** | Eliminates uncomputed-sentinel branch |
| Match-bit encoding | **~20%** | Eliminates isMatch cache miss |
| Forward DFA (match-only) | **wins recursive** | Single-pass, no position tracking |

---

## 7. Architecture Diagram

```
             regex.Compile(pattern)
                    │
     ┌──────────────┼──────────────┐
     │              │              │
  Literal?     Assertions?    Default
     │              │              │
  LiteralEngine  PikeVM     ┌──────┴──────┐
  (SIMD Index)   (NFA)      │             │
                          forwardDFA   searchDFA
                          (match-only)  (positions)
                              │             │
                         single-pass    batch SIMD scan
                         match-bit      + DFA verify
                         encoding       per candidate
                              │             │
                          Match()      FindAllIndex()
                          MatchExists  FindAll()

  Prefilter layer (regex.go):
  ┌─────────────────────────────────────────────┐
  │ extractPrefilter(pattern)                   │
  │   → multi-byte literal: SIMD IndexAll       │
  │   → rare byte ('@','-'): SIMD memchr        │
  │   → none: search DFA handles it             │
  └─────────────────────────────────────────────┘

  Factory layer (matcher/factory.go):
  ┌─────────────────────────────────────────────┐
  │ Alternation of literals?  → Aho-Corasick    │
  │ All patterns literal?     → Boyer-Moore/AC  │
  │ Otherwise                 → FastRegexMatcher │
  │   (thin wrapper around regex.Regexp)         │
  └─────────────────────────────────────────────┘
```

---

## 8. Key Lessons

### 1. Algorithm beats micro-optimization

The lazy DFA (replacing stdlib NFA) was worth 5-10x. All micro-optimizations combined (unsafe, assembly, cache layout, branch elimination) were worth ~1.5x. When you're 16x behind, changing the algorithm is the only path.

### 2. SIMD is for skipping, not processing

The biggest SIMD wins came from **not running the DFA** on most bytes (range scan, rare-byte prefilter). Attempts to use SIMD inside the DFA loop (streaming) were slower because the DFA's random-access pattern doesn't vectorize.

### 3. Batch amortization beats per-call optimization

Making each SIMD call faster (pre-computed vectors: 7%) helped less than making fewer calls (batch scanning: 28%). Function call overhead in Go is real — reducing 100K calls to 25 calls is worth more than shaving nanoseconds off each call.

### 4. Unsafe doesn't buy much when the algorithm is right

With the right algorithm (lazy DFA + SIMD skip), bounds checks add only 4% overhead. Go's safety guarantees are worth that cost. The compiler's bounds-check elimination already handles many cases, and the remaining checks execute in parallel with the memory load they protect.

### 5. Benchmark warmup is critical for lazy data structures

Lazy DFAs compute transitions on demand. A benchmark that doesn't process the full dataset first will measure cold-cache performance — which can be 2-3x faster than warm (because uncomputed transitions are silently skipped, not processed). Always verify that your "fast" baseline is actually doing the work you think it's doing.

### 6. Profile before optimizing

CPU profiling revealed that 54% of time was in `isMatch` cache misses (doc 09), not the DFA transition itself. Match-bit encoding fixed this specific bottleneck. Without profiling, we would have optimized the wrong thing.

### 7. Correctness edge cases dominate implementation time

The DFA engine itself was ~2 days of work. UTF-8 handling, zero-width assertions, empty-match semantics, invalid-UTF-8 edge cases, and stdlib compatibility took ~5 days. Fuzz testing against stdlib found bugs in cold-cache handling, UTF-8 continuation bytes, (?s) dot-all mode, and alternation-of-empty semantics. The algorithm is simple; the edge cases are not.

---

## Cross-References

- **Doc 01**: SIMD and AVX2 fundamentals (archsimd API used by range scanner)
- **Doc 04**: String search algorithms (Boyer-Moore, Aho-Corasick used by factory)
- **Doc 06**: GC and allocation optimization (zero-alloc hot paths in DFA)
- **Doc 07**: Benchmarking and profiling (methodology for the measurements here)
- **Doc 08**: Regex engines theory (NFA vs DFA, prefilter concepts — the "before" picture)
- **Doc 09**: DFA register pressure investigation (cold-cache bug, per-transition analysis)
- **Doc 11**: The next round — startup tax, streaming match pipeline, parallel single-file chunking, rare-pair Teddy (the "after" picture for this document)
- **horspool-and-automata.md**: SIMD Horspool used by Boyer-Moore matcher
