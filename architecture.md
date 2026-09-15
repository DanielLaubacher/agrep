# agrep Architecture

agrep is a Linux-only, high-performance grep alternative written in pure Go. Every layer is designed to minimize syscalls, avoid allocations on hot paths, and exploit Linux-specific kernel features.

## Design Goals

1. **Linux-only** -- use raw syscalls (`getdents64`, `open`, `mmap`, `fadvise`, `madvise`, `writev`, `inotify`, `epoll`) instead of portable Go abstractions.
2. **SIMD-accelerated** -- fixed-string search uses AVX2 intrinsics via Go 1.26 `simd/archsimd`.
3. **Search-then-split** -- search the entire file buffer first, then extract line boundaries only around matches. Avoids per-line overhead for files with sparse matches.
4. **Zero allocations on hot paths** -- `[]byte` everywhere, `sync.Pool` for buffers, no `string` conversions during search.
5. **Pure Go, no cgo** -- no C bindings. PCRE2 support comes from a pure Go port (`go.elara.ws/pcre`), SIMD from `simd/archsimd`.

## Feature Summary

- **AVX2 SIMD search** — fixed-string patterns use a SIMD-friendly Horspool algorithm that compares 32 byte positions per iteration
- **Search-then-split** — searches the entire file buffer first, then extracts line boundaries only around matches (avoids per-line overhead)
- **Memory-mapped I/O** — large files are mmap'd with `MADV_SEQUENTIAL` + `FADV_SEQUENTIAL` for zero-copy search (demand-paged, no `MAP_POPULATE`, enabling early exit for `-l` mode)
- **Raw syscalls** — `getdents64`, `open`, `pread`, `mmap`, `writev`, `inotify`, `epoll` — no portable Go abstractions
- **Multiple pattern engines** — custom lazy-DFA regex engine with SIMD prefilters, Boyer-Moore with SIMD, rare-pair Teddy multi-pattern (2-8 fixed patterns), Aho-Corasick for larger sets, optional PCRE2 (pure Go port, `make build-pcre`)
- **Parallel everywhere** — recursive searches fan out across a worker pool (`NumCPU * 2` goroutines, deterministic output ordering), and large single files are searched in parallel line-aligned chunks
- **Regex pipeline** (`-t`/`-o`) — chain patterns with different engines; SIMD fixed-string stages eliminate lines before expensive regex runs
- **Multi-literal prefilter** — regex AST analysis extracts all required literals for cascaded SIMD rejection before the regex engine runs
- **Watch mode** — inotify + epoll file watching with log rotation handling
- **Agent mode** — token budgets, corpus surveys, batch multi-query, verifiable citation spans, trigram index (see [agent-mode.md](agent-mode.md))
- **JSON output** — JSON Lines format for programmatic consumption
- **Pure Go, no cgo** — no C bindings or assembly files

## Pipeline

```
                  +-----------+
                  |  CLI      |  cmd/agrep/main.go
                  |           |  parses flags, builds Config
                  +-----+-----+
                        |
                  +-----v-----+
                  |  run.go   |  internal/cli/run.go
                  |           |  orchestrates one of 4 modes:
                  |           |    stdin | files | recursive | watch
                  +-----+-----+
                        |
          +-------------+-------------+
          |             |             |
    +-----v-----+ +----v----+ +------v------+
    |  Walker   | | Reader  | |  Matcher    |
    | getdents  | | mmap /  | | regex / BM  |
    |           | | pread   | | AC / PCRE   |
    +-----------+ +---------+ |   + SIMD    |
                              +------+------+
                                     |
                              +------v------+
                              | Formatter   |
                              | text / json |
                              +------+------+
                                     |
                              +------v------+
                              |  Writer     |
                              |  writev()   |
                              +-------------+
```

In recursive mode, a **Scheduler** (worker pool) sits between the Walker and Matcher, distributing files across `NumCPU * 2` goroutines. Each worker receives a file and claims its sequence number under one lock, so numbering follows walk order exactly. An **OrderedWriter** reassembles results in deterministic order using sequence numbers.

## Directory Traversal

`internal/walker/` replaces `filepath.WalkDir` with raw Linux syscalls.

