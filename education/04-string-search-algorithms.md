# String Search and Pattern Matching Algorithms in agrep

This document is an exhaustive reference on the pattern matching algorithms used in agrep. It covers the abstract interface, the architectural decision that dominates performance (search-then-split), each concrete matcher implementation, the SIMD acceleration layer, the selection heuristic, context matching, and the pointer-free data model. Every concept is explained from first principles, with file paths and code drawn directly from the codebase.

---

## Table of Contents

1. [The Matcher Interface](#1-the-matcher-interface)
2. [The Search-Then-Split Architecture](#2-the-search-then-split-architecture)
3. [Line Extraction and Snippet Resolution](#3-line-extraction-and-snippet-resolution)
4. [Incremental Line Number Computation](#4-incremental-line-number-computation)
5. [Match Deduplication](#5-match-deduplication)
6. [BoyerMooreMatcher: Fixed String Search](#6-boyermorematcher-fixed-string-search)
7. [The SIMD Acceleration Layer](#7-the-simd-acceleration-layer)
8. [AhoCorasickMatcher: Multi-Pattern Search](#8-ahocorasickmatcher-multi-pattern-search)
9. [RegexMatcher: RE2 Engine](#9-regexmatcher-re2-engine)
10. [PCREMatcher: Perl-Compatible Regex](#10-pcrematcher-perl-compatible-regex)
11. [Matcher Selection Heuristic](#11-matcher-selection-heuristic)
12. [ContextMatcher: Before/After Lines](#12-contextmatcher-beforeafter-lines)
13. [Inversion: The `-v` Flag](#13-inversion-the--v-flag)
14. [The MatchSet Data Model](#14-the-matchset-data-model)
15. [Counting Lines Without Allocating Matches](#15-counting-lines-without-allocating-matches)
16. [Performance Analysis](#16-performance-analysis)
17. [Algorithmic Complexity Summary](#17-algorithmic-complexity-summary)
18. [Cross-References](#18-cross-references)

---

## 1. The Matcher Interface

**File:** `internal/matcher/match.go`

All pattern matching in agrep is abstracted behind a single interface:

```go
type Matcher interface {
    FindAll(data []byte) MatchSet
    MatchExists(data []byte) bool
    CountAll(data []byte) int
    FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool)
}
```

This interface defines four methods, each representing a progressively cheaper operation:

### FindAll -- Full Match Extraction

`FindAll(data []byte) MatchSet` receives the entire file contents as a byte slice and returns every match with full metadata: line numbers, byte offsets, line content boundaries, and highlight positions. This is the most expensive operation because it must build a complete `MatchSet` with `Match` structs and position arrays.

This method is used when the output needs to display matching lines with their line numbers and highlighted match positions -- the default output mode.

### CountAll -- Line Counting

`CountAll(data []byte) int` counts how many lines contain at least one match, without constructing `Match` structs or extracting line content. It only needs to determine line boundaries around match offsets, not extract the content or compute highlight positions.

This method is used by `agrep -c` (count mode). It avoids allocating `Match` structs entirely.

### MatchExists -- Early Return

`MatchExists(data []byte) bool` returns `true` as soon as it finds the first match in the data, without any extraction. It does not count, does not resolve line boundaries, and does not build any data structures. It simply answers: "Is there at least one match?"

This method is used by `agrep -l` (files-with-matches mode) and `agrep -L` (files-without-matches mode). Once a match is found, the entire file can be skipped. For a 100MB file where the pattern appears on line 3, `MatchExists` might examine only the first few hundred bytes.

### FindLine -- Single Line Matching

`FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool)` checks a single line for matches. The caller provides the line content, its 1-based line number, and its byte offset in the file. This method exists specifically for the `ContextMatcher`, which must process lines individually to track context windows.

### Why Four Methods?

A simpler design would have a single `FindAll` method, with callers inspecting the result for existence or counting. But each method enables fundamentally different optimizations:

- `MatchExists` can halt at the first match. For `bytes.Index` or the Aho-Corasick automaton, this means returning the moment the first occurrence is found, potentially skipping 99.99% of the file.
- `CountAll` avoids allocating `Match` structs and position arrays entirely. It only needs to track line boundaries.
- `FindAll` does the full work only when the output actually needs match metadata.

This stratification means that `agrep -l` (list matching files) operates at a fundamentally different cost than `agrep` (display matches). The interface makes this explicit.

---

## 2. The Search-Then-Split Architecture

This is the single most important algorithmic decision in agrep. It is the primary reason the tool achieves multi-gigabyte-per-second throughput on common workloads.

### The Traditional Approach: Split-Then-Search

Most grep implementations, and the natural first instinct, follow this pattern:

```
file bytes  -->  split into lines  -->  for each line: search for pattern
```

The process:
1. Read the file into memory.
2. Scan through the entire buffer to find newline characters, splitting it into individual lines.
3. For each line, run the pattern search algorithm.
4. Collect matches.

For a 1MB file with 20,000 lines and 5 matches, this approach performs 20,000 individual pattern search operations. Each search, even for a highly optimized algorithm, has overhead: function call, loop setup, boundary checks. With 20,000 calls, that overhead dominates.

Worse, the line-splitting pass itself must touch every byte of the file to find newlines, which is O(n) work before any searching even begins.

### agrep's Approach: Search-Then-Split

agrep inverts the order:

```
file bytes  -->  search entire buffer  -->  for each match offset: extract line boundaries
```

The process:
1. Read the file into memory.
2. Search the entire buffer as a single contiguous byte slice.
3. The search returns a list of byte offsets where matches occur.
4. For each offset, extract the line boundaries around it.

For the same 1MB file with 20,000 lines and 5 matches, this approach performs 1 search operation (on the whole 1MB buffer) plus 5 line boundary extractions.

### Why This Is Faster

**The no-match case dominates real workloads.** When searching a codebase, the vast majority of files do not contain the pattern at all. With search-then-split, a file with no matches requires exactly one search pass -- and modern SIMD-accelerated search (`bytes.Index` with AVX2) processes 32 bytes per CPU cycle. A 1MB file with no matches takes roughly 30 microseconds.

With split-then-search, that same 1MB file requires splitting into 20,000 lines (touching every byte to find newlines) and then 20,000 search calls, each with function call overhead. Even if each search is fast, the overhead of 20,000 iterations adds up.

**The sparse-match case is nearly as good.** For a file with 5 matches out of 20,000 lines, search-then-split does one full-buffer search plus 5 tiny local scans (finding the nearest newline before and after each match offset). The full-buffer search is the dominant cost, and it operates at SIMD speeds.

**The dense-match case is the worst case but still acceptable.** If every line matches (e.g., searching for "e" in English text), search-then-split finds 20,000 offsets and does 20,000 line extractions -- the same work as split-then-search. But even in this case, it is not slower, because the line extractions are cheap forward/backward scans for the nearest newline.

### The Key Insight

The search operation benefits enormously from operating on large, contiguous buffers. SIMD instructions process 32 bytes at a time, and branch prediction works well for long sequential scans. The line extraction operations are trivially cheap -- finding the nearest newline before and after a known offset is a bounded scan of at most a few hundred bytes (controlled by `maxCols`).

By doing the expensive operation (pattern search) on the bulk data and the cheap operation (newline finding) on small windows, agrep maximizes throughput.

### Measured Impact

On the agrep benchmark suite (440KB data, 10K lines):

| Scenario | Throughput | What Happens |
|----------|-----------|--------------|
| NoMatch | 9.7 GB/s | One SIMD scan, zero line extractions |
| SparseMatch (1 in 1000 lines) | 7.5 GB/s | One SIMD scan, ~10 line extractions |
| DenseMatch (every line) | ~190 MB/s | One SIMD scan, 10,000 line extractions + Match allocations |

The 50x throughput difference between no-match and dense-match shows clearly how much of the cost comes from line extraction and Match struct creation, not from the search itself.

---

## 3. Line Extraction and Snippet Resolution

**File:** `internal/matcher/lineindex.go`

Once a search returns a match offset (a byte position in the file buffer), agrep must determine the line that contains that offset. This is the job of `snippetFromOffset`.

### The snippetFromOffset Function

```go
func snippetFromOffset(data []byte, off int, maxCols int) (snippetStart int, snippetLen int, posInSnippet int) {
    n := len(data)

    // Determine search bounds
    var lo, hi int
    if maxCols > 0 {
        lo = off - maxCols
        if lo < 0 {
            lo = 0
        }
        hi = off + maxCols
        if hi > n {
            hi = n
        }
    } else {
        lo = 0
        hi = n
    }

    // Find line start: last '\n' before off within [lo, off)
    lineStart := lo
    if i := bytes.LastIndexByte(data[lo:off], '\n'); i >= 0 {
        lineStart = lo + i + 1
    }

    // Find line end: first '\n' at or after off within [off, hi)
    lineEnd := hi
    if i := bytes.IndexByte(data[off:hi], '\n'); i >= 0 {
        lineEnd = off + i
    }

    return lineStart, lineEnd - lineStart, off - lineStart
}
```

### How It Works

Given a byte offset `off` where a match was found in `data`, the function:

1. **Establishes a search window.** If `maxCols > 0`, the window is `[off - maxCols, off + maxCols]`. If `maxCols <= 0`, the window is the entire buffer.

2. **Finds the line start.** Scans backward from `off` (within the window) for the last `\n` character. The line starts at the byte after that newline. If no newline is found within the window, the line starts at the window's lower bound.

3. **Finds the line end.** Scans forward from `off` (within the window) for the first `\n` character. The line ends at that newline. If no newline is found within the window, the line ends at the window's upper bound.

4. **Returns three values:**
   - `snippetStart`: the byte offset of the line's start in `data`
   - `snippetLen`: the length of the line in bytes
   - `posInSnippet`: the position of the match within the line (for highlighting)

### The maxCols Bound

The `maxCols` parameter (default 75) prevents pathological behavior on files with extremely long lines. Consider a 100KB single-line JSON file where a match occurs at offset 50,000. Without `maxCols`, the backward scan would search 50,000 bytes for a newline that does not exist, and the forward scan would search another 50,000 bytes. That is 100KB of scanning per match.

With `maxCols = 75`, the backward scan checks at most 75 bytes and the forward scan checks at most 75 bytes -- 150 bytes total regardless of line length. The output will show a truncated snippet of the line rather than the full 100KB line, which is the correct behavior for terminal display.

When `maxCols` is set to 0 (full-line mode), the function resolves complete line boundaries by scanning the entire buffer in both directions. This is used when the caller needs exact line content, not truncated snippets.

### Why bytes.LastIndexByte and bytes.IndexByte?

These are not hand-written loops. Go's `bytes.IndexByte` and `bytes.LastIndexByte` are implemented with SSE2/AVX2 assembly on amd64. They scan 16 or 32 bytes per iteration. Even the "cheap" line extraction step is SIMD-accelerated.

---

## 4. Incremental Line Number Computation

**File:** `internal/matcher/lineindex.go`, function `matchSetFromOffsets`

Computing line numbers is one of the hidden costs in a grep tool. The naive approach -- counting all newlines from the start of the file to the match offset -- is O(file size) per match. For a file with 1000 matches, that would be O(1000 * file size).

agrep uses incremental computation. It maintains a running line number counter and only counts the newlines between consecutive match offsets:

```go
matches := make([]Match, 0, len(offsets))
lineNum := 1
prevOff := 0

for _, off := range offsets {
    snippetStart, snippetLen, posInSnippet := snippetFromOffset(data, off, maxCols)

    if needLineNums {
        lineNum += bytes.Count(data[prevOff:off], []byte{'\n'})
        prevOff = off
    }

    // ... build Match struct using lineNum ...
}
```

### Complexity Analysis

If match offsets are at positions `o_1, o_2, ..., o_k` in a file of size `n`:

- **Naive approach:** `bytes.Count(data[0:o_i], '\n')` for each match `i`. Total work: `O(o_1 + o_2 + ... + o_k)`, which in the worst case is `O(k * n)`.

- **Incremental approach:** `bytes.Count(data[o_{i-1}:o_i], '\n')` for each match `i`. Total work: `O((o_1 - 0) + (o_2 - o_1) + ... + (o_k - o_{k-1}))` = `O(o_k)`. In the worst case (matches at start and end), this is `O(n)`. In the common case (clustered matches), it is much less.

The incremental approach guarantees that line number computation is at most `O(n)` total, regardless of the number of matches.

### The needLineNums Optimization

The `needLineNums` field is set based on whether the output format actually requires line numbers. When it is `false` (e.g., in `agrep -l` mode or when line numbers are not displayed), the `bytes.Count` call is skipped entirely and all matches get `lineNum = 1`. This saves a significant amount of work for modes that do not display line numbers.

---

## 5. Match Deduplication

**File:** `internal/matcher/lineindex.go`, function `matchSetFromOffsets`

When multiple match offsets fall on the same line, agrep must not create duplicate `Match` structs. Consider searching for "the" in the line `"the quick brown the lazy"`. The search finds offsets at positions 0 and 16, but both are on the same line. The output should show the line once, with two highlight positions.

agrep handles this by tracking the start position of the last snippet:

```go
lastSnippetStart := -1

for _, off := range offsets {
    snippetStart, snippetLen, posInSnippet := snippetFromOffset(data, off, maxCols)

    // ... line number computation ...

    posIdx := len(positions)
    positions = append(positions, [2]int{posInSnippet, posInSnippet + patternLen})

    if snippetStart == lastSnippetStart {
        // Same line as previous match -- extend its position range
        last := &matches[len(matches)-1]
        last.PosCount = posIdx - last.PosIdx + 1
    } else {
        matches = append(matches, Match{
            LineNum:    lineNum,
            LineStart:  snippetStart,
            LineLen:    snippetLen,
            ByteOffset: int64(snippetStart),
            PosIdx:     posIdx,
            PosCount:   1,
        })
        lastSnippetStart = snippetStart
    }
}
```

### How It Works

1. Each match offset produces a `snippetStart` (the byte offset of the line's beginning).
2. If `snippetStart` equals the previous match's `snippetStart`, both matches are on the same line.
3. For same-line matches, the code extends the existing `Match`'s `PosCount` to include the new highlight position, rather than creating a new `Match`.
4. The highlight positions are stored consecutively in the shared `Positions` array. The `Match`'s `PosIdx` and `PosCount` define a contiguous range into this array.

This works because match offsets from `IndexAll` are sorted in ascending order. Matches on the same line will always be consecutive in the offset list, so the `lastSnippetStart` check catches all duplicates.

### Why Snippet Start Instead of Line Number?

Using `snippetStart` instead of `lineNum` for deduplication is a subtle correctness detail. When `needLineNums` is `false`, all matches have `lineNum = 1`. If deduplication used line number, all matches would be merged into a single `Match`. By using `snippetStart` (a byte offset that is always correct), deduplication works regardless of whether line numbers are computed.

---

## 6. BoyerMooreMatcher: Fixed String Search

**File:** `internal/matcher/boyermoore.go`

For single fixed patterns (the most common grep use case), agrep uses `BoyerMooreMatcher`. Despite its name, the actual search is delegated to optimized lower-level functions rather than implementing the classic Boyer-Moore algorithm directly.

### The Boyer-Moore Algorithm: Background

The Boyer-Moore algorithm (1977) is one of the most important string search algorithms. Its key insight: instead of scanning the text left-to-right and comparing with the pattern left-to-right (like the naive O(nm) approach), Boyer-Moore compares characters from right to left within the pattern and uses precomputed tables to skip positions.

**Bad character rule:** When a mismatch occurs at position `j` in the pattern (comparing right-to-left), and the mismatched character in the text is `c`, shift the pattern to align the rightmost occurrence of `c` in the pattern with position `j`. If `c` does not appear in the pattern at all, shift the entire pattern past the mismatch.

**Good suffix rule:** When a mismatch occurs after matching a suffix `s` of the pattern, shift the pattern to align with the next occurrence of `s` in the pattern (preceded by a different character). If no such occurrence exists, shift to align with the longest prefix of the pattern that is also a suffix of `s`.

These rules enable sublinear average-case performance. For a pattern of length `m` and text of length `n`, Boyer-Moore achieves O(n/m) comparisons in the best case -- it can skip over characters in the text that it never examines.

### The Horspool Simplification

The Boyer-Moore-Horspool algorithm (1980) uses only the bad character rule applied to the last character of the pattern (or the text character aligned with the end of the pattern). This simplification loses the good suffix rule but is easier to implement and often performs similarly in practice because:

1. The bad character shift from the last position is usually sufficient for good performance.
2. The simpler code has better branch prediction and instruction-level parallelism.
3. For short patterns (typical in grep), the good suffix rule rarely provides additional benefit.

### What agrep Actually Does

Rather than implementing Boyer-Moore or Horspool directly, agrep delegates to optimized primitives:

```go
type BoyerMooreMatcher struct {
    pattern      []byte
    patternLow   []byte // lowered pattern for case-insensitive
    ignoreCase   bool
    invert       bool
    maxCols      int
    needLineNums bool
}
```

**Case-sensitive search** uses `simd.IndexAll(data, pattern)`, which internally calls `bytes.Index` -- Go's standard library implementation that uses hand-tuned AVX2 assembly on amd64. The Go stdlib has invested significant effort into this codepath, and benchmarks showed it was 3x faster than a hand-written Horspool for no-match and sparse-match cases.

**Case-insensitive search** uses `simd.IndexAllCaseInsensitive(data, patternLower)`, which implements a custom AVX2 first+last byte prefilter (a Horspool-style algorithm). There is no stdlib equivalent for case-insensitive search, so this is where custom SIMD provides genuine benefit.

### The Three Method Implementations

**FindAll** -- Full extraction:

```go
func (m *BoyerMooreMatcher) FindAll(data []byte) MatchSet {
    if m.invert {
        return m.findAllInvert(data)
    }

    var offsets []int
    if m.ignoreCase {
        offsets = simd.IndexAllCaseInsensitive(data, m.patternLow)
    } else {
        offsets = simd.IndexAll(data, m.patternLow)
    }
    return matchSetFromOffsets(data, offsets, len(m.patternLow), m.maxCols, m.needLineNums)
}
```

This is the search-then-split pattern in action. `simd.IndexAll` searches the entire buffer and returns all match offsets. `matchSetFromOffsets` resolves those offsets into line boundaries and builds the `MatchSet`.

**MatchExists** -- Early return:

```go
func (m *BoyerMooreMatcher) MatchExists(data []byte) bool {
    if m.invert {
        return len(data) > 0
    }
    if m.ignoreCase {
        return simd.IndexCaseInsensitive(data, m.patternLow) >= 0
    }
    return simd.Index(data, m.patternLow) >= 0
}
```

Uses `simd.Index` (singular) rather than `simd.IndexAll`. `Index` returns as soon as it finds the first occurrence. For `agrep -l` on a large file where the pattern appears on line 3, this might examine only a few hundred bytes.

**CountAll** -- Counting without allocation:

```go
func (m *BoyerMooreMatcher) CountAll(data []byte) int {
    if m.invert {
        return countInvert(data, func(line []byte) bool {
            if m.ignoreCase {
                return simd.IndexCaseInsensitive(line, m.patternLow) < 0
            }
            return simd.Index(line, m.patternLow) < 0
        })
    }

    if m.ignoreCase {
        return countUniqueLines(data, simd.IndexAllCaseInsensitive(data, m.patternLow))
    }
    return countUniqueLines(data, simd.IndexAll(data, m.patternLow))
}
```

Gets all offsets with `IndexAll`, then counts unique lines with `countUniqueLines` (see [Section 15](#15-counting-lines-without-allocating-matches)). No `Match` structs are created.

### Why Not Implement Boyer-Moore Directly?

Go's `bytes.Index` already uses an optimized Rabin-Karp / SIMD hybrid on amd64:

1. For short patterns (1-7 bytes), it uses `IndexByte` to find the first byte, then verifies the rest.
2. For longer patterns, it uses a Rabin-Karp rolling hash with SIMD-accelerated scanning.
3. The implementation is in hand-written assembly, highly tuned for modern x86 microarchitectures.

A pure-Go Boyer-Moore implementation, even with SIMD for the comparisons, cannot compete with this hand-tuned assembly for case-sensitive search. The overhead of the Go function call, the shift table lookups, and the branch mispredictions from right-to-left scanning add up.

Custom SIMD only wins for case-insensitive search, where the stdlib has no equivalent. The first+last byte prefilter (see [Section 7](#7-the-simd-acceleration-layer)) is a Horspool-style algorithm that uses AVX2 to check 32 candidate positions simultaneously.

---

## 7. The SIMD Acceleration Layer

**Files:**
- `internal/simd/simd.go` -- single-byte operations (IndexByte, LastIndexByte, Count, ToLowerASCII)
- `internal/simd/index.go` -- multi-byte operations (Index, IndexAll, IndexCaseInsensitive, IndexAllCaseInsensitive)

The SIMD layer provides the low-level search primitives that the matchers use. It is built on Go 1.26's `simd/archsimd` package, which exposes AVX2 intrinsics as Go functions.

### Case-Sensitive: Delegating to bytes.Index

For case-sensitive multi-byte search, the SIMD layer simply delegates:

```go
func Index(data, pattern []byte) int {
    return bytes.Index(data, pattern)
}
```

This is intentional. Go's stdlib `bytes.Index` uses highly optimized AVX2 assembly. In benchmarks, it was faster than any pure-Go SIMD implementation. The wrapper exists so that all search calls go through the `simd` package, keeping the API consistent and making it easy to swap implementations in the future.

### Case-Sensitive IndexAll: Stack Buffer Optimization

`IndexAll` finds all non-overlapping occurrences by repeatedly calling `bytes.Index`:

```go
func IndexAll(data, pattern []byte) []int {
    // ...
    var stackBuf [16]int
    n := 0
    var overflow []int
    i := 0

    for {
        idx := bytes.Index(data[i:], pattern)
        if idx < 0 {
            break
        }
        if n < len(stackBuf) {
            stackBuf[n] = i + idx
        } else {
            if overflow == nil {
                overflow = make([]int, 0, 64)
                overflow = append(overflow, stackBuf[:]...)
            }
            overflow = append(overflow, i+idx)
        }
        n++
        i += idx + plen
    }

    if n == 0 {
        return nil
    }
    if overflow != nil {
        return overflow
    }
    result := make([]int, n)
    copy(result, stackBuf[:n])
    return result
}
```

The stack buffer optimization is significant:

1. **`var stackBuf [16]int`** -- a 128-byte array allocated on the stack. No heap allocation.
2. If there are 16 or fewer matches, all offsets go into `stackBuf`. Only when the result is returned does it allocate a heap slice and copy.
3. If there are 0 matches (the common case for no-match files), `nil` is returned with zero heap allocations.
4. If there are more than 16 matches, it spills to a heap-allocated `overflow` slice.

This means the no-match case (which dominates) has zero allocations. The sparse-match case (typically < 16 matches per file) allocates only the result slice. Only the dense-match case pays for the overflow allocation.

### Single-Byte IndexAll: Full AVX2

For single-byte patterns, `indexAllByte` uses full AVX2 rather than delegating to `bytes.IndexByte`:

```go
func indexAllByte(data []byte, c byte) []int {
    var stackBuf [16]int
    n := 0
    var overflow []int
    needle := archsimd.BroadcastUint8x32(c)
    i := 0

    for i+32 <= len(data) {
        chunk := archsimd.LoadUint8x32Slice(data[i:])
        mask := chunk.Equal(needle)
        b := mask.ToBits()
        for b != 0 {
            j := bits.TrailingZeros32(b)
            // ... store i+j ...
            b &= b - 1  // clear lowest set bit
        }
        i += 32
    }
    // ... scalar tail ...
}
```

The algorithm:
1. `BroadcastUint8x32(c)` creates a 32-byte vector where every byte is `c`.
2. `LoadUint8x32Slice` loads 32 bytes from the data.
3. `Equal` compares each byte, producing a mask where matching positions are set.
4. `ToBits` converts the 32-byte mask to a 32-bit integer (one bit per byte position).
5. `TrailingZeros32` extracts the position of the lowest set bit (first match in the chunk).
6. `b &= b - 1` clears that bit and continues to the next match.

This processes 32 bytes per iteration and extracts multiple matches per iteration without branching per byte.

### Case-Insensitive Search: First+Last Byte Prefilter

This is where custom SIMD provides genuine performance benefit over the standard library. The algorithm is a Horspool-style first+last byte prefilter:

```go
func IndexCaseInsensitive(data, patternLower []byte) int {
    plen := len(patternLower)

    firstLo := patternLower[0]
    firstHi := toUpperASCII(firstLo)
    lastLo := patternLower[plen-1]
    lastHi := toUpperASCII(lastLo)

    bFirstLo := archsimd.BroadcastUint8x32(firstLo)
    bFirstHi := archsimd.BroadcastUint8x32(firstHi)
    bLastLo := archsimd.BroadcastUint8x32(lastLo)
    bLastHi := archsimd.BroadcastUint8x32(lastHi)

    i := 0
    limit := len(data) - plen + 1

    for i+32 <= limit {
        blockFirst := archsimd.LoadUint8x32Slice(data[i:])
        blockLast := archsimd.LoadUint8x32Slice(data[i+plen-1:])

        mFirstLo := blockFirst.Equal(bFirstLo)
        mFirstHi := blockFirst.Equal(bFirstHi)
        mFirst := mFirstLo.Or(mFirstHi)

        mLastLo := blockLast.Equal(bLastLo)
        mLastHi := blockLast.Equal(bLastHi)
        mLast := mLastLo.Or(mLastHi)

        b := mFirst.And(mLast).ToBits()

        for b != 0 {
            j := bits.TrailingZeros32(b)
            if matchCaseInsensitive(data[i+j:i+j+plen], patternLower) {
                return i + j
            }
            b &= b - 1
        }

        i += 32
    }
    // ... scalar tail ...
}
```

The algorithm, step by step:

1. **Precompute broadcast vectors.** For the pattern's first and last bytes, create AVX2 vectors containing the lowercase and uppercase variants. For pattern "Hello", first bytes are `h` and `H`, last bytes are `o` and `O`.

2. **Load two overlapping blocks.** `blockFirst` is 32 bytes starting at position `i`. `blockLast` is 32 bytes starting at position `i + plen - 1`. These represent the first and last character of potential matches at 32 consecutive positions.

3. **Check first byte.** `mFirst` is the OR of matching lowercase or uppercase first bytes. This identifies positions where the first character could match.

4. **Check last byte.** `mLast` is the OR of matching lowercase or uppercase last bytes at the corresponding pattern-end positions.

5. **AND the masks.** `b = mFirst.And(mLast).ToBits()` identifies positions where both the first and last bytes match (case-insensitively). This is the prefilter -- it eliminates the vast majority of positions.

6. **Verify candidates.** For each surviving candidate position (typically very few), do a full byte-by-byte case-insensitive comparison with `matchCaseInsensitive`.

### Why First+Last Byte?

Checking only the first byte would produce too many false positives. If the first byte is 'e', roughly 10-15% of positions in English text would match. Checking both first and last bytes reduces false positives to roughly 0.1-2%, depending on the bytes and the text.

The cost of the SIMD prefilter is essentially fixed per 32-byte block (a handful of SIMD operations). The cost of the full verification is proportional to the number of surviving candidates times the pattern length. For typical patterns and text, the verification cost is negligible because the prefilter eliminates almost all candidates.

### The matchCaseInsensitive Helper

```go
func matchCaseInsensitive(data, patternLower []byte) bool {
    for i, b := range data {
        if toLowerASCII(b) != patternLower[i] {
            return false
        }
    }
    return true
}
```

This is a simple byte-by-byte comparison with ASCII lowering. It is only called on candidate positions that pass the first+last byte prefilter, so it runs very infrequently on the hot path.

### AVX2 ToLowerASCII

**File:** `internal/simd/simd.go`

For bulk lowercasing (used in some internal paths), the SIMD layer provides an AVX2-accelerated `ToLowerASCII`:

```go
func ToLowerASCII(dst, src []byte) {
    vecA := archsimd.BroadcastUint8x32('A')
    vecZ := archsimd.BroadcastUint8x32('Z')
    vec32 := archsimd.BroadcastUint8x32(0x20)
    i := 0

    for i+32 <= n {
        chunk := archsimd.LoadUint8x32Slice(src[i:])
        isUpper := chunk.GreaterEqual(vecA).And(chunk.LessEqual(vecZ))
        lowered := chunk.Add(vec32.Masked(isUpper))
        lowered.StoreSlice(dst[i:])
        i += 32
    }
    // ... scalar tail ...
}
```

This converts uppercase ASCII letters to lowercase 32 bytes at a time:
1. Compare each byte against 'A' and 'Z' to identify uppercase letters.
2. Create a mask with 0x20 only in positions that are uppercase.
3. Add the mask to the original bytes (adding 0x20 to uppercase letters converts them to lowercase; adding 0 to everything else leaves it unchanged).

---

## 8. AhoCorasickMatcher: Multi-Pattern Search

**File:** `internal/matcher/ahocorasick.go`

When multiple fixed patterns are given (e.g., `agrep -F -e 'error' -e 'warning' -e 'fatal'`), agrep uses the Aho-Corasick algorithm. This is a fundamentally different approach from running Boyer-Moore once per pattern -- it searches for all patterns simultaneously in a single pass.

### The Aho-Corasick Algorithm: Background

The Aho-Corasick algorithm (1975) combines a trie (prefix tree) with failure links inspired by the Knuth-Morris-Pratt (KMP) algorithm. It was originally developed at Bell Labs for the `fgrep` Unix utility.

**The problem it solves:** Given a set of `k` patterns with total length `m`, find all occurrences of any pattern in a text of length `n`. The naive approach (running a separate search for each pattern) takes O(k * n) time. Aho-Corasick takes O(n + m + z) time, where `z` is the number of matches -- independent of `k`.

### Data Structure: The Automaton

```go
type acNode struct {
    children [256]*acNode  // 256-way branching for O(1) transitions
    fail     *acNode       // failure link
    output   []int         // indices of patterns that match at this node
    depth    int           // depth in the trie
}

type AhoCorasickMatcher struct {
    root         *acNode
    patterns     [][]byte
    ignoreCase   bool
    invert       bool
    maxCols      int
    needLineNums bool
}
```

Each `acNode` has 256 child pointers (one per possible byte value), a failure link, and a list of pattern indices that match at this node.

**Memory cost:** Each node uses 256 * 8 = 2048 bytes for child pointers alone. For a small number of patterns (typical grep usage -- usually 2-20 patterns), this is acceptable. The total number of nodes equals the sum of pattern lengths (each byte in each pattern creates at most one node), so for patterns totaling 100 bytes, the trie uses roughly 200KB.

**Why 256-way branching?** The alternative is a hash map or sorted array of children. But for byte-level matching, `children[b]` is a single array access -- O(1) with no hash computation or binary search. Since grep patterns are short and few, the 2KB per node is a worthwhile tradeoff for constant-time transitions.

### Phase 1: Trie Construction

```go
func (m *AhoCorasickMatcher) addPattern(pattern []byte, index int) {
    node := m.root
    for _, b := range pattern {
        if node.children[b] == nil {
            node.children[b] = &acNode{depth: node.depth + 1}
        }
        node = node.children[b]
    }
    node.output = append(node.output, index)
}
```

For each pattern, walk from the root, creating nodes as needed. At the final node, record the pattern index in the `output` list. This is standard trie insertion.

Example: Patterns "he", "she", "his", "hers" produce:

```
root -> h -> e (output: "he")
             -> r -> s (output: "hers")
        -> i -> s (output: "his")
     -> s -> h -> e (output: "she")
```

### Phase 2: Failure Link Construction

Failure links are the core innovation of Aho-Corasick. They connect each node to the longest proper suffix of its path that is also a prefix of some pattern. This is analogous to the failure function in the KMP algorithm, extended to a trie.

```go
func (m *AhoCorasickMatcher) buildFailureLinks() {
    queue := make([]*acNode, 0, 256)

    // Depth-1 nodes: fail links point to root
    for i := range 256 {
        child := m.root.children[i]
        if child != nil {
            child.fail = m.root
            queue = append(queue, child)
        }
    }

    // BFS for deeper nodes
    for len(queue) > 0 {
        current := queue[0]
        queue = queue[1:]

        for i := range 256 {
            child := current.children[i]
            if child == nil {
                continue
            }

            queue = append(queue, child)

            // Follow failure links to find longest proper suffix
            fail := current.fail
            for fail != nil && fail.children[i] == nil {
                fail = fail.fail
            }
            if fail == nil {
                child.fail = m.root
            } else {
                child.fail = fail.children[i]
            }

            // Merge output from failure node
            if child.fail != nil && len(child.fail.output) > 0 {
                child.output = append(child.output, child.fail.output...)
            }
        }
    }
}
```

**How failure links are computed:**

For a node at the end of path `"xyz"`, the failure link points to the node at the end of the longest suffix of `"xyz"` that exists in the trie. The algorithm uses BFS (breadth-first search) so that when computing the failure link for a node at depth `d`, all failure links for nodes at depth `< d` have already been computed.

For a node reached by byte `b` from its parent:
1. Start at the parent's failure link.
2. Follow failure links until finding a node that has a child for byte `b`, or reaching the root.
3. If a node with a child for `b` is found, the failure link points to that child.
4. Otherwise, the failure link points to the root.

**Output merging:** The line `child.output = append(child.output, child.fail.output...)` is critical. It ensures that when the automaton reaches a node, the `output` list contains not only patterns that end at this exact node but also patterns that end at any node reachable via failure links. This is called "dictionary suffix links" or "output links" in the literature. Without this optimization, the automaton would need to traverse the entire failure link chain at each position to collect all matches.

### Phase 3: Automaton Walk (The Actual Search)

```go
func (m *AhoCorasickMatcher) searchLine(text []byte) []acMatch {
    var matches []acMatch
    node := m.root

    for i, b := range text {
        if m.ignoreCase {
            b = toLower(b)
        }

        // Follow failure links until we find a matching transition or reach root
        for node != m.root && node.children[b] == nil {
            node = node.fail
        }
        if node.children[b] != nil {
            node = node.children[b]
        }

        // Collect all matches at this position
        if len(node.output) > 0 {
            for _, pidx := range node.output {
                plen := len(m.patterns[pidx])
                matches = append(matches, acMatch{
                    patternIdx: pidx,
                    offset:     i - plen + 1,
                    length:     plen,
                })
            }
        }
    }

    return matches
}
```

For each byte in the text:
1. If the current node has a child for this byte, follow it.
2. If not, follow failure links until finding a node with a child for this byte, or reaching the root.
3. At the new node, check if any patterns match here (the `output` list).
4. If so, record each match with its offset (computed from the current position minus the pattern length plus one).

**Time complexity:** Each byte in the text advances the automaton by at most one step forward (following a child) and potentially several steps backward (following failure links). But the total number of failure link traversals across the entire text is bounded by the total number of forward steps, because each failure link traversal decreases the depth of the current node. This gives an amortized O(1) cost per byte, for a total of O(n + z) where z is the number of matches.

### Note on the searchLine Name

Despite being named `searchLine`, this function actually searches the entire file buffer in the `FindAll` and `CountAll` methods. The name is historical -- it was originally written for per-line search. The search-then-split pattern means it receives the full buffer, not individual lines.

### Early Exit for MatchExists

```go
func (m *AhoCorasickMatcher) MatchExists(data []byte) bool {
    if m.invert {
        return len(data) > 0
    }
    node := m.root
    for _, b := range data {
        if m.ignoreCase {
            b = toLower(b)
        }
        for node != m.root && node.children[b] == nil {
            node = node.fail
        }
        if node.children[b] != nil {
            node = node.children[b]
        }
        if len(node.output) > 0 {
            return true
        }
    }
    return false
}
```

This is identical to `searchLine` but returns `true` immediately on the first match. For `agrep -l` with multiple patterns, this is extremely efficient -- it processes each byte at most once and stops as soon as any pattern matches.

---

## 9. RegexMatcher: RE2 Engine

**File:** `internal/matcher/regex.go`

For patterns containing regex metacharacters (`\.+*?()|[]{}^$`), agrep uses Go's `regexp` package, which implements the RE2 algorithm.

### RE2: Background

RE2 (Regular Expression 2) was developed by Russ Cox at Google. Its defining characteristic is that it guarantees linear-time matching: O(n) for a text of length n, regardless of the pattern. This is in contrast to PCRE-style backtracking engines which can exhibit exponential-time behavior on pathological inputs (catastrophic backtracking).

RE2 achieves this by using a finite automaton (NFA/DFA) rather than backtracking. The tradeoff: RE2 does not support some PCRE features (lookahead, lookbehind, backreferences, atomic groups) because these features cannot be implemented by a finite automaton in linear time.

### Implementation

```go
type RegexMatcher struct {
    re           *regexp.Regexp
    invert       bool
    maxCols      int
    needLineNums bool
}

func NewRegexMatcher(pattern string, ignoreCase bool, invert bool) (*RegexMatcher, error) {
    if ignoreCase {
        pattern = "(?i)" + pattern
    }
    re, err := regexp.Compile(pattern)
    if err != nil {
        return nil, err
    }
    return &RegexMatcher{re: re, invert: invert}, nil
}
```

Case-insensitive matching is handled by prepending `(?i)` to the pattern, which is RE2's inline flag for case-insensitive mode.

### Search-Then-Split with Regex

```go
func (m *RegexMatcher) FindAll(data []byte) MatchSet {
    if m.invert {
        return m.findAllInvert(data)
    }

    locs := m.re.FindAllIndex(data, -1)  // Search entire buffer
    if len(locs) == 0 {
        return MatchSet{}
    }

    return matchSetFromLocs(data, locs, m.maxCols, m.needLineNums)
}
```

The same search-then-split pattern applies. `FindAllIndex(data, -1)` searches the entire file buffer and returns `[][]int` -- a slice of `[start, end]` pairs for each match. `matchSetFromLocs` then resolves those locations into line boundaries.

Note that `matchSetFromLocs` (as opposed to `matchSetFromOffsets` used by BoyerMoore) handles variable-length matches. A regex like `ERR\w+` can match strings of different lengths, so each match has both a start and end position.

### Literal Prefix Optimization in RE2

Go's `regexp` package internally performs literal prefix optimization. For a pattern like `ERROR.*port`, the engine recognizes that `ERROR` is a literal prefix and uses `bytes.Index` (AVX2-accelerated) to find candidates before running the full NFA. This means simple regex patterns with literal prefixes get a significant portion of the SIMD benefit automatically.

### MatchExists and CountAll

```go
func (m *RegexMatcher) MatchExists(data []byte) bool {
    if m.invert {
        return len(data) > 0
    }
    return m.re.Match(data)
}

func (m *RegexMatcher) CountAll(data []byte) int {
    if m.invert {
        return countInvert(data, func(line []byte) bool {
            return !m.re.Match(line)
        })
    }
    locs := m.re.FindAllIndex(data, -1)
    return countLocsUniqueLines(data, locs)
}
```

`MatchExists` uses `regexp.Match`, which can stop at the first match without extracting positions. `CountAll` uses `countLocsUniqueLines`, a variant of `countUniqueLines` that works with `[][]int` location pairs instead of `[]int` offsets.

---

## 10. PCREMatcher: Perl-Compatible Regex

**File:** `internal/matcher/pcre.go`

For patterns requiring PCRE2 features (lookahead `(?=...)`, lookbehind `(?<=...)`, backreferences `\1`, atomic groups `(?>...)`, possessive quantifiers `++`, conditional patterns, etc.), agrep supports PCRE2 via the `go.elara.ws/pcre` package.

### Implementation

```go
type PCREMatcher struct {
    re           *pcre.Regexp
    ignoreCase   bool
    invert       bool
    maxCols      int
    needLineNums bool
}

func NewPCREMatcher(pattern string, ignoreCase bool, invert bool) (*PCREMatcher, error) {
    var opts pcre.CompileOption
    if ignoreCase {
        opts |= pcre.Caseless
    }
    re, err := pcre.CompileOpts(pattern, opts)
    if err != nil {
        return nil, err
    }
    return &PCREMatcher{re: re, ignoreCase: ignoreCase, invert: invert}, nil
}
```

### Same Search-Then-Split Pattern

```go
func (m *PCREMatcher) FindAll(data []byte) MatchSet {
    if m.invert {
        return m.findAllInvert(data)
    }
    locs := m.re.FindAllIndex(data, -1)
    if len(locs) == 0 {
        return MatchSet{}
    }
    return matchSetFromLocs(data, locs, m.maxCols, m.needLineNums)
}
```

The code structure is identical to `RegexMatcher`. The `go.elara.ws/pcre` package provides the same `FindAllIndex` API as Go's `regexp` package, so the integration is seamless.

### Resource Management

```go
func (m *PCREMatcher) Close() {
    if m.re != nil {
        m.re.Close()
    }
}
```

Unlike Go's `regexp.Regexp`, the PCRE matcher holds resources that must be explicitly released. The `Close` method releases the compiled PCRE regex.

### Known Issues

The `go.elara.ws/pcre` package uses `modernc.org/libc`, which is a C standard library transpiled to Go. This transpiled code performs pointer arithmetic that triggers Go's `checkptr` instrumentation under the race detector. This means:

- PCRE tests crash under `go test -race`. The test suite uses `AGREP_SKIP_PCRE=1` to skip PCRE tests when the race detector is enabled.
- PCRE benchmarks crash due to a GC finalizer SIGSEGV. PCRE is excluded from `make bench`.
- The Makefile handles this automatically: it runs race-enabled tests first (skipping PCRE), then PCRE tests separately without `-race`.

PCRE is only activated when the user explicitly passes the `-P` flag. It is never auto-selected by the factory.

---

## 11. Matcher Selection Heuristic

**File:** `internal/matcher/factory.go`

The `NewMatcher` factory function selects the most efficient matcher for the given patterns:

```go
func NewMatcher(patterns []string, fixed bool, usePCRE bool, ignoreCase bool,
                invert bool, opts MatcherOpts) (Matcher, error) {
    if len(patterns) == 0 {
        return nil, fmt.Errorf("no patterns provided")
    }

    if usePCRE           -> PCREMatcher
    if fixed && len==1   -> BoyerMooreMatcher (SIMD-accelerated)
    if fixed && len>1    -> AhoCorasickMatcher (multi-pattern)
    if all patterns are literal (no metacharacters):
        if len==1        -> BoyerMooreMatcher
        if len>1         -> AhoCorasickMatcher
    else                 -> RegexMatcher (RE2)
}
```

### The Decision Tree

1. **PCRE flag (`-P`):** If the user explicitly requests PCRE, use `PCREMatcher`. Multiple patterns are combined with `|` and grouped with `(?:...)` to form a single alternation pattern.

2. **Fixed flag (`-F`):** If the user explicitly declares fixed strings:
   - Single pattern: `BoyerMooreMatcher` (fastest for single-string search).
   - Multiple patterns: `AhoCorasickMatcher` (single-pass multi-pattern).

3. **Automatic literal detection:** If no explicit flags, check if all patterns are literal (no metacharacters):
   - If yes, use `BoyerMooreMatcher` or `AhoCorasickMatcher` as above.
   - If no, use `RegexMatcher`.

4. **Regex fallback:** For patterns with metacharacters, use `RegexMatcher`. Multiple patterns are combined with `|` alternation.

### The isLiteral Function

```go
func isLiteral(pattern string) bool {
    return !strings.ContainsAny(pattern, `\.+*?()|[]{}^$`)
}
```

This function checks if a pattern contains any regex metacharacters. If not, the pattern can be treated as a fixed string and searched with the much faster SIMD-accelerated `BoyerMooreMatcher`.

This means `agrep 'hello'` (without any flags) automatically uses `BoyerMooreMatcher` instead of `RegexMatcher`, because `hello` contains no metacharacters. The user gets SIMD-accelerated search without needing to pass `-F`.

### Why This Matters

The performance difference between matchers is dramatic:

| Matcher | No-Match Throughput | Algorithm |
|---------|-------------------|-----------|
| BoyerMooreMatcher | 9.7 GB/s | SIMD bytes.Index |
| AhoCorasickMatcher | ~500 MB/s | Automaton walk (1 byte/iteration) |
| RegexMatcher | ~200-800 MB/s | RE2 NFA/DFA |
| PCREMatcher | ~100-400 MB/s | PCRE2 backtracking |

Automatically detecting literal patterns and routing them to `BoyerMooreMatcher` gives a 10-50x speedup for the common case of searching for a plain string.

---

## 12. ContextMatcher: Before/After Lines

**File:** `internal/matcher/context.go`

The `ContextMatcher` wraps any inner `Matcher` to add `-B` (before), `-A` (after), and `-C` (context) support. This is the one place where agrep uses the split-then-search approach rather than search-then-split.

### Why Context Requires Split-Then-Search

Context matching requires knowing the surrounding lines, not just the matching lines. If a match is on line 10 and `-B 3` is requested, lines 7, 8, and 9 must also be included in the output. These lines were not found by the pattern search -- they are context.

To know which lines are context, you must first split the file into lines, then determine which lines match, then expand the match set to include surrounding lines. This is inherently a split-then-search operation.

### Implementation

```go
type ContextMatcher struct {
    inner  Matcher
    before int
    after  int
}

func NewContextMatcher(inner Matcher, before, after int) Matcher {
    if before == 0 && after == 0 {
        return inner
    }
    return &ContextMatcher{inner: inner, before: before, after: after}
}
```

Note the optimization: if both `before` and `after` are 0, the wrapper is not created and the inner matcher is returned directly. This avoids the overhead of context handling when it is not needed.

### The FindAll Implementation

The `FindAll` method proceeds in three phases:

**Phase 1: Split into lines.**

```go
type lineInfo struct {
    start int
    len   int
}
var lines []lineInfo
offset := 0
remaining := data
for len(remaining) > 0 {
    idx := bytes.IndexByte(remaining, '\n')
    // ... extract line boundaries ...
    lines = append(lines, lineInfo{start: offset, len: lineLen})
    // ...
}
```

This scans the entire file to find all line boundaries. This is the O(n) pass that search-then-split avoids, but context matching requires it.

**Phase 2: Find matching lines.**

```go
matchSet := make(map[int]matchInfo)
for i, li := range lines {
    line := data[li.start : li.start+li.len]
    ms, ok := m.inner.FindLine(line, i+1, int64(li.start))
    if ok {
        matchSet[i] = matchInfo{ms: ms}
    }
}
```

Each line is checked individually using the inner matcher's `FindLine` method. This is where the per-line search happens.

**Phase 3: Expand to include context and build result.**

```go
include := make(map[int]bool)
for idx := range matchSet {
    for i := idx - m.before; i <= idx+m.after; i++ {
        if i >= 0 && i < len(lines) {
            include[i] = true
        }
    }
}
```

For each matching line, mark the surrounding lines (within the before/after range) for inclusion. Then iterate through all lines in order, outputting included lines with appropriate metadata.

### Group Separators

When there is a gap between context groups (non-contiguous included lines), a separator is inserted:

```go
if lastIncluded >= 0 && i > lastIncluded+1 && len(result.Matches) > 0 {
    result.Matches = append(result.Matches, Match{
        LineNum:   0,
        LineStart: -1,  // sentinel for separator
        LineLen:   0,
        IsContext: true,
    })
}
```

The separator is represented as a `Match` with `LineNum = 0` and `LineStart = -1`. The output formatter checks for this sentinel and renders `--` (matching GNU grep's behavior).

### MatchExists and CountAll Delegation

```go
func (m *ContextMatcher) MatchExists(data []byte) bool {
    return m.inner.MatchExists(data)
}

func (m *ContextMatcher) CountAll(data []byte) int {
    return m.inner.CountAll(data)
}
```

Context does not affect existence checks or counting. `MatchExists` and `CountAll` delegate directly to the inner matcher, using the efficient search-then-split path. Only `FindAll` (which needs to produce output with context lines) pays the cost of split-then-search.

---

## 13. Inversion: The `-v` Flag

All four matchers implement the `-v` (invert match) flag, which selects lines that do NOT match the pattern.

### MatchExists with Inversion

For all matchers, inverted `MatchExists` is trivial:

```go
func (m *BoyerMooreMatcher) MatchExists(data []byte) bool {
    if m.invert {
        return len(data) > 0
    }
    // ...
}
```

If inversion is active, any non-empty data contains at least one non-matching line (or at minimum, the data itself is a non-matching entity). This is O(1).

### FindAll with Inversion

Inverted `FindAll` must split into lines and check each one:

```go
func (m *BoyerMooreMatcher) findAllInvert(data []byte) MatchSet {
    ms := MatchSet{Data: data}
    var offset int64
    lineNum := 1
    remaining := data

    for len(remaining) > 0 {
        idx := bytes.IndexByte(remaining, '\n')
        var lineLen int
        if idx >= 0 {
            lineLen = idx
        } else {
            lineLen = len(remaining)
        }
        lineStart := int(offset)
        line := remaining[:lineLen]

        var found int
        if m.ignoreCase {
            found = simd.IndexCaseInsensitive(line, m.patternLow)
        } else {
            found = simd.Index(line, m.patternLow)
        }
        if found < 0 {
            ms.Matches = append(ms.Matches, Match{
                LineNum:    lineNum,
                LineStart:  lineStart,
                LineLen:    lineLen,
                ByteOffset: offset,
            })
        }

        // ... advance to next line ...
    }

    return ms
}
```

Inverted matching inherently requires split-then-search: you must check each line individually to determine if it does NOT match. You cannot use the search-then-split approach because you need to identify lines where the pattern is absent, not present.

This makes `-v` inherently slower than non-inverted matching. But the per-line search still uses SIMD-accelerated `simd.Index`, so each line check is fast.

### CountAll with Inversion

All matchers share the `countInvert` helper:

```go
func countInvert(data []byte, matchFunc func(line []byte) bool) int {
    count := 0
    for len(data) > 0 {
        idx := bytes.IndexByte(data, '\n')
        var line []byte
        if idx >= 0 {
            line = data[:idx]
            data = data[idx+1:]
        } else {
            line = data
            data = nil
        }
        if matchFunc(line) {
            count++
        }
    }
    return count
}
```

This is a generic split-and-test loop. The `matchFunc` parameter is a closure that returns `true` for lines that should be counted (i.e., lines that do NOT match the pattern).

---

## 14. The MatchSet Data Model

**File:** `internal/matcher/match.go`

The `MatchSet` and `Match` types are carefully designed to minimize garbage collection pressure.

### The Match Struct

```go
type Match struct {
    LineNum    int    // 1-based line number (0 = group separator)
    LineStart  int    // byte offset of line snippet start in MatchSet.Data
    LineLen    int    // length of line snippet in bytes
    ByteOffset int64  // byte offset of line start within the original file
    PosIdx     int    // start index into MatchSet.Positions
    PosCount   int    // number of highlight positions for this match
    IsContext  bool
}
```

**Critical design decision: no pointer fields.** Every field in `Match` is a value type (`int`, `int64`, `bool`). There are no `[]byte` slices, no `string` values, no pointers of any kind.

Why this matters: Go's garbage collector must scan every pointer in the heap to determine which objects are reachable. A `[]Match` with 10,000 entries and pointer fields would create 10,000+ pointers for the GC to scan. With only value types, the GC sees `[]Match` as an opaque blob of integers -- zero pointers to scan.

### The MatchSet Struct

```go
type MatchSet struct {
    Data      []byte    // the file data buffer (mmap or pooled)
    Matches   []Match   // pointer-free match structs
    Positions [][2]int  // shared positions array; each match indexes a sub-range
}
```

`MatchSet` contains exactly three pointer types (slices are pointers internally): `Data`, `Matches`, and `Positions`. Regardless of whether there are 5 or 50,000 matches, the GC scans exactly 3 pointers.

### Zero-Copy Line Access

Line content is never copied into the `Match` struct. Instead, the match stores `LineStart` and `LineLen`, and the actual bytes are retrieved via:

```go
func (ms *MatchSet) LineBytes(i int) []byte {
    m := &ms.Matches[i]
    return ms.Data[m.LineStart : m.LineStart+m.LineLen]
}
```

This returns a sub-slice of `Data` -- no allocation, no copy. The `Data` buffer is the original file contents (either mmap'd or read into a pooled buffer), so line content is accessed at zero cost.

### Shared Positions Array

Highlight positions (the `[start, end]` byte ranges within a line that should be highlighted) are stored in a single shared `Positions` array. Each `Match` has `PosIdx` (start index) and `PosCount` (number of positions), defining a contiguous range:

```go
func (ms *MatchSet) MatchPositions(i int) [][2]int {
    m := &ms.Matches[i]
    if m.PosCount == 0 {
        return nil
    }
    return ms.Positions[m.PosIdx : m.PosIdx+m.PosCount]
}
```

This avoids allocating a separate `[][2]int` slice per match. All positions are contiguous in memory, which is cache-friendly and GC-friendly.

---

## 15. Counting Lines Without Allocating Matches

**File:** `internal/matcher/lineindex.go`

The `countUniqueLines` function is used by `CountAll` to count matching lines without building `Match` structs.

### The Algorithm

```go
func countUniqueLines(data []byte, offsets []int) int {
    if len(offsets) == 0 {
        return 0
    }

    count := 0
    lineEnd := -1

    for _, off := range offsets {
        if off > lineEnd {
            count++
            i := bytes.IndexByte(data[off:], '\n')
            if i >= 0 {
                lineEnd = off + i
            } else {
                lineEnd = len(data)
            }
        }
    }

    return count
}
```

### How It Works

The function maintains `lineEnd`: the byte offset of the end of the current line (the next `\n` after the most recent match). For each offset:

1. If the offset is beyond `lineEnd`, this is a new line. Increment `count` and find the new line end.
2. If the offset is within `lineEnd`, this is the same line as the previous match. Skip it.

This is a single-pass O(n) algorithm where n is the number of offsets. Finding the line end (`bytes.IndexByte`) is a bounded forward scan -- typically just a few bytes until the next newline.

### Why Not Just Count Offsets?

Multiple match offsets can fall on the same line. Searching for "the" in "the quick brown the lazy" produces two offsets (0 and 16), but it is one matching line. `countUniqueLines` correctly counts this as 1, not 2.

### The countLocsUniqueLines Variant

For matchers that produce `[][]int` locations (regex, PCRE, Aho-Corasick), there is an equivalent function:

```go
func countLocsUniqueLines(data []byte, locs [][]int) int {
    // Same algorithm, using loc[0] (match start) instead of off
}
```

---

## 16. Performance Analysis

### Why agrep Is Fast: A Summary

The performance of agrep's pattern matching comes from the combination of several techniques, each addressing a different bottleneck:

1. **Search-then-split** eliminates per-line overhead for the common case (no match or sparse match). The file is searched as a single contiguous buffer, leveraging SIMD.

2. **SIMD-accelerated search** via `bytes.Index` (for case-sensitive) and custom AVX2 first+last byte prefilter (for case-insensitive) processes 32 bytes per cycle.

3. **Automatic literal detection** routes simple patterns to the fastest matcher without requiring the user to pass `-F`.

4. **Tiered methods** (`MatchExists`/`CountAll`/`FindAll`) allow callers to pay only for what they need. `agrep -l` never builds Match structs.

5. **Pointer-free Match structs** eliminate GC scanning overhead for large result sets.

6. **Zero-copy line access** via sub-slices of the original buffer avoids copying line content.

7. **Stack buffer optimization** in `IndexAll` avoids heap allocation for the no-match and sparse-match cases.

8. **Incremental line numbering** avoids redundant newline counting.

9. **Bounded snippet extraction** via `maxCols` prevents pathological behavior on long lines.

### Where Performance Is Lost

1. **Dense matches** (every line matches): The cost of building 10,000+ Match structs and position arrays dominates. This is allocation-bound, not search-bound.

2. **Inverted matching** (`-v`): Must split into lines and check each one individually. Cannot use search-then-split.

3. **Context matching** (`-B`/`-A`/`-C`): Must split into lines to determine context windows. The split cost is O(n).

4. **Regex patterns**: RE2 NFA/DFA is slower than SIMD fixed-string search. Patterns with alternation or character classes cannot be reduced to fixed strings.

5. **Case-insensitive matching**: The custom AVX2 prefilter is fast but still slower than case-sensitive `bytes.Index` because it must check both cases and do a full verification on candidates.

### Benchmark Numbers

Micro-benchmarks (440KB data, 10K lines):

| Scenario | Throughput | Notes |
|----------|-----------|-------|
| NoMatch (BoyerMoore) | 9.7 GB/s | 29x improvement over split-then-search |
| SparseMatch (1 in 1000 lines) | 7.5 GB/s | 41x improvement |
| DenseMatch (every line) | ~190 MB/s | Allocation-bound |

End-to-end benchmarks (37K files, `--no-ignore --hidden -l`):

| Tool | Time | Notes |
|------|------|-------|
| agrep | ~304ms | SIMD + search-then-split |
| ripgrep | ~301ms | Essentially tied |
| agrep (case-insensitive, /usr/include) | 144ms | 1.25x faster than ripgrep (180ms) |

---

## 17. Algorithmic Complexity Summary

| Matcher | Build Time | Search Time | Space |
|---------|-----------|-------------|-------|
| BoyerMooreMatcher | O(m) | O(n) average, O(nm) worst | O(m) |
| AhoCorasickMatcher | O(m * 256) for failure links | O(n + z) | O(m * 256) per node |
| RegexMatcher (RE2) | O(m) compile | O(n) guaranteed | O(2^m) DFA states worst case |
| PCREMatcher | O(m) compile | O(n) average, O(2^n) worst | O(m) |

Where:
- `n` = text length
- `m` = pattern length (or total pattern length for multi-pattern)
- `z` = number of matches

Key observations:
- BoyerMoore has O(n) average but O(nm) worst case. The worst case requires a pathological pattern+text combination (e.g., pattern "aaa" in text "aaaaaaa"). In practice, this almost never occurs with natural text.
- Aho-Corasick's O(n + z) is independent of the number of patterns. This is why it is used for multi-pattern instead of running BoyerMoore `k` times.
- RE2 guarantees O(n) regardless of pattern, at the cost of potentially large DFA state tables for complex patterns.
- PCRE has no O(n) guarantee due to backtracking. Pathological patterns can cause exponential time.

---

## 18. Cross-References

- **SIMD implementation details:** See `01-simd-and-avx2.md` for the AVX2 intrinsics, `archsimd` API, vector operations, and benchmark analysis of when SIMD helps vs. when it does not.
- **GC and allocation optimization:** See `06-gc-and-allocation-optimization.md` for detailed analysis of the pointer-free Match struct design, `sync.Pool` usage, and how to profile allocation pressure.
- **Benchmarking and profiling:** See `07-benchmarking-and-profiling.md` for how to run matcher benchmarks, interpret results, and profile with pprof.

### Source File Index

| File | Purpose |
|------|---------|
| `internal/matcher/match.go` | Matcher interface, Match struct, MatchSet struct |
| `internal/matcher/boyermoore.go` | BoyerMooreMatcher (single fixed pattern, SIMD) |
| `internal/matcher/ahocorasick.go` | AhoCorasickMatcher (multi-pattern automaton) |
| `internal/matcher/regex.go` | RegexMatcher (RE2 engine) |
| `internal/matcher/pcre.go` | PCREMatcher (PCRE2 via pure Go port) |
| `internal/matcher/factory.go` | NewMatcher factory, isLiteral detection |
| `internal/matcher/lineindex.go` | snippetFromOffset, matchSetFromOffsets, countUniqueLines |
| `internal/matcher/context.go` | ContextMatcher (before/after context lines) |
| `internal/simd/simd.go` | AVX2 primitives: IndexByte, LastIndexByte, Count, ToLowerASCII |
| `internal/simd/index.go` | Multi-byte search: Index, IndexAll, IndexCaseInsensitive, IndexAllCaseInsensitive |
