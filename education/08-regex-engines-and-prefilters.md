# Regex Engines, Literal Prefilters, and the 16x Gap

Why does ripgrep search regex patterns 13-16x faster than agrep on the same hardware? The answer is not a single optimization but an entire architectural layer: **literal prefiltering**. This document explains how regex engines work at the automaton level, what literal prefiltering is, how ripgrep's Rust `regex` crate implements it (including the Teddy SIMD algorithm), why Go's `regexp` package cannot match it, and what options exist for closing the gap in a pure-Go codebase.

---

## Table of Contents

1. [The Benchmark That Reveals the Gap](#1-the-benchmark-that-reveals-the-gap)
2. [How Regex Engines Work: NFA vs DFA](#2-how-regex-engines-work-nfa-vs-dfa)
3. [Go's regexp Package: Thompson NFA Simulation](#3-gos-regexp-package-thompson-nfa-simulation)
4. [Rust's regex Crate: Hybrid NFA/DFA with Prefilters](#4-rusts-regex-crate-hybrid-nfadfa-with-prefilters)
5. [Literal Extraction from Regex ASTs](#5-literal-extraction-from-regex-asts)
6. [The Prefilter Cascade](#6-the-prefilter-cascade)
7. [Teddy: SIMD Multi-Pattern Prefiltering](#7-teddy-simd-multi-pattern-prefiltering)
8. [memchr: Single and Multi-Byte SIMD Search](#8-memchr-single-and-multi-byte-simd-search)
9. [Why the Gap Depends on the Pattern](#9-why-the-gap-depends-on-the-pattern)
10. [agrep's Existing Literal Optimization](#10-agreps-existing-literal-optimization)
11. [Approaches to Closing the Gap](#11-approaches-to-closing-the-gap)
12. [Regex Prefiltering: A Worked Example](#12-regex-prefiltering-a-worked-example)
13. [The DFA Advantage Beyond Prefilters](#13-the-dfa-advantage-beyond-prefilters)
14. [Benchmark Results and Analysis](#14-benchmark-results-and-analysis)
15. [Key Takeaways](#15-key-takeaways)
16. [Cross-References](#16-cross-references)

---

## 1. The Benchmark That Reveals the Gap

The following benchmarks compare agrep and ripgrep on `/usr/include` (62,419 files) across different pattern types. The results were collected with `hyperfine --warmup 3 --runs 10` on an Intel Xeon E-2176M (12 threads):

| Scenario | agrep | rg | Ratio |
|---|---|---|---|
| Fixed string `"define"`, `-l` | 164ms | 198ms | **agrep 1.21x faster** |
| Fixed string `"define"`, `-i -l` | 173ms | 235ms | **agrep 1.36x faster** |
| Fixed string `"define"`, `-c` | 227ms | 260ms | **agrep 1.14x faster** |
| Multi-pattern (`-e` x3), `-l` | 215ms | 229ms | **agrep 1.06x faster** |
| Fixed string, full output | 357ms | 271ms | rg 1.32x faster |
| Fixed string, `-n` output | 422ms | 295ms | rg 1.43x faster |
| No match, `-l` | 162ms | 140ms | rg 1.16x faster |
| Regex `"err(or\|no\|code)"`, `-l` | 2,628ms | 164ms | **rg 16x faster** |
| Regex `"[0-9][a-z][0-9][a-z]"`, `-l` | 2,982ms | 186ms | **rg 16x faster** |
| Regex `"[aeiou]{2}[^aeiou]{2}[aeiou]"`, `-l` | 433ms | 219ms | rg 2.0x faster |
| Regex `"^.{10,50}$"`, `-l` | 167ms | 250ms | **agrep 1.5x faster** |

Three distinct performance regimes emerge:

1. **Fixed strings**: agrep wins by 1.06x-1.36x. Both tools use SIMD-accelerated literal search, but agrep's raw `getdents64` walker and Linux-specific I/O path give it an edge.

2. **Output-heavy workloads**: rg wins by 1.3x-1.4x. Rust's stdio machinery is faster for high-volume line output.

3. **Regex patterns**: rg wins by 2x-16x. This is the gap this article explains.

The most striking result is the last row: `^.{10,50}$` is a regex where **agrep is 1.5x faster**. This pattern matches almost every line in every file (most lines are between 10-50 characters), so both engines must process every byte through the regex automaton. There is no shortcut. In this I/O-saturated regime, agrep's faster file walker dominates.

The contrast with `[0-9][a-z][0-9][a-z]` (16x slower) is the key insight: that pattern has sparse matches across 62K files, and rg can skip most of the input using a prefilter. agrep cannot -- it feeds every byte through Go's NFA.

---

## 2. How Regex Engines Work: NFA vs DFA

Every regular expression is compiled into an **automaton** -- a state machine that processes input one byte (or character) at a time. There are two fundamental types:

### Nondeterministic Finite Automaton (NFA)

An NFA can be in **multiple states simultaneously**. For the pattern `[0-9][a-z]`, the NFA looks like:

```
        [0-9]         [a-z]
  (q0) -------> (q1) -------> (q2 accept)
   ^
   |  (also stays in q0 for non-matching bytes)
   +---
```

When processing input `x3f`, the NFA simulation proceeds:

```
Byte 'x': states = {q0}           -- 'x' not in [0-9], stay in q0
Byte '3': states = {q0, q1}       -- '3' is in [0-9], enter q1; also stay in q0
Byte 'f': states = {q0, q2}       -- 'f' is in [a-z], q1->q2 (accept!); also stay in q0
```

At each byte, the NFA must track **every possible state** the automaton could be in. This is called the **Thompson NFA simulation** (after Ken Thompson, who invented this technique in 1968). The cost per byte is O(S) where S is the number of states in the NFA.

For a simple pattern like `[0-9][a-z]`, S is small (3 states). But for patterns with alternation and quantifiers like `(cat|car|cab){2,5}`, S can grow substantially. The worst case is O(n * m) where n is the input length and m is the number of NFA states.

### Deterministic Finite Automaton (DFA)

A DFA is always in **exactly one state**. It preprocesses the NFA to compute, for every (state, byte) pair, the single next state. The resulting transition table has O(S^2) entries in the worst case, but each input byte requires exactly one table lookup.

For `[0-9][a-z]`, the DFA transition table:

```
         [0-9]    [a-z]    other
  q0:    q1       q0       q0
  q1:    q1       q2*      q0       (* = accept)
```

Processing `x3f`:

```
Byte 'x': state = q0 -> q0    (one lookup)
Byte '3': state = q0 -> q1    (one lookup)
Byte 'f': state = q1 -> q2    (one lookup, accept!)
```

The cost per byte is O(1) -- a single array index. This is dramatically faster than the NFA simulation. However, building the full DFA transition table upfront can require exponential memory. The classic example is the pattern `.*a.{20}` -- the DFA for this pattern has 2^21 (2 million+) states because the DFA must "remember" whether it saw `a` in any of the last 20 positions.

### Lazy DFA (Hybrid Approach)

The **lazy DFA** (also called "on-demand DFA" or "hybrid NFA/DFA") computes DFA states as they are needed during matching, caching them for reuse. It combines the O(1)-per-byte speed of a DFA with bounded memory usage:

1. Start with the initial DFA state (which represents the NFA's start state set).
2. For each input byte, check if the transition from the current state on this byte has already been computed.
3. If yes (cache hit): follow the cached transition. Cost: one hash table lookup, approximately O(1).
4. If no (cache miss): run the Thompson NFA simulation for this one byte to compute the next state set, cache the result, and continue.

For typical inputs, the lazy DFA reaches a "steady state" where all transitions are cached after processing a few hundred bytes. From that point on, it runs at DFA speed (one lookup per byte). The cache uses bounded memory -- when it fills up, it is flushed and rebuilt as needed.

This is the core engine behind Rust's `regex` crate and is the primary reason its raw automaton speed exceeds Go's NFA.

---

## 3. Go's regexp Package: Thompson NFA Simulation

**Source:** `regexp/exec.go` in the Go standard library.

Go's `regexp` package implements a **pure Thompson NFA simulation** with a one-pass optimization for certain simple patterns. It does not build a DFA, does not cache states between bytes, and does not have a lazy DFA mode.

### The Execution Loop

The core of Go's regex execution is a loop that, for each byte of input, iterates over all currently active NFA threads (states):

```
for each byte in input:
    for each active thread:
        compute next state(s) from this thread
        if any thread reaches an accept state:
            record match
```

The key characteristics:

1. **O(n * m) per scan**: For each of the n input bytes, up to m NFA states must be checked. For simple patterns, m is small (2-5 states). For patterns with many alternatives or quantifiers, m can be 50-100+.

2. **No state caching**: The NFA state set is recomputed from scratch at every byte position. There is no transition table, no memoization, no DFA construction. Each byte position is independent.

3. **Guaranteed linear time**: The Thompson NFA simulation is O(n * m) for all inputs. It never experiences the exponential blowup that backtracking engines (Perl, PCRE, Java) can suffer on pathological patterns like `(a?){30}a{30}`.

4. **No SIMD prefilter**: The engine does not analyze the pattern for extractable literals. It does not use `bytes.Index` or any SIMD routine during the match loop. Every byte goes through the NFA simulation.

### The onepass Optimization

Go's `regexp` has one important optimization: for patterns that can be determined to have exactly one possible match at each position (no ambiguity in the NFA), it compiles a "one-pass" matcher that runs in O(n) instead of O(n * m). But this only works for a small subset of patterns -- typically simple literal prefixes or character classes without quantifiers.

### The Literal Prefix Optimization

Go's `regexp` does extract a literal prefix when one exists. For a pattern like `ERROR.*port`, the engine recognizes that any match must start with `ERROR` and uses `bytes.Index` (which is AVX2-accelerated) to find candidates before entering the NFA simulation:

```go
// Simplified from regexp/exec.go
if re.prefix != "" {
    advance := bytes.Index(input[pos:], re.prefixBytes)
    if advance < 0 {
        break // No more candidates
    }
    pos += advance
    // Now run NFA from pos
}
```

This is a form of prefiltering, but it only works when the pattern has a literal prefix. Patterns like `[0-9][a-z]`, `(cat|dog)`, or `.*error` have no literal prefix, so the optimization does not apply. The NFA simulation runs on every byte.

---

## 4. Rust's regex Crate: Hybrid NFA/DFA with Prefilters

Rust's `regex` crate (maintained by Andrew Gallick, ripgrep's author) implements a layered architecture where pattern matching is not a single algorithm but a cascade of increasingly expensive strategies. The key insight is: **most bytes in a typical grep workload do not participate in any match**, so the fastest path is to skip non-matching regions without engaging the expensive automaton at all.

The architecture, from cheapest to most expensive:

```
Input bytes
    |
    v
[1. Prefilter: SIMD literal search]  <-- processes 32 bytes/cycle
    |                                      skips 99%+ of input
    | (candidate position found)
    v
[2. Lazy DFA verification]            <-- processes 1 byte/cycle
    |                                      runs only at candidates
    | (DFA cache miss or complex state)
    v
[3. Bounded NFA backtracker]          <-- processes 1 byte/cycle, higher constant
    |                                      runs only for complex patterns
    | (needs capture groups)
    v
[4. PikeVM NFA simulation]            <-- most expensive, full capture support
```

For a pattern like `err(or|no|code)`:
- The prefilter extracts the literal prefix `err` and searches for it using SIMD `memchr`/`memmem` at 10+ GB/s.
- At each candidate position where `err` is found, the lazy DFA runs for a few bytes to verify the full match.
- On a typical C header file, `err` might appear every few thousand bytes, so the prefilter skips 99.9% of the input.

For a pattern like `[0-9][a-z][0-9][a-z]`:
- There is no literal prefix, but the prefilter can still extract byte classes. The Teddy algorithm (section 7) searches for candidate first bytes from the set `[0-9]` using SIMD.
- At each digit found, the lazy DFA runs to check if `[a-z][0-9][a-z]` follows.
- Digits are sparse in C headers, so again most of the input is skipped.

For `^.{10,50}$`:
- The `^` anchor means matches only occur at line beginnings.
- The `.{10,50}$` requires counting characters to the line end.
- Every line is a candidate. The prefilter cannot help.
- The lazy DFA must process every byte.
- In this regime, the raw engine speed matters, and I/O becomes the bottleneck.

---

## 5. Literal Extraction from Regex ASTs

The `regex-syntax` crate (a dependency of `regex`) provides the literal extraction machinery. It walks the abstract syntax tree (AST) of the parsed regex and extracts strings that must appear in any match.

### Extraction Rules

The extraction follows the structure of the AST:

**Concatenation**: `abc` extracts the literal `"abc"`.

**Alternation**: `cat|car|cab` extracts the common prefix `"ca"` and the individual suffixes `"t"`, `"r"`, `"b"`. It can also extract the full set `{"cat", "car", "cab"}` when the set is small enough.

**Character classes**: `[aeiou]` extracts the set of bytes `{a, e, i, o, u}`. This is used as a prefilter byte set -- if the first byte of the pattern must be one of these, only positions containing one of these bytes need to be checked.

**Repetition**: `a+` extracts the literal `"a"` as a required prefix. `a*` extracts nothing (zero repetitions are possible). `a{3,}` extracts `"aaa"`.

**Anchors**: `^` and `$` are noted but do not contribute to literal extraction. They constrain where the matcher looks (line starts and ends) but don't add to the prefilter byte set.

**Dot**: `.` matches any byte, so it contributes nothing to literal extraction. A pattern like `.*error` has no literal prefix (the `.*` consumes everything), but `error` is extracted as a required interior literal.

### Example: `err(or|no|code)`

```
AST:
  Concat
    Literal("err")
    Group
      Alternation
        Literal("or")
        Literal("no")
        Literal("code")
```

Extraction produces:
- **Required prefix**: `"err"` (3 bytes)
- **Full literal set** (if used as multi-pattern prefilter): `{"error", "errno", "errcode"}`
- **Prefilter choice**: Since `"err"` is a 3-byte common prefix, the engine can use a single `memmem` search for `"err"` and verify the suffix at each candidate.

### Example: `[0-9][a-z][0-9][a-z]`

```
AST:
  Concat
    Class([0-9])
    Class([a-z])
    Class([0-9])
    Class([a-z])
```

Extraction produces:
- **No literal prefix** (the first element is a character class, not a literal).
- **First byte set**: `{0x30..0x39}` (the bytes for '0'-'9').
- **Prefilter choice**: Search for any byte in `{0x30..0x39}` using multi-byte `memchr` and verify at each candidate. This is much cheaper than running the full NFA on every byte.

### Example: `^.{10,50}$`

```
AST:
  Concat
    Anchor(Start)
    Repetition(., 10..50)
    Anchor(End)
```

Extraction produces:
- **No literals, no useful byte set**: `.` matches everything, anchors are non-literal.
- **Prefilter choice**: None. The engine falls back to the full automaton.

---

## 6. The Prefilter Cascade

Once literals are extracted, the engine chooses a prefilter strategy based on what was found. The choice follows a cascade from most to least efficient:

### Priority 1: Single Literal Prefix

If the pattern has a literal prefix of 2+ bytes (like `err` from `err(or|no|code)`), the engine uses **memmem** (SIMD-accelerated substring search).

This is conceptually identical to what agrep's `BoyerMooreMatcher` does with `bytes.Index` -- scan the input at 10+ GB/s, only engaging the automaton at candidate positions.

Throughput: 10-30 GB/s depending on pattern length and input data.

### Priority 2: Small Literal Set

If the pattern produces a small set of complete literals (like `{"error", "errno", "errcode"}`), the engine uses **Teddy** (section 7) -- a SIMD multi-pattern matcher.

Teddy can search for up to 8 patterns of up to 16 bytes each, processing 32 bytes of input per SIMD iteration. It's similar in purpose to Aho-Corasick but uses SIMD shuffles instead of an automaton walk.

Throughput: 5-15 GB/s depending on pattern set size.

### Priority 3: First-Byte Class

If the pattern starts with a character class (like `[0-9]` from `[0-9][a-z][0-9][a-z]`), the engine uses **memchr** variants to search for any byte in the class.

- Single byte: `memchr` (searches for one byte using AVX2, 32 positions per iteration)
- 2-3 bytes: `memchr2`/`memchr3` (searches for any of 2-3 bytes simultaneously)
- Larger class: Falls back to a 256-bit bitmask lookup or Teddy with first-byte fingerprints.

For `[0-9]` (10 possible bytes), the engine might search for a few representative bytes using `memchr3` or use Teddy with the byte set as fingerprints.

Throughput: 10-20 GB/s for `memchr`, 5-10 GB/s for class searches.

### Priority 4: No Prefilter

If no useful literals or byte classes can be extracted, the engine falls through to the raw lazy DFA or NFA on every byte.

Throughput: 0.5-2 GB/s depending on pattern complexity.

### The Prefilter Contract

Every prefilter must satisfy one invariant: **it must never skip a true match**. The prefilter is allowed to produce false positives (candidate positions that turn out not to be real matches), but it must never produce false negatives. This means:

1. The prefilter finds a candidate position.
2. The full automaton verifies the candidate.
3. If the candidate is a false positive, the prefilter resumes searching from after the candidate.
4. If the candidate is a real match, the match is recorded and the prefilter resumes after the match.

This two-phase design means the prefilter's throughput is the dominant cost, because on typical grep workloads, 99%+ of the input is in "scanning" mode where the prefilter is running.

---

## 7. Teddy: SIMD Multi-Pattern Prefiltering

Teddy is the most distinctive algorithm in ripgrep's prefilter layer. It was designed specifically for the problem of searching for a small set of short patterns in a large input, using SIMD shuffles to test all patterns at all positions simultaneously.

### The Problem

Given patterns `{"error", "errno", "errcode"}` and a 1MB input buffer, find all positions where any pattern starts. A naive approach checks each pattern at each position -- O(n * k) where k is the number of patterns. Aho-Corasick reduces this to O(n) but processes one byte per iteration. Teddy processes 32 bytes per iteration using AVX2.

### The Core Idea: Byte Fingerprints

Teddy works by reducing each pattern to a "fingerprint" based on selected byte positions, then using SIMD shuffle instructions to test whether the input contains any fingerprint at any of 32 positions simultaneously.

**Step 1: Select fingerprint bytes.** Choose 1-3 byte positions from the patterns to form the fingerprint. For patterns of length >= 2, use the first two bytes. For `{"error", "errno", "errcode"}`:

```
Pattern    Byte 0    Byte 1
"error"    'e'       'r'
"errno"    'e'       'r'
"errcode"  'e'       'r'
```

All three patterns share the same first two bytes. This is a degenerate case (the common prefix `er` means a single `memmem` would be more efficient). For a more interesting example, consider `{"cat", "dog", "fox"}`:

```
Pattern    Byte 0    Byte 1
"cat"      'c'       'a'
"dog"      'd'       'o'
"fox"      'f'       'o'
```

**Step 2: Build nibble masks.** Each byte is split into its low nibble (bits 0-3) and high nibble (bits 4-7). For each position (byte 0, byte 1), two 16-byte lookup tables are constructed:

For byte 0 low nibbles:
```
'c' = 0x63 -> low nibble 0x3 -> table[0x3] |= (1 << 0)  (pattern 0: "cat")
'd' = 0x64 -> low nibble 0x4 -> table[0x4] |= (1 << 1)  (pattern 1: "dog")
'f' = 0x66 -> low nibble 0x6 -> table[0x6] |= (1 << 2)  (pattern 2: "fox")
```

For byte 0 high nibbles:
```
'c' = 0x63 -> high nibble 0x6 -> table[0x6] |= (1 << 0)
'd' = 0x64 -> high nibble 0x6 -> table[0x6] |= (1 << 1)
'f' = 0x66 -> high nibble 0x6 -> table[0x6] |= (1 << 2)
```

**Step 3: PSHUFB search.** For each 32-byte chunk of input, Teddy uses the `VPSHUFB` (packed shuffle bytes) instruction to simultaneously look up all 32 bytes in the nibble tables.

`VPSHUFB` takes two 256-bit registers:
- The **data register** contains 32 bytes of input (each byte's low nibble selects which table entry to retrieve).
- The **table register** contains the 16-entry lookup table (broadcast to fill both 128-bit lanes).

The result is a 256-bit register where each byte contains the pattern bitmask for that nibble value.

The algorithm:
1. Load 32 bytes of input.
2. Extract low nibbles of all 32 bytes (mask with `0x0F`).
3. `VPSHUFB` with the low-nibble table -> 32-byte result `lo_result`.
4. Extract high nibbles of all 32 bytes (shift right by 4).
5. `VPSHUFB` with the high-nibble table -> 32-byte result `hi_result`.
6. AND the two results: `candidates = lo_result & hi_result`.
7. Any non-zero byte in `candidates` indicates that the corresponding input byte matches a pattern's fingerprint at that position.

**Step 4: Multi-byte verification.** For 2-byte fingerprints, the process is repeated for byte position 1 (shifted by one byte in the input), and the results are ANDed:

```
candidates = byte0_candidates & shift(byte1_candidates, 1)
```

Non-zero bytes in `candidates` indicate positions where both the first and second bytes of some pattern match. These are candidate positions that are then verified with a scalar memcmp.

### Cost Analysis

- **Per iteration**: 32 input bytes processed with ~8-12 SIMD instructions (loads, nibble extracts, 2-4 VPSHUFB, ANDs, movemask).
- **False positive rate**: For random data with k patterns, approximately k/256 per byte position (one nibble match is 1/16, two nibbles ANDed is 1/256, times k patterns). For k=8 patterns, about 3% of positions are false positives -- cheaply eliminated by scalar memcmp.
- **Throughput**: ~5-15 GB/s, depending on the number of patterns and false positive rate.

### Limitations

- Maximum ~8 patterns (limited by the pattern bitmask fitting in a byte).
- Maximum ~16 bytes per pattern for fingerprinting (limited by the PSHUFB table size).
- Patterns shorter than 2 bytes degenerate to single-byte memchr.
- Very short patterns with common first bytes (all starting with `e`, for example) produce many false positives.

When the pattern set exceeds Teddy's limits, the engine falls back to Aho-Corasick with a SIMD start-byte prefilter.

---

## 8. memchr: Single and Multi-Byte SIMD Search

The `memchr` crate provides SIMD-accelerated routines for searching for individual bytes or small sets of bytes. These are the lowest-level building blocks of the prefilter cascade.

### memchr (single byte)

Searches for one byte in a buffer. The AVX2 implementation:

```
1. Broadcast the target byte to all 32 lanes of a YMM register.
2. For each 32-byte chunk:
   a. Load 32 bytes of input.
   b. VPCMPEQB: compare all 32 bytes simultaneously (result: 0xFF for match, 0x00 for mismatch).
   c. VPMOVMSKB: extract a 32-bit bitmask (one bit per byte, set if match).
   d. If bitmask is non-zero, compute position from trailing zeros.
```

This is essentially the same algorithm agrep uses in `internal/simd/simd.go` for `IndexByte`. Both agrep and ripgrep achieve similar throughput here because Go's standard library `bytes.IndexByte` already uses AVX2 assembly.

### memchr2 and memchr3 (2-3 bytes)

Searches for any of 2 or 3 target bytes simultaneously. For `memchr2(buf, 'a', 'e')`:

```
1. Broadcast 'a' to ymm0 and 'e' to ymm1.
2. For each 32-byte chunk:
   a. Load 32 bytes into ymm2.
   b. VPCMPEQB ymm2, ymm0 -> ymm3 (matches for 'a')
   c. VPCMPEQB ymm2, ymm1 -> ymm4 (matches for 'e')
   d. VPOR ymm3, ymm4 -> ymm5 (matches for either)
   e. VPMOVMSKB ymm5 -> bitmask
```

The key insight: ORing the comparison results is a single instruction. Searching for 2-3 bytes costs only 1-2 extra instructions per 32-byte chunk compared to searching for 1 byte. The throughput is nearly identical to single-byte `memchr`.

This is important for regex prefiltering because character classes like `[aeiou]` can be decomposed: if the class has 2-3 high-frequency representatives, `memchr2`/`memchr3` can quickly find candidate positions.

### Comparison with agrep

agrep's SIMD layer (`internal/simd/`) implements the same `IndexByte` and `Count` algorithms using Go 1.26's `simd/archsimd` package:

```go
// internal/simd/simd.go
func IndexByte(data []byte, c byte) int {
    needle := archsimd.BroadcastUint8x32(c)
    for i := 0; i+32 <= len(data); i += 32 {
        chunk := archsimd.LoadUint8x32Slice(data[i:])
        mask := chunk.Equal(needle)
        b := mask.ToBits()
        if b != 0 {
            return i + bits.TrailingZeros32(b)
        }
    }
    // scalar tail...
}
```

This achieves the same performance as Rust's `memchr` for single-byte search. The gap only appears when a prefilter is needed but does not exist in the Go code path.

---

## 9. Why the Gap Depends on the Pattern

The benchmark results reveal a clear pattern: the rg/agrep performance ratio depends almost entirely on how much work the prefilter can skip.

### Quantifying Prefilter Effectiveness

Define the **prefilter skip ratio** as the fraction of input bytes that the prefilter skips (does not send to the automaton). For a 1MB file:

| Pattern | Prefilter Type | Skip Ratio | rg speedup |
|---|---|---|---|
| `"define"` (literal) | memmem | >99.9% | n/a (both use SIMD) |
| `err(or\|no\|code)` | memmem on `"err"` | >99% | 16x |
| `[0-9][a-z][0-9][a-z]` | memchr on `[0-9]` | ~95% | 16x |
| `[aeiou]{2}[^aeiou]{2}[aeiou]` | memchr on `[aeiou]` | ~60% | 2x |
| `^.{10,50}$` | none | 0% | 0.67x (agrep faster) |

The relationship is approximately:

```
rg speedup ≈ 1 / (1 - skip_ratio + skip_ratio / SIMD_throughput_ratio)
```

When the skip ratio is high (>99%), rg approaches its SIMD scan speed (~10 GB/s) while agrep is stuck at NFA speed (~0.5 GB/s). The ratio is 10/0.5 = 20x, close to the observed 16x.

When the skip ratio is moderate (~60%, as with `[aeiou]` which appears in ~40% of ASCII bytes), the speedup drops to ~2x because rg must still run the automaton on nearly half the bytes.

When the skip ratio is 0%, both tools run their automaton on every byte. The automaton speed difference (lazy DFA vs Thompson NFA) is modest (~1.5-2x), and agrep's faster I/O can overcome it.

### The Sparse Match Effect

The skip ratio is not just a function of the pattern -- it depends on the **data distribution**. Consider `[0-9]` as a first-byte prefilter:

- In C header files: digits are rare (~2-5% of bytes). Skip ratio: ~95%.
- In CSV data files: digits might be 30-40% of bytes. Skip ratio: ~60%.
- In binary data: bytes are uniformly distributed. Skip ratio: ~96% (10/256).

This is why the `[aeiou]` benchmark shows only a 2x gap: vowels are common in English text and C identifiers (~40% of bytes contain a vowel), so the prefilter cannot skip much.

---

## 10. agrep's Existing Literal Optimization

agrep already handles the most important case: when the user's pattern is a literal string (no regex metacharacters), the factory routes it to `BoyerMooreMatcher`, which uses SIMD-accelerated search.

**File:** `internal/matcher/factory.go`

```go
func NewMatcher(patterns []string, fixed bool, usePCRE bool, ignoreCase bool,
                invert bool, opts MatcherOpts) (Matcher, error) {
    // ...
    // Optimization: if all patterns are literal strings (no regex metacharacters),
    // use BoyerMooreMatcher / AhoCorasickMatcher for SIMD-accelerated search.
    allLiteral := true
    for _, p := range patterns {
        if !isLiteral(p) {
            allLiteral = false
            break
        }
    }
    if allLiteral {
        if len(patterns) == 1 {
            m := NewBoyerMooreMatcher(patterns[0], ignoreCase, invert)
            // ...
        }
        m := NewAhoCorasickMatcher(patterns, ignoreCase, invert)
        // ...
    }
    // Fall through to RegexMatcher
}
```

The `isLiteral` function checks for regex metacharacters:

```go
func isLiteral(pattern string) bool {
    return !strings.ContainsAny(pattern, `\.+*?()|[]{}^$`)
}
```

This means `agrep "define"` automatically uses `BoyerMooreMatcher` (SIMD) instead of `RegexMatcher` (NFA), giving the same performance as if the user had passed `-F`. This is why agrep beats rg on fixed-string benchmarks -- both tools use SIMD search, but agrep's walker is faster.

The gap only appears when the pattern contains metacharacters, forcing the `RegexMatcher` path.

---

## 11. Approaches to Closing the Gap

There are several strategies for reducing the regex performance gap, each with different trade-offs:

### Approach 1: Regex Literal Prefix Extraction

**Idea**: Before compiling a regex with Go's `regexp`, parse the pattern and extract the longest literal prefix. If one exists, use `BoyerMooreMatcher` (SIMD) to find candidates, then verify each candidate with the full regex.

**Example**:

```
Pattern:  "ERROR.*port\s+\d+"
Prefix:   "ERROR" (5 bytes)
Strategy: BoyerMoore("ERROR") -> candidate positions -> regexp.Match(candidate_line)
```

**Complexity**: Moderate. Requires a regex parser (Go's `regexp/syntax` package provides one) and logic to extract the prefix. The two-phase approach (SIMD scan + NFA verify) is straightforward to implement.

**Limitations**: Only helps patterns with literal prefixes. Patterns like `[0-9].*error` have no literal prefix. Patterns like `(cat|dog)` have no common prefix.

**Expected improvement**: Dramatic for patterns like `ERROR.*`, `func\s+\w+`, `import\s+.*`. No improvement for character class patterns.

### Approach 2: Required Substring Extraction

**Idea**: Extract not just prefixes but any required substring from the pattern. For `.*error.*port`, the substring `error` must appear in every match.

**Example**:

```
Pattern:   ".*error.*port"
Substring: "error" (longest required literal)
Strategy:  BoyerMoore("error") -> candidate lines -> regexp.Match(line)
```

**Complexity**: Higher than prefix extraction. The `regexp/syntax` package's AST must be walked to find required literals at any position, not just the start. Must correctly handle alternation (where different branches may not share a common substring).

**Limitations**: Multi-branch patterns like `(cat|dog)` might not have a shared substring. Patterns consisting entirely of character classes have no extractable substring.

**Expected improvement**: Handles more patterns than pure prefix extraction.

### Approach 3: First-Byte Class Prefilter

**Idea**: If the pattern starts with a character class, search for any byte in that class using SIMD, then verify at candidate positions.

**Example**:

```
Pattern:     "[0-9][a-z][0-9][a-z]"
First class: [0-9] = bytes 0x30-0x39
Strategy:    SIMD scan for any of {0x30..0x39} -> candidate positions -> regexp.Match
```

**Complexity**: Requires parsing the first element of the regex AST to determine the set of possible first bytes. The SIMD scan can use a 256-bit bitmask (one bit per possible byte value) to test 32 input bytes simultaneously:

```go
// Conceptual implementation
func indexByteClass(data []byte, class [256]bool) int {
    // Build 256-bit bitmask from class
    // For each 32-byte chunk, test all bytes against bitmask using SIMD
}
```

Go 1.26's `archsimd` does not directly provide this operation, but it can be built from `BroadcastUint8x32`, `VPCMPEQB`, and `VPOR` -- searching for the few most common class members (like `memchr3` does).

**Limitations**: Only helps when the first character class is selective. `[a-z]` matches ~40% of bytes in C code, so the skip ratio would only be ~60%.

### Approach 4: Hybrid SIMD + NFA per Line

**Idea**: Instead of running the NFA on the entire file buffer, first SIMD-scan for lines that could possibly contain a match (using extracted literals or byte classes), then run the NFA only on candidate lines.

```
For each 4KB block:
    SIMD scan for required bytes/literals
    If no candidates found: skip entire block
    Else: split into lines, run regex on candidate lines only
```

**Complexity**: Moderate, and composable with approaches 1-3. The key is that even a coarse prefilter (high false positive rate) can eliminate blocks that definitely have no match.

### Approach 5: Port a Faster Regex Engine

**Idea**: Implement a lazy DFA in pure Go, similar to Rust's `regex-automata`.

**Complexity**: Very high. A production-quality lazy DFA with proper cache management, Unicode support, and capture groups is thousands of lines of carefully optimized code. The Rust `regex` crate represents many person-years of engineering.

**Expected improvement**: 2-4x on patterns where prefilters don't help (the raw DFA speed advantage). The prefilter optimization (approaches 1-4) provides a much larger improvement (10-16x) for the common case and is dramatically less complex to implement.

### Recommendation

The pragmatic path is a combination of approaches 1 and 2: **extract the longest required literal from the regex pattern and use it as a SIMD prefilter**. This covers the majority of real-world regex patterns (most contain at least one required literal substring) and requires only a few hundred lines of code built on top of the existing `regexp/syntax` parser and `BoyerMooreMatcher`.

---

## 12. Regex Prefiltering: A Worked Example

To make the prefilter concept concrete, here is a detailed walkthrough of how a hypothetical `PrefilterRegexMatcher` would process the pattern `err(or|no|code)` against a file buffer.

### Step 1: Parse and Extract

Parse the pattern with `regexp/syntax`:

```go
import "regexp/syntax"

re, _ := syntax.Parse("err(or|no|code)", syntax.Perl)
// AST:
//   Concat
//     Literal("err")
//     Capture
//       Alternation
//         Literal("or")
//         Literal("no")
//         Literal("code")
```

Walk the AST to find the longest required literal. The concatenation starts with `Literal("err")` -- this is a required prefix because every match must begin with `err`.

Extracted prefix: `"err"` (3 bytes).

### Step 2: Build the Two-Phase Matcher

```go
type PrefilterRegexMatcher struct {
    prefix  []byte          // extracted literal: "err"
    re      *regexp.Regexp  // compiled full regex
    scanner *BoyerMooreMatcher // SIMD prefilter for prefix
}
```

### Step 3: MatchExists (files-only mode)

For `agrep -l "err(or|no|code)"`, we need to know if any match exists in the file:

```go
func (m *PrefilterRegexMatcher) MatchExists(data []byte) bool {
    // Phase 1: SIMD scan for "err" prefix
    offset := 0
    for {
        idx := bytes.Index(data[offset:], m.prefix)
        if idx < 0 {
            return false // No more candidates
        }
        candidate := offset + idx

        // Phase 2: Verify with full regex at candidate position
        // Find the line containing the candidate
        lineStart := bytes.LastIndexByte(data[:candidate], '\n') + 1
        lineEnd := bytes.IndexByte(data[candidate:], '\n')
        if lineEnd < 0 {
            lineEnd = len(data) - candidate
        }
        line := data[lineStart : candidate+lineEnd]

        if m.re.Match(line) {
            return true
        }
        offset = candidate + len(m.prefix)
    }
}
```

### Step 4: Execution Trace

Input buffer (simplified):
```
#define FOO 1\n
#include <errno.h>\n
int errcode = get_error();\n
```

**SIMD Phase 1**: `bytes.Index(data, "err")` scans at ~10 GB/s.
- Skips `#define FOO 1\n#include <` (27 bytes) -- no `err` found.
- Finds `err` at position 27 (in `errno`).

**NFA Phase 2**: Run `regexp.Match("errno.h>")` against `err(or|no|code)`.
- The NFA matches `errno` (the `no` alternative). Match found.
- Return `true` immediately.

**Cost**: 27 bytes scanned by SIMD (~3 cycles), 8 bytes verified by NFA (~50 cycles). Total: ~53 cycles for the match, versus ~250 cycles if the NFA had to process all 27 + 8 = 35 bytes.

On a real 100KB file where `err` appears every ~5000 bytes, SIMD processes 95% of the bytes and the NFA only processes ~1000 bytes total. The effective throughput approaches the SIMD scan speed.

---

## 13. The DFA Advantage Beyond Prefilters

Even when no prefilter is possible, Rust's lazy DFA is faster than Go's Thompson NFA. This section explains why.

### State Caching in Practice

Consider matching `[aeiou]{2}[^aeiou]{2}[aeiou]` against English text. The NFA has 6 states. The lazy DFA will encounter a small number of distinct (state, byte) transitions:

After processing a few hundred bytes of text, the lazy DFA's transition cache contains entries like:

```
(start, 'a') -> state_1    (vowel seen)
(start, 'b') -> start      (consonant, reset)
(state_1, 'e') -> state_2  (two vowels)
(state_2, 'b') -> state_3  (first consonant)
...
```

There are at most 6 states * 256 byte values = 1,536 possible transitions. After the cache is populated, every byte requires only a table lookup -- no NFA simulation.

Go's Thompson NFA, by contrast, recomputes the state set at every byte. For 6 NFA states, each byte requires iterating over all active states (typically 2-4) and computing their transitions. The per-byte cost is roughly 4x higher than a cached DFA lookup.

### Measured Impact

From the benchmarks, on the `[aeiou]{2}[^aeiou]{2}[aeiou]` pattern (where rg can't prefilter much because vowels are common):

- agrep: 433ms (NFA on every byte)
- rg: 219ms (lazy DFA, transitions cached after first few lines)
- Ratio: **2.0x**

This 2x factor is the raw DFA-vs-NFA advantage, separate from any prefilter benefit. It is consistent with the theoretical analysis: cached table lookup vs multi-state NFA simulation.

---

## 14. Benchmark Results and Analysis

### Complete Results Table

Test corpus: `/usr/include`, 62,419 files, Intel Xeon E-2176M @ 2.70GHz, 12 threads.

| # | Scenario | Pattern | agrep | rg | Ratio | Primary Factor |
|---|---|---|---|---|---|---|
| 1 | Fixed, `-l` | `"define"` | 164ms | 198ms | 1.21x agrep | Walker speed |
| 2 | Fixed, `-i -l` | `"define"` | 173ms | 235ms | 1.36x agrep | SIMD case folding |
| 3 | Fixed, `-c` | `"define"` | 227ms | 260ms | 1.14x agrep | Walker speed |
| 4 | Multi, `-l` | 3 patterns | 215ms | 229ms | 1.06x agrep | Walker speed |
| 5 | Fixed, output | `"define"` | 357ms | 271ms | 1.32x rg | Rust stdio |
| 6 | Fixed, `-n` | `"define"` | 422ms | 295ms | 1.43x rg | Rust stdio + linenum |
| 7 | No match, `-l` | impossible | 162ms | 140ms | 1.16x rg | DFA skip |
| 8 | Regex, `-l` | `err(or\|no\|code)` | 2,628ms | 164ms | 16x rg | Prefilter on `"err"` |
| 9 | Regex, `-l` | `[0-9][a-z][0-9][a-z]` | 2,982ms | 186ms | 16x rg | memchr `[0-9]` |
| 10 | Regex, `-l` | `[aeiou]{2}[^aeiou]{2}[aeiou]` | 433ms | 219ms | 2.0x rg | Lazy DFA (prefilter weak) |
| 11 | Regex, `-l` | `^.{10,50}$` | 167ms | 250ms | 1.5x agrep | No prefilter, I/O bound |

### Performance Regime Map

```
                    Prefilter skip ratio
                    0%            50%            99%+
                    |              |              |
agrep faster  <----+--------------+--------------+----> rg faster
                    |              |              |
                ^.{10,50}$    [aeiou]{2}..   err(or|no|code)
                (1.5x agrep)  (2x rg)      (16x rg)
```

The crossover point -- where agrep and rg are approximately equal -- occurs around a skip ratio of ~30-40%. Below this, agrep's I/O advantage dominates. Above this, rg's prefilter advantage dominates.

### Where agrep Wins

1. **Files-only mode (`-l`)**: agrep's raw `getdents64` + `O_NOATIME` walker traverses the filesystem faster than rg's `walkdir` (Rust's cross-platform directory walker). This is a ~10-20% advantage.

2. **Case-insensitive search (`-i`)**: agrep's `IndexCaseInsensitive` uses a custom AVX2 Horspool with SIMD case folding, while rg falls back to a more general approach. This gives agrep a ~36% advantage.

3. **Patterns that match almost everything**: When the regex matches nearly every line (like `^.{10,50}$`), no prefilter can help, and the I/O-bound nature of the workload means agrep's faster walker wins.

### Where rg Wins

1. **Any regex pattern with extractable literals**: The prefilter+lazy DFA combination gives rg a 10-16x advantage. This is the dominant case for real-world regex grep usage.

2. **Output formatting**: rg's Rust-native buffered writer is ~1.3-1.4x faster than agrep's Go output path for high-volume line output.

3. **No-match full scan**: rg is ~16% faster even when scanning all files with no matches. This reflects the lazy DFA's lower per-byte cost compared to Go's NFA (even with the NFA processing zero matches, it has higher overhead per file).

---

## 15. Key Takeaways

1. **The prefilter is the single biggest performance differentiator in regex grep.** The difference between scanning at SIMD speed (10+ GB/s) and running an NFA simulation (0.5 GB/s) is 20x. On workloads where the prefilter can skip 99%+ of the input, this translates directly to a 13-16x end-to-end speedup.

2. **Literal extraction from regex ASTs is the enabling technology.** The prefilter can only work if the engine can determine, at compile time, what bytes or strings must appear in any match. This is a static analysis problem on the regex parse tree, not a runtime optimization.

3. **The lazy DFA provides a secondary 2x advantage.** Even when no prefilter is possible, a cached DFA lookup is roughly twice as fast as a Thompson NFA simulation per byte. This is significant but smaller than the prefilter effect.

4. **Fixed-string search is already at parity.** Both agrep and rg use SIMD-accelerated literal search for fixed strings. agrep's advantage in this regime comes from I/O, not from the search algorithm.

5. **The gap is pattern-dependent, not fundamental.** agrep beats rg on several workloads (fixed strings, case-insensitive, near-universal match patterns). The 16x gap is specific to regex patterns with extractable literals -- exactly the case where a prefilter could help but doesn't exist in agrep's code path.

6. **Pragmatic prefiltering closes most of the gap.** Extracting the longest required literal substring from a regex and using it as a SIMD prefilter would eliminate the 16x gap for the majority of real-world patterns, without the complexity of implementing a lazy DFA. This is the highest-leverage optimization available for agrep's regex path.

---

## 16. Cross-References

- [01: SIMD and AVX2](01-simd-and-avx2.md) -- The SIMD primitives (`IndexByte`, `Index`, Horspool) that would power a regex prefilter, plus the `archsimd` API used throughout agrep.
- [04: String Search Algorithms](04-string-search-algorithms.md) -- The Matcher interface, search-then-split architecture, and BoyerMooreMatcher that already provides SIMD-accelerated literal search. A regex prefilter would compose with this infrastructure.
- [06: GC and Allocation Optimization](06-gc-and-allocation-optimization.md) -- The `[][2]int` vs `[][]int` optimization that reduced AhoCorasick allocations by 99.9%.
- [07: Benchmarking and Profiling](07-benchmarking-and-profiling.md) -- How to run the benchmarks, use `hyperfine` for end-to-end comparison, and interpret throughput numbers.

### External References

- **Andrew Gallick, "regex-automata" crate**: The Rust regex engine that implements the literal extraction, prefilter cascade, and lazy DFA described in this article.
- **Russ Cox, "Regular Expression Matching Can Be Simple And Fast"** (2007): The foundational article on Thompson NFA simulation, which Go's `regexp` package implements.
- **Ken Thompson, "Regular Expression Search Algorithm"** (1968): The original paper describing NFA simulation for regex matching.
- **Andrew Gallick, "Teddy" algorithm**: Originally described in Intel's Hyperscan project, adapted for ripgrep's regex crate. Uses PSHUFB for multi-pattern SIMD search.
- **Intel Hyperscan**: The production regex engine (used in network intrusion detection) where Teddy was first implemented at scale. Open-sourced by Intel.