1. Open directory with `unix.Open(path, O_RDONLY | O_DIRECTORY | O_NOATIME, 0)`.
2. Read entries with `unix.Getdents(fd, buf)` into a 32 KB buffer.
3. Parse raw `linux_dirent64` structs in-place (`unsafe.Pointer`). Each entry's `d_type` field classifies it as `DT_REG`, `DT_DIR`, `DT_LNK`, or `DT_UNKNOWN` without any `stat` syscall.
4. Regular files: emit path-only `FileEntry{Path}` — file opening and stat are deferred to the reader.
5. Directories: listed by a pool of `NumCPU` lister goroutines (each directory read, name-sorted, and classified once), while a single emitter replays the listings in sorted-DFS order. Emission order is a correctness contract — repeated runs produce byte-identical output, so `--max-tokens` costs the same on a retry — and the parallel listing keeps the search workers fed (a serial sorted walk cost ~45% wall time on a 65K-file tree). Skip `.git`, `.svn`, `.hg`, `node_modules`, and hidden dirs (`.` prefix) unless `--hidden` is set.
6. `DT_UNKNOWN` (rare, some filesystems like XFS): fall back to `unix.Stat` to determine type.
7. `.gitignore` support: loads and stacks ignore rules per directory, matching patterns against relative paths.

**Result**: eliminates one `lstat` per file. On a tree with 100K files, that's 100K fewer syscalls compared to `filepath.WalkDir`.

## File Reading

`internal/input/` provides two strategies, selected by file size.

### Buffered Reader (files < 8 MB)

1. `unix.Open` with `O_RDONLY | O_NOATIME`.
2. `unix.Fstat` to get size.
3. Allocate `[]byte` of exact size.
4. `unix.Pread` in a loop (positional read, no seek state).
5. Close fd immediately.

### Mmap Reader (files >= 8 MB)

1. `unix.Open` with `O_RDONLY | O_NOATIME`.
2. `unix.Fadvise(fd, 0, size, FADV_SEQUENTIAL)` -- hint the kernel to read ahead aggressively.
3. `syscall.Mmap(fd, 0, size, PROT_READ, MAP_PRIVATE)` -- demand-paged (no `MAP_POPULATE`), enabling early exit for `-l` mode without reading the entire file.
4. `unix.Madvise(data, MADV_SEQUENTIAL)` -- reinforce sequential access hint.
5. On cleanup: `unix.Madvise(data, MADV_DONTNEED)` to release page cache, then `syscall.Munmap`, then close fd.

An `AdaptiveReader` automatically selects between the two based on a configurable threshold (default 8 MB).

`O_NOATIME` is used on every file open to eliminate atime inode writes. Falls back gracefully if the process lacks `CAP_FOWNER`.

## Pattern Matching

`internal/matcher/` provides four matcher backends plus pipeline composition, all implementing the same interface:

```go
type Matcher interface {
    FindAll(data []byte) MatchSet
    MatchExists(data []byte) bool
    CountAll(data []byte) int
    FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool)
}
```

`MatchExists` provides a fast path for `-l` / `--files-with-matches` mode, skipping line boundary extraction entirely. `CountAll` provides a fast path for `-c` / `--count` mode.

### Selection Logic

| Condition | Matcher | Engine |
|---|---|---|
| `-P` (PCRE) | `PCREMatcher` | `go.elara.ws/pcre` (pure Go PCRE2 port; `pcre` build tag only — see Dependencies) |
| `-F` + 1 pattern | `BoyerMooreMatcher` | `bytes.Index` (stdlib AVX2 asm); case-insensitive uses custom SIMD Horspool |
| `-F` + 2..8 patterns | `TeddyMatcher` | Rare-pair Teddy: nibble-PSHUFB SIMD prefilter probing the set's two rarest byte positions (~11x faster than the AC trie) |
| `-F` + >8 patterns | `AhoCorasickMatcher` | Hand-written trie with `[256]*node` children + BFS failure links |
| Literal pattern (no metacharacters) | `BoyerMooreMatcher` / `AhoCorasickMatcher` | Auto-promoted from regex to fixed-string search |
| Default (regex) | `RegexMatcher` | Go stdlib `regexp` (RE2) with multi-literal SIMD prefilter |
| `-t` pipeline | `PipelineMatcher` | Chains any of the above; stages filter lines in AND sequence |
| Multiple pipelines (OR) | `MultiPipelineMatcher` | OR's multiple `PipelineMatcher` results, deduplicates by line |

### Regex Pipeline (`-t` / `-o`)

