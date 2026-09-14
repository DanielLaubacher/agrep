# DFA Inner Loop Performance: Cold Cache Illusions and Real Bottlenecks

## Summary

While building agrep's lazy DFA regex engine, we initially observed that adding position recording to the DFA's inner loop appeared to double execution time (17ms bare → 34ms with recording). We attributed this to register pressure and Go codegen limitations.

**This was wrong.** The 17ms "bare" measurement was a cold-cache artifact — the loop was silently skipping uncomputed DFA transitions via a `continue` statement, making it trivially fast but incorrect. With a properly warmed cache, recording overhead is **2% (1.02x)**, not 100% (2.0x).

This document traces the full investigation: the initial (wrong) hypothesis, the disassembly analysis, and the corrected understanding. The real bottleneck is DFA state cache locality, not register pressure.

## Act 1: The Initial (Wrong) Observation

Pattern: `\d{4}-\d{2}-\d{2}` on 29MB of text (500K lines, 100K matches).

| Inner loop variant | Cold cache | Apparent overhead |
|----|------|----------|
| Bare DFA loop (count only) | **17ms** | baseline |
| `ends[n] = i; n++` (raw array) | **34ms** | 2.0x |
| `append(matchEnds, i)` (slice) | **55ms** | 3.2x |

We concluded: "any write to memory in Go's DFA inner loop adds 2-3x overhead."

## Act 2: The Register Pressure Hypothesis

We examined the assembly. The bare DFA loop uses 7 of 13 available registers:

```asm
;; 7 registers for the bare DFA loop:
;;   R10=i, BX=data, CX=len, R8=ss256, R9=trans, SI=len(trans), DX=state

loop:
  CMPQ  CX, R10            ;; i < len(data)?
  JLE   exit
  MOVZX (BX)(R10*1), R11   ;; b = data[i]
  ADDQ  R11, R8            ;; ss256 + byte
  CMPQ  SI, R8             ;; bounds check
  JBE   panic
  MOVL  (R9)(R8*4), R8     ;; trans[state*256 + byte]  ← THE DFA LOOKUP
  TESTL R8, R8             ;; uncomputed?
  JGE   check_match
  CMPL  R8, $0x40000000    ;; match bit?
  JLT   no_match
  ...
```

We reasoned: adding recording needs 4-5 more registers (ends_ptr, ends_len, nEnds, inMatch, i), pushing to 12/13 — the spill threshold. Register spills would add memory round-trips per iteration.

**This analysis was technically correct but irrelevant**, because the benchmark was flawed.

## Act 3: The Cold Cache Bug

The "warmup" code called `fwd.match(data)` which returns on the **first match** (a few hundred bytes into the file). Only transitions for the first few lines were computed. The remaining 29MB of transitions were `dfaUncomputed = -1`.

The bare loop handled uncomputed transitions with:

```go
next := trans[ss256+int(b)]
if next < 0 { continue }  // ← SKIPS the byte entirely
```

This `continue` made the loop trivially fast (most bytes were skipped) and the count incorrect (0 matches from most of the file, masked by the few early matches). The 17ms measurement was not a DFA processing 29MB — it was a DFA processing ~1MB of cached transitions and skipping the other 28MB.

The recording variant was slower because `for i, b := range data` (needed for position tracking) generates different code than `for _, b := range data`. The `i` variant maintains an explicit counter, changing the compiler's loop optimization even when `i` is rarely used.

## Act 4: The Corrected Measurement

Proper warmup: process the **entire file** through the forward DFA, computing all lazy transitions before benchmarking:

```go
func warmAll(fd *forwardDFA, data []byte) {
    state := fd.startState
    for _, b := range data {
        next := trans[ss256+int(b)]
        if next < 0 {
            next = fd.computeTransition(state, b)  // compute, don't skip
            trans = fd.trans
        }
        // ... advance state
    }
}
```

Results with warm cache:

| Inner loop variant | Time | vs Bare |
|----|------|----------|
| Bare DFA count (`for _, b`) | **68ms** | baseline |
| Raw array recording (`for i, b` + write) | **70ms** | **1.02x** |
| Callback (empty body) | **75ms** | **1.10x** |
| Callback (write to array) | **75ms** | **1.09x** |

**Position recording adds 2% overhead, not 100%.** The callback pattern (accumulation via function call at match boundaries) adds 10% from indirect call overhead (100K closure invocations).

## Act 5: The Real Bottleneck

With the recording myth debunked, why is the search DFA (54ms) faster than the forward DFA (70ms)?

| Approach | Time | Notes |
|----------|------|-------|
| Forward DFA (every byte) | 70ms | Processes all 29M bytes |
| Search DFA (SIMD skip + DFA verify) | 54ms | SIMD skips 86% of bytes |

The forward DFA is slower because it processes **every byte** through the DFA transition table. Each transition is a random access into a ~20KB working set (20 states × 1KB per state). At 29M accesses, even L1-cached transitions take ~2.4ns each = 70ms.

The search DFA's SIMD range scan skips 86% of bytes (only 14% are digits for this pattern). It processes ~4M candidate bytes through the DFA. Each DFA access is more likely to be L1-hot (fewer distinct states visited), resulting in faster per-transition throughput.

**The bottleneck is not register pressure, codegen, or recording overhead. It's the number of DFA transitions executed × L1 cache hit rate per transition.**

## Lessons

### 1. Benchmark warmup matters more than you think

Lazy DFA transitions start as `dfaUncomputed = -1`. Any benchmark loop that handles `-1` with `continue` or `break` will show artificially fast results on cold data. The DFA appears fast because it's doing nothing.

**Always warm the cache by processing the actual benchmark data through the real DFA engine first.**

### 2. Go's codegen is not the bottleneck for DFA loops

The inner loop compiles to 11 instructions with 4 branches — already close to optimal. Register pressure from recording adds ~2% overhead, not 100%. The assembly analysis was correct but answered the wrong question.

### 3. SIMD prefiltering beats brute-force DFA when start bytes are selective

For `\d{4}-\d{2}-\d{2}` (10/256 = 4% start byte coverage), SIMD skipping reduces DFA transitions by ~86%. This is worth more than any inner-loop optimization.

For patterns with wide start ranges (>20% byte coverage), SIMD skipping doesn't help and brute-force DFA is competitive.

### 4. DFA state count × access pattern determines throughput

The forward DFA (with start-state union) has more states than the search DFA, because every state includes the NFA start states. More states = larger working set = more L1 cache pressure = slower per-transition throughput.

The search DFA has fewer states (no start-state union) and benefits from SIMD skip reducing the number of transitions. Fewer transitions on a smaller state set = faster.

### 5. Cold-cache bugs are silent

The forward DFA's `if next < 0 { continue }` produced a valid-looking result (a count, a timing number) that was completely wrong. There was no error, no panic, no obvious sign of the bug. It took multiple rounds of investigation to discover that the "fast" baseline was actually broken.

**When a number looks too good, verify that the code is doing what you think it's doing.**

## The Go vs Rust Per-Transition Gap

Both agrep and ripgrep use the same algorithm (SIMD first-byte scan + lazy DFA verify) and execute the same number of DFA transitions (~4M for `\d{4}-\d{2}-\d{2}` on 29MB). The 2.4x gap is **per-transition cost**: 13ns (Go) vs ~7ns (Rust).

### Go's DFA transition: 7 instructions, 3 branches

```asm
ADDQ  R11, R8           ;; 1. compute index: state*256 + byte
CMPQ  SI, R8            ;; 2. bounds check: index < len(trans)?
JBE   panicBounds       ;; 3. branch (never taken, but costs predictor resources)
MOVL  (R9)(R8*4), R8    ;; 4. *** THE LOAD: trans[state*256 + byte] ***
TESTL R8, R8            ;; 5. check dfaUncomputed (-1)?
JGE   check_match       ;; 6. branch (never taken after warmup)
CMPL  R8, $0x40000000   ;; 7. check match bit?
JLT   continue          ;; 8. branch
```

