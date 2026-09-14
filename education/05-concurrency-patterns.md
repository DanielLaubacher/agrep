# Concurrency Patterns and Parallel Architecture in agrep

This document provides an exhaustive analysis of the concurrency patterns used in agrep, a high-performance Linux-only grep tool written in Go. Every concurrent structure, synchronization primitive, and design decision is examined from first principles. All code references point to the actual implementation files.

---

## Table of Contents

1. [The Big Picture: Pipeline Architecture](#1-the-big-picture-pipeline-architecture)
2. [Stage 1: Parallel BFS Directory Walker](#2-stage-1-parallel-bfs-directory-walker)
3. [Stage 2: Worker Pool Scheduler](#3-stage-2-worker-pool-scheduler)
4. [Stage 3: Ordered Output Resequencing](#4-stage-3-ordered-output-resequencing)
5. [Channel Buffer Sizing and Backpressure](#5-channel-buffer-sizing-and-backpressure)
6. [The Atomic O_NOATIME Pattern](#6-the-atomic-onoatime-pattern)
7. [Buffer Lifecycle and Ownership Across Goroutines](#7-buffer-lifecycle-and-ownership-across-goroutines)
8. [sync.Pool for Read Buffers](#8-syncpool-for-read-buffers)
9. [Config as Value: Eliminating Shared Mutable State](#9-config-as-value-eliminating-shared-mutable-state)
10. [The Four Execution Modes](#10-the-four-execution-modes)
11. [Pointer-Free Match Structs and GC Pressure](#11-pointer-free-match-structs-and-gc-pressure)
12. [Termination Propagation and Clean Shutdown](#12-termination-propagation-and-clean-shutdown)
13. [Why Not io_uring](#13-why-not-io_uring)
14. [Summary of Synchronization Primitives](#14-summary-of-synchronization-primitives)
15. [Cross-References](#15-cross-references)

---

## 1. The Big Picture: Pipeline Architecture

agrep's recursive search mode (`agrep -r pattern directory`) uses a three-stage concurrent pipeline. Each stage runs in its own goroutine(s), connected by buffered channels:

```
Walker (NumCPU goroutines)
    -> fileCh (buffered chan FileEntry, cap=256)
        -> Scheduler (NumCPU*2 worker goroutines)
            -> resultCh (buffered chan Result, cap=workers*2)
                -> OrderedWriter (1 goroutine)
                    -> stdout (writev syscall)
```

This is a classic producer-consumer pipeline, but with some non-obvious properties:

- **Stage 1 (Walker)** has internal parallelism: multiple goroutines share a work queue to traverse the directory tree concurrently. It produces `FileEntry` values (just a file path).
- **Stage 2 (Scheduler)** is a worker pool: multiple goroutines pull files from the same input channel, each independently opening, reading, matching, and producing a `Result`.
- **Stage 3 (OrderedWriter)** is a single goroutine that resequences results to guarantee deterministic output order despite the non-deterministic processing order.

The orchestration happens in a single function:

**File: `internal/cli/run.go`, lines 170-201**

```go
func runRecursive(paths []string, m matcher.Matcher, reader input.Reader,
    formatter output.Formatter, w *output.Writer, cfg Config, mode searchMode) int {

    fileCh, errCh := walker.Walk(paths, walker.WalkOptions{
        Recursive:      true,
        NoIgnore:       cfg.NoIgnore,
        Hidden:         cfg.Hidden,
        FollowSymlinks: cfg.FollowSymlinks,
        Globs:          cfg.Globs,
    })

    // Log walk errors in background
    go func() {
        for err := range errCh {
            logWarn("walk: %v", err)
        }
    }()

    // Create scheduler and run workers
    sched := scheduler.New(cfg.Workers, m, reader,
        mode == searchFilesOnly, mode == searchCountOnly)
    resultCh := sched.Run(fileCh)

    // Write results in order
    var hasMatch atomic.Bool
    ow := output.NewOrderedWriter(w, formatter, true)
    ow.WriteOrdered(resultCh, func() {
        hasMatch.Store(true)
    })

    if hasMatch.Load() {
        return 0
    }
    return 1
}
```

Notice the structure: `runRecursive` sets up the pipeline and then blocks on `ow.WriteOrdered(resultCh, ...)`. That call consumes the entire `resultCh` channel. When all results have been written, `WriteOrdered` returns, and `runRecursive` checks whether any matches were found. The entire pipeline's lifetime is bounded by this single function call.

### Why a Pipeline?

The alternative to a pipeline is a monolithic approach: one goroutine walks, reads, matches, and writes for each file. The problem is that these operations have very different performance characteristics:

| Operation | Bottleneck | Latency |
|-----------|-----------|---------|
| Directory traversal (getdents64) | I/O, kernel VFS cache | ~1-10us per dir |
| File open + read (pread/mmap) | I/O, page cache | ~5-100us per file |
| Pattern matching (SIMD/regex) | CPU | ~1-50us per file |
| Output formatting + write | I/O (stdout pipe/terminal) | ~1-10us per result |

A pipeline lets each stage run at its own natural speed. The walker can race ahead discovering files while workers are still processing earlier files. Workers can match in parallel while the writer sequentially outputs. Buffered channels between stages absorb speed differences.

### Goroutine Count at Peak

At peak load, the goroutine count is:

| Component | Goroutines | Purpose |
|-----------|-----------|---------|
| Walker workers | NumCPU | Parallel directory traversal |
| Error logger | 1 | Drain errCh |
| Scheduler workers | NumCPU * 2 | File read + match |
| WaitGroup closer | 1 | Close resultCh after all workers done |
| OrderedWriter | 0 (runs on main) | WriteOrdered blocks the calling goroutine |
| **Total** | **NumCPU * 3 + 2** | |

On a 16-core machine, that is 50 goroutines. This is modest. Go's scheduler handles thousands of goroutines efficiently, but there is no benefit to spawning more workers than can make forward progress. The CPU-bound matching work is the bottleneck, and `NumCPU * 2` workers (to overlap I/O wait with CPU work) is the sweet spot.

---

## 2. Stage 1: Parallel BFS Directory Walker

**File: `internal/walker/walker.go`**

The walker is the most complex concurrent component. It implements a parallel breadth-first search of the directory tree using raw Linux `getdents64` syscalls.

### 2.1 Data Structures

```go
type parallelWalker struct {
    fileCh         chan<- FileEntry
    errCh          chan<- error
    hidden         bool
    noIgnore       bool
    followSymlinks bool
    includeBinary  bool
    globs          []string

    mu      sync.Mutex
    queue   []walkItem       // shared work queue (dynamically sized)
    pending int              // dirs enqueued but not yet fully processed
    cond    *sync.Cond       // signaled when items enqueued or work done
    done    bool
}

type walkItem struct {
    path    string
    ignores []ignoreLayer  // snapshot of parent's ignore layers
}

type FileEntry struct {
    Path string
}
```

The `parallelWalker` is the shared state for all walker goroutines. The critical synchronization state is: `mu` (mutex), `queue` (work items), `pending` (counter), `cond` (condition variable), and `done` (termination flag).

### 2.2 Why sync.Mutex + sync.Cond Instead of Channels

This is a deliberate design choice worth examining in detail.

**The channel-based alternative would look like:**
```go
workCh := make(chan walkItem, bufSize)
// Workers read from workCh, send new dirs back to workCh
```

This has several problems:

1. **Deadlock risk**: If all workers are trying to send new directories to `workCh` while the channel buffer is full, and no worker is reading from `workCh` because they are all blocked on send, deadlock occurs. This is the classic "producer is also consumer" problem.

2. **Buffer sizing**: You do not know how many directories exist in the tree. A fixed buffer is either too small (deadlock) or wastes memory.

3. **Termination detection**: With channels, detecting "all work is done" requires an external coordination mechanism anyway (a counter, a done channel, etc.). The channel itself cannot tell you whether more work will arrive.

**The Mutex + Cond approach solves all three:**

- The `queue` slice grows dynamically. No fixed buffer size. No deadlock from a full queue.
- `cond.Wait()` is a blocking wait that releases the mutex and suspends the goroutine until signaled. When work arrives, `cond.Signal()` wakes exactly one waiting worker.
- The `pending` counter, protected by `mu`, provides precise termination detection.

This is a textbook application of a condition variable. The condition being waited on is: "the queue is non-empty OR all work is done."

### 2.3 The Work Queue Operations

**Enqueue (called when a subdirectory is discovered):**

```go
func (pw *parallelWalker) enqueue(item walkItem) {
    pw.mu.Lock()
    pw.queue = append(pw.queue, item)
    pw.pending++
    pw.mu.Unlock()
    pw.cond.Signal()  // Wake one waiting worker
}
```

`Signal()` is called outside the lock. This is safe with Go's `sync.Cond`: the signal will wake a goroutine that is currently in `Wait()` or the next goroutine to call `Wait()`. Signaling outside the lock avoids a "hurry up and wait" scenario where the signaled goroutine immediately blocks trying to acquire the mutex.

**Dequeue (called by workers to get the next directory):**

```go
func (pw *parallelWalker) dequeue() (walkItem, bool) {
    pw.mu.Lock()
    for len(pw.queue) == 0 && !pw.done {
        pw.cond.Wait()  // Atomically releases mu and suspends
    }
    if pw.done && len(pw.queue) == 0 {
        pw.mu.Unlock()
        return walkItem{}, false
    }
    item := pw.queue[0]
    pw.queue = pw.queue[1:]
    pw.mu.Unlock()
    return item, true
}
```

The `for` loop around `cond.Wait()` is essential. This is the standard condition variable pattern: you must re-check the condition after waking because:

1. **Spurious wakeups**: The Go runtime may wake a goroutine from `Wait()` without a corresponding `Signal()`. This is rare but permitted by the specification.
2. **Race with other workers**: Another worker might have dequeued the item between the `Signal()` and this goroutine acquiring the mutex.
3. **Broadcast wakeups**: `Broadcast()` wakes ALL waiting goroutines, but there may be fewer items than goroutines. Each goroutine must check whether there is actually work for it.

The `dequeue` returns `(item, true)` if work is available, or `(walkItem{}, false)` if all work is complete. The `false` return triggers the worker to exit.

**Finish (called when a directory is fully processed):**

```go
func (pw *parallelWalker) finish() {
    pw.mu.Lock()
    pw.pending--
    if pw.pending == 0 && len(pw.queue) == 0 {
        pw.done = true
        pw.cond.Broadcast()  // Wake ALL workers for termination
    }
    pw.mu.Unlock()
}
```

`Broadcast()` is used here instead of `Signal()` because ALL waiting workers need to wake up and observe `done == true` so they can exit.

### 2.4 Termination Detection: The Pending Counter Invariant

The `pending` counter is the key to correct termination. It tracks the number of directories that have been enqueued but not yet fully processed. The invariant is:

> At all times, `pending` equals the number of `enqueue()` calls minus the number of `finish()` calls.

The lifecycle of a single directory:

```
enqueue(dir)         -> pending++  (dir is in queue or being processed)
  worker dequeues dir             (pending unchanged - the dir is still "pending")
  worker processes dir entries
  worker enqueues subdirs         -> pending++ for each subdir
  worker calls finish()           -> pending--
```

The critical ordering is: **subdirectories are enqueued BEFORE finish() is called.** This is visible in `processDir`:

```go
func (pw *parallelWalker) processDir(item walkItem, buf []byte, dirents []Dirent) []Dirent {
    fd, err := openDir(item.path)
    // ... read all entries, collect subdirs ...
    unix.Close(fd)

    // Enqueue discovered subdirectories after closing fd
    for _, sub := range subdirs {
        pw.enqueue(sub)     // pending++ for each subdirectory
    }
    return dirents
}
// Called right after processDir returns:
// pw.finish()              // pending-- for the parent directory
```

If `finish()` were called before enqueuing subdirectories, there would be a window where `pending == 0` and `queue` is empty, but there is still work to do (the subdirectories have not been enqueued yet). This would cause premature termination.

**Proof of correctness**: When `pending == 0 && len(queue) == 0`:
- Every directory that was ever enqueued has been fully processed (pending-- was called)
- Before each directory's pending-- was called, all its subdirectories were enqueued (pending++ was called for each)
- Therefore, every directory in the tree has been processed, and no more work will ever arrive
- Setting `done = true` is safe

### 2.5 Per-Worker Allocations

```go
func (pw *parallelWalker) worker() {
    buf := make([]byte, 32*1024)  // Per-worker 32KB getdents buffer
    var dirents []Dirent           // Per-worker reusable dirent slice
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

Each worker allocates its own `buf` and `dirents` once at startup. These are reused for every directory that worker processes across its entire lifetime. There is no sharing of these allocations between workers, so there is no contention or synchronization needed for them.

The `dirents` slice is passed to `processDir` and returned (possibly grown). `ParseDirents` reuses the slice by resetting it to `dst[:0]`, which keeps the backing array:

**File: `internal/walker/dirent.go`, lines 36-37**
```go
func ParseDirents(buf []byte, n int, dst []Dirent) []Dirent {
    entries := dst[:0]  // Reuse backing array
```

Over time, `dirents` grows to accommodate the largest directory encountered by that worker and then stabilizes. No further allocations occur for the dirent slice after the initial growth phase.

### 2.6 Directory File Descriptor Management

```go
func (pw *parallelWalker) processDir(item walkItem, buf []byte, dirents []Dirent) []Dirent {
    fd, err := openDir(item.path)
    if err != nil {
        pw.errCh <- &WalkError{Path: item.path, Err: err}
        return dirents
    }

    var subdirs []walkItem
    // ... read all entries via getdents64 ...
    // ... collect subdirectories into subdirs ...

    unix.Close(fd)  // Close BEFORE enqueuing subdirectories

    for _, sub := range subdirs {
        pw.enqueue(sub)
    }
    return dirents
}
```

The directory file descriptor is closed BEFORE enqueuing subdirectories. This is a deliberate choice to bound fd usage.

**Why this matters:** If the fd were held open until subdirectories were processed, the number of simultaneously open fds would be O(tree_depth * num_workers). For a deep directory tree (e.g., `node_modules` with depth 20) and 16 workers, that could be 320 open directory fds, in addition to the fds that the scheduler workers open for reading files.

By closing the fd before enqueuing subdirectories, the number of simultaneously open directory fds is bounded by the number of walker workers (one fd per worker at any given time). This is O(NumCPU), not O(tree_depth * NumCPU).

### 2.7 Why FileEntry Only Carries a Path

```go
type FileEntry struct {
    Path string
}
```

FileEntry deliberately carries only the file path, not a file descriptor or size. This is a design decision with several justifications:

1. **Fd lifetime management**: If the walker opened a file and passed the fd through the channel, the fd would remain open for an unbounded time (until a scheduler worker gets around to processing it). With a 256-deep channel buffer and thousands of files, this could exhaust the fd limit.

2. **Separation of concerns**: The walker's job is to discover files. The scheduler's job is to read and process them. Each component opens what it needs and closes it when done.

3. **fstat after open**: The scheduler worker needs to fstat the file to determine its size (for mmap vs. buffered decision). It needs an fd for this anyway. Opening the file in the scheduler gives a single open-fstat-read-close lifecycle within one goroutine, which is simple and easy to reason about.

4. **O_NOATIME optimization**: The scheduler's `openFile()` function uses O_NOATIME. This is a different open flag than the walker's `openDir()` which uses O_DIRECTORY. Each component uses the optimal flags for its access pattern.

### 2.8 Launching the Walker

**File: `internal/walker/walker.go`, lines 54-110**

```go
func Walk(roots []string, opts WalkOptions) (<-chan FileEntry, <-chan error) {
    fileCh := make(chan FileEntry, 256)
    errCh := make(chan error, 16)

    go func() {
        defer close(fileCh)
        defer close(errCh)

        if !opts.Recursive {
            // Non-recursive: just stat each root path
            for _, root := range roots {
                var stat unix.Stat_t
                if err := unix.Stat(root, &stat); err != nil {
                    errCh <- &WalkError{Path: root, Err: err}
                    continue
                }
                if stat.Mode&unix.S_IFMT == unix.S_IFREG {
                    fileCh <- FileEntry{Path: root}
                }
            }
            return
        }

        pw := &parallelWalker{
            fileCh:         fileCh,
            errCh:          errCh,
            hidden:         opts.Hidden,
            noIgnore:       opts.NoIgnore,
            followSymlinks: opts.FollowSymlinks,
            includeBinary:  opts.IncludeBinary,
            globs:          opts.Globs,
        }
        pw.cond = sync.NewCond(&pw.mu)

        // Seed work queue with root directories
        for _, root := range roots {
            var layers []ignoreLayer
            if !opts.NoIgnore {
                layers = []ignoreLayer{loadIgnoreLayer(root)}
            }
            pw.enqueue(walkItem{path: root, ignores: layers})
        }

        // Launch parallel walker goroutines
        workers := runtime.NumCPU()
        var wg sync.WaitGroup
        for range workers {
            wg.Add(1)
            go func() {
                defer wg.Done()
                pw.worker()
            }()
        }
        wg.Wait()
    }()

    return fileCh, errCh
}
```

The entire walker runs inside a single goroutine that spawns NumCPU child goroutines and waits for them all. When all workers finish (`wg.Wait()` returns), the deferred `close(fileCh)` and `close(errCh)` execute, signaling to downstream consumers that no more items will arrive.

The root directories are enqueued before any workers start. This is safe because `enqueue` just appends to a slice and increments a counter. Workers will find items waiting when they first call `dequeue`.

---

## 3. Stage 2: Worker Pool Scheduler

**File: `internal/scheduler/scheduler.go`**

The scheduler is deliberately simple. Its job is to take files from the walker and produce search results. The complexity of work distribution is handled entirely by Go's channel semantics.

### 3.1 The Scheduler Structure

```go
type Scheduler struct {
    workers   int
    matcher   matcher.Matcher
    reader    input.Reader
    filesOnly bool  // -l mode: just check if any match exists
    countOnly bool  // -c mode: count matching lines only
}

func New(workers int, m matcher.Matcher, r input.Reader,
    filesOnly bool, countOnly bool) *Scheduler {
    if workers <= 0 {
        workers = runtime.NumCPU() * 2
    }
    return &Scheduler{
        workers:   workers,
        matcher:   m,
        reader:    r,
        filesOnly: filesOnly,
        countOnly: countOnly,
    }
}
```

The `Scheduler` struct is created once and is immutable after construction. All fields are read-only during `Run()`. This means all worker goroutines can read the struct fields without synchronization.

Note the important detail: the `Matcher` and `Reader` interfaces must be safe for concurrent use. The matcher implementations are stateless (they operate on the input `[]byte` and produce output without modifying any shared state). The `adaptiveReader` opens a new fd per call, so it is inherently goroutine-safe.

### 3.2 The Run Method

```go
func (s *Scheduler) Run(files <-chan walker.FileEntry) <-chan output.Result {
    resultCh := make(chan output.Result, s.workers*2)
    var seq atomic.Int64

    var wg sync.WaitGroup
    for range s.workers {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for entry := range files {
                seqNum := int(seq.Add(1))
                result := s.processFile(entry)
                result.SeqNum = seqNum
                resultCh <- result
            }
        }()
    }

    go func() {
        wg.Wait()
        close(resultCh)
    }()

    return resultCh
}
```

This is a standard fan-out pattern with a twist (sequence numbers). Let us examine each piece.

### 3.3 Channel-Based Work Distribution

```go
for entry := range files {
```

All workers range over the same `files` channel. When multiple goroutines receive from the same channel, Go guarantees:

1. **Each value is received by exactly one goroutine.** There is no duplication.
2. **The receive is approximately fair.** Go's channel implementation uses a FIFO queue of waiting receivers. When a value is sent, the longest-waiting goroutine gets it. Over time, work is distributed evenly.
3. **When the channel is closed, the range loop exits.** All workers terminate cleanly.

This is simpler than explicit work stealing (where workers steal from each other's queues). The tradeoff is that channel-based distribution has higher overhead per item (~50ns for an uncontended channel operation) compared to lock-free work stealing (~10-20ns). For agrep, this is negligible because each item represents a file that takes microseconds to process.

### 3.4 Atomic Sequence Numbers

```go
var seq atomic.Int64
// ...
seqNum := int(seq.Add(1))
result := s.processFile(entry)
result.SeqNum = seqNum
```

The sequence number is assigned BEFORE `processFile` is called. This means the sequence reflects the order in which files are dequeued from `fileCh`, not the order in which processing completes.

Why `atomic.Int64` instead of a mutex-protected counter? Performance. `atomic.Add` compiles to a single `LOCK XADD` instruction on x86-64. There is no goroutine scheduling overhead, no lock acquisition, and no possibility of priority inversion. Under high contention (all workers hitting the counter simultaneously), atomic operations degrade gracefully because the CPU handles the cache-line bouncing in hardware.

Why assign the sequence number before processing? Consider the alternative: assigning after processing. Worker A processes a large file (takes 10ms), Worker B processes a small file (takes 0.1ms). If sequence numbers are assigned after processing, B would get a lower sequence number than A even though A received its file first. This would not preserve the file discovery order.

By assigning before processing, the sequence reflects the order files were received from the walker. The OrderedWriter then outputs results in this order, giving deterministic output regardless of processing speed variance.

### 3.5 Why NumCPU * 2 Workers

The default worker count is `runtime.NumCPU() * 2`. This is not arbitrary.

Each worker's processing loop is:
1. Open file (syscall: ~1-5us, blocks the goroutine)
2. fstat file (syscall: ~0.5-1us, blocks)
3. Read/mmap file (syscall: ~5-100us, blocks)
4. Match pattern (CPU: ~1-50us, does not block)
5. Close/munmap (syscall: ~1us, blocks)
6. Send result on channel (~50ns)

Steps 1-3 and 5 are I/O operations where the goroutine is blocked waiting for a kernel response. During this time, the CPU core is idle. With NumCPU workers, there would be idle cores whenever workers are blocked on I/O. With NumCPU * 2 workers, while half the workers are blocked on I/O, the other half can run their matchers on the CPU. The 2x factor roughly accounts for the ratio of I/O time to CPU time in a typical grep workload.

Go's M:N scheduler (goroutines multiplexed onto OS threads) handles this efficiently. When a goroutine blocks on a syscall, the Go runtime parks it and schedules another goroutine on that OS thread. The 2x factor provides enough goroutines to keep all OS threads busy.

### 3.6 The processFile Method

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

    // Binary detection: skip binary files entirely (like ripgrep)
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
            result.Closer = closeReader  // Keep buffer alive for formatting
        } else {
            closeReader()
        }
    }
    return result
}
```

There are three fast paths optimized for different search modes:

1. **filesOnly (-l)**: Uses `MatchExists`, which returns at the first match. No line extraction, no position tracking. The `closeReader()` is called immediately -- the buffer is not needed for formatting since only the file path is printed.

2. **countOnly (-c)**: Uses `CountAll`, which counts matching lines without building `Match` structs. Again, the buffer is released immediately.

3. **full search (default)**: Uses `FindAll`, which builds the complete `MatchSet` with line numbers, positions, and byte offsets. If matches are found, the `closeReader` function is attached to the result as `result.Closer`. This is critical: the `MatchSet.Data` field is a slice that points into the file's read buffer. The buffer must stay alive until the OrderedWriter has finished formatting the result.

### 3.7 WaitGroup Shutdown

```go
go func() {
    wg.Wait()
    close(resultCh)
}()
```

When `fileCh` is closed by the walker, all workers' `range files` loops exit. Each worker calls `defer wg.Done()`. The anonymous goroutine waits until all workers have finished, then closes `resultCh`. This signals to the OrderedWriter that no more results will arrive.

The WaitGroup goroutine is a standard Go pattern. It cannot be done inline because `Run` must return `resultCh` immediately (so the caller can start consuming results). The cleanup must happen asynchronously after all workers finish.

---

## 4. Stage 3: Ordered Output Resequencing

**File: `internal/output/writer.go`**

Results arrive from the scheduler in arbitrary order. If Worker 3 finishes before Worker 1, its result arrives first on `resultCh`. The OrderedWriter resequences results to match the file discovery order.

### 4.1 The Resequencing Algorithm

```go
func (ow *OrderedWriter) WriteOrdered(results <-chan Result, onMatch func()) {
    nextSeq := 1
    pending := make(map[int]Result)
    var buf []byte  // reused across all writeResult calls

    for r := range results {
        if r.Err == nil && r.HasMatch() {
            if onMatch != nil {
                onMatch()
            }
        }

        if r.SeqNum == nextSeq {
            buf = ow.writeResult(buf, r)
            nextSeq++
            // Flush any consecutive pending results
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
            pending[r.SeqNum] = r  // Buffer out-of-order result
        }
    }
}
```

This is a resequencing buffer (also known as a reorder buffer, common in CPU microarchitecture). The algorithm:

1. Maintain `nextSeq`: the sequence number we expect to write next. Starts at 1.
2. When a result arrives:
   - If its sequence number equals `nextSeq`, write it immediately.
   - Then check if the next expected sequence number is also buffered in `pending`. If so, write that too. Repeat (drain consecutive pending results).
   - If the sequence number does not equal `nextSeq`, store the result in the `pending` map for later.

**Example execution:**

```
Arrival order: SeqNum=3, SeqNum=1, SeqNum=2, SeqNum=5, SeqNum=4

Step 1: Receive SeqNum=3, nextSeq=1 -> buffer in pending
        pending = {3: r3}

Step 2: Receive SeqNum=1, nextSeq=1 -> write r1, nextSeq=2
        Check pending[2]? No.
        pending = {3: r3}

Step 3: Receive SeqNum=2, nextSeq=2 -> write r2, nextSeq=3
        Check pending[3]? Yes! Write r3, nextSeq=4
        Check pending[4]? No.
        pending = {}

Step 4: Receive SeqNum=5, nextSeq=4 -> buffer in pending
        pending = {5: r5}

Step 5: Receive SeqNum=4, nextSeq=4 -> write r4, nextSeq=5
        Check pending[5]? Yes! Write r5, nextSeq=6
        Check pending[6]? No.
        pending = {}

Output order: r1, r2, r3, r4, r5  (correct!)
```

### 4.2 Memory Characteristics of the Pending Map

The `pending` map's size at any moment equals the "gap" between the fastest and slowest worker. In the worst case, if one worker gets stuck on a very large file while all other workers race ahead, the map could hold `workers-1` results.

In practice, the gap is small (< 20 entries) because:
- File sizes in a typical project follow a power-law distribution, but most files are small (< 100KB).
- Worker processing time variance is low for small files.
- The 256-deep `fileCh` buffer means all workers get files in approximately the same order, so their sequence numbers stay close together.

Each pending `Result` holds a `MatchSet` (which may contain match data). For matched files, the `MatchSet.Data` slice still points into the original file buffer (kept alive by `Closer`). For non-matched files, the buffer has already been released. So the memory cost of pending results is primarily the `Result` struct itself plus any match metadata, not the file data.

### 4.3 The writeResult Helper

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

Key details:

1. **`buf[:0]` reuse**: The format buffer is reset to zero length but keeps its backing array. This means `Format` can `append` to it without allocating, as long as the output fits within the existing capacity. The buffer grows over time to accommodate the largest single result and then stabilizes.

2. **Closer is called after Format**: The `Closer()` function releases the underlying file data buffer (either munmapping the file or returning the pooled buffer). This must happen after `Format` has finished reading from `MatchSet.Data`, because `Format` accesses the line content via byte offsets into that buffer.

3. **Error results are skipped**: If a file failed to read, there is nothing to format. The `Closer` (if any) is called to clean up, but no output is produced.

### 4.4 Why Ordering Matters

Without ordering, the same `agrep -r pattern directory` command would produce different output on different runs. This is problematic for:

- **Diff-based testing**: Tests that compare agrep's output to expected output would be flaky.
- **Piping to other tools**: `agrep | sort` would work, but `agrep | head -20` would give different results each time.
- **User expectations**: When a user runs the same command twice, they expect the same output. Non-deterministic output erodes trust.
- **Debugging**: When investigating a bug, reproducing the exact output is valuable.

ripgrep also sorts output in its default mode for these reasons.

### 4.5 The onMatch Callback

```go
var hasMatch atomic.Bool
ow.WriteOrdered(resultCh, func() {
    hasMatch.Store(true)
})
```

The `onMatch` callback is invoked every time a result with matches is received. It uses `atomic.Bool` to record whether any match was found, without any locking. Since `WriteOrdered` runs on a single goroutine, the `onMatch` callback is only ever called from one goroutine, so even a plain `bool` would be safe. However, `atomic.Bool` is used because `hasMatch.Load()` is called from the main goroutine after `WriteOrdered` returns. Using `atomic` provides a formal happens-before guarantee that the `Store` is visible to the subsequent `Load`.

### 4.6 The Writer: writev Syscall

**File: `internal/output/writer.go`, lines 1-34**

```go
type Writer struct {
    fd int
}

func NewWriter() *Writer {
    return &Writer{fd: int(os.Stdout.Fd())}
}

func (w *Writer) Write(data []byte) error {
    if len(data) == 0 {
        return nil
    }
    for len(data) > 0 {
        iovs := [][]byte{data}
        n, err := unix.Writev(w.fd, iovs)
        if err != nil {
            return err
        }
        data = data[n:]
    }
    return nil
}
```

The Writer uses `writev` (scatter-gather I/O) to write to stdout. For a single buffer, `writev` with one iov is equivalent to `write`. The `writev` syscall is used instead of `os.Stdout.Write` to bypass Go's internal buffering and locking on `os.File`. Since the OrderedWriter is single-threaded, there is no need for Go's file mutex, and the direct syscall avoids that overhead.

The `for` loop handles partial writes: if `writev` writes fewer bytes than requested (which can happen if stdout is a pipe and the pipe buffer is full), it retries with the remaining data.

---

## 5. Channel Buffer Sizing and Backpressure

The buffer sizes in the pipeline are carefully chosen to balance throughput and memory usage.

### 5.1 fileCh: 256 entries

```go
fileCh := make(chan FileEntry, 256)
```

**Why 256?** The walker discovers files much faster than workers can process them. `getdents64` returns dozens of entries per syscall, while each worker spends microseconds to milliseconds per file. A large buffer lets the walker stay ahead, ensuring workers never starve.

**Memory cost:** Each `FileEntry` contains a single `string` (16 bytes on 64-bit: pointer + length). 256 entries = 4KB of channel buffer overhead plus the string data. Typical paths are ~50-100 bytes, so total memory is ~30KB. This is negligible.

**Backpressure:** When the buffer is full, walker goroutines block on `pw.fileCh <- FileEntry{...}`. This is desirable: it prevents the walker from racing arbitrarily far ahead and consuming memory for thousands of queued paths. The walker pauses, workers drain some entries, and the walker resumes.

### 5.2 resultCh: workers * 2 entries

```go
resultCh := make(chan output.Result, s.workers*2)
```

**Why workers * 2?** Each worker produces one result per file. If the OrderedWriter is temporarily slow (e.g., writing a large formatted result to a pipe with a full buffer), workers need somewhere to put their results. With `workers*2` buffer capacity, every worker can have one result in-flight (being processed) and one buffered (waiting to be consumed), without blocking.

**Memory cost:** Each `Result` contains a file path, sequence number, `MatchSet` (which is three slices), a match count, an error, and a `Closer` func. For non-matching files, this is approximately 120 bytes per entry. For matching files, the `MatchSet` slices add overhead, but the bulk of the data (the file content) is referenced by pointer, not copied. With `workers*2 = 64` entries on a 16-core machine, the buffer overhead is ~8KB.

### 5.3 errCh: 16 entries

```go
errCh := make(chan error, 16)
```

**Why 16?** Errors (permission denied, broken symlinks, etc.) are infrequent. A small buffer prevents the walker from blocking on error reporting. If more than 16 errors accumulate without being drained, the walker blocks. This is acceptable because having more than 16 simultaneous errors usually indicates a systemic problem (e.g., searching a directory you do not have permission to read).

### 5.4 Backpressure Propagation

Backpressure flows backward through the pipeline via channel blocking:

```
stdout slow (pipe full)
  -> Writer.Write blocks on writev
    -> OrderedWriter.writeResult blocks
      -> OrderedWriter.WriteOrdered blocks on writing, stops consuming resultCh
        -> resultCh fills up
          -> Workers block on resultCh <- result
            -> Workers stop consuming fileCh
              -> fileCh fills up
                -> Walker blocks on fileCh <- FileEntry
                  -> Walker goroutines block, stop discovering files
```

This is an elegant property of Go channels. No explicit flow control code is needed. The system self-regulates: when any stage slows down, upstream stages automatically slow down to match.

---

## 6. The Atomic O_NOATIME Pattern

**Files: `internal/walker/walker.go` (lines 16-33), `internal/input/mmap.go` (lines 107-124)**

Both the walker and the input reader use O_NOATIME when opening files/directories to avoid updating the access time inode field, which would cause unnecessary disk writes. However, O_NOATIME requires file ownership or CAP_FOWNER capability, so it might fail with EPERM.

### 6.1 The Pattern

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

### 6.2 Analysis

This is a lock-free, eventually-consistent optimization pattern:

1. **Initial state**: `noatimeWorks = 1`. All goroutines try O_NOATIME first.
2. **On success**: Return the fd immediately. No state change needed.
3. **On EPERM**: Set `noatimeWorks = 0` atomically. Fall through to the non-O_NOATIME open.
4. **After first EPERM**: All subsequent goroutines skip the O_NOATIME attempt entirely (one atomic Load per open).

**Race conditions and why they are harmless:**

Multiple goroutines may simultaneously read `noatimeWorks == 1`, attempt O_NOATIME, and all get EPERM. Each will call `Store(0)`. This is a redundant write, but it is idempotent (storing 0 when the value is already 0 has no effect). The "cost" is a few extra failed syscalls before the flag propagates. On a 16-core machine, at most 16 goroutines might wastefully try O_NOATIME. After that, zero wasted syscalls for the remainder of the search.

**Why the atomic is not a lock**: `atomic.Int32.Load()` compiles to a single `MOV` instruction on x86-64 (plain loads are sequentially consistent on x86). `Store` compiles to a single `XCHG` (or `MOV` with a memory fence). These are 1-2 CPU cycles, compared to ~20-50 cycles for a mutex lock/unlock.

### 6.3 Separate Flags for Walker and Input

The walker (`internal/walker/walker.go`) and the input reader (`internal/input/mmap.go`) have their own separate `noatimeWorks` flags. They do not share a flag. This is intentional:

- The walker opens directories with O_DIRECTORY. Directories may have different ownership characteristics than regular files.
- The input reader opens regular files with O_RDONLY. These may have different ownership.
- A user might own the files but not the directories (or vice versa).
- Having separate flags ensures each component adapts independently to its access pattern.

---

## 7. Buffer Lifecycle and Ownership Across Goroutines

Understanding how file data buffers move through the pipeline is essential for understanding the concurrency model.

### 7.1 Buffer Creation (Scheduler Worker Goroutine)

When a scheduler worker calls `s.reader.Read(entry.Path)`, the `adaptiveReader` either:

- **Buffers the file** (for small files): Obtains a buffer from `sync.Pool`, reads the file into it via `pread`. The `Closer` returns the buffer to the pool.
- **Memory-maps the file** (for large files): Uses `mmap` to create a virtual memory mapping. The `Closer` calls `munmap` and closes the fd.

**File: `internal/input/mmap.go`, lines 80-103**

```go
func (r *adaptiveReader) Read(path string) (ReadResult, error) {
    fd, err := openFile(path)
    // ...
    if size >= r.threshold {
        return readMmap(fd, size, path)
    }
    return readBuffered(fd, size)
}
```

In both cases, `ReadResult.Data` is a `[]byte` pointing to the file's content, and `ReadResult.Closer` is a function that releases the underlying resource.

### 7.2 Buffer Transfer Through the Pipeline

The buffer ownership follows a precise path:

```
Scheduler Worker creates buffer (via reader.Read)
  -> matcher.FindAll(data) creates MatchSet with Data=data
    -> MatchSet.Data points into the original buffer
  -> result.Closer = closeReader (captures the Closer)
  -> result sent on resultCh
    -> BUFFER OWNERSHIP TRANSFERS TO THE CHANNEL
  -> OrderedWriter receives result
    -> formatter.Format reads from result.MatchSet.Data
    -> result.Closer() is called AFTER formatting
      -> BUFFER IS RELEASED (munmap or pool return)
```

The critical invariant: **the buffer must stay alive from the moment MatchSet is created until after formatting is complete.** This is enforced by storing the `Closer` function in the `Result` struct and calling it only in `OrderedWriter.writeResult()`, after `Format()` has finished reading the data.

### 7.3 Non-Matching Files

When a file has no matches, the buffer is released immediately in `processFile`:

```go
result.MatchSet = s.matcher.FindAll(readResult.Data)
if result.MatchSet.HasMatch() {
    result.Closer = closeReader  // Keep buffer alive
} else {
    closeReader()  // Release buffer immediately
}
```

This means non-matching files do not hold buffers while waiting in the `resultCh` or the OrderedWriter's `pending` map. Since most files in a typical search do not match, this dramatically reduces memory usage.

### 7.4 Fast-Path Modes Release Buffers Early

For `-l` (files-only) and `-c` (count-only) modes, the buffer is released in `processFile` regardless of whether matches were found:

```go
if s.filesOnly {
    if s.matcher.MatchExists(readResult.Data) {
        result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
    }
    closeReader()  // Buffer released immediately
}
```

In `-l` mode, only the file path is printed (no line content). The `MatchSet` contains a dummy `Match{}` to signal "has match" but references no file data. The buffer can be released immediately because it is never accessed again.

---

## 8. sync.Pool for Read Buffers

**File: `internal/input/buffered.go`**

The buffered reader uses `sync.Pool` to reuse read buffers across files, reducing GC pressure.

### 8.1 The Pool

```go
var bufPool = sync.Pool{
    New: func() any {
        b := make([]byte, 0, 64*1024)  // 64KB initial capacity
        return &b
    },
}
```

The pool stores `*[]byte` (pointer to slice), not `[]byte` (slice directly). This is important: `sync.Pool` stores `any` (interface). If we stored a `[]byte` directly, the interface would contain a copy of the slice header, and we would lose track of capacity growth. By storing a pointer, the pool always refers to the same slice header, which retains information about any capacity growth.

### 8.2 Get, Grow, and Return

```go
func readBuffered(fd int, size int64) (ReadResult, error) {
    bp := bufPool.Get().(*[]byte)
    buf := *bp
    if cap(buf) < int(size) {
        buf = make([]byte, size)  // Grow if needed
    } else {
        buf = buf[:size]          // Resize within existing capacity
    }

    // ... pread the file into buf ...

    return ReadResult{
        Data: buf[:totalRead],
        Closer: func() error {
            *bp = buf       // Update the pooled slice header
            bufPool.Put(bp) // Return to pool
            return nil
        },
    }, nil
}
```

**The lifecycle:**

1. **Get**: Retrieve a `*[]byte` from the pool (or allocate a new 64KB one).
2. **Grow**: If the file is larger than the buffer's capacity, allocate a new, larger buffer. The old buffer is lost (GC will collect it), and the `bp` pointer will later store the new, larger buffer.
3. **Use**: Read the file into the buffer.
4. **Return**: The `Closer` updates `*bp = buf` (ensuring the pooled pointer refers to the potentially-grown buffer) and calls `bufPool.Put(bp)`.

**Concurrency safety**: `sync.Pool` is designed for concurrent access. Multiple goroutines can Get and Put simultaneously. The pool is sharded internally (per-P pools) to minimize contention. Each goroutine's private pool is checked first, then shared pools.

**GC interaction**: `sync.Pool` may discard entries during garbage collection. This is acceptable: discarded buffers will be collected, and new ones will be allocated on demand. The pool is an optimization, not a requirement for correctness.

### 8.3 Why Pool Across Files, Not Across Workers

One alternative would be to give each worker its own dedicated buffer (like the walker gives each worker its own `getdents` buffer). The pool-based approach is preferred for file reading because:

1. **File sizes vary widely.** A worker might process a 1KB file followed by a 100MB file. A dedicated per-worker buffer would either be too small (requiring frequent reallocation) or too large (wasting memory when processing small files).
2. **The pool adapts naturally.** Large buffers flow back to the pool and are reused by whichever worker next needs a large buffer.
3. **Non-matching files return buffers early.** With a pool, the buffer immediately becomes available for another worker. With dedicated per-worker buffers, the buffer sits idle until that specific worker processes its next file.

---

## 9. Config as Value: Eliminating Shared Mutable State

**File: `internal/cli/config.go`**

```go
type Config struct {
    Patterns       []string
    Fixed          bool
    PCRE           bool
    IgnoreCase     bool
    Recursive      bool
    LineNumbers    bool
    CountOnly      bool
    Invert         bool
    FileNamesOnly  bool
    ContextBefore  int
    ContextAfter   int
    WatchMode      bool
    JSONOutput     bool
    Color          ColorMode
    Workers        int
    NoIgnore       bool
    Hidden         bool
    FollowSymlinks bool
    SmartCase      bool
    Globs          []string
    MaxColumns     int
    MmapThreshold  int64
    Paths          []string
}
```

`Config` is a plain struct, passed by value:

```go
func Run(cfg Config) int { ... }
```

### 9.1 Why Not a Pointer

Passing `Config` by value means each function gets its own copy. This has important concurrency implications:

1. **No data races**: If `Config` were passed as `*Config`, multiple goroutines reading fields while another goroutine modified it would be a data race. By passing by value, modifications in one function do not affect other functions' copies.

2. **Safe mutation**: `Run()` modifies `cfg.IgnoreCase` (for smart-case logic) without affecting the caller's copy. This is a local transformation, invisible to the outside world.

3. **Thread-safe by construction**: No mutex is needed. No atomic operations are needed. Each goroutine that receives a `Config` owns its copy.

### 9.2 Destructuring at Boundaries

The `Config` is not passed wholesale to every component. Instead, each component receives only what it needs:

```go
// Walker gets WalkOptions, not Config
fileCh, errCh := walker.Walk(paths, walker.WalkOptions{
    Recursive:      true,
    NoIgnore:       cfg.NoIgnore,
    Hidden:         cfg.Hidden,
    FollowSymlinks: cfg.FollowSymlinks,
    Globs:          cfg.Globs,
})

// Scheduler gets individual fields, not Config
sched := scheduler.New(cfg.Workers, m, reader,
    mode == searchFilesOnly, mode == searchCountOnly)
```

This is dependency injection at the value level. Each component declares exactly what it needs via its constructor signature. There is no hidden coupling between components via a shared config object.

---

## 10. The Four Execution Modes

**File: `internal/cli/run.go`**

agrep supports four execution modes, each with different concurrency characteristics:

### 10.1 Stdin Mode (Single-Goroutine)

```go
func runStdin(reader input.Reader, m matcher.Matcher,
    formatter output.Formatter, w *output.Writer) int {
    result := searchReader(reader, "", m, searchFull)
    if result.HasMatch() {
        buf := formatter.Format(nil, result, false)
        if result.Closer != nil {
            result.Closer()
        }
        w.Write(buf)
        return 0
    }
    if result.Closer != nil {
        result.Closer()
    }
    return 1
}
```

No concurrency. A single goroutine reads all of stdin, matches, formats, and writes. This is appropriate because stdin is a single stream that must be read sequentially.

### 10.2 Files Mode (Single-Goroutine)

```go
func runFiles(paths []string, m matcher.Matcher, reader input.Reader,
    formatter output.Formatter, w *output.Writer, mode searchMode) int {
    multiFile := len(paths) > 1
    hasMatch := false
    var buf []byte

    for _, path := range paths {
        result := searchReader(reader, path, m, mode)
        // ... error handling ...
        buf = formatter.Format(buf[:0], result, multiFile)
        if result.Closer != nil {
            result.Closer()
        }
        w.Write(buf)
    }
    // ...
}
```

No concurrency. Files are processed sequentially. This mode is used when the user specifies explicit file paths without `-r`. The single `buf` variable is reused across all files via `buf[:0]`.

Why no parallelism here? When the user specifies a small number of explicit files, the overhead of setting up channels, goroutines, and ordering would exceed the benefit. Sequential processing is simpler and avoids the complexity of ordered output.

### 10.3 Recursive Mode (Full Pipeline)

```go
func runRecursive(paths []string, m matcher.Matcher, reader input.Reader,
    formatter output.Formatter, w *output.Writer, cfg Config, mode searchMode) int {
    // ... full pipeline as described throughout this document ...
}
```

Full concurrent pipeline. This is where all the machinery described in this document comes into play. Used when `-r` is specified and directory traversal is needed.

### 10.4 Watch Mode (Event-Driven)

```go
func runWatch(paths []string, m matcher.Matcher, formatter output.Formatter,
    w *output.Writer, cfg Config) int {
    watcher, err := watch.New()
    // ...
    for evt := range events {
        switch evt.Type {
        case watch.EventModified:
            data, err := watcher.ReadNew(evt.Path)
            // ... search new content ...
        case watch.EventCreated:
            watcher.Add(evt.Path)
        case watch.EventDeleted:
            logWarn("watched file removed: %s", evt.Path)
        }
    }
    // ...
}
```

Event-driven, single-goroutine processing. The watch mode uses inotify + epoll (via the `internal/watch` package) to receive file change notifications. Each event is processed synchronously. This is appropriate because watch events arrive incrementally, and the data to search per event is typically small (just the new bytes appended to a file).

---

## 11. Pointer-Free Match Structs and GC Pressure

**File: `internal/matcher/match.go`**

The `Match` and `MatchSet` types are designed to minimize GC scanning overhead in the concurrent pipeline.

### 11.1 The Match Struct

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

`Match` contains only value types (integers and a boolean). No pointers, no strings, no slices, no interfaces. This means:

1. **A `[]Match` is opaque to the GC.** The garbage collector scans memory looking for pointers. A slice of pointer-free structs has no internal pointers to scan. The GC treats it as a blob of bytes, regardless of how many matches it contains.

2. **O(1) GC overhead per file, not O(matches).** A file with 10,000 matches does not cause 10,000x more GC work than a file with 1 match.

### 11.2 The MatchSet Container

```go
type MatchSet struct {
    Data      []byte    // the file data buffer
    Matches   []Match   // pointer-free match structs
    Positions [][2]int  // shared positions array
}
```

Only `MatchSet` contains pointer types (three slice headers = three pointers each). The GC scans exactly 3 pointers per `MatchSet`, regardless of how many matches or positions it contains.

Line content is not stored in each `Match`. Instead, `Match.LineStart` and `Match.LineLen` are byte offsets into `MatchSet.Data`. This avoids creating a `string` or `[]byte` per match, which would add GC-visible pointers.

### 11.3 Why This Matters for Concurrency

When `Result` values sit in the `resultCh` channel buffer or the OrderedWriter's `pending` map, the GC must scan them. With pointer-free `Match` structs, the GC work per result is constant regardless of match density. A file with 50,000 matches (a dense grep on a large file) has the same GC overhead as a file with 1 match.

In a concurrent pipeline processing thousands of files, the cumulative GC savings are significant. Without this design, dense-match files would cause GC pauses that stall ALL goroutines (Go's GC is stop-the-world for marking setup).

---

## 12. Termination Propagation and Clean Shutdown

The pipeline shuts down cleanly through a cascade of channel closures:

```
1. Walker workers all finish
   -> wg.Wait() returns in Walk's goroutine
   -> close(fileCh), close(errCh)

2. close(fileCh) causes:
   -> All scheduler workers exit their "range files" loops
   -> Each worker calls defer wg.Done()
   -> Scheduler's wg.Wait() goroutine unblocks
   -> close(resultCh)

3. close(resultCh) causes:
   -> OrderedWriter's "range results" loop exits
   -> WriteOrdered returns

4. WriteOrdered returning causes:
   -> runRecursive checks hasMatch and returns exit code
```

This is a clean, sequential termination cascade with no explicit shutdown signaling. Each stage simply closes its output channel when it finishes, and the downstream stage detects this via `range` loop termination.

### 12.1 Error Channel Drain Goroutine

```go
go func() {
    for err := range errCh {
        logWarn("walk: %v", err)
    }
}()
```

This goroutine runs for the entire duration of the pipeline. It exits when `errCh` is closed (which happens when the walker finishes). Because it is a fire-and-forget goroutine (its termination is not awaited), it becomes garbage-collected after `errCh` is closed and the goroutine returns.

This is a deliberate choice: walker errors are non-fatal and are logged to stderr. The main pipeline should not block waiting for error handling. If errors were instead synchronously handled, a burst of errors could stall the walker.

### 12.2 No Context Cancellation

The pipeline does not use `context.Context` for cancellation. There is no mechanism to abort mid-search (e.g., for early termination in `-l` mode after the first match). This is a simplicity trade-off:

- For `-l` (files-only) mode, each individual file search exits early via `MatchExists` (returns at the first match), but the pipeline continues processing all files.
- True early termination would require propagating a cancel signal through all channels, which adds complexity (checking `ctx.Done()` in every loop iteration) for a modest benefit.
- The full pipeline typically completes in < 1 second for most searches, so early termination saves little wall-clock time.

---

## 13. Why Not io_uring

agrep includes an experimental `internal/uring/` package with a minimal io_uring wrapper. Benchmarking showed that batched io_uring was 1.3-3x SLOWER than direct syscalls for grep workloads. The reasons are instructive:

### 13.1 Small Batch Sizes

io_uring benefits come from amortizing the io_uring_enter syscall over many operations. But the walker drip-feeds files one at a time through a channel. By the time a scheduler worker batches up operations, the batch is typically 1-3 items. The overhead of submitting to the SQ ring and checking the CQ ring exceeds the cost of just calling `open` + `pread` + `close` directly.

### 13.2 Goroutine Contention

Multiple scheduler workers sharing a single io_uring instance creates lock contention on the SQ/CQ rings. Multiple io_uring instances (one per worker) waste kernel resources and still have small batch sizes.

### 13.3 Go's M:N Scheduler Efficiency

Go's runtime already handles the "goroutine blocked on syscall" case efficiently. When a goroutine blocks on `pread`, the Go scheduler parks it and runs another goroutine. This gives the same benefit that io_uring's asynchronous I/O would provide (CPU does useful work while waiting for I/O), but without the complexity of managing completion queues.

### 13.4 CQE Ordering

io_uring CQEs (completion queue entries) complete out of order. Tracking which completion corresponds to which original operation requires careful UserData management. This adds complexity that is not justified by the performance results.

**Bottom line**: io_uring shines for high-QPS network servers (thousands of concurrent connections), not for file I/O workloads that are already well-parallelized with a goroutine-per-file model.

---

## 14. Summary of Synchronization Primitives

| Primitive | Location | Purpose |
|-----------|----------|---------|
| `sync.Mutex` + `sync.Cond` | `walker.parallelWalker` | Shared work queue with dynamic sizing and precise termination detection |
| `sync.WaitGroup` | `walker.Walk` (inner goroutine) | Wait for all walker goroutines to finish before closing `fileCh` |
| `sync.WaitGroup` | `scheduler.Run` | Wait for all worker goroutines to finish before closing `resultCh` |
| `atomic.Int64` | `scheduler.Run` (seq counter) | Lock-free monotonic sequence number assignment |
| `atomic.Int32` | `walker.noatimeWorks`, `input.noatimeWorks` | Lock-free, eventually-consistent O_NOATIME flag |
| `atomic.Bool` | `runRecursive` (hasMatch) | Lock-free match-found flag, read after pipeline completes |
| `sync.Pool` | `input.bufPool` | Concurrent-safe reuse of file read buffers across workers |
| Buffered channels | `fileCh`, `resultCh`, `errCh` | Inter-stage communication with backpressure |
| `map[int]Result` | `OrderedWriter.WriteOrdered` | Single-goroutine reorder buffer (no synchronization needed) |

### Primitives NOT Used (and Why)

| Primitive | Why Not |
|-----------|---------|
| `sync.RWMutex` | No read-heavy shared state. The walker's queue has mixed read-write access. |
| `context.Context` | No cancellation needed. Pipeline runs to completion. |
| `select` | No multi-channel reads. Each goroutine reads from one channel. |
| `sync.Once` | No lazy initialization needed. All resources are created upfront. |
| `sync.Map` | No concurrent map access. The `pending` map in OrderedWriter is single-goroutine. |
| `chan struct{}` (done channels) | Channel closure is used instead for shutdown signaling. |
| `runtime.LockOSThread` | No need to pin goroutines to OS threads. Go's scheduler handles this. |

---

## 15. Cross-References

- **`02-linux-syscalls.md`**: Details on `getdents64`, `openat`, `O_NOATIME`, `pread`, `mmap`, `writev`, and other Linux syscalls used in the pipeline.
- **`03-memory-management.md`**: How file data buffers (mmap and pooled) flow through the pipeline, and the `Closer` ownership protocol.
- **`07-benchmarking-and-profiling.md`**: How to profile concurrent code with `pprof`, including goroutine profiles, mutex contention profiles, and block profiles. Techniques for identifying bottleneck stages in the pipeline.
