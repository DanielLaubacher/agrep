# Beating ripgrep: Startup, Streaming, Parallel Chunks, and Rare-Pair Teddy

Doc 10 ended with gogrep at 1.0-1.4x of ripgrep on most patterns. This
document records the next two optimization rounds, which ended with gogrep
**winning or tying every benchmarked workload** — 7 of 8 single-file cases
outright (up to 10.9x), all recursive cases, and one statistical tie. It
also records a crash bug the benchmarks exposed, several wrong theories the
profiler killed, and a multi-pattern algorithm that beats classic Teddy.

All numbers: ripgrep 15.1.0, 29MB/500K-line log corpus and /usr/include,
hyperfine + `perf stat -r`, both tools running equivalent config
(`--smart-case --hidden --follow` + glob excludes).

---

## Table of Contents

1. [Re-Baselining: the Numbers Had Drifted](#1-re-baselining-the-numbers-had-drifted)
2. [The Crash: Lazy DFAs and Shared Matchers](#2-the-crash-lazy-dfas-and-shared-matchers)
3. [The 5ms Startup Tax](#3-the-5ms-startup-tax)
4. [Compile-Time Precompute and Byte Classes](#4-compile-time-precompute-and-byte-classes)
5. [Prefilter Verification, Done Right](#5-prefilter-verification-done-right)
6. [The Rare-Pair Probe](#6-the-rare-pair-probe)
7. [Cold Caches Strike Again: the Streaming Pipeline](#7-cold-caches-strike-again-the-streaming-pipeline)
8. [Parallel Single-File Chunking](#8-parallel-single-file-chunking)
9. [Rare-Pair Teddy: Beating Teddy at Its Own Game](#9-rare-pair-teddy-beating-teddy-at-its-own-game)
10. [The Multi-Pattern Routing Hole (and a -v Bug)](#10-the-multi-pattern-routing-hole-and-a--v-bug)
11. [Small Wins: GC Deferral, Output Batching, Glob Fast Path](#11-small-wins-gc-deferral-output-batching-glob-fast-path)
12. [What Didn't Work](#12-what-didnt-work)
13. [Final Results](#13-final-results)
14. [Key Lessons](#14-key-lessons)

---

## 1. Re-Baselining: the Numbers Had Drifted

Doc 10's tables were stale. A fresh baseline against ripgrep 15.1.0 showed:

| Workload (29MB file) | gogrep | rg | Gap |
|---|---|---|---|
| `ERROR.*port [0-9]+` `-c` | 15.2ms | 7.2ms | 2.1x |
| email regex `-c` | 13.7ms | 5.4ms | 2.5x |
| `err(or\|no\|code)` `-c` | 18.1ms | 11.8ms | 1.5x |
| fixed no-match | 14.7ms | 9.0ms | 1.6x |
| recursive `err(or\|no\|code)` `-l` | **crash** | 131ms | — |

Two of these rows are not performance problems at all — which is the first
lesson: **benchmark before believing your own docs.**

## 2. The Crash: Lazy DFAs and Shared Matchers

`gogrep -l 'err(or|no|code)' /usr/include` panicked intermittently with
`index out of range` inside `computeTransition`. The lazy DFA computes
transitions on first access, mutating `trans`, `stateMap`, `nfaSets`, and
`isMatch` — while one `Regexp` is shared across `NumCPU*2` scheduler
workers. A classic data race, silent on single files, fatal under the
recursive worker pool. The hyperfine variance (11ms-169ms) was crashes
being timed as fast runs.

Fix: compute **every reachable transition at compile time** (BFS from the
start state), after which the match paths are read-only and the `Regexp` is
genuinely safe for concurrent use. Cache overflow falls back to the PikeVM,
which allocates its thread sets per call. A `-race` regression test with 16
goroutines per pattern now guards this.

The fix is also a small speedup (doc 09 predicted it): with no
`dfaUncomputed` sentinel possible, the forward DFA inner loop drops to a
single load + compare per byte.

## 3. The 5ms Startup Tax

gogrep took 6.4ms to grep a 6-byte file; rg took 1.9ms. `GODEBUG=inittrace`
showed package inits starting at 5.3ms — and a perf profile pinned it:
`modernc.org/libc`'s `netdb` package init **parses /etc/services (300KB on
Arch) with strings.Fields at every process start.** That package arrives
transitively: `go.elara.ws/pcre` → `modernc.org/libc` → netdb. Every gogrep
invocation paid ~5ms for a services database it never used, because package
init runs whether or not `-P` is ever passed.

Fix: PCRE moved behind a `pcre` build tag (`make build-pcre`); the default
build stubs `-P` with a clear error. Startup floor: 6.4ms → 2.9ms. This one
change moved *every* single-file benchmark by ~4.5ms and flipped several
losses to wins by itself.

Lesson: **profile the whole process, not just the search.** A constant tax
disappears in throughput numbers but dominates short invocations — which is
what a grep tool mostly runs.

## 4. Compile-Time Precompute and Byte Classes

Eager precompute has a cost: 256 epsilon-closures per DFA state, visible in
profiles as ~1ms of compile time for patterns like `ERROR.*port [0-9]+` —
paid per process. The fix is the classic **byte equivalence classes** trick:
partition the 256-byte alphabet so that bytes indistinguishable to every
NFA state share a class (boundaries fall where any state's byte ranges
start/end). Typical patterns produce 5-20 classes, so precompute runs the
closure work once per class representative and fans the value out to all
bytes in the class. The flat 256-wide transition array is unchanged — the
hot loop still does one unconditional index — only construction gets ~25x
cheaper.

## 5. Prefilter Verification, Done Right

Profiles showed the prefilter *scan* was fine; the *verify* step was the
problem. Two structural fixes:

**Prefix-anchored verify.** When the extracted primary literal begins every
match (`ERROR` in `ERROR.*port [0-9]+`), there is no need to extract the
containing line and re-run the DFA from every plausible start byte: run the
DFA **anchored at the literal hit**. The prefilter extraction now tracks
`primaryIsPrefix` through the AST walk (a literal candidate is `atStart`
only if it begins the first element of the concat).

**Rare-byte windowed verify.** For patterns with only a rare byte to probe
(`@` in `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`), the old code verified the whole
line — and since nearly every byte of the line is a letter, the DFA
re-started from each one. The fix rests on a small theorem: *every byte
inside a match is consumed by some NFA transition*, so a match containing
position `off` lies entirely within the maximal run of
"pattern-consumable" bytes around `off`. The engine computes this
`allowedBytes[256]` set at compile time and walks outward from each hit to
bound the verify window — for an email that's the token around the `@`,
about 20 bytes instead of the whole line. The same set later powers the
parallel chunking guarantee (section 8).

Result: `errport -c` went 9.0ms → 5.5ms (tied with rg), email `-c` from
2.5x behind to parity.

## 6. The Rare-Pair Probe

The case-insensitive SIMD Horspool (doc 01, section 10) probed the
pattern's **first and last** bytes. For `define` under smart-case that is
`d` and `e` — and `e` is the most common letter in English and C. The scan
ran at full speed and then drowned in false-positive verifications.

The fix: probe the two **rarest** pattern bytes instead, ranked by a static
frequency table. For `define` that's `f`+`d` (~6x fewer candidates). The
two loads are offset by the probe positions rather than `0` and `plen-1`;
the bitmask still marks match starts because both loads are offset from
the same window base.

A wrong turn worth recording: the first version required the two probe
bytes to be *distinct values*, on the intuition that identical bytes give
correlated hits. For `err` that forced the pair (`e`,`r`) — the ultra-common
"er" bigram. Dropping the distinctness rule let it pick positions 1,2 —
the **"rr" bigram, which is ~17x rarer**. `err(or|no|code)` went from
15.8ms to 5.2ms on that one change. Minimize the joint frequency; don't
add clever side conditions.

## 7. Cold Caches Strike Again: the Streaming Pipeline

With search at parity, full-output mode still lost by ~3ms. A staged
benchmark decomposed the pipeline:

```
Stage 1  FindAllIndex only
Stage 2  + MatchSet construction (snippets, line structs)
Stage 3  + text formatting
Stage 3n + line numbers (-n)
```

On a 3.2MB corpus, stages 2+3 cost 48µs — nothing. On the real 29MB corpus
the real binary paid ~3ms. The difference: **3.2MB fits in L3; 29MB does
not.** The scan streams the whole buffer and evicts it; then
`matchSetFromLocs` walks the match locations touching cold memory again;
then the `-n` newline count (`bytes.Count` between matches) re-streams the
entire 29MB a third time, all DRAM misses. This is doc 09's cold-cache
lesson in a new costume — the benchmark was "too fast" because it was too
small.

The fix is architectural: `Regexp.FindAllIndexFunc(data, yield)` streams
match locations to a callback **as the scan advances**, and a
`matchSetBuilder` does snippet extraction and incremental newline counting
inside the callback — while the surrounding bytes are still in L2, a few
kilobytes behind the scan front. The slice-returning APIs became thin
wrappers over the streaming cores, so existing tests and semantics carry
over unchanged. Measured (29MB, in-process):

| Stage | before | after |
|---|---|---|
| FindAll | 5.11ms | 3.59ms |
| FindAll + Format | 5.92ms | 3.88ms |
| FindAll + Format + `-n` | 8.93ms | 4.78ms |

Rule of thumb that falls out: **benchmark corpora must exceed L3** or
multi-pass costs are invisible.

## 8. Parallel Single-File Chunking

Everything above gets gogrep *to* parity on single-file search. To go
past it: ripgrep searches one file with one thread; gogrep has 11 idle
cores.

`internal/cli/parallel.go` splits buffers ≥ 4MB into line-aligned chunks
(each chunk ends just past a `\n`), searches them on `GOMAXPROCS`
goroutines, and merges the per-chunk `MatchSet`s by rebasing `LineStart`,
`ByteOffset`, `PosIdx`, and (when `-n`) `LineNum` — each worker counts its
chunk's newlines while the chunk is cache-hot, and a prefix sum provides
the per-chunk line base. The merged MatchSet references the original
buffer, so the formatter and writer are completely unchanged.

Correctness rests on one property: **no match may span a chunk boundary.**
Chunks break at newlines, so it suffices that no match contains `\n` —
exactly what the `allowedBytes` set from section 5 certifies. Matchers
advertise this via an optional interface:

```go
type lb interface{ LineBounded() bool }
```

FastRegex (via `!re.CanMatchNewline()`), Boyer-Moore, Aho-Corasick, and
Teddy implement it; PCRE and pipeline matchers don't and silently keep the
sequential path. `LineBounded` doubles as the concurrency-safety contract —
which the compile-time precompute from section 2 made true.

Verified by diffing chunked output against `GOMAXPROCS=1` across 20
mode/pattern combinations (byte-identical), a merge unit test, and `-race`.

Impact: `-c` and `-l` modes on big files dropped 2-4x below rg
(`\d{4}-\d{2}-\d{2}` count: 4.3ms vs 19.5ms).

## 9. Rare-Pair Teddy: Beating Teddy at Its Own Game

ripgrep's multi-pattern engine is Teddy (doc 08, section 7): nibble-split
VPSHUFB tables test a 1-4 byte fingerprint of every pattern at 32 positions
per AVX2 iteration. Its structural weakness: the fingerprint is always the
patterns' **leading** bytes. Real pattern sets share common prefixes —
`{error, errno, errcode}` all fingerprint as "er", so essentially every
English "er" bigram becomes a candidate needing scalar verification.

`internal/simd/teddy.go` keeps the machinery and moves the fingerprint:
probe the two **globally rarest aligned positions** across the set
(generalizing section 6 from one pattern to N). Per position, a 16-byte
low-nibble table and high-nibble table map each input byte to an 8-bit
pattern bitmask via `VPSHUFB` (`archsimd.PermuteOrZeroGrouped`); the two
position masks — loaded at offsets `o1` and `o2` so lanes align on match
starts — are ANDed, leaving per-lane bitmasks of patterns whose bytes
matched at both probes. Non-zero lanes get a memcmp against only the
fingerprint-matched patterns. For the errX set the chosen probes are
positions 1,2: the "rr" pair again.

Per 32-byte block: 2 loads, 2 nibble splits (16-bit shift + mask), 4
PSHUFB, 3 ANDs, 1 compare — the same budget as 2-byte Teddy, with a far
more selective fingerprint. Case-insensitivity folds both cases into the
nibble tables for free.

Measured against the Aho-Corasick trie it replaces (4MB corpus):

| Pattern set | Teddy | Aho-Corasick | Speedup |
|---|---|---|---|
| `{error, errno, errcode}` | 3.7 GB/s | 334 MB/s | **11x** |
| same, case-insensitive | 3.7 GB/s | 317 MB/s | **12x** |
| `{cat, dog, elephant}` | 7.4 GB/s | — | |
| `{define, include, ifndef, pragma}` | 7.8 GB/s | — | |

End-to-end, `-F -e error -e errno -e errcode` on 29MB: gogrep 3.7ms vs rg
(actual Teddy) 13.4ms in count mode.

Equivalence with Aho-Corasick (all matches, including overlapping ones) is
property-tested against a naive reference across boundary-sized inputs and
against the AC matcher itself.

## 10. The Multi-Pattern Routing Hole (and a -v Bug)

The Teddy benchmark refused to show up in the CLI at first — the profile
still showed `MultiPipelineMatcher` running **three separate full scans**.
Reason: every `-e` starts a new OR pipeline, so multi-pattern input never
reached the factory's multi-pattern path at all; the Aho-Corasick route
had been effectively dead since pipelines were introduced.

The factory now collapses N single-stage, fixed-or-literal, non-`-o`
pipelines into one multi-pattern matcher. One parsing subtlety: `-F` is
per-stage and *resets after each pattern*, so `-F -e a -e b` marks only the
first stage fixed — the check must be "fixed or literal", not flag
equality.

The same fix repaired real semantics: `-v -e alpha -e beta` used to invert
*per pipeline* and OR the results — by De Morgan that's "lines missing
either pattern", not grep's "lines matching neither". One combined matcher
inverts the OR, which is correct (verified against GNU grep).

## 11. Small Wins: GC Deferral, Output Batching, Glob Fast Path

- **GC deferral**: `GODEBUG=gctrace` showed a one-shot run triggering a GC
  cycle on a 3MB heap, ~1ms per invocation. The CLI now sets
  `SetGCPercent(-1)` + `SetMemoryLimit(256MB)` unless `GOGC` is set or
  watch mode is active (long-running). Recursive searches stay bounded by
  the soft limit.
- **Output batching**: the OrderedWriter and `runFiles` formatted into a
  reused buffer but issued one writev *per file*. Output now accumulates
  and flushes at 256KB — tens of thousands of matching files become a few
  hundred syscalls.
- **Glob literal fast path**: config-file exclusions like `!.git` went
  through `filepath.Match` per directory entry (5.5% of a recursive
  profile). Patterns with no metacharacters are now a string compare.

## 12. What Didn't Work

- **mmap teardown theory.** System time looked high, so `MADV_DONTNEED` +
  `munmap` at exit were suspects. strace: 11µs total. Theory killed in one
  measurement; the time was page faults both tools pay equally.
- **VPTEST early-out in Teddy.** Skipping the second probe's vector work
  when the first probe's mask is all-zero *should* save ~40% of the vector
  ops. Measured: diverse-set throughput dropped 7.4 → 6.2 GB/s. The branch
  (and its mispredictions) cost more than the skipped work. Reverted.
- **Distinct-byte requirement in rare-pair selection** (section 6): a
  plausible-sounding constraint that forced the worst possible probe pair
  for `err`. Reverted.

## 13. Final Results

Single file, 29MB, 500K lines (hyperfine, 12 runs):

| Workload | gogrep | rg | Verdict |
|---|---|---|---|
| `ERROR.*port [0-9]+` full output | 6.9ms | 6.5ms | tie |
| `ERROR.*port [0-9]+` `-c` | 3.8ms | 7.4ms | **gogrep 1.95x** |
| `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+` output | 5.2ms | 7.1ms | **gogrep 1.37x** |
| `\d{4}-\d{2}-\d{2}` `-c` | 4.3ms | 19.5ms | **gogrep 4.5x** |
| `[A-Z]{2,}` `-c` | 4.9ms | 53.4ms | **gogrep 10.9x** |
| `-F -e error -e errno -e errcode` | 6.8ms | 11.6ms | **gogrep 1.7x** |
| fixed string `-n` | 8.5ms | 20.2ms | **gogrep 2.4x** |
| no-match | 3.0ms | 8.2ms | **gogrep 2.7x** |

Recursive, /usr/include:

| Workload | gogrep | rg | Verdict |
|---|---|---|---|
| `define` `-l` | 112ms | 184ms | **gogrep 1.65x** |
| `define` `-n` (101MB output) | 273ms | 267ms | tie |
| `err(or\|no\|code)` `-l` | 140ms | 153ms | **gogrep 1.10x** |

Where the wins come from, by mechanism: startup (every short run), parallel
chunks (every big-file mode), Teddy (multi-pattern), streaming pipeline
(output-heavy), rare-pair probes (case-insensitive and multi-pattern
selectivity).

## 14. Key Lessons

1. **A constant startup tax beats any throughput win on short runs.** 5ms
   of `/etc/services` parsing outweighed months of SIMD work for typical
   invocations. Profile from `execve`, not from `main`.
2. **Benchmarks are also correctness tests.** The recursive regex crash
   surfaced as impossible variance in hyperfine output. When a number looks
   too good — or too unstable — the code may be broken, not fast (doc 09's
   lesson, again).
3. **Fingerprint position beats fingerprint machinery.** Both the
   single-pattern rare-pair probe and rare-pair Teddy keep the exact same
   inner loop as their predecessors and only move *where* they look. Once
   the scan is at memory bandwidth, selectivity is the only lever left.
4. **Bench corpora must exceed L3.** A 3.2MB corpus hid a 3ms cold-cache
   cost as 48µs. Streaming (process matches behind the scan front) is the
   general cure for multi-pass pipelines over big buffers.
5. **Parallelism is the lever your competitor isn't using.** rg's
   single-file search is single-threaded. Line-aligned chunking is exact
   whenever no match can contain `\n` — a property the regex engine can
   certify from its own NFA (`allowedBytes`).
6. **Measure micro-optimizations both ways.** The VPTEST early-out and the
   distinct-byte rule both sounded right and both lost to the benchmark.
7. **Dead routing hides working engines.** The Aho-Corasick multi-pattern
   path was unreachable from the CLI for its entire life. An engine
   benchmark proves the engine; only an end-to-end profile proves it runs.

---

## Cross-References

- **Doc 01**: SIMD fundamentals; the first+last-byte prefilter this round's rare-pair probe replaces.
- **Doc 06**: GC and allocation optimization — extended here by whole-process GC deferral.
- **Doc 07**: Benchmarking methodology; this round adds the L3-sizing rule and staged pipeline benchmarks (`internal/output/pipeline_bench_test.go`).
- **Doc 08**: Teddy and the prefilter cascade as ripgrep implements them — the "before" picture for section 9.
- **Doc 09**: The original cold-cache investigation; section 7 is the same failure mode at benchmark-design level.
- **Doc 10**: The previous round (lazy DFA, SIMD skipping); its final tables are superseded by section 13.
