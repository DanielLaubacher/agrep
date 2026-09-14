# Memory Management: mmap, Adaptive Reading, and Buffer Lifecycle

This document covers how agrep manages memory across its entire file-processing pipeline. It explains memory-mapped I/O from first principles, the adaptive reader that selects between strategies, the pooled buffered reader that eliminates per-file allocations, and the buffer lifecycle that threads ownership from reader to formatter across goroutine boundaries. It also covers the two-layer binary file defense that avoids wasting I/O on non-text files.

---

## Table of Contents

1. [Memory-Mapped I/O (mmap)](#1-memory-mapped-io-mmap)
   - [What mmap Is](#11-what-mmap-is)
   - [Virtual Memory Fundamentals](#12-virtual-memory-fundamentals)
   - [The mmap Lifecycle in agrep](#13-the-mmap-lifecycle-in-agrep)
   - [mmap vs read(): When Each Wins](#14-mmap-vs-read-when-each-wins)
   - [Why Not MAP_POPULATE](#15-why-not-map_populate)
2. [Adaptive Reader --- Smart Strategy Selection](#2-adaptive-reader--smart-strategy-selection)
   - [The Decision Logic](#21-the-decision-logic)
   - [Why a Single open + fstat](#22-why-a-single-open--fstat)
   - [The 8MB Threshold](#23-the-8mb-threshold)
   - [O_NOATIME and the Atomic Fallback](#24-o_noatime-and-the-atomic-fallback)
3. [Pooled Buffered Reader (sync.Pool)](#3-pooled-buffered-reader-syncpool)
   - [The Pool Design](#31-the-pool-design)
   - [Why Pointer-to-Slice](#32-why-pointer-to-slice)
   - [The pread Loop](#33-the-pread-loop)
   - [Growth and Steady-State Behavior](#34-growth-and-steady-state-behavior)
4. [Buffer Lifecycle Through the Pipeline](#4-buffer-lifecycle-through-the-pipeline)
   - [The Zero-Copy Chain](#41-the-zero-copy-chain)
   - [Deferred Release Optimization](#42-deferred-release-optimization)
   - [Lifecycle in the Scheduler (Recursive Mode)](#43-lifecycle-in-the-scheduler-recursive-mode)
   - [Lifecycle in the OrderedWriter](#44-lifecycle-in-the-orderedwriter)
   - [Lifecycle in Non-Recursive Mode](#45-lifecycle-in-non-recursive-mode)
   - [What Happens If You Get It Wrong](#46-what-happens-if-you-get-it-wrong)
5. [Binary File Filtering --- Two-Layer Defense](#5-binary-file-filtering--two-layer-defense)
   - [Layer 1: Extension-Based Filtering](#51-layer-1-extension-based-filtering)
   - [Layer 2: Content-Based Detection](#52-layer-2-content-based-detection)
   - [Versioned Shared Library Edge Case](#53-versioned-shared-library-edge-case)
   - [Syscall Savings](#54-syscall-savings)
6. [The Reader Interface](#6-the-reader-interface)
   - [ReadResult and the Closer Contract](#61-readresult-and-the-closer-contract)
   - [The noopCloser Optimization](#62-the-noopcloser-optimization)
   - [StdinReader: The Special Case](#63-stdinreader-the-special-case)
7. [Memory Pressure and GC Interactions](#7-memory-pressure-and-gc-interactions)
   - [mmap and the Go Garbage Collector](#71-mmap-and-the-go-garbage-collector)
   - [Pointer-Free Match Structs](#72-pointer-free-match-structs)
   - [Pool Sizing Under Concurrency](#73-pool-sizing-under-concurrency)
8. [Cross-References](#8-cross-references)

---

## 1. Memory-Mapped I/O (mmap)

### 1.1 What mmap Is

The `mmap` system call maps a file's contents directly into the calling process's virtual address space. After an mmap call, the file appears as a contiguous byte array in memory --- you can index into it, slice it, and pass it to any function that operates on `[]byte`. No explicit `read()` call is needed.

Under a traditional `read()` system call, the kernel:
1. Reads data from disk into the kernel's page cache (if not already cached).
2. Copies data from the page cache into a user-space buffer you provided.

Under `mmap`, the kernel:
1. Sets up page table entries that point to the file's pages in the page cache.
2. Returns. No data is copied.
3. When your code accesses a byte, the CPU's MMU triggers a **page fault** if the page is not yet resident.
4. The page fault handler loads the page from disk into the page cache and updates the page table entry.
5. The CPU retries the memory access --- this time it succeeds.

The critical difference: `read()` always copies data from kernel space to user space. `mmap` eliminates that copy entirely --- your process reads directly from the page cache. For a grep tool scanning large files, this copy elimination is significant.

### 1.2 Virtual Memory Fundamentals

To understand mmap properly, you need to understand the virtual memory system it sits on top of.

**Page tables.** Every process has a page table that maps virtual addresses to physical addresses. When you access a virtual address, the CPU's Memory Management Unit (MMU) walks the page table to find the physical address. On x86-64, this is a 4-level radix tree (PGD -> P4D -> PUD -> PMD -> PTE), and each walk touches up to 5 cache lines. This is the "64-byte page table walk" cost of mmap.

**Page faults.** When a page table entry is not present (the page has not been loaded from disk), the CPU raises a page fault exception. The kernel's page fault handler runs, which:
1. Identifies which file and offset the faulting address corresponds to.
2. Allocates a physical page.
3. Reads the file data from disk (or page cache) into that physical page.
4. Updates the page table entry to point to the physical page.
5. Returns control to the faulting instruction, which retries and succeeds.

A **minor page fault** occurs when the data is already in the page cache (another process mapped it, or the kernel pre-read it). The handler just updates the page table --- no disk I/O. A **major page fault** requires actual disk I/O. The `fadvise` and `madvise` hints in agrep are specifically designed to convert major faults into minor faults by triggering readahead.

**Pages.** Memory is managed in pages (typically 4KB on x86-64). Even if you only read 1 byte from a file, the kernel loads an entire 4KB page. For small files, this means mmap loads proportionally more data than needed, which is one reason it loses to buffered reads for small files.

### 1.3 The mmap Lifecycle in agrep

The implementation lives in `internal/input/mmap.go`. Here is the complete `readMmap` function with every step explained.

**Step 1 --- Advise the kernel about access pattern BEFORE mapping:**

```go
unix.Fadvise(fd, 0, size, unix.FADV_SEQUENTIAL)
```

`fadvise(2)` tells the kernel how you plan to access a file. `FADV_SEQUENTIAL` doubles the kernel's default readahead window. The default readahead is typically 128KB (32 pages); `FADV_SEQUENTIAL` increases it to 256KB or more (the kernel's readahead algorithm adapts based on observed access patterns, but `FADV_SEQUENTIAL` biases it aggressively forward).

This call is made *before* the mmap because the kernel uses this hint both for explicit `read()` calls and for page fault-driven I/O on mmap'd regions. By setting it before mapping, the first page fault will already benefit from the enlarged readahead window.

Why this matters for grep: grep reads a file front-to-back, exactly once. The sequential hint tells the kernel to aggressively prefetch pages ahead of the current access point, converting what would be major page faults (disk I/O) into minor page faults (just page table updates). Without this hint, the kernel would use a more conservative readahead window optimized for random access patterns.

**Step 2 --- Create the mapping:**

```go
data, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE)
```

The arguments:
- `fd`: the file descriptor. The kernel needs this to know which file backs the mapping. The fd must remain open for the entire lifetime of the mapping because the kernel references it when servicing page faults.
- `0` (offset): start mapping from the beginning of the file.
- `int(size)`: map the entire file.
- `PROT_READ`: the mapping is read-only. Any write attempt will cause a segfault (SIGSEGV). This is correct for grep --- we never modify file content. Read-only protection also means the kernel never needs to set up copy-on-write page table entries (the "dirty" bit is never set).
- `MAP_PRIVATE`: create a private, copy-on-write mapping. Even though we never write (and PROT_READ enforces this), `MAP_PRIVATE` is more efficient than `MAP_SHARED` for read-only use. With `MAP_SHARED`, the kernel must maintain page coherency guarantees: if another process modifies the file, the change must be visible through the mapping. With `MAP_PRIVATE`, the kernel can serve pages directly from the page cache without coherency tracking overhead. The kernel can also share physical pages between processes that have both mapped the same file with `MAP_PRIVATE`, because the copy-on-write mechanism only kicks in on writes (which never happen).

If `Mmap` fails (e.g., the file was truncated between `fstat` and `mmap`, or the process hit its virtual memory limit), agrep falls back to the buffered reader:

```go
if err != nil {
    return readBuffered(fd, size)
}
```

This fallback is important for robustness. The `readBuffered` function takes ownership of the fd and will close it.

**Step 3 --- Additional sequential hint on the mapped region:**

```go
unix.Madvise(data, unix.MADV_SEQUENTIAL)
```

`madvise(2)` is the mmap-specific equivalent of `fadvise`. While `fadvise` operates on a file descriptor and affects both read and mmap I/O, `madvise` operates directly on a virtual address range. `MADV_SEQUENTIAL` tells the kernel's page fault handler:

1. **Prefetch aggressively forward**: when a page fault occurs at address `A`, also initiate I/O for pages at `A+4K`, `A+8K`, etc.
2. **Free pages behind the access point**: once we have moved past a page, it can be reclaimed immediately. The kernel does not keep it in the page cache for potential re-access.

The combination of `fadvise(FADV_SEQUENTIAL)` and `madvise(MADV_SEQUENTIAL)` is belt-and-suspenders. `fadvise` affects the readahead algorithm at the block device level. `madvise` affects the page fault handler at the virtual memory level. Together they ensure maximum readahead from both subsystems.

**Step 4 --- The search happens on the `data` byte slice.**

The returned `data` from `syscall.Mmap` is a `[]byte` whose backing array is the mapped region. The matcher operates directly on this slice. No data is copied anywhere. When the matcher accesses `data[offset]`, if that page is not yet resident, a page fault occurs transparently, the page is loaded, and the access completes. From the Go code's perspective, it is just reading a byte slice.

This is the core advantage of mmap for grep: the file data exists in exactly one place (the page cache), and the matcher reads it directly. There is no intermediate buffer.

**Step 5 --- Cleanup in the Closer function:**

```go
Closer: func() error {
    unix.Madvise(data, unix.MADV_DONTNEED)
    syscall.Munmap(data)
    unix.Close(fd)
    return nil
},
```

Three operations, and the order matters:

1. `MADV_DONTNEED`: tells the kernel that the physical pages backing this mapping can be freed immediately. Without this hint, the kernel might keep the pages in the page cache speculatively (in case another process accesses the same file). For a grep tool that scans thousands of files, we want those pages freed promptly to avoid polluting the page cache and evicting more useful data.

2. `Munmap`: removes the virtual memory mapping. After this call, the `data` slice is dangling --- any access to it will segfault. This is why the Closer must only be called after all formatting is complete.

3. `Close(fd)`: closes the file descriptor. This must happen *after* munmap, not before. While the Linux kernel documentation says that closing the fd does not invalidate an existing mapping (the kernel holds an internal reference to the file's inode), the fd must be kept open during the mapping's lifetime because page faults on the mapped region require the kernel to read from the file. In practice, closing the fd before munmap works on Linux because the kernel increments the inode's reference count during mmap, but agrep follows the conservative ordering: munmap first, close second.

### 1.4 mmap vs read(): When Each Wins

**mmap wins for large files because:**
- Zero copy: data is read once from disk into the page cache and accessed directly. `read()` copies it a second time from page cache to user buffer.
- OS-managed memory: the kernel can evict mmap'd pages under memory pressure and re-fault them later. With `read()`, the user buffer is pinned in Go's heap.
- Natural concurrency: multiple goroutines can read different parts of a mapping simultaneously. With `read()`, each goroutine needs its own buffer.

**read() wins for small files because:**
- No page fault overhead: `read()` is a single system call that copies N bytes. mmap requires a `mmap` syscall, then N/4096 page faults (each involving a page table walk and potential TLB miss), then a `munmap` syscall.
- Page table walk cost: on x86-64, each page fault involves walking a 4-level page table (PGD -> P4D -> PUD -> PMD -> PTE). Each level is a memory access that may miss the CPU cache. For a 4KB file, that is one page fault with a ~60ns page table walk plus TLB insert, versus a single ~200ns `read()` syscall that copies 4KB.
- TLB pollution: each mapped page consumes a TLB entry. Small files churn TLB entries rapidly, causing TLB misses that slow down the rest of the program.
- Kernel overhead: `mmap` and `munmap` are more expensive system calls than `read` because they modify the process's virtual memory layout, which requires updating the kernel's VMA (Virtual Memory Area) structures under locks.

**The crossover point** depends on hardware and kernel version, but is typically in the hundreds-of-KB to low-MB range. agrep defaults to 8MB, which was determined empirically.

### 1.5 Why Not MAP_POPULATE

The `MAP_POPULATE` flag tells the kernel to pre-fault all pages during the `mmap` call itself, rather than waiting for demand-paging. agrep intentionally omits this flag:

```go
// FADV_SEQUENTIAL + MADV_SEQUENTIAL handle readahead;
// we skip MAP_POPULATE so pages fault in on demand, enabling early exit
// for -l/MatchExists without reading the entire file.
```

Consider the `-l` (files-with-matches) mode. When a match is found in the first few kilobytes, `MatchExists` returns `true` immediately. With demand-paging (no `MAP_POPULATE`), only the pages that were actually accessed are loaded --- perhaps 1-2 pages out of thousands. With `MAP_POPULATE`, the entire file would be read from disk before the search even begins, wasting I/O proportional to the file's total size minus the position of the first match.

For a 100MB log file where the pattern appears on line 3, `MAP_POPULATE` would read all 25,600 pages (100MB); without it, demand-paging reads perhaps 2-3 pages (12KB). Combined with `MADV_SEQUENTIAL` readahead, the pages immediately ahead of the access point are still prefetched --- you get the readahead benefit without the wasted pre-fault of the entire file.

---

## 2. Adaptive Reader --- Smart Strategy Selection

### 2.1 The Decision Logic

The `AdaptiveReader` (`internal/input/mmap.go`, line 70-103) makes a single per-file decision: mmap or buffered read. The full implementation:

```go
func NewAdaptiveReader(mmapThreshold int64) Reader {
    return &adaptiveReader{
        threshold: mmapThreshold,
    }
}

type adaptiveReader struct {
    threshold int64
}

func (r *adaptiveReader) Read(path string) (ReadResult, error) {
    // Single open, single fstat --- no redundant Stat(path) allocation
    fd, err := openFile(path)
    if err != nil {
        return ReadResult{}, fmt.Errorf("open %s: %w", path, err)
    }

    var stat unix.Stat_t
    if err := unix.Fstat(fd, &stat); err != nil {
        unix.Close(fd)
        return ReadResult{}, fmt.Errorf("stat %s: %w", path, err)
    }

    size := stat.Size
    if size == 0 {
        unix.Close(fd)
        return ReadResult{Data: nil, Closer: noopCloser}, nil
    }

    if size >= r.threshold {
        return readMmap(fd, size, path)
    }
    return readBuffered(fd, size)
}
```

The flow is:
1. Open the file once (with `O_NOATIME`).
2. `fstat` the open fd to get the size.
3. If size is zero, close the fd and return an empty result.
4. If size >= threshold (default 8MB), use mmap.
5. Otherwise, use buffered pread.

Both `readMmap` and `readBuffered` take ownership of the fd. The adaptive reader never closes the fd itself after branching.

### 2.2 Why a Single open + fstat

A naive implementation might call `unix.Stat(path)` first to get the size, then `unix.Open(path)` to open the file. This has two problems:

**Problem 1: TOCTOU race.** The file might be deleted, truncated, or replaced between `Stat` and `Open`. By opening first and using `fstat` on the resulting fd, the size always corresponds to the exact file we have open.

**Problem 2: Allocation from path conversion.** `unix.Stat(path)` internally calls `ByteSliceFromString(path)` to convert the Go string to a NUL-terminated C string for the `stat(2)` syscall. This allocates a `[]byte` on the heap. By using `fstat(fd)` instead (which takes an integer fd, no string), this allocation is avoided. On a search of 37,000 files, that is 37,000 heap allocations eliminated.

`unix.Open(path)` also calls `ByteSliceFromString(path)`, but that allocation is unavoidable --- you need the path to open the file. The point is to avoid doing it *twice*.

### 2.3 The 8MB Threshold

The `MmapThreshold` field in `Config` (`internal/cli/config.go`) defaults to 8MB (8,388,608 bytes). This was determined empirically by benchmarking both strategies across a range of file sizes.

At 8MB, the mmap overhead (the mmap/munmap syscall pair, page table setup, and page faults) is amortized over enough data that the zero-copy advantage dominates. Below 8MB, the buffered reader's single `pread` (or small number of pread calls) plus a memory copy is cheaper than the page fault overhead.

The threshold is configurable so users can tune it for their workload. For example:
- On systems with very fast NVMe storage, the threshold might be lower (disk I/O completes so fast that page faults are cheap).
- On systems with slow storage, the threshold might be higher (each page fault is expensive, so you want to avoid them for more files).
- For `-l` mode where early exit is common, a lower threshold might hurt because mmap enables early exit without reading the whole file (only faulted pages are loaded).

### 2.4 O_NOATIME and the Atomic Fallback

Every file open in agrep uses `O_NOATIME` (`internal/input/mmap.go`, line 111-124):

```go
var noatimeWorks atomic.Int32

func init() { noatimeWorks.Store(1) }

func openFile(path string) (int, error) {
    if noatimeWorks.Load() != 0 {
        fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOATIME, 0)
        if err == nil {
            return fd, nil
        }
        if err == unix.EPERM {
            noatimeWorks.Store(0)
        }
    }
    return unix.Open(path, unix.O_RDONLY, 0)
}
```

`O_NOATIME` tells the kernel not to update the file's access time (atime) when reading it. Without this flag, every `open()` + `read()` writes a new atime to the file's inode on disk. For a grep tool scanning 37,000 files, that is 37,000 inode writes eliminated --- a significant I/O reduction, especially on rotational disks where seek time for the inode write would stall the pipeline.

`O_NOATIME` requires either:
- The process's effective UID matches the file's owner, or
- The process has `CAP_FOWNER` capability.

If neither condition holds, `open()` with `O_NOATIME` returns `EPERM`. agrep handles this with a global atomic flag:

1. First call: try with `O_NOATIME`. If `EPERM`, atomically set `noatimeWorks` to 0.
2. All subsequent calls: skip `O_NOATIME` entirely (one fewer syscall attempt).

The `atomic.Int32` ensures this is safe under concurrent access from multiple worker goroutines. The `Store(0)` on `EPERM` is idempotent --- multiple goroutines might race to store 0, but the result is the same.

Note: the walker (`internal/walker/walker.go`, line 14-33) has its own identical `noatimeWorks` atomic and `openDir` function for directory opens. These are separate atomics because directory opens and file opens may have different permission contexts (though in practice they usually match).

---

## 3. Pooled Buffered Reader (sync.Pool)

### 3.1 The Pool Design

For files below the mmap threshold, agrep uses pooled buffers to avoid per-file heap allocations (`internal/input/buffered.go`):

```go
var bufPool = sync.Pool{
    New: func() any {
        b := make([]byte, 0, 64*1024) // 64KB initial capacity
        return &b
    },
}
```

`sync.Pool` is Go's mechanism for reusing temporary objects across goroutines. The runtime automatically manages pool entries:
- `Get()` retrieves an entry (or calls `New` if the pool is empty).
- `Put()` returns an entry to the pool.
- The GC may clear pool entries between GC cycles (this is by design --- pools are for caching, not permanent storage).

The initial capacity of 64KB was chosen as a reasonable starting point for small files. Most source code files are under 64KB, so the initial buffer will serve them without reallocation. Files larger than 64KB but smaller than 8MB (the mmap threshold) will cause the buffer to grow, and that grown capacity is preserved in the pool for future use.

### 3.2 Why Pointer-to-Slice

The pool stores `*[]byte` (pointer-to-slice), not `[]byte` (raw slice). This is a subtle but critical design choice.

A Go slice is a 3-word struct: `{pointer, length, capacity}`. When you pass a slice to `append` or reallocate it, Go may allocate a new backing array and return a new slice value with a different pointer. The original slice variable is unchanged.

If the pool stored `[]byte` directly:

```go
// WRONG: hypothetical pool storing []byte directly
buf := bufPool.Get().([]byte)
if cap(buf) < int(size) {
    buf = make([]byte, size) // new backing array --- old one is still in the pool
}
// ... use buf ...
bufPool.Put(buf) // puts the NEW buf; old one was lost in the pool
```

Actually, the issue is more subtle. `sync.Pool.Get()` returns the value. If we stored the slice directly, `Get()` returns a copy of the slice header. When we grow the slice, we get a new header. When we `Put()` the new header, it goes into the pool. The old header was already removed by `Get()`. So this actually works... except for one case: if we `Get()` a 64KB buffer but need 1MB, we allocate a new 1MB buffer and the old 64KB buffer is garbage-collected. The pool never accumulates large buffers.

With `*[]byte`:

```go
bp := bufPool.Get().(*[]byte)  // bp is a pointer to a slice header
buf := *bp                      // dereference to get the actual slice
if cap(buf) < int(size) {
    buf = make([]byte, size)    // grow --- buf is now a new slice
}
// ... use buf ...
*bp = buf          // write the (possibly grown) slice BACK through the pointer
bufPool.Put(bp)    // return the same pointer; it now points to the grown slice
```

The pointer `bp` acts as a stable handle. The `Closer` function captures `bp`, and when called, writes the current `buf` (which may have a larger backing array) back through the pointer before returning it to the pool. This means:

1. The pool accumulates the *largest* buffer each worker has ever needed.
2. After a warm-up period, no more allocations happen for the buffered path.
3. Each goroutine's pool shard converges to hold a buffer sized for the largest file it has encountered.

### 3.3 The pread Loop

The buffered reader uses `pread(2)` instead of `read(2)`:

```go
var totalRead int
for totalRead < int(size) {
    n, err := unix.Pread(fd, buf[totalRead:], int64(totalRead))
    if err != nil {
        unix.Close(fd)
        *bp = buf
        bufPool.Put(bp)
        return ReadResult{}, err
    }
    if n == 0 {
        break // EOF
    }
    totalRead += n
}
```

**Why pread instead of read:**
- `pread(fd, buf, offset)` reads from a specific offset without modifying the file descriptor's seek position.
- `read(fd, buf)` reads from the current seek position and advances it.
- With `pread`, there is no shared seek state, so multiple goroutines can theoretically read from the same fd without interference (though agrep opens separate fds per file).
- More importantly, `pread` is a single syscall that combines seek + read atomically. Using `read` after `lseek` would be two syscalls.

**Why a loop:**
`pread(2)` is not guaranteed to read the full requested amount. Short reads can occur for various reasons:
- The file is on a network filesystem.
- A signal interrupted the syscall.
- The kernel decided to return a partial result.

The loop continues until either `totalRead` reaches the expected `size`, or `pread` returns 0 (EOF). The `n == 0` check handles the case where the file was truncated between the `fstat` (which determined `size`) and the `pread`.

**Error handling with pool return:**
On error, the buffer is returned to the pool before returning the error. This prevents buffer leaks. The `*bp = buf` assignment ensures the pool gets the current (possibly grown) buffer back even on the error path.

After reading completes, the fd is closed immediately:

```go
unix.Close(fd)
```

Unlike mmap, the buffered reader does not need the fd after reading. The data is fully copied into the buffer. This is another advantage of the buffered path for small files: the fd is held for a shorter duration, reducing the risk of hitting the process's file descriptor limit when scanning many files concurrently.

### 3.4 Growth and Steady-State Behavior

Consider a search across a codebase with these file sizes: most files are 2-20KB, a few are 100-500KB, and rare files are 1-7MB.

**First pass:**
1. Worker goroutine gets a 64KB buffer from the pool.
2. Reads a 5KB file. Buffer used at 5KB of 64KB capacity. Returned to pool.
3. Gets the same buffer. Reads a 300KB file. 300KB > 64KB, so `make([]byte, 300KB)` allocates a new buffer. Old 64KB buffer is abandoned (GC'd). Pool entry now points to 300KB buffer.
4. Reads a 10KB file. 10KB < 300KB, reuse without allocation.
5. Reads a 5MB file. 5MB > 300KB, allocate 5MB buffer. Pool entry now points to 5MB buffer.

**Second pass (same worker):**
1. Gets the 5MB buffer from pool.
2. Every file < 5MB reuses it with zero allocations.

**Steady state:** Each worker's pool shard holds a buffer sized for the largest file that worker has ever seen. Assuming files are distributed roughly evenly across workers, each worker ends up with a buffer around the size of the largest file in the codebase (up to the 8MB mmap threshold). Total memory overhead: `num_workers * max_small_file_size`. For 16 workers and a max small file of 7MB, that is ~112MB of pooled buffers --- acceptable for a command-line tool.

The GC can reclaim pool entries between cycles if memory pressure is high. This means under memory pressure, buffers may be freed and reallocated, trading CPU for memory. This is the intended behavior of `sync.Pool`.

---

## 4. Buffer Lifecycle Through the Pipeline

### 4.1 The Zero-Copy Chain

This is the most important section for understanding agrep's memory architecture. A file's data buffer passes through multiple stages and crosses goroutine boundaries without being copied:

```
Reader.Read(path)
  -> ReadResult{Data: []byte, Closer: func() error}
      |
      v
Matcher.FindAll(data)
  -> MatchSet{Data: data, Matches: []Match, Positions: [][2]int}
      |                     ^
      |                     | Match.LineStart is an OFFSET into Data, not a copy
      v
result.Closer = readResult.Closer  (threaded through output.Result)
      |
      v  (sent over channel to OrderedWriter in recursive mode)
Formatter.Format(buf, result, ...)
  -> reads ms.Data[m.LineStart : m.LineStart+m.LineLen]  (accesses the original buffer)
      |
      v
result.Closer()
  -> releases mmap (munmap + close fd) or returns pooled buffer
```

The critical invariant: **the buffer (`ReadResult.Data`) must remain valid until `Closer()` is called, and `Closer()` must not be called until all formatting is complete.**

Here is how this works concretely. The `Match` struct (`internal/matcher/match.go`) stores offsets, not copies:

```go
type Match struct {
    LineNum    int   // 1-based line number
    LineStart  int   // byte offset of line start in MatchSet.Data
    LineLen    int   // length of line in bytes
    ByteOffset int64 // byte offset within the original file
    PosIdx     int   // start index into MatchSet.Positions
    PosCount   int   // number of highlight positions
    IsContext  bool
}
```

`LineStart` and `LineLen` are indices into `MatchSet.Data`, which is the same `[]byte` returned by the reader. When the `TextFormatter` formats a match (`internal/output/text.go`, line 66-68):

```go
lineBytes = ms.Data[m.LineStart : m.LineStart+m.LineLen]
```

This creates a sub-slice of the original data --- no copy, just a new slice header pointing into the same backing array. If that backing array has been unmapped (mmap) or returned to the pool (buffered), this access is a use-after-free:
- For mmap: the virtual address range has been unmapped. Accessing it causes SIGSEGV.
- For pooled buffers: the buffer may have been reused by another goroutine for a different file. Accessing it reads garbage data (silent corruption).

### 4.2 Deferred Release Optimization

The `searchReader` function (`internal/cli/run.go`, line 268-315) implements the key optimization: release non-matching files' buffers immediately, but keep matching files' buffers alive:

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
        closeReader()
        return result
    }

    if walker.IsBinary(readResult.Data) {
        closeReader()
        return result
    }

    switch mode {
    case searchFilesOnly:
        if m.MatchExists(readResult.Data) {
            result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
        }
        closeReader() // Always release: -l mode doesn't need the buffer for formatting

    case searchCountOnly:
        count := m.CountAll(readResult.Data)
        result.MatchCount = count
        closeReader() // Always release: -c mode only needs the count

    default: // searchFull
        result.MatchSet = m.FindAll(readResult.Data)
        if result.MatchSet.HasMatch() {
            result.Closer = closeReader // Keep buffer alive for formatting
        } else {
            closeReader()              // No matches: release immediately
        }
    }
    return result
}
```

There are three early-release paths:
1. **Empty files** (`readResult.Data == nil`): nothing to search, release immediately.
2. **Binary files** (`IsBinary` detected NUL bytes): skip searching, release immediately.
3. **Non-matching files** (no matches found): the MatchSet is empty, so there is nothing to format. Release immediately.

And three special-mode paths:
4. **`-l` mode** (`searchFilesOnly`): only checks if a match exists, does not extract line content. The formatter only needs the file path, not the buffer. Release immediately.
5. **`-c` mode** (`searchCountOnly`): only counts matches, does not extract line content. The formatter only needs the count. Release immediately.
6. **Full mode with matches**: the buffer must survive until formatting. The Closer is attached to the `output.Result`.

In a typical grep search, most files do not match. If you are searching for a specific error message across 37,000 files, maybe 50 files match. This means 36,950 files release their buffers immediately (returning pooled buffers to the pool, or unmapping mmap'd regions). Only 50 files' buffers are held until the OrderedWriter formats them.

This keeps memory usage proportional to the number of matching files being actively formatted, not the total number of files being searched. Under the concurrent scheduler with 32 workers, at most 32 files are being read simultaneously, and of those, most will release immediately. The steady-state buffer memory is approximately: `num_workers * avg_file_size + matching_files_awaiting_formatting * avg_match_file_size`.

### 4.3 Lifecycle in the Scheduler (Recursive Mode)

The scheduler (`internal/scheduler/scheduler.go`) implements the same deferred-release pattern in its `processFile` method:

```go
func (s *Scheduler) processFile(entry walker.FileEntry) output.Result {
    result := output.Result{FilePath: entry.Path}

    readResult, err := s.reader.Read(entry.Path)
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
        closeReader()
        return result
    }

    if walker.IsBinary(readResult.Data) {
        closeReader()
        return result
    }

    if s.filesOnly {
        if s.matcher.MatchExists(readResult.Data) {
            result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
        }
        closeReader()
    } else if s.countOnly {
        count := s.matcher.CountAll(readResult.Data)
        result.MatchCount = count
        closeReader()
    } else {
        result.MatchSet = s.matcher.FindAll(readResult.Data)
        if result.MatchSet.HasMatch() {
            result.Closer = closeReader
        } else {
            closeReader()
        }
    }
    return result
}
```

The result (with its Closer) is sent over a channel to the OrderedWriter:

```go
resultCh <- result
```

This is the goroutine boundary crossing. The buffer was allocated/mapped by the reader in one goroutine (a scheduler worker), but it will be consumed and released in a different goroutine (the main goroutine running the OrderedWriter). The Closer function closure captures the `readResult` variable, which holds the `Data` slice and the original Closer from the reader. When the OrderedWriter calls `result.Closer()`, it releases the buffer regardless of which goroutine is executing.

### 4.4 Lifecycle in the OrderedWriter

The `OrderedWriter` (`internal/output/writer.go`, line 36-100) consumes results and writes them in sequence order:

```go
func (ow *OrderedWriter) writeResult(buf []byte, r Result) []byte {
    if r.Err != nil {
        if r.Closer != nil {
            r.Closer()
        }
        return buf
    }
    buf = ow.formatter.Format(buf[:0], r, ow.multiFile)
    if r.Closer != nil {
        r.Closer()
    }
    ow.writer.Write(buf)
    return buf
}
```

The sequence is critical:

1. **Format first**: `formatter.Format(buf[:0], r, ow.multiFile)` reads from `r.MatchSet.Data` via the `Match` offsets. At this point, the buffer must be valid.
2. **Close second**: `r.Closer()` releases the buffer (unmaps or returns to pool). After this call, `r.MatchSet.Data` is invalid.
3. **Write third**: `ow.writer.Write(buf)` writes the formatted output. This reads from `buf` (the formatter's own buffer), not from the file data. This is safe because the formatted output was already copied into `buf` during step 1.

Note that the formatter writes *into* `buf` by appending bytes (file paths, line numbers, ANSI color codes, and the line content copied from `MatchSet.Data`). After formatting, `buf` contains a self-contained copy of the output text. The original file data is no longer needed, so the Closer is called.

For error results, the Closer is called immediately without formatting (there is nothing to format).

**Out-of-order handling:** Results may arrive out of sequence number order because workers process files concurrently. The OrderedWriter buffers out-of-order results in a `pending` map:

```go
if r.SeqNum == nextSeq {
    buf = ow.writeResult(buf, r)
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
```

A result in the `pending` map keeps its buffer alive (via its Closer). This means out-of-order results hold their buffers until their turn comes. In the worst case, if result #1 is very slow and results #2 through #1000 arrive first, all 999 results hold their buffers simultaneously. In practice, the walker feeds files roughly in directory order, and the scheduler processes them roughly in order, so the pending map stays small.

### 4.5 Lifecycle in Non-Recursive Mode

In `runFiles` (`internal/cli/run.go`, line 143-168), files are processed sequentially in a single goroutine:

```go
for _, path := range paths {
    result := searchReader(reader, path, m, mode)
    if result.Err != nil {
        logWarn("%s: %v", path, result.Err)
        continue
    }
    if result.HasMatch() {
        hasMatch = true
    }
    buf = formatter.Format(buf[:0], result, multiFile)
    if result.Closer != nil {
        result.Closer()
    }
    w.Write(buf)
}
```

Same pattern: format first, then close. No goroutine boundary crossing, so the lifecycle is simpler --- everything happens in the same goroutine. The `buf` is reused across files (`buf[:0]` reuses the backing array).

### 4.6 What Happens If You Get It Wrong

Buffer lifecycle bugs are among the most insidious bugs in agrep because they manifest as:

**For mmap buffers:**
- **SIGSEGV** if the buffer is accessed after `Munmap`. The process crashes with no recovery. The segfault will point to a user-space address in the unmapped range, not to a kernel address, so it will be caught by the Go runtime's signal handler and reported as a panic.

**For pooled buffers:**
- **Silent data corruption** if the buffer is accessed after being returned to the pool. Another worker may have `Get()`'d the same buffer and filled it with a different file's content. The formatter would output the wrong file's data mixed into the output. This would be extremely difficult to debug because it would appear as intermittent garbled output that depends on timing.

**For either:**
- **Memory leak** if the Closer is never called. mmap'd regions accumulate, eventually hitting the process's `vm.max_map_count` limit (default 65,530 on Linux). Pooled buffers accumulate outside the pool, relying on GC to clean them up. Both waste memory.
- **File descriptor leak** if the mmap Closer is never called (it also closes the fd). Eventually the process hits its `RLIMIT_NOFILE` limit and cannot open any more files.

---

## 5. Binary File Filtering --- Two-Layer Defense

### 5.1 Layer 1: Extension-Based Filtering

Before a binary file is ever opened, the walker checks its extension (`internal/walker/filter.go`, line 22-44):

```go
func IsBinaryExtension(name string) bool {
    dot := strings.LastIndexByte(name, '.')
    if dot < 0 {
        return false
    }
    ext := name[dot:]

    // Fast path for single-char extensions
    if len(ext) == 2 {
        switch ext[1] {
        case 'a', 'o', 'z':
            return true
        }
    }

    _, ok := binaryExts[ext]
    if ok {
        return true
    }

    // Handle versioned shared libraries: libfoo.so.1.2.3
    if strings.Contains(name, ".so.") {
        return true
    }
    return false
}
```

The function uses `strings.LastIndexByte` to find the last dot, extracting the final extension. The two-stage check optimizes for common cases:

1. **Single-character fast path**: `.a` (static libraries), `.o` (object files), `.z` (compressed files) are checked with a switch statement --- no map lookup, no hashing. These three extensions cover a huge number of binary files in typical system directories.

2. **Map lookup**: the `binaryExts` map covers approximately 60 formats across categories:
   - Compiled/linked: `.so`, `.dll`, `.exe`, `.bin`, `.elf`, `.class`, `.pyc`, `.wasm`
   - Archives/compressed: `.gz`, `.bz2`, `.xz`, `.zst`, `.zip`, `.tar`, `.rar`, `.7z`, `.deb`, `.rpm`, `.jar`
   - Images: `.png`, `.jpg`, `.jpeg`, `.gif`, `.bmp`, `.ico`, `.webp`, `.psd`, `.svg`
   - Audio/video: `.mp3`, `.mp4`, `.ogg`, `.flac`, `.wav`, `.avi`, `.mkv`, `.webm`
   - Fonts: `.ttf`, `.otf`, `.woff`, `.woff2`
   - Documents: `.pdf`, `.doc`, `.docx`, `.xls`, `.xlsx`, `.ppt`, `.pptx`
   - Databases: `.db`, `.sqlite`, `.mdb`
   - Misc: `.swp`, `.swo` (vim swap files), `.DS_Store`

This check happens during directory traversal in `processDir` (`internal/walker/walker.go`, line 237):

```go
case DT_REG:
    if !pw.hidden && len(entry.Name) > 0 && entry.Name[0] == '.' {
        continue
    }
    if !pw.includeBinary && IsBinaryExtension(entry.Name) {
        continue
    }
```

When a file is skipped by extension, no `open`, `fstat`, `pread`, or `close` syscalls are made for that file. The only cost is the string operations in `IsBinaryExtension`, which operate on the filename already available from the `getdents64` buffer.

### 5.2 Layer 2: Content-Based Detection

Extension filtering catches known binary formats, but some files have misleading extensions (a `.txt` file that is actually binary, or a file with no extension that is binary). After reading the file's contents, the scheduler checks for NUL bytes (`internal/walker/filter.go`, line 10-16):

```go
func IsBinary(data []byte) bool {
    limit := 8192
    if len(data) < limit {
        limit = len(data)
    }
    return bytes.IndexByte(data[:limit], 0) >= 0
}
```

This scans the first 8KB (8192 bytes) for NUL bytes (`\x00`). This matches GNU grep's behavior. The rationale:

- Text files virtually never contain NUL bytes (NUL is not a valid character in any common text encoding: ASCII, UTF-8, Latin-1, etc.).
- Binary files almost always contain NUL bytes within the first 8KB (ELF headers, image format headers, compressed data, etc.).
- 8KB is enough to catch binary files with high confidence while keeping the scan cost minimal.
- `bytes.IndexByte` on Go's standard library uses SIMD-accelerated assembly (SSE2/AVX2 on amd64), so scanning 8KB is extremely fast --- typically under 100ns.

This check happens in `searchReader` / `processFile` after the data is read but before matching:

```go
if walker.IsBinary(readResult.Data) {
    closeReader()
    return result
}
```

The buffer is released immediately. No match is attempted on binary data.

### 5.3 Versioned Shared Library Edge Case

Linux shared libraries are often versioned: `libfoo.so.1`, `libfoo.so.1.2.3`, `libcurl.so.4.8.0`. The function `IsBinaryExtension` extracts the extension using `strings.LastIndexByte(name, '.')`:

- `libfoo.so` -> last dot at position 6 -> extension is `.so` -> found in `binaryExts` map -> detected as binary.
- `libfoo.so.1` -> last dot at position 9 -> extension is `.1` -> NOT in `binaryExts` map -> would be missed.
- `libfoo.so.1.2.3` -> last dot at position 13 -> extension is `.3` -> NOT in `binaryExts` map -> would be missed.

The special check handles this:

```go
if strings.Contains(name, ".so.") {
    return true
}
```

Any filename containing the substring `.so.` is treated as binary. This catches all versioned shared library patterns. The `strings.Contains` check is only reached if the extension-based checks did not match, so it does not add overhead for non-library files.

### 5.4 Syscall Savings

Each skipped binary file saves 4 syscalls in the search pipeline:
1. `open` (via `openFile`)
2. `fstat` (to get file size)
3. `pread` or page faults (to read data)
4. `close`

On a directory like `/usr/lib` with approximately 80,000 binary files (shared libraries, object files, archives, etc.), extension-based filtering saves approximately 320,000 syscalls. At a conservative estimate of 500ns per syscall, that is 160ms saved --- meaningful for a tool targeting sub-300ms total runtime.

The two-layer approach is ordered by cost:
- Layer 1 (extension check): ~10ns per file (string operations on already-available filename).
- Layer 2 (NUL byte scan): ~500ns per file (requires opening and reading the file first, but the scan itself is ~100ns).

By putting the cheap check first, agrep avoids the expensive check for the vast majority of binary files.

---

## 6. The Reader Interface

### 6.1 ReadResult and the Closer Contract

The reader interface and its return type (`internal/input/reader.go`):

```go
type ReadResult struct {
    Data   []byte
    Closer func() error
}

type Reader interface {
    Read(path string) (ReadResult, error)
}
```

The `ReadResult` is the core abstraction that enables the strategy pattern (mmap vs buffered vs stdin). Every reader returns the same type, and the rest of the pipeline treats it uniformly.

The `Closer` contract:
1. **Must be called exactly once** after the `Data` is no longer needed.
2. **Must not be called while `Data` is still being accessed** (by the formatter or any other consumer).
3. **May be nil** for empty files (where `noopCloser` is used instead --- see below).
4. **Is idempotent in practice** (calling munmap or pool.Put twice is safe, though the code ensures single calls).
5. **Returns an error** for interface consistency, though the current implementations always return nil.

### 6.2 The noopCloser Optimization

For empty files, agrep uses a package-level function instead of an anonymous closure:

```go
func noopCloser() error { return nil }
```

Used as:

```go
if size == 0 {
    unix.Close(fd)
    return ReadResult{Data: nil, Closer: noopCloser}, nil
}
```

Why not `func() error { return nil }` inline? Each inline anonymous function literal allocates a closure on the heap (even if it captures no variables, the Go compiler may allocate a function value). By using a package-level function, the function value is a compile-time constant with zero allocation.

For a search across 37,000 files where perhaps 100 are empty, this saves 100 heap allocations. Individually trivial, but it exemplifies the "zero allocations on hot path" design principle.

### 6.3 StdinReader: The Special Case

The stdin reader (`internal/input/stdin.go`) uses a completely different strategy:

```go
func (r *StdinReader) Read(_ string) (ReadResult, error) {
    data, err := io.ReadAll(os.Stdin)
    if err != nil {
        return ReadResult{}, err
    }
    return ReadResult{
        Data:   data,
        Closer: func() error { return nil },
    }, nil
}
```

Stdin cannot be mmap'd (it is a pipe, not a regular file) and its size is unknown in advance (no `fstat`). `io.ReadAll` reads until EOF into a dynamically-growing buffer. This allocates, but stdin is used at most once per invocation, so it is not on a hot path.

The Closer is a no-op because the buffer is owned by the GC. There is no mmap to unmap and no pool to return to. The Go garbage collector will collect the buffer when the `ReadResult` goes out of scope.

---

## 7. Memory Pressure and GC Interactions

### 7.1 mmap and the Go Garbage Collector

mmap'd regions are *outside* the Go heap. The `syscall.Mmap` call returns a `[]byte` whose backing array is a region of virtual memory managed by the kernel, not by Go's allocator. This has several implications:

**The GC does not track mmap'd memory.** Go's runtime tracks heap memory and triggers GC when `GOGC`% of the previous live heap is allocated. mmap'd regions do not count toward this. A program with 10GB of mmap'd files and 1MB of heap will not trigger GC from the mmap alone. This is usually fine because mmap'd memory is managed by the kernel (which evicts pages under physical memory pressure), but it means the Go runtime's memory statistics (`runtime.MemStats`) underreport true memory usage.

**The slice header IS on the Go heap.** While the backing array is in kernel-managed virtual memory, the `[]byte` header (pointer, length, capacity) is a stack or heap value. The header is tiny (24 bytes) and is tracked by the GC.

**Munmap invalidates the backing array.** After `Munmap`, the `[]byte` header still exists but points to unmapped memory. If the GC scans this header (because it is still reachable), it will not crash --- the GC does not dereference non-heap pointers. But user code that dereferences it will SIGSEGV.

### 7.2 Pointer-Free Match Structs

The `Match` struct (`internal/matcher/match.go`) is deliberately designed to be pointer-free:

```go
type Match struct {
    LineNum    int
    LineStart  int
    LineLen    int
    ByteOffset int64
    PosIdx     int
    PosCount   int
    IsContext  bool
}
```

All fields are value types (int, int64, bool). No pointers, no slices, no strings, no interfaces. This means:

1. **A `[]Match` does not cause GC scanning.** The GC only needs to scan objects that contain pointers. A slice of pointer-free structs is treated as an opaque blob by the GC. For a file with 10,000 matches, the GC scans the single slice header (24 bytes) but not the 10,000 Match structs.

2. **The `MatchSet` contains exactly 3 pointer-bearing fields**: `Data []byte`, `Matches []Match`, and `Positions [][2]int`. The GC scans these 3 slice headers (72 bytes total) regardless of how many matches exist. This is O(1) GC work per file.

This design is a direct application of the "pointer-free hot data" pattern. If `Match` contained a `string` or `[]byte` field (e.g., storing the actual line text), the GC would need to scan every Match in the slice, making GC work O(N) in the number of matches. By storing offsets into a shared `Data` buffer, the data is resolved on demand (during formatting) without creating GC-visible pointers.

### 7.3 Pool Sizing Under Concurrency

`sync.Pool` maintains per-P (per-processor) private caches plus a shared victim cache. Under the concurrent scheduler with `NumCPU * 2` workers:

- Each P (goroutine scheduler) has a private pool shard.
- `Get()` first checks the local shard (lock-free), then steals from other shards (with locking).
- `Put()` always goes to the local shard.

Because agrep runs `NumCPU * 2` worker goroutines, and Go typically has `NumCPU` P's, there is 2:1 contention on pool shards. In practice, this works well because:
- Workers alternate between reading (pool Get/Put) and matching (CPU-bound). They are rarely all doing pool operations simultaneously.
- Pool contention shows up as slightly higher allocation rates (more `New()` calls), not as deadlocks or crashes.
- The pool's victim cache (introduced in Go 1.13) smooths over GC-triggered pool clearing.

The steady-state pool population is approximately `NumCPU` buffers (one per P shard), each sized for the largest small file encountered by that P's workers. Additional buffers may exist temporarily in the victim cache or in flight between Get and Put.

---

## 8. Cross-References

- **Linux syscalls (open, fstat, pread, fadvise, madvise, mmap, munmap, writev, O_NOATIME)**: See `02-linux-syscalls.md` for detailed explanations of each syscall and why agrep uses them instead of Go's `os` package.
- **GC optimization, sync.Pool internals, pointer-free struct design**: See `06-gc-and-allocation-optimization.md` for comprehensive coverage of how agrep minimizes GC overhead.
- **Concurrency patterns (OrderedWriter, scheduler worker pool, channel-based pipeline)**: See `05-concurrency-patterns.md` for how the buffer lifecycle interacts with goroutine scheduling and ordered output.
- **Matcher architecture (search-then-split, SIMD acceleration)**: See `04-pattern-matching.md` for how matchers consume the buffer and produce MatchSets.
- **Walker (getdents64, dirent parsing, parallel BFS)**: See `02-linux-syscalls.md` for the raw directory traversal that feeds files to the scheduler.

---

## Source Files Referenced

| File | Purpose |
|------|---------|
| `internal/input/reader.go` | `Reader` interface, `ReadResult` struct, `noopCloser` |
| `internal/input/mmap.go` | `MmapReader`, `adaptiveReader`, `readMmap`, `openFile` |
| `internal/input/buffered.go` | `BufferedReader`, `readBuffered`, `bufPool` |
| `internal/input/stdin.go` | `StdinReader` |
| `internal/matcher/match.go` | `Match`, `MatchSet`, `Matcher` interface |
| `internal/output/result.go` | `output.Result` with `Closer` field |
| `internal/output/formatter.go` | `Formatter` interface |
| `internal/output/writer.go` | `Writer`, `OrderedWriter`, `writeResult` |
| `internal/output/text.go` | `TextFormatter.formatMatch` (reads from MatchSet.Data) |
| `internal/scheduler/scheduler.go` | `Scheduler.processFile` (buffer lifecycle in workers) |
| `internal/cli/run.go` | `searchReader`, `runRecursive`, `runFiles` |
| `internal/cli/config.go` | `Config.MmapThreshold` |
| `internal/walker/walker.go` | `processDir` (extension-based binary filtering) |
| `internal/walker/filter.go` | `IsBinary`, `IsBinaryExtension`, `binaryExts` |
