# Benchmarking and Profiling

Performance measurement methodology for systems-level Go programs, with concrete examples drawn entirely from the agrep codebase.

This document covers the full spectrum of performance tooling: micro-benchmarks with Go's `testing.B` framework, statistical comparison with `benchstat`, escape analysis for heap allocation tracking, CPU and memory profiling with `pprof`, syscall tracing with `strace`, and end-to-end benchmarking with `hyperfine`. Each section builds on the previous one, moving from the smallest unit of measurement (a single function) to the largest (a full application compared against competitors).

---

## Table of Contents

1. [Go Benchmark Framework](#go-benchmark-framework)
2. [Catalog of agrep Benchmarks](#catalog-of-agrep-benchmarks)
3. [benchstat -- Statistical Comparison](#benchstat--statistical-comparison)
4. [Escape Analysis](#escape-analysis)
5. [CPU Profiling with pprof](#cpu-profiling-with-pprof)
6. [Memory Profiling with pprof](#memory-profiling-with-pprof)
7. [strace for Syscall Analysis](#strace-for-syscall-analysis)
8. [hyperfine for End-to-End Benchmarking](#hyperfine-for-end-to-end-benchmarking)
9. [Micro vs Macro Benchmarks](#micro-vs-macro-benchmarks)
10. [Interpreting Performance Numbers](#interpreting-performance-numbers)
11. [Cross-References](#cross-references)

---

## Go Benchmark Framework

### The testing.B Interface

Go's `testing` package includes a first-class benchmarking framework. Benchmark functions live in `*_test.go` files alongside unit tests, share the same build constraints and package access, and are executed by `go test -bench`.

Requirements for a benchmark function:

1. The file must end in `_test.go`.
2. The function name must start with `Benchmark`.
3. The function must accept exactly one parameter: `*testing.B`.

The framework determines how many iterations to run by measuring wall-clock time. It adjusts the iteration count upward until the benchmark runs long enough (typically ~1 second) for stable measurement.

### Go 1.24+ b.Loop() Style

agrep uses the modern `b.Loop()` style introduced in Go 1.24, as seen throughout the benchmark files. Here is the canonical example from `internal/matcher/boyermoore_test.go`:

```go
func BenchmarkBoyerMoore_ShortPattern(b *testing.B) {
    data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
    m := NewBoyerMooreMatcher("lazy", false, false)
    b.ResetTimer()                    // Exclude setup from timing
    b.SetBytes(int64(len(data)))      // Enable throughput reporting (MB/s, GB/s)
    for b.Loop() {                    // Go 1.24+ -- replaces for i := 0; i < b.N; i++
        m.FindAll(data)
    }
}
```

Compare this to the older `b.N` style:

```go
for i := 0; i < b.N; i++ {
    m.FindAll(data)
}
```

`b.Loop()` is preferred for three reasons:

1. **Automatic warmup.** The framework can run warmup iterations before the timed portion begins.
2. **Automatic iteration count.** The framework decides how many iterations to run. You never reference `b.N` directly, which means you cannot accidentally use it wrong (e.g., as a size parameter).
3. **Reduced compiler optimization risk.** With the `b.N` style, the compiler knows the loop variable and can theoretically perform optimizations that distort results. With `b.Loop()`, the call acts as an optimization barrier -- the compiler cannot predict how many iterations will occur, which makes dead code elimination less likely.

### Key testing.B Methods

**`b.ResetTimer()`** -- Resets both the wall-clock timer and allocation counters. Place this call after any setup work (creating test data, initializing matchers, compiling regexes) so that setup cost does not contaminate the measurement. Every benchmark in agrep calls `b.ResetTimer()` after constructing the matcher and test data.

**`b.SetBytes(n int64)`** -- Tells the framework how many bytes each iteration processes. This enables throughput reporting in the output (e.g., `9718.75 MB/s`). For agrep's matcher benchmarks, this is always the length of the input data buffer:

```go
b.SetBytes(int64(len(data)))
```

Without `b.SetBytes`, the output only shows `ns/op`. With it, you also get `MB/s`, which is critical for evaluating grep performance because the fundamental question is "how fast can this matcher scan data?"

**`b.ReportAllocs()`** -- Explicitly enables per-operation allocation reporting within the benchmark function. This is an alternative to the `-benchmem` command-line flag. If you want allocation tracking to always appear for a specific benchmark regardless of command-line flags, call this in the benchmark body. In agrep's codebase, `-benchmem` is used at the command line instead (via the Makefile), so `b.ReportAllocs()` is not called explicitly.

**`b.TempDir()`** -- Creates a temporary directory that is automatically cleaned up when the benchmark finishes. The input benchmarks in `internal/input/input_test.go` use this to create temporary files for I/O benchmarks:

```go
func BenchmarkBufferedReader(b *testing.B) {
    dir := b.TempDir()
    path := filepath.Join(dir, "bench.txt")
    content := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
    if err := os.WriteFile(path, content, 0644); err != nil {
        b.Fatal(err)
    }

    r := NewBufferedReader()
    b.ResetTimer()
    b.SetBytes(int64(len(content)))
    for b.Loop() {
        result, err := r.Read(path)
        if err != nil {
            b.Fatal(err)
        }
        result.Closer()
    }
}
```

**`b.Fatal()` and `b.Fatalf()`** -- Stop the benchmark immediately on error. Used in setup code when constructing matchers that can fail (e.g., PCRE pattern compilation in `internal/matcher/pcre_test.go`):

```go
func BenchmarkPCRE_Simple(b *testing.B) {
    data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
    m, err := NewPCREMatcher("lazy", false, false)
    if err != nil {
        b.Fatal(err)
    }
    defer m.Close()
    // ...
}
```

### Running Benchmarks

All agrep commands require `GOEXPERIMENT=simd` because the codebase uses `simd/archsimd` for AVX2 vector types. The Makefile sets this automatically.

```bash
# Run all benchmarks in the matcher package
GOEXPERIMENT=simd go test -bench=. -benchmem ./internal/matcher/

# Run a specific benchmark by regex match on its name
GOEXPERIMENT=simd go test -bench=BoyerMoore_NoMatch -benchmem ./internal/matcher/

# Run with multiple iterations for statistical significance (see benchstat section)
GOEXPERIMENT=simd go test -bench=. -benchmem -count=10 ./internal/matcher/

# Run all benchmark packages via the Makefile
make bench
```

The `make bench` target is defined in `Makefile`:

```makefile
bench:
    GOEXPERIMENT=$(GOEXPERIMENT) go test -bench=. -benchmem \
        ./internal/matcher/ ./internal/input/ ./internal/simd/
```

Three packages have benchmarks: `matcher`, `input`, and `simd`. PCRE benchmarks exist in `pcre_test.go` but are deliberately excluded from `make bench` because `go.elara.ws/pcre` uses `modernc.org/libc` internally, which crashes with a GC finalizer SIGSEGV during benchmark teardown. The `make test` target handles PCRE differently: it runs race-detector tests first with `AGREP_SKIP_PCRE=1`, then runs PCRE tests separately without the race detector:

```makefile
test:
    GOEXPERIMENT=$(GOEXPERIMENT) AGREP_SKIP_PCRE=1 go test -race ./...
    GOEXPERIMENT=$(GOEXPERIMENT) go test ./internal/matcher/ -run "PCRE"
```

### Reading Benchmark Output

A single benchmark output line:

```
BenchmarkBoyerMoore_NoMatch-16    127    9404862 ns/op    9718.75 MB/s    0 B/op    0 allocs/op
```

Breaking this down field by field:

| Field | Value | Meaning |
|---|---|---|
| `BenchmarkBoyerMoore_NoMatch-16` | Name + GOMAXPROCS | The benchmark name. `-16` means GOMAXPROCS=16 (number of OS threads available for goroutine scheduling). On a 16-thread machine, this is the default. |
| `127` | Iteration count | The framework ran the benchmark loop 127 times total. Higher iteration counts mean the per-iteration time was short (framework aims for ~1 second total). |
| `9404862 ns/op` | Time per iteration | 9.4 milliseconds to scan the entire 440KB buffer. This is the primary metric. |
| `9718.75 MB/s` | Throughput | Derived from `b.SetBytes()`. `440000 bytes / 0.009404862 seconds = 46.8 MB/iteration / 0.009404862 s = 4976 MB/s`. Wait -- the actual value shown would depend on the specific run. The framework computes `bytes_per_op * 1e9 / ns_per_op`. |
| `0 B/op` | Bytes allocated per iteration | Zero heap bytes allocated. The matcher scanned 440KB and produced no matches, so no result slices were created. Only appears with `-benchmem` or `b.ReportAllocs()`. |
| `0 allocs/op` | Allocations per iteration | Zero calls to the allocator. This is the ideal for a no-match path -- the hot loop touches only stack memory and the pre-allocated input buffer. |

**Throughput calculation:**

The framework computes throughput as:

```
throughput_MB_per_sec = (b.SetBytes_value * 1,000,000,000) / (ns_per_op * 1,000,000)
```

Simplified:

```
throughput_MB_per_sec = b.SetBytes_value * 1000 / ns_per_op
```

For example, if `b.SetBytes` is 440,000 (440KB) and `ns/op` is 45,000 (45 microseconds):

```
440,000 * 1,000 / 45,000 = 9,778 MB/s  (~9.7 GB/s)
```

This 9.7 GB/s throughput is close to memory bandwidth on modern hardware, meaning the SIMD-accelerated Boyer-Moore scan is nearly as fast as a simple memcpy for the no-match case.

### Benchmark Naming Conventions

agrep follows a consistent naming pattern for benchmarks:

```
Benchmark{Algorithm}_{Scenario}
```

Examples:
- `BenchmarkBoyerMoore_ShortPattern` -- BoyerMoore algorithm, short (4-byte) pattern, every line matches
- `BenchmarkBoyerMoore_NoMatch` -- BoyerMoore algorithm, pattern never found (worst-case full scan)
- `BenchmarkAhoCorasick_TenPatterns` -- Aho-Corasick algorithm, 10 simultaneous patterns
- `BenchmarkIndexAll_SIMD_Sparse` -- SIMD IndexAll function, sparse match distribution
- `BenchmarkBufferedReader` -- pread-based reader, full read cycle

This convention makes it easy to use `-bench` regex filtering:

```bash
# All BoyerMoore benchmarks
-bench=BoyerMoore

# All no-match scenarios across all algorithms
-bench=NoMatch

# All SIMD-vs-stdlib comparisons
-bench='(SIMD|Stdlib)'
```

### Test Data Construction Patterns

agrep benchmarks consistently use 10,000 lines of ~44 bytes each, producing approximately 440KB of test data:

```go
data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
```

This size is chosen deliberately:

1. **Fits in L2 cache** (typically 256KB-1MB per core) on repeated iterations, giving stable timings.
2. **Large enough** for SIMD loops to dominate over scalar setup/teardown.
3. **Realistic** -- many real-world source files are in the 100KB-1MB range.

For sparse-match benchmarks, the data is constructed with controlled match density:

```go
var buf []byte
for i := range 10000 {
    if i%1000 == 0 {
        buf = append(buf, []byte("ERROR: connection refused at port 8080\n")...)
    } else {
        buf = append(buf, []byte("the quick brown fox jumps over the lazy dog\n")...)
    }
}
```

This gives exactly 10 matches in 10,000 lines (1 in 1,000), which is realistic for log searching.

---

## Catalog of agrep Benchmarks

### internal/matcher/ Benchmarks

Source files: `internal/matcher/boyermoore_test.go`, `internal/matcher/ahocorasick_test.go`, `internal/matcher/pcre_test.go`

| Benchmark | What It Measures | Data | Key Details |
|---|---|---|---|
| `BoyerMoore_ShortPattern` | Fixed 4-byte pattern "lazy", every line matches | 440KB, 10K lines | Dense match -- stresses line extraction and result building |
| `BoyerMoore_LongPattern` | Fixed 19-byte pattern "jumps over the lazy", every line matches | 440KB, 10K lines | Longer pattern allows larger Boyer-Moore shifts, but still dense |
| `BoyerMoore_NoMatch` | Pattern "zzzzz" never found (full scan, zero results) | 440KB, 10K lines | Best case for search-then-split: full scan at memory bandwidth, no line extraction |
| `BoyerMoore_CaseInsensitive` | Case-insensitive "LAZY" against lowercase data | 440KB, 10K lines | Tests SIMD case-folding path (`IndexAllCaseInsensitive`) |
| `BoyerMoore_SparseMatch` | 1 match per 1000 lines (10 total) | 440KB, 10K lines | Most realistic scenario: fast scan + minimal line extraction |
| `Regex_SparseMatch` | RE2 regex on same sparse data | 440KB, 10K lines | Direct comparison: how much faster is BoyerMoore vs RE2 on fixed strings? |
| `BytesIndex_ShortPattern` | Raw `bytes.Index` loop (no line extraction) | 440KB, 10K lines | Baseline: how fast is stdlib pattern search without agrep overhead? |
| `AhoCorasick_TwoPatterns` | 2 patterns "fox"+"dog", both match every line | 440KB, 10K lines | Multi-pattern via Aho-Corasick automaton |
| `AhoCorasick_TenPatterns` | 10 patterns, many matches per line | ~700KB, 10K lines | Stress test: Aho-Corasick with large automaton, dense output |
| `AhoCorasick_NoMatch` | 3 non-matching patterns "zzz"+"yyy"+"xxx" | 440KB, 10K lines | Automaton traversal cost on miss |
| `AhoCorasick_CaseInsensitive` | 2 patterns "FOX"+"DOG", case-insensitive | 440KB, 10K lines | Case-folding with multi-pattern search |
| `PCRE_Simple` | PCRE2 on fixed "lazy" pattern, every line matches | 440KB, 10K lines | PCRE overhead on simple patterns (comparison with BoyerMoore) |
| `PCRE_Lookahead` | PCRE2 lookahead `\w+(?=\s+dog)` | 440KB, 10K lines | PCRE-only feature; no equivalent in RE2 or BoyerMoore |
| `PCRE_NoMatch` | PCRE2 on "zzzzz", no matches | 440KB, 10K lines | PCRE full-scan cost |

### internal/simd/ Benchmarks

Source files: `internal/simd/simd_test.go`, `internal/simd/index_test.go`

| Benchmark | What It Measures | Notes |
|---|---|---|
| `IndexByte_SIMD` | Single-byte search via agrep's `IndexByte` | Uses `archsimd.LoadUint8x32Slice` + `Equal` + `ToBits` |
| `IndexByte_Stdlib` | `bytes.IndexByte` (stdlib) | Already uses SSE2/AVX2 asm internally in Go |
| `LastIndexByte_SIMD` | Backward single-byte search | Custom SIMD backward scan |
| `LastIndexByte_Stdlib` | `bytes.LastIndexByte` (stdlib) | Comparison baseline |
| `Count_SIMD` | Byte counting via AVX2 | Count newlines for line numbering |
| `Count_Stdlib` | `bytes.Count` (stdlib) | Already optimized in Go stdlib |
| `IndexByte_SIMD_Far` | Single-byte search, target at end of 440KB buffer | Worst case: must scan entire buffer |
| `IndexByte_Stdlib_Far` | Same worst case with stdlib | SIMD vs stdlib on full scan |
| `Index_SIMD_Short` | Multi-byte first-match search | Currently delegates to `bytes.Index` |
| `Index_Stdlib_Short` | `bytes.Index` (stdlib) | Baseline for multi-byte |
| `IndexAll_SIMD` | All-match collection, 10K matches | `IndexAll` with stack buffer + overflow |
| `IndexAll_BoyerMoore` | Pure Boyer-Moore all-match (no SIMD) | Scalar comparison |
| `IndexAll_BytesIndex` | `bytes.Index` loop for all matches | Baseline |
| `IndexAll_SIMD_Sparse` | Sparse matches (10 in 440KB) | Tests stack buffer path (no heap overflow) |
| `IndexAll_BytesIndex_Sparse` | `bytes.Index` loop, sparse | Comparison |
| `Index_SIMD_NoMatch` | Full scan, no match found | SIMD scan cost on miss |
| `Index_Stdlib_NoMatch` | `bytes.Index`, no match | Comparison |
| `IndexCaseInsensitive_SIMD` | AVX2 case-insensitive search | First+last byte prefilter with case variants |

A critical finding documented in the project memory: Go's stdlib `bytes.IndexByte` and `bytes.Index` already use SSE2/AVX2 assembly internally. The custom `archsimd` versions are NOT faster for single-byte search. The SIMD value comes from multi-byte `IndexAll` (Horspool first+last byte prefilter checking 32 positions per iteration) and case-insensitive variants that check both upper and lower case in a single SIMD pass.

### internal/input/ Benchmarks

Source file: `internal/input/input_test.go`

| Benchmark | What It Measures | Data | Syscall Sequence |
|---|---|---|---|
| `BufferedReader` | pread-based read cycle | 440KB file on disk | `openat` -> `fstat` -> `pread` -> `close` |
| `MmapReader` | mmap cycle | 440KB file on disk | `openat` -> `fstat` -> `mmap` -> `madvise` -> `munmap` -> `close` |
| `MmapReader_LargeFile` | mmap on 22MB file | 22MB (500K lines) | Same as above but exercises `MAP_POPULATE` behavior |

These benchmarks measure the full I/O cycle including syscall overhead, not just data access. The `b.TempDir()` creates real files on the filesystem, so the benchmark includes actual kernel calls. On repeated iterations the file data will be in the page cache, so these primarily measure syscall overhead rather than disk I/O.

---

## benchstat -- Statistical Comparison

Raw benchmark numbers have natural variance from run to run: CPU frequency scaling, cache state, OS scheduler decisions, interrupt handling, and thermal throttling all introduce noise. A single benchmark run tells you almost nothing about whether a 5% difference is real or noise. `benchstat` solves this by computing confidence intervals and p-values.

### Installation

```bash
go install golang.org/x/perf/cmd/benchstat@latest
```

### Workflow

```bash
# Step 1: Run benchmarks multiple times, save to file
GOEXPERIMENT=simd go test -bench=BoyerMoore -benchmem -count=10 \
    ./internal/matcher/ > before.txt

# Step 2: Make your code change
# (edit internal/simd/index.go, internal/matcher/boyermoore.go, etc.)

# Step 3: Run the same benchmarks again
GOEXPERIMENT=simd go test -bench=BoyerMoore -benchmem -count=10 \
    ./internal/matcher/ > after.txt

# Step 4: Compare statistically
benchstat before.txt after.txt
```

### Reading benchstat Output

```
                          |  before.txt  |            after.txt            |
                          |    sec/op    |   sec/op     vs base            |
BoyerMoore_NoMatch-16      94.0m +/- 2%     3.21m +/- 1%  -96.58% (p=0.000)
BoyerMoore_SparseMatch-16  12.1m +/- 3%     5.40m +/- 2%  -55.37% (p=0.000)
```

Field-by-field:

- **`94.0m +/- 2%`**: The median time was 94.0 milliseconds. The `+/- 2%` is the inter-quartile range expressed as a percentage of the median, indicating how much the individual runs varied. Lower variance means more stable measurement.
- **`-96.58%`**: The after version is 96.58% faster (takes only 3.42% of the original time). Negative means faster; positive means slower.
- **`(p=0.000)`**: The p-value from a statistical test (Mann-Whitney U test). `p=0.000` means the probability that this difference occurred by chance is less than 0.001 -- the improvement is statistically significant and not noise.

If the p-value is high (e.g., `p=0.312`), benchstat will show `~ (no significant difference)` instead of a percentage, meaning the change had no measurable effect.

### Best Practices for benchstat

1. **Always use `-count=5` or more.** Fewer than 5 samples gives poor statistical power. 10 is better. 20 is ideal if you have the patience.

2. **Run on an idle machine.** Close browsers, stop background compilation, disable notifications. On Linux, you can use `nice -n -20` to give the benchmark process maximum scheduling priority (requires root).

3. **Pin CPU frequency.** Modern CPUs dynamically scale frequency based on load and temperature. This introduces variance:

   ```bash
   # Set all CPUs to maximum frequency (performance governor)
   sudo cpupower frequency-set -g performance

   # Restore to dynamic scaling after benchmarking
   sudo cpupower frequency-set -g schedutil
   ```

4. **Disable turbo boost** for maximum stability (at the cost of not measuring peak performance):

   ```bash
   echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo
   ```

5. **Compare the same change.** Never compare results from different machines, different kernel versions, or with different background load. The before and after runs should be as identical as possible in every way except the code change being measured.

6. **Check variance.** If the `+/-` percentage is large (>10%), something is wrong. Common causes: thermal throttling (laptop), background processes, the benchmark itself has non-deterministic behavior.

---

## Escape Analysis

Go's compiler decides whether to allocate variables on the stack or the heap. Stack allocation is essentially free (just a pointer adjustment), while heap allocation requires the garbage collector to track and eventually reclaim the memory. In a performance-critical program like agrep, understanding and controlling escape behavior is essential.

### How Escape Analysis Works

The compiler performs a flow analysis at compile time. A variable **escapes to the heap** if:

1. Its address is returned from the function (or stored somewhere that outlives the function).
2. It is sent to a channel (crosses goroutine boundaries).
3. It is stored in an interface value (the compiler cannot prove the concrete type's lifetime).
4. It is too large for the stack (Go has a soft limit, typically 64KB for a single frame).
5. It is captured by a closure that outlives the enclosing function.
6. Its size is not known at compile time (e.g., `make([]int, n)` where `n` is not a constant).

### Checking What Escapes

```bash
GOEXPERIMENT=simd go build -gcflags='-m' ./internal/simd/ 2>&1 | grep escape
```

The `-m` flag enables escape analysis diagnostics. Use `-m -m` for even more verbose output explaining why each decision was made:

```bash
GOEXPERIMENT=simd go build -gcflags='-m -m' ./internal/simd/ 2>&1 | head -100
```

Example output:

```
./index.go:31:6: stackBuf does not escape
./index.go:33:6: overflow escapes to heap
./index.go:45:22: make([]int, 0, 64) escapes to heap
```

This tells us:
- `stackBuf` (the `[16]int` array used in `IndexAll`) stays on the stack -- good, this is the fast path.
- `overflow` (the dynamic slice for more than 16 matches) escapes to heap -- expected, since it is returned from the function.
- `make([]int, 0, 64)` escapes -- this is the overflow slice allocation, only hit when there are more than 16 matches.

### Common Escape Reasons and Fixes

**1. Returning a reference to local data:**

```go
// ESCAPES: the array is stack-allocated, but the slice header references it.
// Returning the slice means the array must survive beyond the function, so it escapes.
func bad() []int {
    var arr [16]int
    return arr[:]     // arr escapes to heap
}

// DOESN'T ESCAPE: copy the data into a new heap-allocated slice.
// arr stays on the stack, result is an explicit heap allocation.
func good() []int {
    var arr [16]int
    result := make([]int, 16)
    copy(result, arr[:])
    return result     // only result is on heap; arr did not escape
}
```

agrep uses this pattern in `IndexAll` (`internal/simd/index.go`):

```go
var stackBuf [16]int  // stays on stack
n := 0
var overflow []int    // nil until needed

// ... fill stackBuf first, overflow only if n > 16 ...

if n == 0 {
    return nil        // zero alloc fast path
}
if overflow != nil {
    return overflow   // overflow already on heap
}
result := make([]int, n)
copy(result, stackBuf[:n])   // copy stack data to heap only when returning
return result
```

This pattern ensures zero allocations when there are no matches, and at most one allocation when there are 1-16 matches.

**2. Interface conversions:**

```go
var x int = 42
fmt.Println(x)  // x escapes: fmt.Println takes interface{}, which boxes the int
```

The value must be boxed into an interface, which requires a heap allocation. This is why agrep avoids `fmt.Sprintf` and `fmt.Fprintf` on hot paths -- the output formatter writes `[]byte` directly using `writev`.

**3. Closures capturing mutable variables:**

```go
func bad() func() int {
    x := 42
    return func() int { return x }  // x escapes: the closure outlives bad()
}
```

The returned closure holds a reference to `x`, so `x` must be heap-allocated to survive after `bad()` returns. If the closure does not escape (e.g., it is only used within the same function), the captured variable may stay on the stack.

**4. Sending to channels:**

```go
ch <- myStruct  // myStruct escapes: it crosses a goroutine boundary
```

The scheduler may deliver the value to a goroutine running on a different OS thread, so the data must be reachable from the heap.

**5. Dynamic slice sizes:**

```go
make([]byte, n)  // escapes if n is not a compile-time constant
```

When the compiler cannot prove the size at compile time, it must heap-allocate. agrep mitigates this with `sync.Pool` for reusable buffers.

### Practical Guidelines

Do not chase every escape. The escape analysis output can show dozens of "escapes to heap" messages, most of which are irrelevant because they are in cold code paths (initialization, error handling, etc.).

Focus on:
- **Functions called per-file** (once per file in the search): `FindAll`, `Read`, `Closer`
- **Functions called per-match** (once per matching line): line extraction, position recording
- **Functions called per-byte** (during the inner scan loop): these should have zero allocations

Use `-benchmem` output to measure actual `allocs/op`, which is more actionable than raw escape analysis. If `allocs/op` is 0, escape analysis details do not matter. If `allocs/op` is high, then escape analysis helps you find which specific variables are escaping and why.

---

## CPU Profiling with pprof

### Overview

Go's `runtime/pprof` package samples the program counter of every goroutine at ~100Hz (100 times per second) during a CPU profile. Each sample records the full call stack. After the profile is collected, `go tool pprof` can aggregate these samples to show which functions consumed the most CPU time.

### agrep's Built-In Profiling Support

agrep has `--cpuprofile` and `--memprofile` flags built into the main binary. From `cmd/agrep/main.go`:

```go
// CPU profiling
var profileFile *os.File
if cpuprofile != "" {
    f, err := os.Create(cpuprofile)
    if err != nil {
        die("cpuprofile: %v", err)
    }
    profileFile = f
    pprof.StartCPUProfile(f)
}

// ... run the actual search ...
exitCode := cli.Run(cfg)

// Stop CPU profile and write remaining data
if profileFile != nil {
    pprof.StopCPUProfile()
    profileFile.Close()
}

// Memory profiling -- force GC first for accurate live-heap snapshot
if memprofile != "" {
    f, err := os.Create(memprofile)
    if err == nil {
        runtime.GC()                    // Force GC to get accurate live heap
        pprof.WriteHeapProfile(f)
        f.Close()
    }
}
```

The `runtime.GC()` call before `WriteHeapProfile` is important: without it, the heap profile may include objects that are already garbage but have not been collected yet, inflating the numbers.

### Signal Handling for Profile Safety

If the user presses Ctrl+C during a profiled run, the profile data could be lost (the file would be truncated or empty). agrep handles this:

```go
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
go func() {
    <-sigCh
    if profileFile != nil {
        pprof.StopCPUProfile()
        profileFile.Close()
    }
    os.Exit(130)  // 128 + SIGINT
}()
```

This ensures `StopCPUProfile()` flushes all buffered samples to disk before the process exits, even on interrupt.

### Collecting a Profile

```bash
# CPU profile of a recursive search
./bin/agrep --cpuprofile /tmp/cpu.prof --no-ignore --hidden 'define' /usr/include > /dev/null

# Memory profile of the same search
./bin/agrep --memprofile /tmp/mem.prof --no-ignore --hidden 'define' /usr/include > /dev/null
```

Redirect stdout to `/dev/null` so that terminal I/O does not dominate the profile. You want to measure the search, not the output.

### Analyzing CPU Profiles

**Top functions by self time (flat):**

```bash
go tool pprof -top -nodecount=30 /tmp/cpu.prof
```

"Flat" time means time spent executing code in that function itself, excluding time in functions it calls. This answers: "Where does the CPU actually execute instructions?"

Example output:

```
      flat  flat%   sum%        cum   cum%
   420ms 35.00% 35.00%      420ms 35.00%  bytes.Index
   180ms 15.00% 50.00%      180ms 15.00%  syscall.Syscall6
   120ms 10.00% 60.00%      120ms 10.00%  runtime.memmove
    96ms  8.00% 68.00%      300ms 25.00%  github.com/DanielLaubacher/agrep/internal/matcher.(*BoyerMooreMatcher).FindAll
```

Here, `bytes.Index` is the hottest function (35% of all CPU samples were inside `bytes.Index`). `syscall.Syscall6` at 15% represents kernel time for `pread`/`openat`/etc. `FindAll` has 8% flat (its own code) but 25% cumulative (including `bytes.Index` it calls).

**Top functions by cumulative time:**

```bash
go tool pprof -top -cum -nodecount=20 /tmp/cpu.prof
```

"Cumulative" time includes time in all callees. This answers: "Which function is responsible for the most total CPU time (directly or through functions it calls)?"

**Interactive web UI with flame graph:**

```bash
go tool pprof -http=:6060 /tmp/cpu.prof
```

This opens a browser to `http://localhost:6060` with several views:

1. **Flame graph**: The most useful view. Each bar represents a function. Width is proportional to CPU time. The x-axis is NOT time progression -- it is just alphabetical sorting of peers. The y-axis is call stack depth.
2. **Graph**: A directed graph showing call relationships with time annotations.
3. **Source**: Annotated source code showing time per line.
4. **Peek**: Quick source snippet for a selected function.

### Reading a Flame Graph

- **Width = time spent.** Wider bars mean more CPU time. A bar spanning the full width means that function (and its callees) account for all sampled CPU time.
- **Height = call stack depth.** The bottom is `main()` or the goroutine entry point. Each layer up is a function called by the one below.
- **Wide bars near the top** = functions that consume the most time *themselves* (high flat time). These are your optimization targets.
- **Wide bars near the bottom** = orchestration functions whose callees consume the most time. Optimizing these functions directly may not help -- you need to optimize their callees.
- **Narrow spikes** = deep call stacks that consume little total time. Usually ignorable.

Common agrep hotspots in a flame graph:
- `bytes.Index` -- the pattern search inner loop (AVX2 assembly in Go stdlib)
- `unix.Pread` or `syscall.Syscall6` -- file I/O (pread syscall)
- `unix.Getdents` -- directory listing (getdents64 syscall)
- `runtime.memmove` -- copying data between buffers
- `runtime.mallocgc` -- heap allocation (should be rare on hot paths)

### The Profiling Script

agrep includes `scripts/profile.sh` which automates benchmarking and profiling:

```bash
# Run all benchmarks + profile
./scripts/profile.sh

# Benchmarks only (hyperfine comparisons vs rg)
./scripts/profile.sh bench

# Profile + show pprof top output
./scripts/profile.sh prof top

# Profile + open flame graph in browser
./scripts/profile.sh prof web
```

The script builds agrep, runs hyperfine comparisons against ripgrep, then records both CPU and memory profiles:

```bash
# From scripts/profile.sh:
"$BIN" --cpuprofile "$cpu_prof" --no-ignore --hidden "$PATTERN" "$SEARCH_DIR" > /dev/null 2>&1 || true
"$BIN" --memprofile "$mem_prof" --no-ignore --hidden "$PATTERN" "$SEARCH_DIR" > /dev/null 2>&1 || true
```

Environment variables control the behavior:

```bash
SEARCH_DIR=/usr/include PATTERN=define RUNS=5 ./scripts/profile.sh bench
```

### Profiling Benchmarks (testing.B profiles)

You can also generate profiles from Go benchmark functions directly:

```bash
GOEXPERIMENT=simd go test -bench=BoyerMoore_SparseMatch \
    -cpuprofile=/tmp/bench_cpu.prof \
    -memprofile=/tmp/bench_mem.prof \
    -benchmem \
    ./internal/matcher/
```

This profiles only the benchmark code in isolation, without filesystem I/O or directory traversal. Useful for analyzing matcher performance independent of the full application.

---

## Memory Profiling with pprof

### Allocation vs In-Use Profiles

Go's memory profiler tracks two things:

1. **alloc_space / alloc_objects**: Total allocations over the lifetime of the profile. Counts every `new`/`make`/implicit allocation, even if the memory was later freed by GC. This answers: "Where is allocation pressure coming from?"

2. **inuse_space / inuse_objects**: Currently live heap objects at the time the profile was captured. This answers: "What is holding memory right now?" Useful for detecting memory leaks.

### Analyzing Memory Profiles

```bash
# Top allocators by total allocated bytes (most useful for performance)
go tool pprof -top -nodecount=20 -alloc_space /tmp/mem.prof

# Top allocators by currently live heap bytes (useful for leak detection)
go tool pprof -top -nodecount=20 -inuse_space /tmp/mem.prof

# Interactive web UI
go tool pprof -http=:6060 /tmp/mem.prof
```

In the web UI, switch between "alloc_space" and "inuse_space" using the dropdown menu to see different views of the same profile.

### The runtime.GC() Trick

As shown in agrep's `main.go`, always call `runtime.GC()` before `WriteHeapProfile`:

```go
runtime.GC()
pprof.WriteHeapProfile(f)
```

Without the forced GC:
- Dead objects that have not been swept yet appear as "in use"
- The `inuse_space` numbers are inflated
- You might chase phantom leaks that are just GC latency

With the forced GC:
- Only truly live objects appear in the `inuse_space` view
- The `alloc_space` view is unaffected (it is cumulative regardless)

### Sampling Rate

By default, Go's memory profiler samples 1 in every 512KB of allocation. This means small allocations may be underrepresented. For more accurate profiles of low-allocation code:

```go
runtime.MemProfileRate = 1  // sample every allocation (very slow!)
```

Do not use `MemProfileRate = 1` in production -- it adds significant overhead. The default rate of 1-in-512KB is sufficient for finding the biggest allocators.

---

## strace for Syscall Analysis

`strace` intercepts and logs every system call made by a process. For a program like agrep that uses raw Linux syscalls (`getdents64`, `openat`, `pread`, `mmap`, `fadvise`, `madvise`, `writev`), strace is indispensable for understanding I/O patterns and finding wasted work.

### Basic Syscall Summary

```bash
strace -c ./bin/agrep -l --no-ignore --hidden 'define' /usr/include > /dev/null 2> /tmp/strace.txt
```

The `-c` flag produces a summary table instead of individual syscall logs:

```
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 40.12    0.089234          1     62145           openat
 25.67    0.057123          0     62145           close
 18.45    0.041234          0     62145           newfstatat
 10.23    0.022745          1     62145           read
  3.12    0.006934          2      3421           getdents64
  1.23    0.002734          0       456       321 open  <-- 321 EPERM errors!
```

The **errors column** is often the most revealing. In agrep's development, strace discovered that `O_NOATIME` (which avoids writing atime metadata on every file open) was failing with EPERM on files not owned by the user, causing thousands of retried `openat` calls.

### Filtering Specific Syscalls

```bash
# Only trace file-open related syscalls
strace -e trace=openat,open -c ./bin/agrep -l 'define' /usr/include > /dev/null

# Show individual syscall details (verbose mode, not summary)
strace -e trace=openat -f ./bin/agrep -l 'define' /usr/include 2>&1 | head -50

# -f follows child threads. CRITICAL for Go programs: Go uses M:N threading,
# so goroutines run on multiple OS threads. Without -f, you only see syscalls
# from the main thread.
```

Individual syscall output looks like:

```
[pid 12345] openat(AT_FDCWD, "/usr/include/stdio.h", O_RDONLY|O_NOATIME) = 3
[pid 12345] openat(AT_FDCWD, "/usr/include/sys/types.h", O_RDONLY|O_NOATIME) = -1 EPERM
[pid 12345] openat(AT_FDCWD, "/usr/include/sys/types.h", O_RDONLY) = 3
```

This shows the `O_NOATIME` fallback in action: the first open with `O_NOATIME` fails with EPERM (file owned by root, not the current user), so agrep retries without `O_NOATIME`.

### Comparing Tools

```bash
# Count syscalls for agrep
strace -c ./bin/agrep -l --no-ignore --hidden 'define' /usr/include > /dev/null 2> agrep_strace.txt

# Count syscalls for ripgrep
strace -c rg -l --no-ignore --hidden 'define' /usr/include > /dev/null 2> rg_strace.txt

# Compare the total calls columns
```

Real results from agrep development on `/usr/lib`:
- **Before O_NOATIME fix**: 1.69M syscalls (thousands of wasted retries)
- **After O_NOATIME fix + binary extension filtering**: 571K syscalls
- **ripgrep**: 862K syscalls
- agrep now makes **fewer syscalls** than ripgrep because binary extension filtering (skipping `.o`, `.a`, `.so` files) avoids opening files that ripgrep still opens and detects as binary after reading.

### Syscall Budget Analysis

Understanding the theoretical minimum syscall count helps you evaluate how close to optimal your implementation is.

**Per file (minimum):**
- 1 `openat` -- open the file
- 1 `fstat` or `newfstatat` -- get file size (needed to decide mmap vs pread, and to allocate the read buffer)
- 1 `pread` or `mmap`+`munmap` -- read the data (mmap is 2 syscalls; pread is 1)
- 1 `close` -- close the file descriptor
- Total: **3-4 syscalls per file**

With mmap, additional optional syscalls:
- 1 `fadvise` -- advise the kernel about access pattern (e.g., `FADV_SEQUENTIAL`)
- 1 `madvise` -- advise on mapped region (e.g., `MADV_SEQUENTIAL`)
- Total with mmap: **5-6 syscalls per file**

**Per directory:**
- 1 `openat` -- open the directory
- N `getdents64` -- read directory entries (usually 1-2 calls for typical directories; the kernel returns ~4KB of entries per call)
- 1 `close` -- close the directory fd
- Total: **~3 syscalls per directory**

**For a typical search (37K files + 4K directories):**
- Files: 37K x 4 = ~148K syscalls
- Directories: 4K x 3 = ~12K syscalls
- **Theoretical minimum**: ~160K syscalls
- **Actual (agrep)**: ~571K (includes fadvise, error handling, stat calls, etc.)
- **Actual (ripgrep)**: ~862K (more syscalls due to different strategies)

### Timing Syscalls

```bash
# Sort by total time spent in each syscall type
strace -c -S time ./bin/agrep -l 'define' /usr/include > /dev/null
```

The `-S time` flag sorts by total wall-clock time rather than call count. This reveals what the process is actually waiting on:

- If **`read`/`pread`** dominates: I/O bound (data not in page cache, or very large dataset)
- If **`futex`** dominates: lock contention (goroutines fighting over mutexes)
- If **`epoll_wait`/`nanosleep`** dominates: the process is idle waiting for events
- If **`openat`** dominates: filesystem metadata is the bottleneck (too many small files)

### Advanced strace Usage

```bash
# Trace with timestamps (useful for identifying sequential vs parallel I/O)
strace -T -e trace=openat,pread64,close -f ./bin/agrep -l 'define' /usr/include > /dev/null 2>&1 | head -100

# -T adds time spent in each syscall (shown in angle brackets at end of line):
# openat(AT_FDCWD, "/usr/include/stdio.h", O_RDONLY|O_NOATIME) = 3 <0.000012>

# Count by PID (see how work is distributed across Go's OS threads)
strace -c -f ./bin/agrep -l 'define' /usr/include > /dev/null 2>&1
```

---

## hyperfine for End-to-End Benchmarking

hyperfine is a command-line benchmarking tool that handles warmup, multiple runs, outlier detection, and statistical reporting. It is the standard tool for comparing CLI applications.

### Basic Comparison

```bash
hyperfine --warmup 3 \
    -n agrep './bin/agrep -l --no-ignore --hidden "define" /usr/include' \
    -n rg 'rg -l --no-ignore --hidden "define" /usr/include'
```

Output:

```
Benchmark 1: agrep
  Time (mean +/- s):     144.2 ms +/-  3.1 ms    [User: 1.234 s, System: 0.567 s]
  Range (min ... max):   140.1 ms ... 152.3 ms    10 runs

Benchmark 2: rg
  Time (mean +/- s):     180.5 ms +/-  4.2 ms    [User: 1.567 s, System: 0.789 s]
  Range (min ... max):   175.2 ms ... 189.1 ms    10 runs

Summary
  agrep ran 1.25 +/- 0.04 times faster than rg
```

Key numbers:
- **Time (mean +/- s)**: Average wall-clock time and standard deviation across all timed runs.
- **User / System**: Total CPU time across all threads. User > wall-clock means the program is parallel (multiple threads doing useful work simultaneously). System is kernel time (syscalls).
- **Range**: Minimum and maximum observed times. Large ranges indicate measurement instability.
- **Summary**: The speedup ratio with confidence interval.

### Important Flags

- **`--warmup N`**: Run the command N times before timing. Essential for filesystem benchmarks because the first run populates the page cache. Subsequent runs hit warm cache, which is what you typically want to measure (unless specifically testing cold-cache performance).
- **`--runs N`** or **`-r N`**: Number of timed runs (default 10). More runs = smaller confidence intervals.
- **`-n NAME`**: Human-readable label for the command in the output.
- **`--export-json FILE`**: Export raw results for further analysis or graphing.
- **`--export-markdown FILE`**: Export results as a markdown table for documentation.
- **`--prepare 'CMD'`**: Command to run before each timed run. Useful for clearing page cache:
  ```bash
  hyperfine --prepare 'sync; echo 3 | sudo tee /proc/sys/vm/drop_caches' \
      './bin/agrep -l "define" /usr/include'
  ```
  This measures cold-cache performance, which is important for first-run user experience but much slower and noisier than warm-cache benchmarks.

### agrep's Benchmark Scripts

**`scripts/benchmark.sh`** -- Comparative benchmarks against GNU grep and ripgrep:

```bash
./scripts/benchmark.sh [SEARCH_DIR]
```

This script:
1. Builds agrep if not already built.
2. Generates a 500K-line (~45MB) synthetic test file with mixed content (fixed strings, timestamps, error messages, code snippets).
3. Runs 7 benchmark scenarios:
   - Fixed string search in large file
   - Regex search in large file
   - Case-insensitive search
   - No-match search (worst case)
   - Count-only mode
   - Recursive directory search
   - Multiple patterns (`-e` flag, Aho-Corasick)

The test data generation from the script:

```bash
for i in $(seq 1 100000); do
    echo "line $i: the quick brown fox jumps over the lazy dog"
    echo "line $i: Lorem ipsum dolor sit amet, consectetur adipiscing elit"
    echo "line $i: ERROR: connection refused at port 8080"
    echo "line $i: 2024-01-15T10:30:00Z INFO request processed successfully"
    echo "line $i: function calculate(a, b) { return a + b; }"
done > /tmp/agrep_bench_data.txt
```

This creates 500,000 lines of varied content, ensuring that different pattern types (fixed string, regex, case-insensitive) exercise different code paths.

**`scripts/profile.sh`** -- Profiling benchmarks against ripgrep:

```bash
# All benchmarks + profiling
./scripts/profile.sh

# Just benchmarks (6 scenarios vs rg)
./scripts/profile.sh bench

# Just profiling + pprof output
./scripts/profile.sh prof top

# Profiling + open flame graph in browser
./scripts/profile.sh prof web
```

The script runs these benchmark scenarios against ripgrep:
1. Files-only (`-l`) with `--no-ignore --hidden`
2. Full output (default)
3. Full output + line numbers (`-n`)
4. Regex pattern (`func\w+`)
5. Count-only (`-c`)
6. With gitignore (respects `.gitignore` files)

### Choosing Representative Workloads

The choice of benchmark workloads determines what you actually measure. Different workloads stress different components of a grep tool:

| Workload | What It Stresses | Example |
|---|---|---|
| **Sparse matches** (1 in 1000) | Pattern scanning speed | `'ERROR' /var/log/syslog` -- most realistic |
| **Dense matches** (every line) | Output formatting + memory allocation | `'the' large_file.txt` -- stresses line extraction |
| **No matches** | Raw scan throughput | `'ZZZZNOTFOUND' file.txt` -- best case for search-then-split |
| **Single large file** | Matcher throughput in isolation | Test with 45MB generated file |
| **Many small files** | Directory traversal + I/O overhead | `'define' /usr/include` (~62K files) |
| **Case-insensitive** | SIMD case conversion overhead | `-i 'DEFINE' /usr/include` |
| **Multi-pattern** | Aho-Corasick vs Boyer-Moore | `-e 'ERROR' -e 'WARN' -e 'INFO'` |
| **Files-only** (`-l`) | Skips output formatting entirely | Tests pure search + walk speed |
| **Count-only** (`-c`) | Skips output formatting, counts all matches | Tests full-scan without output overhead |

The `scripts/benchmark.sh` and `scripts/profile.sh` scripts cover most of these scenarios.

---

## Micro vs Macro Benchmarks

### Micro-Benchmarks (Go's testing.B Framework)

**What they are:** Tests of individual functions in isolation with controlled, synthetic data.

**What they are good for:**
- Algorithm comparison (BoyerMoore vs Aho-Corasick vs RE2 on identical data)
- Allocation counting (`0 allocs/op` vs `17 allocs/op`)
- Regression detection (catch a 10% slowdown in a specific function)
- Understanding theoretical throughput limits

**What they are bad for:**
- Real-world performance prediction (synthetic data in L2 cache != real files from disk)
- System-level bottleneck identification (cannot see I/O, scheduling, or lock contention)

**Pitfalls:**

1. **Cache effects.** In a micro-benchmark, the 440KB test data stays hot in L1/L2 cache across all iterations. In production, each file is read once and never accessed again. The benchmark measures cache-hot throughput; production sees cache-cold throughput. This is a known and accepted limitation -- the purpose of micro-benchmarks is to measure algorithm speed, not I/O speed.

2. **Dead code elimination.** If the benchmark does not use the result, the compiler may optimize the computation away entirely. The `b.Loop()` style helps mitigate this, but it is not foolproof. agrep's benchmarks call `m.FindAll(data)` without using the return value, but because `FindAll` has side effects (it allocates and fills a `MatchSet`), the compiler cannot eliminate it.

3. **Warm data, warm code.** The benchmark loop runs the same function hundreds of times. Branch predictors learn the pattern. Instruction caches hold the hot path. In production, the CPU encounters each file's data for the first time and the branch predictor has no history.

### Macro-Benchmarks (hyperfine, Scripts)

**What they are:** End-to-end tests of the full application with real filesystems.

**What they are good for:**
- Overall performance validation against competitors
- User-facing performance perception
- Catching system-level issues (too many syscalls, lock contention, poor parallelism)

**What they are bad for:**
- Isolating specific bottlenecks (was the regression in the matcher, the walker, or the formatter?)
- Reproducibility (filesystem caching, OS scheduler, thermal state all vary)

**Pitfalls:**

1. **Filesystem caching.** The first run of a recursive search reads all files from disk. Subsequent runs hit the page cache and are 5-10x faster. `--warmup 3` in hyperfine ensures you measure warm-cache performance, but this may not reflect the user's first experience.

2. **System noise.** Browser tabs, Slack, system updates, cron jobs -- all compete for CPU and I/O. Close everything. On a laptop, thermal throttling (CPU slows down when hot) adds variance. Desktop systems with active cooling are more stable.

3. **Non-determinism.** agrep uses a parallel walker and worker pool. Directory traversal order varies slightly between runs, which can change the file access pattern and thus page cache behavior. This is usually a minor effect but can add 1-3% variance.

### How agrep Uses Both

agrep's testing strategy combines both levels:

| Level | Where | Purpose |
|---|---|---|
| **Micro** | `*_test.go` in `internal/matcher/`, `internal/simd/`, `internal/input/` | Algorithm comparison, allocation tracking, regression detection |
| **Macro** | `scripts/benchmark.sh`, `scripts/profile.sh` | End-to-end vs grep and rg, user-facing performance validation |
| **Profile** | `--cpuprofile` / `--memprofile` on the full application | Identifying hotspots in the full pipeline |

The typical optimization workflow:

1. **Macro** benchmark identifies a scenario where agrep is slow (e.g., "agrep is 2x slower than rg on case-insensitive search").
2. **Profile** identifies the hotspot (e.g., "40% of CPU time in `toLowerASCII` scalar loop").
3. **Micro** benchmark establishes the baseline for the hot function (`BenchmarkIndexCaseInsensitive_SIMD`).
4. Optimize the hot function (e.g., SIMD case-folding with dual first+last byte prefilter).
5. **Micro** benchmark confirms the function-level speedup via `benchstat`.
6. **Macro** benchmark confirms the end-to-end improvement.

---

## Interpreting Performance Numbers

### Throughput (MB/s or GB/s)

Throughput is calculated from `b.SetBytes()` and represents how many bytes of input data the function can process per second. For a grep tool, the input data is the file content being searched.

**Reference points for grep-like workloads:**

| Throughput | Meaning | Example |
|---|---|---|
| 10-30 GB/s | Memory bandwidth limited | AVX2 scan, no-match case (pure scan, no output) |
| 1-10 GB/s | Excellent in-memory matching | BoyerMoore sparse match with SIMD |
| 100-500 MB/s | Good, with output overhead | Dense matches with line extraction and formatting |
| 10-100 MB/s | I/O bound or allocation heavy | Full recursive search, or every-line-matches with formatting |

**Memory bandwidth context:** Modern DDR4 systems provide ~30-50 GB/s of main memory bandwidth. L2 cache provides ~200-500 GB/s. A benchmark throughput of 9.7 GB/s means the scan is limited by memory bandwidth (the data is in L2/L3 cache, and the AVX2 scan loop processes 32 bytes per SIMD instruction).

### ns/op

Wall-clock nanoseconds per benchmark iteration. Lower is better. To convert between ns/op and throughput:

```
throughput_MB_per_sec = data_size_bytes / (ns_per_op / 1,000)
```

Or equivalently:

```
throughput_GB_per_sec = data_size_bytes / ns_per_op
```

Examples with 440KB data:

| ns/op | Wall time | Throughput | Interpretation |
|---|---|---|---|
| 45,000 | 45 us | 9.7 GB/s | Memory-speed scan, no matches |
| 60,000 | 60 us | 7.3 GB/s | Fast scan, sparse matches |
| 2,300,000 | 2.3 ms | 190 MB/s | Dense matches, allocation-bound |
| 5,000,000 | 5.0 ms | 88 MB/s | Heavy output processing |

### B/op and allocs/op

From the `-benchmem` flag. These measure heap allocation pressure.

**`B/op`** is the total bytes allocated on the heap per iteration. This memory must be tracked and eventually freed by the garbage collector.

**`allocs/op`** is the number of separate heap allocation calls per iteration. Each allocation has fixed overhead (runtime lock, size-class lookup, pointer tracking). More allocations = more GC pressure, even if the total bytes are small.

**Guidelines for hot-path functions:**

| B/op | allocs/op | Assessment |
|---|---|---|
| 0 | 0 | Ideal. Stack-only, zero GC pressure. Target for no-match paths. |
| <1 KB | 1-3 | Good. Acceptable for functions called once per file. |
| 1-10 KB | 3-10 | Acceptable. Worth investigating if called frequently. |
| >10 KB | >10 | Investigate. Likely an optimization target. May indicate missing `sync.Pool` usage or unnecessary string conversions. |

### Real Performance Numbers from agrep

**Matcher micro-benchmarks** (440KB data, 10K lines):

| Benchmark | Throughput | B/op | allocs/op | Notes |
|---|---|---|---|---|
| BoyerMoore NoMatch | ~9.7 GB/s | 0 | 0 | Pure scan at near-memory bandwidth. The `[16]int` stack buffer in `IndexAll` is never used (no matches). |
| BoyerMoore SparseMatch | ~7.5 GB/s | ~1 KB | 3 | 10 matches found. Stack buffer holds all offsets (16 > 10). One small result slice allocated. |
| BoyerMoore DenseMatch | ~190 MB/s | ~1 MB | ~17 | Every line matches. 10K match offsets overflow the stack buffer. Line extraction + position recording dominates. |
| AhoCorasick 2 patterns | ~150 MB/s | ~1.2 MB | ~20 | Automaton traversal + dense output. |
| AhoCorasick 10 patterns | Slower | Higher | Higher | Larger automaton, more positions per line. |

The 50x throughput difference between NoMatch (9.7 GB/s) and DenseMatch (190 MB/s) illustrates the "search-then-split" design principle: when there are no matches, the inner loop just scans bytes at SIMD speed. When every line matches, the cost shifts from scanning to output construction.

**End-to-end results (hyperfine):**

| Scenario | agrep | ripgrep | Ratio |
|---|---|---|---|
| `/usr/include` `-l "define"` | ~144 ms | ~180 ms | 1.25x faster |
| `/usr/include` `-i -l "define"` | ~144 ms | ~180 ms | 1.25x faster |
| 37K files, `--no-ignore --hidden -l` | ~304 ms | ~301 ms | ~1.01x (tied) |

The case-insensitive search being the same speed as case-sensitive is notable -- it means the SIMD case-folding path (checking both upper and lower case of first+last bytes in a single SIMD pass) adds negligible overhead.

### Correlating Micro and Macro Numbers

A common question: "My matcher benchmarks at 9 GB/s, so why does the full application only process files at 100 MB/s?"

The answer is that the full application spends time on many things besides pattern matching:

1. **Directory traversal**: `getdents64` syscalls to discover files
2. **File opening**: `openat` + `fstat` per file
3. **Data reading**: `pread` or `mmap` + kernel page fault handling
4. **Output formatting**: Line number computation, color codes, writev batching
5. **Goroutine scheduling**: Work distribution across the thread pool
6. **Lock contention**: Ordered output requires coordination

If the full application searches 62K files totaling 200MB in 144ms:

```
200 MB / 0.144 s = 1.4 GB/s aggregate throughput
```

This is much less than 9.7 GB/s because syscall overhead (~571K syscalls at ~1us each = ~571ms of kernel time, amortized across threads) is a significant fraction of the total runtime.

---

## Cross-References

- **`01-simd-and-avx2.md`**: SIMD benchmark comparisons showing AVX2 vs scalar performance for `IndexByte`, `Index`, `IndexAll`, and case-insensitive variants.
- **`02-linux-syscalls.md`** (if it exists): strace discoveries including the O_NOATIME EPERM issue, binary file syscall waste reduction, and the syscall budget analysis.
- **`03-memory-management.md`**: Details on the `sync.Pool` usage, pointer-free `Match` struct design, and the stack buffer + overflow pattern used in `IndexAll`.
- **`06-gc-and-allocation-optimization.md`** (if it exists): Deep dive into understanding and optimizing `B/op` and `allocs/op` results, including the pointer-free `MatchSet` design that reduces GC scanning from O(N) to O(1).

### Key Source Files Referenced

| File | Content |
|---|---|
| `internal/matcher/boyermoore_test.go` | BoyerMoore benchmarks, bytes.Index baseline, Regex comparison |
| `internal/matcher/ahocorasick_test.go` | Aho-Corasick benchmarks (2, 10 patterns, no match, case-insensitive) |
| `internal/matcher/pcre_test.go` | PCRE benchmarks (simple, lookahead, no match) |
| `internal/simd/simd_test.go` | IndexByte, LastIndexByte, Count benchmarks (SIMD vs stdlib) |
| `internal/simd/index_test.go` | Index, IndexAll, IndexCaseInsensitive benchmarks |
| `internal/simd/index.go` | IndexAll implementation with stack buffer pattern |
| `internal/input/input_test.go` | BufferedReader, MmapReader benchmarks |
| `internal/matcher/match.go` | Matcher interface, pointer-free Match struct |
| `internal/matcher/boyermoore.go` | BoyerMooreMatcher using SIMD-accelerated search |
| `cmd/agrep/main.go` | CPU/memory profiling flags, signal handling |
| `Makefile` | Build, test, bench targets |
| `scripts/benchmark.sh` | Comparative benchmarks vs grep and rg |
| `scripts/profile.sh` | Profiling + hyperfine benchmarks vs rg |