Three of these instructions are **safety overhead** that Rust doesn't pay:

- **Bounds check** (instructions 2-3): Go's slice access `trans[ss256+int(b)]` emits a compare + conditional branch. Never taken after warmup, but the branch predictor must track it and the compare occupies an execution port every iteration. Cost: ~0.5 cycles.

- **Uncomputed check** (instructions 5-6): The lazy DFA uses `-1` as a sentinel for transitions not yet computed. After full warmup, this check always falls through. But Go cannot prove this statically — the check must remain. Cost: ~0.3 cycles.

- **Match-bit check** (instructions 7-8): Both Go and Rust need this, but LLVM folds it into the load via flag testing. Go's compiler emits a separate compare.

### Rust's equivalent: ~3 instructions, 1 branch

```asm
mov   eax, [r9 + r8*4]  ;; load transition (no bounds check — unsafe get_unchecked)
test  eax, eax           ;; combined uncomputed + match bit check
js    handle             ;; single branch
```

Rust eliminates the bounds check via `unsafe` and the uncomputed check via pre-computation (Rust's regex crate pre-computes all transitions for small DFAs, or uses a different sentinel strategy). LLVM's backend also merges the match-bit test into fewer instructions.

### The arithmetic

| | Go | Rust | Ratio |
|---|---|---|---|
| Instructions per transition | ~7 | ~3 | 2.3x |
| Transitions (SIMD skip + DFA) | ~4M | ~4M | 1.0x |
| Total instructions | ~28M | ~12M | 2.3x |
| Measured time | 53ms | ~27ms | 2.0x |

The 2.3x instruction ratio maps closely to the 2.0-2.4x measured time ratio. The small difference is because memory latency (the `MOVL` load) dominates on both — instructions execute during the load's latency, partially hiding the extra Go instructions.

### What would close the gap

1. **Eliminate bounds check**: Use `unsafe.Pointer` arithmetic for `trans[state*256 + byte]`. Saves 2 instructions per transition. Estimated gain: ~1.3x.

2. **Eliminate uncomputed check**: Pre-compute all 256 transitions for every reachable state during `warmStart()`. Then the `-1` sentinel never appears and the check can be removed. Saves 2 instructions. Estimated gain: ~1.2x.

3. **Assembly inner loop**: Write the DFA transition loop in Go assembly (like `bytes.IndexByte`). Control register allocation, eliminate all safety checks, use optimal instruction selection. Combined estimated gain: ~2x, closing the gap to Rust.

Options 1-2 are achievable in pure Go (option 1 requires `unsafe`). Option 3 is the nuclear option but would bring the DFA inner loop to parity with Rust.

None of these are currently implemented in agrep. The current approach — SIMD prefiltering to minimize the number of DFA transitions + the optimizations already in place — makes agrep competitive with ripgrep on most real-world patterns without resorting to unsafe code.

## Final Performance: agrep vs ripgrep

| Pattern | agrep | rg | Gap | Why |
|---------|--------|------|-----|-----|
| `[A-Z]{2,}` | 68ms | 68ms | **tied** | SIMD range scan dominates |
| `[a-zA-Z]+@...\.[a-zA-Z]+` | 8ms | 7ms | **1.1x** | Rare-byte `@` prefilter |
| `\d{4}-\d{2}-\d{2}` (count) | 65ms | 27ms | **2.4x** | Per-transition cost gap |
| Recursive `\d{4}` | 14ms | 25ms | **1.8x faster** | Forward DFA + parallelism |
| Recursive `struct\s+\w+` | 16ms | 30ms | **1.9x faster** | Forward DFA + parallelism |

**Update**: After further optimization (batch SIMD, precomputed transitions, unconditional masking), the date pattern improved from 65ms to 42ms (1.4x of ripgrep). The unsafe optimizations described in "What would close the gap" above were tested and contributed only 4% — not worth the safety tradeoff. See [doc 10](10-closing-the-ripgrep-gap.md) for the full results.