The `-t` (pipe) and `-o` (only-matching) flags enable regex composition — chaining patterns where each stage filters lines that matched the previous stage. The final stage's match positions define the output highlights.

```
agrep -Fe 'ERROR' -Fte 'timeout' -toe '\d+' app.log
       ^^^^^^^^^^  ^^^^^^^^^^^^^^  ^^^^^^^^^^^
       stage 0     stage 1         stage 2
       (SIMD BM)   (SIMD BM)      (RE2, -o output)
```

**Semantics:**
- `-e` without `-t`: starts a new OR branch (backwards compatible with multiple `-e`).
- `-te`: appends to the current pipeline (AND chain).
- `-o` on a stage: output only the matched text (like `grep -o`).
- `-F`, `-P` are per-stage: each stage can use a different engine.
- Flags combine naturally: `-Ftoe 'pat'` = fixed + pipe + only-match + pattern.

**Pipeline execution (`PipelineMatcher.FindAll`):**
1. Stage 0 runs `FindAll(data)` on the full buffer — gets candidate lines.
2. Stages 1..N-1 run `MatchExists(line)` on each candidate (cheap filter).
3. Final stage runs `FindAll(line)` on survivors to extract match positions.
4. Line metadata (line number, byte offset) is preserved from stage 0.

**Performance:** When all stages are fixed strings (`-F`), the pipeline uses only SIMD search — no regex engine at all. This can outperform ripgrep on ultra-sparse multi-condition searches (measured 1.14x faster at 0.1% hit rate).

### Custom Regex Engine (internal/regex)

Non-literal patterns compile to a hand-rolled lazy-DFA engine (see
`education/10-closing-the-ripgrep-gap.md`). Key properties:

- **Compile-time precomputation**: every reachable DFA transition is computed
  eagerly at `Compile` time (BFS over states), so match paths are read-only
  and one `Regexp` is safe to share across all scheduler workers. On
  state-cache overflow the engine falls back to the PikeVM (allocates per
  call, also safe). Precomputation runs once per **byte equivalence class**
  (typically 5-20 classes, not 256 bytes), keeping compile cost microseconds.
- **Prefix-anchored verify**: when the extracted primary literal begins every
  match (e.g. `ERROR` in `ERROR.*port [0-9]+`), candidates from the SIMD
  literal scan are verified by running the DFA anchored at the hit — no line
  extraction, no re-scanning from every plausible start byte.
- **Rare-byte windowed verify**: when only a rare byte is extractable (e.g.
  `@` in an email pattern), each hit is verified inside the maximal run of
  pattern-consumable bytes around it (walking outward over the `allowedBytes`
  set), instead of the whole line.
- **`Match()` is candidate-driven**: `-l` mode verifies at prefilter hits and
  early-exits, rather than walking the forward DFA over the entire file.

### Multi-Literal Prefilter

`RegexMatcher` automatically extracts all required literal substrings from the regex AST (via `extractLiterals` in `literal.go`). For a regex like `ERROR.*code=[45]\d{2}.*request_id=[0-9a-f]+`:

1. AST analysis finds three required literals: `"ERROR"`, `"code="`, `"request_id="`.
2. The first literal (`"ERROR"`) becomes the primary SIMD prefilter.
3. Remaining literals (>= 4 bytes) become extra filters, checked **in source order** and **position-aware** (each must appear after the previous).
4. Only lines passing all literal checks run through the regex engine.

Position-aware ordering is critical for minified files (single-line, multi-MB) where line-level filtering is meaningless — the ordered check ensures the literals appear in the correct sequence within the line, rejecting false positives that a line-level check would miss.

### Rare-Pair Teddy (internal/simd/teddy.go)

For sets of 2-8 fixed patterns, agrep uses a variant of ripgrep's Teddy
algorithm with one structural change: instead of fingerprinting each
pattern's **first** bytes (Teddy's weakness — sets like `{error, errno,
errcode}` share the ultra-common prefix "er"), it probes the two **globally
rarest aligned byte positions** across the set, chosen by a static byte
frequency table. For the errX set that selects the "rr" pair — ~17x fewer
false-positive candidates. The scan tests 32 start positions per iteration
via nibble-split VPSHUFB membership masks ANDed across the two probe
offsets; each candidate lane carries an 8-bit pattern bitmask so only
fingerprint-matched patterns are memcmp-verified. Measured ~3.7-7.8 GB/s vs
~330 MB/s for the Aho-Corasick trie it replaces.

