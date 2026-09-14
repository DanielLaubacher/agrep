# GC Pressure, Escape Analysis, and Zero-Allocation Patterns

This document covers garbage collector internals, escape analysis mechanics, and every allocation-elimination technique used in agrep. Understanding these concepts is essential for writing high-throughput Go programs where the GC can become the dominant bottleneck.

---

## Table of Contents

1. [Go's Garbage Collector and Why Allocations Matter](#gos-garbage-collector-and-why-allocations-matter)
2. [Pointer-Free Match Struct](#pointer-free-match-struct)
3. [Stack Allocation and Escape Analysis](#stack-allocation-and-escape-analysis)
4. [The Stack Buffer Escape Trap and Fix](#the-stack-buffer-escape-trap-and-fix)
5. [sync.Pool for Buffer Reuse](#syncpool-for-buffer-reuse)
6. [Append-Style Formatters (Zero-Alloc Output)](#append-style-formatters-zero-alloc-output)
7. [unsafe.String for Zero-Copy String Construction](#unsafestring-for-zero-copy-string-construction)
8. [Per-Worker Buffer Reuse](#per-worker-buffer-reuse)
9. [Deferred Buffer Release](#deferred-buffer-release)
10. [The noopCloser Pattern](#the-noopcloser-pattern)
11. [Measuring and Verifying](#measuring-and-verifying)
12. [Cross-References](#cross-references)

---

## Go's Garbage Collector and Why Allocations Matter

### How Go's GC Works (Simplified)

Go uses a **concurrent, tri-color mark-and-sweep** garbage collector. The fundamental lifecycle:

1. **Allocation**: When code calls `make`, `new`, or creates a composite literal that escapes to the heap, Go's memory allocator (`mallocgc`) finds free space in a size-classed span and returns a pointer. The allocator maintains per-P (per-processor) caches (`mcache`) backed by a central `mcentral` and `mheap`, organized by size class (8 bytes, 16 bytes, 32 bytes, ..., up to 32KB; larger objects go to the page heap directly).

2. **Mark phase**: The GC starts from **roots** -- goroutine stacks, global variables, and finalizer-registered objects. It traces every pointer field in every live object, coloring objects in a tri-color scheme:
   - **White**: Not yet visited (candidates for collection).
   - **Gray**: Visited but children not yet scanned.
   - **Black**: Visited and all children scanned.

   The mark phase runs **concurrently** with the application using a write barrier. When a mutator (application goroutine) writes a pointer during marking, the write barrier ensures the GC doesn't miss it. The GC also performs **mark assists** -- if a goroutine is allocating faster than the GC can mark, it forces that goroutine to do some marking work before proceeding.

3. **Sweep phase**: After marking, all white objects are dead. The sweeper returns their memory to the allocator's free lists. Sweeping is done lazily -- spans are swept on demand when the allocator needs them.

The critical insight: **the GC's work is proportional to the number of live pointers, not total memory size**. A 1GB `[]byte` with no internal pointers costs the GC almost nothing to scan (just the slice header itself, which is one pointer). A 1KB struct with 100 pointer fields costs far more GC time per scan cycle.

### Why Allocations Matter for Performance

Three distinct costs compound to make heap allocations expensive in hot paths:

**1. Allocation cost (per `make`/`new`/literal)**

Every heap allocation invokes `mallocgc`:
- Lock the per-P mcache (fast path: per-P, no contention).
- Find a free slot in the appropriate size-class span.
- If the span is full, fetch a new span from mcentral (may require a lock).
- Zero the memory (unless `mallocgc` is told not to).
- Write the type descriptor pointer for the GC scanner.

For small objects (<= 32KB), this is fast but not free. For large objects, it's a `mmap`/`madvise` syscall-level operation.

**2. GC scanning cost (per cycle)**

During each GC cycle, the collector must visit every live pointer. Consider a `[]Match` where `Match` is a struct with two `[]byte` fields:

```
Match{Line: []byte, Positions: [][2]int}  // 2 pointer fields per Match
```

With 10,000 matches in flight: 20,000 pointers the GC must dereference and mark. Each pointer dereference is a potential cache miss, and the marking work cannot be parallelized within a single object graph efficiently.

Compare with a pointer-free `Match` that stores integer offsets:

```
Match{LineStart: int, LineLen: int, PosIdx: int, PosCount: int}  // 0 pointer fields
```

10,000 of these cost zero GC scanning. The GC sees the `[]Match` slice header (1 pointer to the backing array) and notes the backing array has no pointer fields -- it skips scanning the entire array.

**3. GC frequency (how often cycles run)**

The Go GC triggers when the heap grows to `GOGC/100` times the live heap from the last cycle. With the default `GOGC=100`, GC triggers when the heap doubles. More allocations per unit time means faster heap growth, which means more frequent GC cycles. Each cycle pays the full scanning cost.

The formula:

```
GC CPU overhead ~= (live pointer count * cycles per second * cost per pointer scan)
```

Reducing any of these three multiplicands reduces GC overhead.

### Why This Matters for agrep

When searching 100,000 files:
- The hot path allocates and frees a read buffer per file (via `sync.Pool`, see below).
- Each file that matches produces a `MatchSet` with potentially thousands of `Match` entries.
- Multiple workers (typically `NumCPU * 2`) are searching files concurrently, so dozens of `MatchSet` results may be live simultaneously.
- The `OrderedWriter` holds results in a pending map until they can be emitted in sequence order.

If each `Match` contained pointer fields, the live pointer count during peak throughput could reach hundreds of thousands, and GC scans would dominate CPU time. The pointer-free `Match` design reduces this to approximately `3 * concurrent_files` pointers -- typically under 100.

---

## Pointer-Free Match Struct

**File**: `internal/matcher/match.go`

This is the single most impactful allocation optimization in agrep. The `Match` struct represents one matched (or context) line, and the design ensures it contains **zero pointer fields**:

```go
// Match is a pointer-free struct — no GC scanning per match
type Match struct {
    LineNum    int   // 1-based line number (0 = group separator)
    LineStart  int   // byte offset of line snippet start in MatchSet.Data
    LineLen    int   // length of line snippet in bytes
    ByteOffset int64 // byte offset of line start within the original file
    PosIdx     int   // start index into MatchSet.Positions
    PosCount   int   // number of highlight positions for this match
    IsContext  bool
}
```

Every field is a scalar: `int`, `int64`, or `bool`. No `[]byte`, no `string`, no `[][2]int`, no `interface{}`. The Go compiler generates a **type descriptor** for `Match` that tells the GC "this type has no pointer fields." When the GC encounters a `[]Match` backing array, it reads the type descriptor, sees "no pointers," and **skips the entire array** regardless of length.

### Where the Pointers Live

The pointer-containing fields are consolidated into `MatchSet`, which acts as the shared owner:

```go
// MatchSet holds the pointer-containing fields (only 3 GC roots per file result)
type MatchSet struct {
    Data      []byte   // the file data buffer (1 pointer for slice header)
    Matches   []Match  // pointer-free match structs (1 pointer for slice header)
    Positions [][2]int // shared positions array (1 pointer for slice header)
}
```

A `MatchSet` has exactly 3 slice headers, each of which is a pointer the GC must trace. That is **O(1) pointers per file result**, regardless of whether the file has 1 match or 50,000 matches.

### How Line Content Is Resolved Without Pointers

Instead of storing `Line []byte` in each `Match`, we store `LineStart` and `LineLen` as integer offsets into `MatchSet.Data`. The content is reconstructed on demand:

```go
func (ms *MatchSet) LineBytes(i int) []byte {
    m := &ms.Matches[i]
    return ms.Data[m.LineStart : m.LineStart+m.LineLen]
}
```

This performs a **sub-slice** operation. In Go, sub-slicing creates a new slice header (a 3-word struct: pointer, length, capacity) that points into the same backing array as the original. The new slice header is returned by value -- it lives on the caller's stack or in a register, not on the heap. **Zero heap allocation.**

The same pattern applies to match highlight positions:

```go
func (ms *MatchSet) MatchPositions(i int) [][2]int {
    m := &ms.Matches[i]
    if m.PosCount == 0 {
        return nil
    }
    return ms.Positions[m.PosIdx : m.PosIdx+m.PosCount]
}
```

Each `Match` stores `PosIdx` (start index into the shared `Positions` slice) and `PosCount` (how many positions this match owns). The sub-slice `ms.Positions[PosIdx : PosIdx+PosCount]` is again a zero-allocation operation.

### The Alternative Design and Its Cost

If `Match` were designed naively:

```go
// BAD: Pointer-heavy design
type Match struct {
    LineNum    int
    Line       []byte    // pointer to backing array
    Positions  [][2]int  // pointer to backing array
    ByteOffset int64
    IsContext  bool
}
```

Each `Match` now has 2 pointer fields (`Line` and `Positions` each contain a pointer as their first word). With 5,000 matches in a dense file:

- **Pointer-free design**: 3 pointers total (the 3 MatchSet slice headers).
- **Pointer-heavy design**: 10,003 pointers (3 MatchSet headers + 5,000 * 2 per-match pointers).

That is a 3,334x increase in GC scanning work per file result. When multiple dense-match files are in flight concurrently, the difference between 2-6% GC CPU overhead and 25-40% GC CPU overhead.

### Why `[2]int` Arrays Don't Count as Pointers

`[2]int` is a fixed-size array of integers. In Go's type system, `[2]int` has no pointer fields. A `[][2]int` is a slice of these arrays -- the slice header has one pointer (to the backing array), but the backing array itself contains no pointers. So `[][2]int` costs exactly 1 pointer for the GC, regardless of how many `[2]int` elements it contains.

This is why the positions are stored as `[][2]int` (slice of int-pairs) rather than, say, `[][]int` (slice of slices), which would make each element a pointer the GC must follow.

---

## Stack Allocation and Escape Analysis

### What Escape Analysis Is

Go's compiler performs **escape analysis** at compile time to determine whether each variable can live on the goroutine's stack or must be allocated on the heap.

- **Stack allocation**: Nearly free. The stack pointer is decremented to "allocate" and the entire stack frame is reclaimed when the function returns. No GC involvement whatsoever.
- **Heap allocation**: Involves `mallocgc`, must be tracked by the GC, and will be swept in a future cycle.

A variable **escapes to the heap** when the compiler cannot prove it does not outlive the function that created it. The compiler is conservative: if there is any possibility the variable could be referenced after the function returns, it must go on the heap.

### How to Check Escape Analysis Decisions

The `-gcflags='-m'` flag prints escape analysis decisions at compile time:

```bash
GOEXPERIMENT=simd go build -gcflags='-m' ./internal/simd/ 2>&1 | grep -E 'escape|heap'
```

Each line shows a variable and why it escapes. Use `-gcflags='-m -m'` (double `-m`) for verbose reasoning that explains the full chain of why something escapes.

Example output:

```
./index.go:60:16: make([]int, n) escapes to heap
./index.go:31:6: stackBuf does not escape
```

### Common Escape Causes

Understanding these patterns is essential for predicting and preventing heap allocations:

**1. Returning a pointer to a local variable**

```go
func newFoo() *Foo {
    f := Foo{X: 42}
    return &f       // f escapes: caller holds a reference that outlives newFoo
}
```

The variable `f` must live on the heap because the returned pointer is valid after `newFoo`'s stack frame is gone.

**2. Returning a slice of a local array**

```go
func makeSlice() []int {
    var arr [8]int
    return arr[:]   // arr escapes: the returned slice references arr's memory
}
```

Even though `arr` is small, the returned slice's backing array is `arr`, so `arr` must survive the function return.

**3. Interface conversion**

```go
func log(v any) { fmt.Println(v) }

func example() {
    x := 42
    log(x)   // x escapes: interface{} boxing allocates when the value isn't pointer-sized
}
```

Converting a non-pointer type to `interface{}` (aka `any`) may allocate to box the value. Small integers and pointers can sometimes be stored in the interface directly (compiler optimization), but larger types always allocate.

**4. Closure capturing a local variable**

```go
func example() func() int {
    x := 42
    return func() int { return x }  // x escapes: closure outlives example()
}
```

The closure captures `x` by reference. Since the closure is returned, `x` must be heap-allocated to remain valid.

**5. Sending to a channel**

```go
func example(ch chan<- *Foo) {
    f := Foo{X: 42}
    ch <- &f   // f escapes: crosses goroutine boundary
}
```

Channels transfer data between goroutines. The compiler cannot prove which goroutine will access the data or when, so the data must be heap-allocated.

**6. Slice or array too large for the stack**

The compiler has a heuristic size limit for stack-allocating arrays and slices. Arrays larger than approximately 10MB will be heap-allocated even if they don't logically escape. Smaller arrays (like `[16]int` -- 128 bytes) easily fit on the stack.

**7. Variable referenced by a heap-allocated value**

```go
func example() {
    x := 42
    p := &x         // p is on the stack
    globalSlice = append(globalSlice, p)  // x escapes because p is stored in a heap slice
}
```

If `p` gets stored somewhere that escapes, then `x` (what `p` points to) must also escape. Escape analysis tracks these transitive dependencies.

### The Escape Analysis Decision Tree (Simplified)

The compiler's reasoning roughly follows this logic:

```
For each local variable v:
  1. Is &v ever taken?
     No  -> v stays on stack (unless it's too large)
     Yes -> continue
  2. Does &v (or any copy of it) reach:
     a. A return statement?           -> v escapes
     b. A channel send?               -> v escapes
     c. An interface assignment?       -> v may escape (depends on usage)
     d. A heap-allocated struct field? -> v escapes
     e. A closure that escapes?        -> v escapes
     f. None of the above?             -> v stays on stack
```

The compiler is conservative: if it cannot prove non-escape, the variable goes to the heap. This means "escape to heap" is the safe default, and stack allocation is the optimization.

---

## The Stack Buffer Escape Trap and Fix

**File**: `internal/simd/index.go`

This is a specific, subtle escape analysis pitfall that was discovered and fixed in agrep. It demonstrates how a seemingly harmless coding pattern can cause a heap allocation on every call, even on the path that returns `nil`.

### The Trap

Consider this natural way to collect results into a stack-allocated buffer:

```go
func IndexAll(data, pattern []byte) []int {
    var stackBuf [16]int        // Looks like it should stay on stack
    offsets := stackBuf[:0]     // offsets is a slice referencing stackBuf

    for /* each match */ {
        offsets = append(offsets, position)
    }

    if len(offsets) == 0 {
        return nil              // We return nil, not offsets -- should be fine?
    }
    return offsets              // offsets references stackBuf
}
```

The programmer's intent: `stackBuf` is a small array on the stack, `offsets` is a slice view into it, and we only return `offsets` when there are matches. On the no-match path, we return `nil`, so `stackBuf` should never escape... right?

**Wrong.** The compiler performs escape analysis **statically**, not dynamically. It sees that:
1. `offsets` references `stackBuf` (via `stackBuf[:0]`).
2. `offsets` is returned on at least one code path (`return offsets`).
3. Therefore `stackBuf` **might** be referenced after the function returns.
4. Therefore `stackBuf` must be heap-allocated.

The compiler cannot know at compile time which branch will execute. It makes one allocation decision for `stackBuf` that applies to all paths. Since one path returns a reference to `stackBuf`, all paths pay the heap allocation.

**Result**: `[16]int` = 128 bytes allocated on the heap for **every call**, including the no-match path. In a grep tool, the no-match path is overwhelmingly the most common (99%+ of files don't contain the search pattern). This means 128 B/op on every file search, completely unnecessarily.

### The Fix

The key insight: **never let any return value reference the stack buffer**. Instead, use the stack buffer as scratch space and copy to a heap allocation only when needed:

```go
func IndexAll(data, pattern []byte) []int {
    // ...

    // Collect into a non-escaping stack buffer first, then copy to heap
    // only if we found matches. This avoids a 128-byte heap alloc on no-match.
    var stackBuf [16]int       // Stays on stack!
    n := 0                     // Counter, not a slice
    var overflow []int         // Heap fallback for >16 matches

    for {
        idx := bytes.Index(data[i:], pattern)
        if idx < 0 {
            break
        }
        if n < len(stackBuf) {
            stackBuf[n] = pos  // Write directly to stack array by index
        } else {
            if overflow == nil {
                overflow = make([]int, 0, 64)
                overflow = append(overflow, stackBuf[:]...) // Copy stack to heap
            }
            overflow = append(overflow, pos)
        }
        n++
    }

    if n == 0 {
        return nil             // Zero alloc path: stackBuf never referenced
    }
    if overflow != nil {
        return overflow        // >16 matches: return the heap slice
    }
    result := make([]int, n)   // 1-16 matches: copy from stack to new heap slice
    copy(result, stackBuf[:n])
    return result
}
```

What the compiler sees:
1. `stackBuf` is a local `[16]int` array.
2. `stackBuf` is only accessed by index (`stackBuf[n] = pos`).
3. `stackBuf[:]` appears in `append(overflow, stackBuf[:]...)`, but that is a copy **from** `stackBuf` into `overflow`. After the copy, `overflow` does not reference `stackBuf`'s memory.
4. The function returns `nil`, `overflow`, or `result` -- none of which reference `stackBuf`.
5. Therefore `stackBuf` does not escape. It stays on the stack.

**Result**: 0 B/op, 0 allocs/op on the no-match path. On the 1-16 match path, one allocation of `n * 8` bytes. On the 17+ match path, one allocation of the overflow slice.

### Three-Tier Cost Structure

| Path | Matches | Allocations | Heap Bytes |
|------|---------|------------|------------|
| No match (99%+ of files) | 0 | 0 | 0 |
| Few matches | 1-16 | 1 | `n * 8` |
| Many matches | 17+ | 1 (amortized) | `cap * 8` |

The common case is free. This is crucial for grep because most files in a search don't match.

### This Pattern Is Used in Three Functions

All three functions in `internal/simd/index.go` use this identical pattern:

1. **`IndexAll`** (lines 18-63): Multi-byte pattern search using `bytes.Index`.
2. **`indexAllByte`** (lines 66-119): Single-byte search using AVX2 SIMD via `archsimd`.
3. **`IndexAllCaseInsensitive`** (lines 185-270): Case-insensitive multi-byte search using AVX2 SIMD first+last byte prefilter.

Each function declares `var stackBuf [16]int`, uses a counter `n`, and has an `overflow []int` fallback. The structure is identical across all three.

### Why 16?

`[16]int` = 128 bytes. This fits comfortably on the stack (Go goroutine stacks start at 2-8KB and grow as needed). 16 entries covers the vast majority of match counts per file in typical grep usage. Files with more than 16 matches are uncommon enough that the heap allocation in the overflow path is acceptable.

Choosing a larger stack buffer (e.g., `[64]int` = 512 bytes) would save one allocation on files with 17-64 matches, but would consume more stack space on every call including the no-match path. 16 is a good balance.

---

## sync.Pool for Buffer Reuse

**File**: `internal/input/buffered.go`

`sync.Pool` is Go's mechanism for reusing temporary objects across goroutines without explicit lifecycle management. agrep uses it to recycle file read buffers.

### The Pool Declaration

```go
var bufPool = sync.Pool{
    New: func() any {
        b := make([]byte, 0, 64*1024) // 64KB initial capacity
        return &b                       // Pointer-to-slice!
    },
}
```

### Why `*[]byte` Instead of `[]byte`

This is a subtle but important design choice. A `[]byte` in Go is a 3-word value type: `(pointer, length, capacity)`. When you put a `[]byte` into a `sync.Pool`, the pool stores a copy of those three words. Here's the problem:

```go
// WRONG: Pool stores []byte by value
var pool = sync.Pool{New: func() any { return make([]byte, 0, 64*1024) }}

func read(fd int, size int64) {
    buf := pool.Get().([]byte)  // Get the 64KB buffer
    buf = buf[:size]
    // ... read into buf ...
    // Suppose size was 200KB. append() allocated a new 200KB backing array.
    // buf is now (newPtr, 200KB, 200KB).
    pool.Put(buf)  // Put stores the NEW slice header into the pool. OK so far.
}
```

This actually works for `[]byte` since we `Put` the updated header. But there is a more fundamental reason to use `*[]byte`: if the buffer is **grown** inside a function that received it by value, the caller's copy doesn't see the growth:

```go
bp := bufPool.Get().(*[]byte)
buf := *bp
if cap(buf) < int(size) {
    buf = make([]byte, size)  // buf now points to a larger allocation
}
// ... use buf ...
*bp = buf          // Update the pool entry with the (possibly grown) slice
bufPool.Put(bp)    // Return the pointer to the pool
```

By storing `*[]byte`, we have a stable pointer (`bp`) that we can update. `*bp = buf` writes the new (pointer, length, capacity) through the indirection, so the pool entry always reflects the latest buffer state.

### The Full Read Cycle

```go
func readBuffered(fd int, size int64) (ReadResult, error) {
    // 1. Get a pooled buffer
    bp := bufPool.Get().(*[]byte)
    buf := *bp

    // 2. Grow if needed (only happens once per size class)
    if cap(buf) < int(size) {
        buf = make([]byte, size)
    } else {
        buf = buf[:size]
    }

    // 3. Read the file
    var totalRead int
    for totalRead < int(size) {
        n, err := unix.Pread(fd, buf[totalRead:], int64(totalRead))
        if err != nil {
            unix.Close(fd)
            *bp = buf
            bufPool.Put(bp)   // Return even on error
            return ReadResult{}, err
        }
        if n == 0 { break }
        totalRead += n
    }
    unix.Close(fd)

    // 4. Return buffer with a Closer that returns it to the pool
    return ReadResult{
        Data: buf[:totalRead],
        Closer: func() error {
            *bp = buf          // Reflect any growth
            bufPool.Put(bp)    // Return to pool
            return nil
        },
    }, nil
}
```

### sync.Pool Internals

Understanding how `sync.Pool` works helps explain why it's effective for agrep:

1. **Per-P private slot**: Each P (logical processor in Go's scheduler) has a private slot in the pool. `Get()` first checks this slot -- if non-empty, it returns the item with zero contention.

2. **Per-P shared list**: If the private slot is empty, `Get()` checks a shared list for that P, then steals from other Ps' shared lists.

3. **GC clears the pool**: At the beginning of each GC cycle, the runtime moves all pooled items to a "victim" cache. At the next GC cycle, the victim cache is discarded. This means pooled items survive at most 2 GC cycles without being reused. This prevents the pool from growing unboundedly and leaking memory.

4. **No size limit**: The pool itself has no cap. If you `Put` 1000 buffers, they'll all be there until the next GC. In practice, agrep has `NumCPU * 2` workers, so at most that many buffers are checked out at once, and buffers are returned quickly after each file.

### Self-Sizing Behavior

Over time, the pooled buffers "right-size" themselves:
- Initially, each buffer is 64KB.
- If a worker reads a 200KB file, the buffer grows to 200KB.
- When the buffer is returned to the pool, it retains the 200KB capacity.
- The next file that worker reads (even if only 10KB) benefits from the pre-grown buffer -- no reallocation needed.
- After enough files, all pool entries have grown to accommodate the largest file in the search set.

This is why the pool stores buffers with `length=0, capacity=N` semantics: the capacity carries the "high water mark" across reuses.

---

## Append-Style Formatters (Zero-Alloc Output)

**File**: `internal/output/text.go`

All output formatting in agrep uses the "append into caller-provided buffer" pattern, achieving zero allocation per formatted result.

### The Pattern

```go
func (f *TextFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
    if f.filesOnly {
        if result.HasMatch() {
            buf = append(buf, result.FilePath...)
            buf = append(buf, '\n')
            return buf
        }
        return buf
    }

    if f.countOnly {
        if multiFile {
            buf = append(buf, result.FilePath...)
            buf = append(buf, ':')
        }
        buf = strconv.AppendInt(buf, int64(result.Count()), 10)
        buf = append(buf, '\n')
        return buf
    }

    ms := &result.MatchSet
    for i := range ms.Matches {
        buf = f.formatMatch(buf, result.FilePath, ms, i, multiFile)
    }
    return buf
}
```

The caller passes `buf[:0]` (reset length to zero but retain the backing array). The formatter appends everything into this buffer and returns it. The caller keeps the returned slice for the next call:

```go
// From OrderedWriter.writeResult in internal/output/writer.go
func (ow *OrderedWriter) writeResult(buf []byte, r Result) []byte {
    // ...
    buf = ow.formatter.Format(buf[:0], r, ow.multiFile)
    // ...
    ow.writer.Write(buf)
    return buf   // Caller keeps the (possibly grown) buf for next call
}
```

### Key Zero-Alloc Techniques

**`strconv.AppendInt` instead of `fmt.Sprintf`**

```go
// ZERO ALLOC: appends integer text directly into buf
buf = strconv.AppendInt(buf, int64(m.LineNum), 10)

// ALLOCATES: creates a temporary string, then converts to []byte
formatted := fmt.Sprintf("%d", m.LineNum)  // 1 string alloc
buf = append(buf, formatted...)             // string->[]byte copy
```

`strconv.AppendInt` writes digits directly into the destination buffer using a small stack-allocated scratch array. No intermediate string, no conversion, no allocation.

The `strconv` package provides `Append*` variants for all types:
- `strconv.AppendInt(buf, n, base)` -- integers
- `strconv.AppendFloat(buf, f, fmt, prec, bits)` -- floats
- `strconv.AppendBool(buf, b)` -- booleans
- `strconv.AppendQuote(buf, s)` -- quoted strings

**ANSI color codes as `[]byte` constants**

**File**: `internal/output/color.go`

```go
var (
    ansiReset   = []byte("\x1b[0m")
    ansiMagenta = []byte("\x1b[35m")   // filename
    ansiGreen   = []byte("\x1b[32m")   // line number
    ansiCyan    = []byte("\x1b[36m")   // separator
    ansiBoldRed = []byte("\x1b[1;31m") // match highlight
)
```

These are package-level `[]byte` variables, initialized once. Using them avoids:
- String-to-`[]byte` conversion on every use (which allocates if the string escapes).
- `fmt.Fprintf` overhead (reflection, interface boxing, buffer management).
- Lipgloss `.Render()` overhead (the comment in the source notes this explicitly).

**Append directly from `[]byte` slices**

```go
buf = append(buf, ansiMagenta...)     // []byte -> []byte: zero alloc
buf = append(buf, filePath...)         // string -> []byte: zero alloc (compiler optimization)
buf = append(buf, '\n')               // byte: zero alloc
```

Go's compiler recognizes `append(buf, someString...)` and generates code that copies the string's bytes directly into the buffer without allocating a `[]byte` from the string.

### The Buffer Lifecycle

1. `WriteOrdered` declares `var buf []byte` (nil, zero capacity).
2. First call to `writeResult` calls `Format(buf[:0], ...)`. Since buf is nil, `buf[:0]` is also nil. The first `append` allocates a small backing array.
3. As the first result is formatted, `append` may grow the buffer several times.
4. After the first large result, `buf` has a backing array large enough for typical output.
5. All subsequent results reuse this backing array: `Format(buf[:0], ...)` resets the length to zero but keeps the capacity.
6. The buffer only grows again if a result is larger than any previous one.

Over the course of searching 100K files, the buffer is allocated once and grown a handful of times. The amortized allocation per file approaches zero.

---

## unsafe.String for Zero-Copy String Construction

**File**: `internal/walker/walker.go`

The `joinPath` function constructs file paths (e.g., `"/home/user/project" + "/" + "main.go"`) without the double allocation that `string(buf)` would cause:

```go
func joinPath(dirPath, name string) string {
    needsSep := len(dirPath) == 0 || dirPath[len(dirPath)-1] != '/'
    n := len(dirPath) + len(name)
    if needsSep {
        n++
    }
    buf := make([]byte, n)
    copy(buf, dirPath)
    i := len(dirPath)
    if needsSep {
        buf[i] = '/'
        i++
    }
    copy(buf[i:], name)
    return unsafe.String(&buf[0], len(buf))
}
```

### What `unsafe.String` Does

`unsafe.String(&buf[0], len(buf))` creates a `string` header that points directly to `buf`'s backing array. A Go string is internally just `(pointer, length)` -- the same as a slice but without the capacity. `unsafe.String` constructs this header without copying the underlying bytes.

### What `string(buf)` Would Do Instead

The normal `string(buf)` conversion:
1. Allocates a new backing array of `len(buf)` bytes on the heap.
2. Copies all bytes from `buf`'s backing array to the new array.
3. Returns a string pointing to the new array.

This is necessary in general because strings are immutable in Go, but `buf` is a mutable `[]byte`. If `string(buf)` didn't copy, subsequent modifications to `buf` would change the string's content, violating the immutability guarantee.

### Why `unsafe.String` Is Safe Here

In `joinPath`, the safety conditions are satisfied:
1. `buf` was just allocated within this function -- no other code holds a reference to it.
2. `buf` is never modified after `unsafe.String` is called (it's the last statement).
3. The returned string is the only reference to `buf`'s backing array going forward.
4. Since Go strings are immutable and no mutable reference (`[]byte`) to the backing array survives the function, the data cannot be corrupted.

The `buf` local variable goes out of scope when `joinPath` returns. The backing array is kept alive by the returned string. There is exactly one reference to the memory, and it's immutable. This is equivalent to what `string(buf)` does, minus the copy.

### Performance Impact

`joinPath` is called once per file and once per directory during traversal. In a search of 100K files across 5K directories, that's 105K calls. Each call saves one allocation and one `memcpy` of the full path length (average ~40 bytes). Total savings: ~105K allocations and ~4MB of copies. Not huge per-call, but it adds up, and more importantly, it eliminates 105K heap objects the GC would need to track.

### Why Not Use `filepath.Join`?

The function comment explains: `filepath.Join` calls `filepath.Clean`, which normalizes paths (removes double slashes, resolves `.` and `..`, etc.). This is unnecessary overhead when we control the inputs: `dirPath` is always a valid directory path from a previous traversal step, and `name` is a plain filename from `getdents64`. We know there are no `.`, `..`, or double slashes to clean. A simple concatenation is both faster and sufficient.

### When NOT to Use `unsafe.String`

- **If the backing `[]byte` might be modified later**: e.g., if `buf` comes from a `sync.Pool` and will be reused for another file. The string would silently change content.
- **If the backing `[]byte` is stack-allocated and the string outlives the function**: the stack frame would be reclaimed, leaving the string pointing at garbage. (In practice, Go's escape analysis would catch this and heap-allocate the array, but relying on this is fragile.)
- **In general**: only when you can guarantee the backing array's lifetime exceeds the string's, and no mutable reference survives.

---

## Per-Worker Buffer Reuse

Several components allocate buffers once per goroutine and reuse them across all iterations of their work loop. This eliminates per-iteration allocations.

### Walker Workers

**File**: `internal/walker/walker.go` (lines 173-184)

```go
func (pw *parallelWalker) worker() {
    buf := make([]byte, 32*1024) // per-worker getdents buffer — allocated once
    var dirents []Dirent          // per-worker reusable dirent slice — allocated once
    for {
        item, ok := pw.dequeue()
        if !ok {
            return
        }
        dirents = pw.processDir(item, buf, dirents)
        pw.finish()
    }
}
```

Two allocations per worker, reused across all directories that worker processes:

1. **`buf`** (32KB): The raw buffer passed to `unix.Getdents(fd, buf)`. The kernel writes `linux_dirent64` structs directly into this buffer. 32KB is chosen to match the typical kernel readahead for directory entries -- one `Getdents` syscall returns all entries for most directories.

2. **`dirents`** (`[]Dirent`): A reusable slice that `ParseDirents` fills with parsed directory entries. `processDir` returns the (possibly grown) slice back to the caller, so the next iteration reuses the same backing array:

```go
dirents = pw.processDir(item, buf, dirents)  // dirents is passed in and returned
```

This is the "pass-and-return" reuse pattern: the function receives the slice, may append to it (growing the backing array if needed), and returns the (possibly updated) slice header. The caller keeps the returned header for the next call.

### OrderedWriter Buffer

**File**: `internal/output/writer.go` (lines 56-85)

```go
func (ow *OrderedWriter) WriteOrdered(results <-chan Result, onMatch func()) {
    nextSeq := 1
    pending := make(map[int]Result)
    var buf []byte // reused across ALL writeResult calls

    for r := range results {
        // ...
        if r.SeqNum == nextSeq {
            buf = ow.writeResult(buf, r)  // Format into buf, write to stdout, return buf
            nextSeq++
            for {
                if p, ok := pending[nextSeq]; ok {
                    buf = ow.writeResult(buf, p)
                    delete(pending, nextSeq)
                    nextSeq++
                } else {
                    break
                }
            }
        } else {
            pending[r.SeqNum] = r
        }
    }
}
```

A single `[]byte` buffer is used across all result formatting. `writeResult` calls `formatter.Format(buf[:0], ...)` which resets the length to zero (reusing the backing array) and formats the new result into it.

### runFiles Buffer

**File**: `internal/cli/run.go` (lines 143-168)

```go
func runFiles(paths []string, m matcher.Matcher, reader input.Reader,
    formatter output.Formatter, w *output.Writer, mode searchMode) int {
    multiFile := len(paths) > 1
    hasMatch := false
    var buf []byte  // single buffer, reused for all files

    for _, path := range paths {
        result := searchReader(reader, path, m, mode)
        // ...
        buf = formatter.Format(buf[:0], result, multiFile)
        // ...
        w.Write(buf)
    }
    // ...
}
```

Same pattern: declare once, pass with `[:0]` to reset, keep the returned (grown) buffer.

---

## Deferred Buffer Release

**Files**: `internal/cli/run.go` (lines 268-315), `internal/scheduler/scheduler.go` (lines 66-110)

Non-matching files release their read buffer **immediately** after the match check. Only matching files keep the buffer alive until formatting is complete.

### The Pattern in `searchReader`

```go
func searchReader(r input.Reader, path string, m matcher.Matcher, mode searchMode) output.Result {
    result := output.Result{FilePath: path}

    readResult, err := r.Read(path)
    if err != nil {
        result.Err = err
        return result
    }

    closeReader := func() {
        if readResult.Closer != nil {
            readResult.Closer()
        }
    }

    if readResult.Data == nil {
        closeReader()  // Empty file: release immediately
        return result
    }

    if walker.IsBinary(readResult.Data) {
        closeReader()  // Binary file: release immediately
        return result
    }

    switch mode {
    case searchFilesOnly:
        if m.MatchExists(readResult.Data) {
            result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
        }
        closeReader()  // -l mode: buffer not needed for output
    case searchCountOnly:
        count := m.CountAll(readResult.Data)
        result.MatchCount = count
        closeReader()  // -c mode: buffer not needed for output
    default:
        result.MatchSet = m.FindAll(readResult.Data)
        if result.MatchSet.HasMatch() {
            result.Closer = closeReader   // KEEP buffer alive for formatting
        } else {
            closeReader()                 // No match: release NOW
        }
    }
    return result
}
```

### Why This Matters

In a typical search:
- 99%+ of files don't match the pattern.
- For non-matching files, the buffer is returned to `sync.Pool` immediately after `FindAll` returns.
- The buffer is available for reuse by the same worker's next file within microseconds.

Without deferred release (i.e., if all buffers were kept alive until the Result was consumed by the writer), memory usage would be proportional to the **channel buffer size** times the **average file size**. With `NumCPU * 2` workers and a result channel buffer of `workers * 2`, that could be `32 * average_file_size` bytes of live buffers.

With deferred release, memory usage is proportional to the **number of in-flight matching results** times their average size. Since matching results are typically rare and consumed quickly by the `OrderedWriter`, the live buffer count stays low -- usually under 10.

### The Same Pattern in the Scheduler

`internal/scheduler/scheduler.go` uses identical logic in `processFile`:

```go
func (s *Scheduler) processFile(entry walker.FileEntry) output.Result {
    // ...
    if s.filesOnly {
        if s.matcher.MatchExists(readResult.Data) {
            result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
        }
        closeReader()   // Always release for -l mode
    } else if s.countOnly {
        count := s.matcher.CountAll(readResult.Data)
        result.MatchCount = count
        closeReader()   // Always release for -c mode
    } else {
        result.MatchSet = s.matcher.FindAll(readResult.Data)
        if result.MatchSet.HasMatch() {
            result.Closer = closeReader   // Keep for formatting
        } else {
            closeReader()                 // Release immediately
        }
    }
    return result
}
```

### Buffer Lifetime Diagram

```
Time -->

Worker reads file A (no match):
  [read A] [match: no] [release A] [read B] ...
                        ^^^^^^^^^
                        Buffer returned to pool immediately

Worker reads file C (match found):
  [read C] [match: yes] -------- [buffer held] -------> [formatted] [release C]
                                                                     ^^^^^^^^^^
                                                                     Released after OrderedWriter
                                                                     formats and writes the result
```

---

## The noopCloser Pattern

**File**: `internal/input/reader.go`

```go
// noopCloser is a package-level no-op closer to avoid allocating a func literal per file.
func noopCloser() error { return nil }
```

Used in `BufferedReader.Read` and `adaptiveReader.Read` for empty files (size == 0):

```go
if stat.Size == 0 {
    unix.Close(fd)
    return ReadResult{Data: nil, Closer: noopCloser}, nil
}
```

### Why Not an Inline Closure?

An inline closure:

```go
return ReadResult{Data: nil, Closer: func() error { return nil }}, nil
```

This creates a new function value on each call. In Go, a function literal that captures no variables is compiled as a package-level function with a wrapper, but the function **value** (a pointer to the closure struct) still needs to be allocated per call in some compiler versions. Even when the compiler optimizes it, using a package-level function is unambiguously zero-allocation and makes the intent clear.

`noopCloser` is a named, package-level function. Using it as a value (`Closer: noopCloser`) takes the address of a fixed, known function. No allocation, no ambiguity.

### Broader Principle

This is a micro-optimization, but it illustrates a general principle: **avoid allocations in common cases, even small ones**. In a search of 100K files, many will be empty (e.g., `__init__.py` files, empty `.gitkeep` files). If each empty file allocated a closure, that's thousands of tiny allocations the GC must track. The `noopCloser` pattern eliminates them entirely.

The same principle appears throughout agrep:
- `separatorLine` is a package-level `[]byte("--")` in `internal/output/text.go` rather than allocated per separator.
- ANSI color codes are package-level `[]byte` variables in `internal/output/color.go` rather than per-format-call allocations.
- `Match` stores integer offsets rather than `[]byte` slices to avoid per-match pointer fields.

Every eliminated allocation compounds: fewer heap objects means fewer GC roots, fewer pointer scans, less mark work, lower GC frequency, and ultimately lower latency and higher throughput.

---

## Measuring and Verifying

### Benchmarks with `-benchmem`

The `-benchmem` flag reports allocations per operation:

```bash
GOEXPERIMENT=simd go test -bench=. -benchmem ./internal/simd/
```

Output:

```
BenchmarkIndexAll/NoMatch-16     200000000    5.2 ns/op    0 B/op    0 allocs/op
BenchmarkIndexAll/FewMatches-16  10000000   142 ns/op     64 B/op    1 allocs/op
```

`0 B/op, 0 allocs/op` confirms the stack buffer pattern is working. If you see `128 B/op, 1 allocs/op` on the no-match path, the stack buffer is escaping.

### Escape Analysis Output

```bash
GOEXPERIMENT=simd go build -gcflags='-m' ./internal/simd/ 2>&1
```

Look for lines mentioning your variables:

```
./index.go:31:6: stackBuf does not escape       # Good!
./index.go:45:20: make([]int, 0, 64) escapes to heap  # Expected: overflow is heap-allocated
./index.go:60:16: make([]int, n) escapes to heap       # Expected: result is heap-allocated
```

Use `-gcflags='-m -m'` for verbose reasoning:

```bash
GOEXPERIMENT=simd go build -gcflags='-m -m' ./internal/simd/ 2>&1
```

This shows the full escape analysis chain, e.g.:

```
./index.go:31:6: stackBuf does not escape because it is never referred to by a heap-allocated value
```

### Memory Profiling with pprof

For runtime analysis of GC behavior:

```bash
GOEXPERIMENT=simd go test -bench=BenchmarkSearch -benchmem -memprofile=mem.prof ./internal/matcher/
go tool pprof -alloc_space mem.prof
```

In the pprof interactive shell:
- `top` -- show functions with most allocations
- `list FunctionName` -- show line-by-line allocations
- `web` -- open a graph in the browser

### GC Trace

For real-time GC behavior during a search:

```bash
GODEBUG=gctrace=1 ./bin/agrep -r "pattern" /some/directory 2>&1 | head -20
```

Output format:

```
gc 1 @0.012s 2%: 0.015+1.2+0.003 ms clock, 0.24+0.8/1.0/0+0.048 ms cpu, 4->4->2 MB, 5 MB goal, 16 P
```

Key fields:
- `2%`: Total CPU time spent in GC (want < 5%).
- `4->4->2 MB`: Heap size before mark, after mark, live after sweep.
- `5 MB goal`: When next GC triggers.
- `16 P`: Number of processors.

If GC CPU is consistently > 10%, you have a GC pressure problem -- look for pointer-heavy structs or excessive allocations in the hot path.

### Runtime Metrics

For programmatic monitoring:

```go
var stats runtime.MemStats
runtime.ReadMemStats(&stats)
fmt.Printf("Total allocs: %d\n", stats.TotalAlloc)
fmt.Printf("Heap objects: %d\n", stats.HeapObjects)
fmt.Printf("GC cycles: %d\n", stats.NumGC)
fmt.Printf("GC pause total: %v\n", time.Duration(stats.PauseTotalNs))
```

---

## Cross-References

- **`01-simd-and-avx2.md`**: Covers the SIMD functions (`IndexAll`, `indexAllByte`, `IndexAllCaseInsensitive`) that use the stack buffer escape-avoidance pattern. Details the AVX2 vectorized search algorithms.
- **`03-memory-management.md`**: Full buffer lifecycle from `sync.Pool` through read, match, format, and release. Covers mmap vs. buffered read strategies and the `adaptiveReader` that switches between them.
- **`04-string-search-algorithms.md`**: Covers Boyer-Moore, Aho-Corasick, and the pointer-free `Match`/`MatchSet` design in the context of the search-then-split architecture.
- **`07-benchmarking-and-profiling.md`**: Detailed guide to measuring allocations with `-benchmem`, escape analysis with `-gcflags='-m'`, and profiling with pprof.