Multiple `-e` patterns that are all fixed/literal collapse into one
multi-pattern matcher (factory fast path), so `-e a -e b -e c` runs a single
Teddy scan instead of N full scans OR'd afterwards — also required for
correct `-v` semantics (invert of the OR, not OR of the inverts).

### Streaming match pipeline + parallel chunking

`Regexp.FindAllIndexFunc` streams match locations to a callback as the scan
advances; the matcher builds its `MatchSet` (line snippets, newline counts
for `-n`) inside the callback while the surrounding bytes are still
cache-hot. On buffers larger than L3 this avoids a second cold pass (the
`-n` line-count pass alone cost ~3ms per 29MB before).

For buffers ≥ 4MB, single-file search runs **parallel line-aligned chunks**
(`internal/cli/parallel.go`) across `GOMAXPROCS` goroutines — ripgrep
searches one file with one thread. Chunk results are rebased (offsets, line
numbers, position indices) and merged into one MatchSet, so everything
downstream is unchanged. Enabled only when the matcher reports
`LineBounded()` (no match can contain `\n`, so line-aligned chunks are
exact) — FastRegex (via the DFA's allowed-byte set), Boyer-Moore, Teddy,
and Aho-Corasick all qualify.

The CLI also defers garbage collection (`SetGCPercent(-1)` +
`SetMemoryLimit(256MB)`, unless `GOGC` is set or in watch mode): a one-shot
grep process was paying ~1ms/run in GC cycles on a tiny heap.

### Search-then-Split

All matchers search the entire file buffer in a single pass, then extract line boundaries only around match positions. This inverts the traditional "split into lines, then search each line" approach.

For a 500K-line file with 3 matches, the old approach made 500K `findInLine()` calls. The new approach makes 1 whole-buffer search + 3 line extractions.

`snippetFromOffset()` extracts line boundaries around each match offset (clamped by `maxCols`), and `matchSetFromOffsets()` computes line numbers incrementally via `bytes.Count` between consecutive match positions, avoiding redundant newline counting.

### SIMD Acceleration

`internal/simd/` uses Go 1.26's `simd/archsimd` for AVX2 intrinsics (requires `GOEXPERIMENT=simd`).

For **case-sensitive** search, `simd.IndexAll` delegates to `bytes.Index` which already uses optimized AVX2 assembly internally in the Go runtime.

For **case-insensitive** search, `simd.IndexAllCaseInsensitive` uses a custom **SIMD-friendly Horspool** algorithm. The two probe positions are the pattern's two **rarest** bytes (by a static text/code frequency table), not first+last — for `define` that probes `f`+`d` instead of `d`+`e`, and for `err` it probes the rare `rr` bigram, cutting false-positive verifications by ~5-6x:

1. Broadcast both lower and upper forms of the two probe bytes into 32-byte AVX2 vectors.
2. For each 32-byte block in the data:
   - Load 32 bytes at position `i` and at position `i + patternLen - 1`.
   - `VPCMPEQB` to compare all 32 positions against lower/upper first bytes, OR the masks.
   - Same for last bytes, OR the masks.
   - `VPAND` the first and last result masks.
   - `VPMOVMSKB` to extract a 32-bit bitmask of candidate positions.
3. For each set bit in the bitmask (typically 0-1 per block): verify the middle bytes with case-insensitive comparison.
4. `VZEROUPPER` before returning to avoid AVX/SSE transition penalties.

This processes 32 candidate positions per iteration. For typical text, the first+last byte filter eliminates >99% of false positives, making the inner verification extremely rare.

Additional SIMD primitives in `internal/simd/simd.go`: `IndexByte`, `LastIndexByte`, `Count`, and `ToLowerASCII`, all using AVX2 `VPCMPEQB` + `VPMOVMSKB` patterns.

## Output

### Text Formatter

Uses raw ANSI escape codes for zero-allocation coloring:
- Filenames: `\x1b[35m` magenta
- Line numbers: `\x1b[32m` green
- Separators: `\x1b[36m` cyan
- Matches: `\x1b[1;31m` bold red

Color mode is auto-detected via `unix.IoctlGetTermios(fd, TCGETS)` (raw TTY detection, no external package). Output buffer is pre-allocated based on match count to avoid `growslice` overhead.

### JSON Formatter

Outputs one JSON object per match line in JSON Lines format.

### Writer

All output goes through `unix.Writev` for scatter-gather I/O, batching filename, separator, line content, and newline into a single syscall.

An `OrderedWriter` buffers out-of-order results from parallel workers and emits them in sequence-number order to maintain deterministic output. Formatted output accumulates in a reused buffer and is flushed in **256 KB batches** (not per file) — on an output-heavy recursive search over ~18K matching files this collapses tens of thousands of write syscalls into a few hundred.

## Watch Mode

`internal/watch/` implements file watching with raw Linux inotify + epoll:

1. `unix.InotifyInit1(IN_CLOEXEC | IN_NONBLOCK)` -- create inotify instance.
2. `unix.InotifyAddWatch(fd, path, IN_MODIFY | IN_CREATE | IN_MOVED_TO | IN_MOVE_SELF | IN_DELETE_SELF)` -- watch for modifications, new files, and log rotation.
3. `unix.EpollCreate1(EPOLL_CLOEXEC)` + `unix.EpollWait` with 100ms timeout -- efficient event loop.
4. On `IN_MODIFY`: `unix.Pread` from last known offset to read only new content. Handles truncation (log rotation) by resetting the offset.

## Concurrency Model

```
Walker goroutine
    |
    | FileEntry channel (buffer 256)
    v
Scheduler (NumCPU * 2 workers)
    |
    | each worker: read file -> match -> emit Result with sequence number
    |
    | Result channel (buffer workers * 2)
    v
OrderedWriter goroutine
    |
    | reorders by sequence number, formats, writes via writev
    v
stdout
```

In non-recursive mode with buffers >= 4 MB, the single file is itself
searched in parallel line-aligned chunks (see "Streaming match pipeline +
parallel chunking" above).

## Key Constants

| Parameter | Value |
|---|---|
| Mmap threshold | 8 MB |
| Worker count | `NumCPU * 2` |
| getdents buffer | 32 KB |
| Binary detection | first 8192 bytes |
| SIMD block width | 32 bytes (AVX2) |
| File channel buffer | 256 |
| Result channel buffer | `workers * 2` |
| Epoll timeout | 100 ms |
| Output flush threshold | 256 KB |
| Parallel-chunk threshold | 4 MB (chunks >= 1 MB, line-aligned) |
| Teddy pattern limit | 2-8 patterns, each >= 2 bytes |
| GC | deferred to a 256 MB soft limit (except watch mode / explicit `GOGC`) |

## Performance vs ripgrep

Measured 2026-09-15 against ripgrep 15.1.0 (29MB/500K-line corpus and
/usr/include; hyperfine `-N`, `AGREP_CONFIG_PATH=/dev/null` and
`rg --no-config` for neutral configs on both sides). agrep wins most
benchmarked workloads, sometimes by a wide margin, but it does **not**
win or tie everything: a recursive multi-word literal-phrase search
(`static inline`) currently loses to ripgrep's tuned `memchr`-based
literal scan, and recursive full-output text search is a coin-flip tie.
Numbers are noisy at this scale (single-digit milliseconds) — treat
anything under ~1.2x as a tie, not a win:

| Workload | agrep | rg | result |
|---|---|---|---|
| `ERROR.*port [0-9]+` full output (29MB) | 6.0ms | 6.3ms | tie |
| `ERROR.*port [0-9]+` `-c` | **3.5ms** | 6.0ms | agrep 1.7x |
| `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+` output | **3.8ms** | 4.4ms | agrep 1.2x |
| `\d{4}-\d{2}-\d{2}` `-c` | **3.9ms** | 14.5ms | agrep 3.8x |
| `[A-Z]{2,}` `-c` | **3.5ms** | 46.8ms | agrep 13.4x |
| `-F -e error -e warning -e critical` | **3.6ms** | 6.7ms | agrep 1.9x |
| fixed string `-n` | **20.0ms** | 22.2ms | agrep 1.1x (tie) |
| no-match | **3.0ms** | 3.7ms | agrep 1.2x |
| recursive `define` `-l` (/usr/include, 809MB) | **107.5ms** | 234.9ms | agrep 2.2x |
| recursive `define` `-n` (101MB output) | 259.1ms | 239.6ms | rg 1.1x (tie) |
| recursive `err(or\|no\|code)` `-l` | 155.0ms | 161.2ms | agrep 1.0x (tie) |
| recursive `static inline` `-l` (/usr/include) | 135.5ms | **117.6ms** | **rg 1.2x faster** |

The honest summary: agrep is faster on most single-file counting/regex
workloads and on recursive file-listing searches driven by a rare
literal, competitive (ties, either direction, within noise) on plain
recursive text output, and currently behind on recursive searches for a
common multi-word literal phrase. Reproduce with the exact commands
above plus `hyperfine -N --warmup 3`; both tools need a neutral config
(`AGREP_CONFIG_PATH=/dev/null`, `rg --no-config`) or a locally-injected
`~/.agrep`/`~/.ripgreprc` will skew the comparison.

### Assertions, -w, -U and globs (2026-09-15 regression report)

A follow-up report found three cliffs and one correctness bug, all fixed:

- **`\b` and `^`/`$` patterns** (3-4s per query) — any assertion forced the
  PikeVM over whole buffers with no literal prefilter. Now: `\bLIT\b`
  (and `-w LIT`) runs on the SIMD literal engine with one byte check per
  side; other assertion patterns keep their literal prefilter (a line
  carries the same neighbours as the buffer, so `^ $ \b \B` verify
  per line exactly) and a *relaxed* DFA — the pattern with assertions
  stripped — rejects candidate lines before the PikeVM runs. `^` and `$`
  also now anchor per line in normal mode (`^#define` used to match only
  a file's first line). See `regex.CompileMode`.
- **`-U` with `-i`** (3.7s) — the stdlib engine has no prefilter under
  `(?i)`. `-U` now uses the internal DFA in `regex.ModeMultiline`, where
  a prefilter may anchor at match starts or gate whole buffers but never
  confine verification to a line.
- **Serial sorted walk** — see Directory Traversal above.
- **`-g '*.py'` matched only top-level files** — include globs pruned
  every subdirectory. Include globs now apply to files only; `/` globs
  match the printed path with `**` (ripgrep's convention).

Book corpus (835 Markdown files, 510MB), ms, report → now, vs rg:
`\berror\b -i` 3701 → 85 (rg 33); `\bgo\s+func\s*\( -s` 3671 → 58
(rg 25); `-U -i func main..defer` 3681 → 51 (rg 54). Python tree
`-rc ImportError` 92 → 47 (rg 72, gogrep 56).

The full optimization history lives in `education/10-closing-the-ripgrep-gap.md`
and `education/11-beating-ripgrep.md`. Reproducible micro-benchmarks:
`internal/output/pipeline_bench_test.go` (staged match→format pipeline),
`internal/simd/teddy_test.go` and `internal/matcher/teddy_test.go`
(Teddy vs Aho-Corasick), plus `make bench`.

## Building & Development

Requirements: **Go 1.26+** with `GOEXPERIMENT=simd`, **Linux AMD64** (x86-64 with AVX2 — Intel Haswell+ or AMD Excavator+), `golang.org/x/sys` for syscall bindings.

```sh
# Build (Makefile sets GOEXPERIMENT=simd automatically)
make build            # → bin/agrep
make build-pcre       # → bin/agrep-pcre with -P support (see Dependencies)
GOEXPERIMENT=simd go build -o bin/agrep ./cmd/agrep   # equivalent

# Install
make install          # or: GOEXPERIMENT=simd go install github.com/DanielLaubacher/agrep/cmd/agrep@latest

# Test — PCRE tests run separately because modernc.org/libc crashes under -race
make test

# Benchmarks (matchers, input, SIMD primitives; compared against bytes.Index baselines)
make bench

# Profile against ripgrep
make profile

# Lint
make lint
```

## Dependencies

| Package | Purpose |
|---|---|
| `golang.org/x/sys` | Linux syscalls (getdents, open, mmap, writev, inotify, epoll) |
| `go.elara.ws/pcre` | Pure Go PCRE2 port (no cgo) — **`pcre` build tag only** |
| `github.com/sabhiram/go-gitignore` | .gitignore pattern matching |
| `simd/archsimd` | Go 1.26 experimental AVX2 intrinsics |

**Why PCRE is opt-in**: `go.elara.ws/pcre` transitively links `modernc.org/libc`,
whose `netdb` package init parses `/etc/services` (~300 KB) at **every process
start** — a measured ~5 ms tax on each invocation, tripling agrep's startup
floor. The default build stubs `-P` out with a clear error; `make build-pcre`
produces `bin/agrep-pcre` with full PCRE support.
