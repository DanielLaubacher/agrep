# Linux Syscalls and the Direct Kernel Interface

This document explains how agrep bypasses Go's standard library to interact
directly with the Linux kernel through system calls. Every decision here is
motivated by measurable performance: fewer allocations, fewer syscalls, less
latency. The goal is to understand *why* each syscall is used, *how* the kernel
implements it, and *what* makes it faster than the portable alternative.

---

## Table of Contents

1. [Background: What Is a System Call?](#background-what-is-a-system-call)
2. [Why Direct Syscalls Instead of Go's os Package](#why-direct-syscalls-instead-of-gos-os-package)
3. [getdents64 -- Raw Directory Listing](#getdents64--raw-directory-listing)
4. [O_NOATIME -- Avoiding Inode Writes](#o_noatime--avoiding-inode-writes)
5. [openat and O_DIRECTORY -- Directory Opens](#openat-and-o_directory--directory-opens)
6. [fstat -- Stat Without Path Resolution](#fstat--stat-without-path-resolution)
7. [pread -- Seekless I/O](#pread--seekless-io)
8. [mmap -- Memory-Mapped File Reading](#mmap--memory-mapped-file-reading)
9. [fadvise and madvise -- Kernel Page Cache Hints](#fadvise-and-madvise--kernel-page-cache-hints)
10. [writev -- Scatter-Gather Output](#writev--scatter-gather-output)
11. [inotify + epoll -- Raw File Watching](#inotify--epoll--raw-file-watching)
12. [io_uring -- When "Faster" Syscalls Are Actually Slower](#io_uring--when-faster-syscalls-are-actually-slower)
13. [Syscall Overhead Accounting](#syscall-overhead-accounting)
14. [Cross-references](#cross-references)

---

## Background: What Is a System Call?

A system call (syscall) is the boundary between user-space code and the Linux
kernel. When your program needs to interact with hardware -- opening a file,
reading bytes from disk, writing to a terminal -- it cannot do so directly.
User-space processes run in a restricted CPU mode (ring 3 on x86-64) that
cannot access hardware or kernel memory. The only way to request kernel services
is through a syscall.

On x86-64 Linux, a syscall works like this:

1. The program places the syscall number in the `rax` register.
2. Arguments go into `rdi`, `rsi`, `rdx`, `r10`, `r8`, `r9` (up to 6 args).
3. The `syscall` instruction triggers a mode switch from ring 3 to ring 0.
4. The kernel's syscall handler runs, performs the requested operation, and
   places the return value in `rax`.
5. The `sysret` instruction returns to user space.

This ring transition is not free. On modern x86-64 hardware, a minimal syscall
(like `getpid`, which just returns a cached value) takes roughly 50-100
nanoseconds. A syscall that actually does I/O (like `read`) takes longer
because the kernel must access page cache, file system metadata, or even
physical disk.

**The fundamental principle**: every syscall has fixed overhead from the ring
transition. Reducing the total number of syscalls (by batching work or
eliminating redundant calls) directly improves performance.

In Go, the `golang.org/x/sys/unix` package provides thin wrappers around raw
Linux syscalls. These wrappers do little more than load registers and execute
the `syscall` instruction. This is in contrast to the `os` package, which adds
layers of abstraction on top.

---

## Why Direct Syscalls Instead of Go's os Package

Go's `os` package is designed for correctness, safety, and cross-platform
compatibility. These are excellent properties for most programs, but they come
at a measurable cost for a performance-critical tool that processes hundreds of
thousands of files.

### The Cost of Abstraction

Consider what happens when you open and read a file through the `os` package:

```go
// The "normal" Go way
f, err := os.Open(path)        // 1. Allocates *os.File on heap
                                // 2. Calls unix.Open internally
                                // 3. Sets up a finalizer for GC-based cleanup
defer f.Close()                 // 4. Registered for deferred execution

info, err := f.Stat()           // 5. Calls unix.Fstat internally
                                // 6. Allocates os.FileInfo (interface) on heap
                                // 7. Allocates the underlying fileStat struct

size := info.Size()             // 8. Virtual method call through interface

buf := make([]byte, size)       // 9. Allocates read buffer
n, err := f.Read(buf)           // 10. Calls unix.Read internally
                                // 11. Updates f's internal offset
                                // 12. Checks for io.EOF translation
```

Now compare agrep's approach using direct syscalls:

```go
// agrep's approach (from internal/input/mmap.go and internal/input/buffered.go)
fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOATIME, 0)  // 1. Returns raw int fd
                                                               // 2. No heap allocation
                                                               // 3. No finalizer

var stat unix.Stat_t                                           // 4. Stack-allocated struct
err = unix.Fstat(fd, &stat)                                    // 5. Fills struct in place
                                                               // 6. No interface, no heap

size := stat.Size                                              // 7. Direct field access

buf := pooledBuffer[:size]                                     // 8. Reused from sync.Pool
n, err := unix.Pread(fd, buf, 0)                               // 9. Single syscall
                                                               // 10. No offset tracking

unix.Close(fd)                                                 // 11. Immediate cleanup
```

### Specific Overhead Sources in the os Package

**1. Heap allocation for os.File**

Every `os.Open` call allocates an `os.File` struct on the heap. This struct
contains an internal `file` struct with a file descriptor, a name string, a
directory info pointer, and a finalizer. For agrep searching 37,000+ files in
a typical recursive scan, that is 37,000 heap allocations just for file handles
-- objects that live for microseconds before being closed.

**2. Finalizer registration**

`os.NewFile` registers a runtime finalizer via `runtime.SetFinalizer`. This
tells the garbage collector to call `f.close()` if the programmer forgot to.
Finalizers add pressure to the GC: the runtime must track them, defer their
execution, and run them in a dedicated goroutine. For short-lived files in a
tight loop, this is pure overhead.

**3. os.FileInfo interface allocation**

`f.Stat()` returns an `os.FileInfo`, which is an interface. In Go, returning an
interface from a function causes the concrete value to escape to the heap (the
compiler cannot prove the interface value will not outlive the stack frame). The
underlying `fileStat` struct is allocated every time. In contrast,
`unix.Fstat(fd, &stat)` fills a stack-allocated `unix.Stat_t` in place -- zero
allocations.

**4. Path string conversion**

When you pass a Go string to a syscall, it must be converted to a
null-terminated C string (the kernel expects C strings). The `os` package does
this internally via `syscall.BytePtrFromString`, which allocates a byte slice,
copies the string, and appends a null byte. `unix.Open` must do this too, but
at least we avoid the double layer. And `unix.Fstat(fd, &stat)` avoids path
resolution entirely -- it operates on the already-open file descriptor.

**5. Error wrapping**

`os.Open` wraps raw errno values into `*os.PathError` structs, which are
heap-allocated and contain the operation name, path string, and underlying
error. These wrappers are useful for debugging but add allocation overhead.
`unix.Open` returns the raw `syscall.Errno` value (an integer), which is
zero-allocation.

### Quantifying the Difference

In agrep's end-to-end benchmark (37K files, `--no-ignore --hidden -l`), the
total syscall count is approximately 672,000: for each file, that is
`open + fstat + pread + close`, plus `getdents` calls for each directory. At
this scale, saving even one allocation per file adds up to tens of thousands of
avoided GC objects.

---

## getdents64 -- Raw Directory Listing

This is the single biggest performance win from using direct syscalls. Directory
listing is the critical path for any recursive file search tool, and the
standard library approach is surprisingly expensive.

### What os.ReadDir Does

When you call `os.ReadDir(path)`, here is what happens internally:

```
os.ReadDir(path)
  -> os.Open(path)             -- allocates *os.File, registers finalizer
  -> f.ReadDir(-1)             -- reads all entries
    -> f.readdir(-1)
      -> syscall.Getdents(fd, buf) -- the actual kernel call (one or more times)
      -> for each raw dirent:
        -> allocates os.DirEntry   -- heap allocation per entry
        -> string(name bytes)      -- string allocation per entry
        -> if type == DT_UNKNOWN:
          -> os.Lstat(fullpath)    -- additional syscall + allocation
```

For a directory with 1,000 files, that is at minimum 1,000 heap allocations
for `os.DirEntry` objects, plus 1,000 string allocations for filenames, plus
the `*os.File` allocation for the directory handle. And if you need the file
type (regular file vs. directory vs. symlink), `os.DirEntry.Type()` may trigger
an additional `Lstat` call per entry.

### What agrep Does

agrep calls `unix.Getdents` directly and parses the raw kernel response.

**Source: `internal/walker/walker.go`, lines 173-184**

```go
// worker processes directories from the work queue until all work is done.
func (pw *parallelWalker) worker() {
    buf := make([]byte, 32*1024) // per-worker getdents buffer
    var dirents []Dirent          // per-worker reusable dirent slice
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

The 32KB buffer is allocated once per worker goroutine and reused for every
directory that worker processes. The `dirents` slice is similarly reused. In
the `processDir` method, the inner loop calls `unix.Getdents` directly:

**Source: `internal/walker/walker.go`, lines 199-209**

```go
for {
    n, err := unix.Getdents(fd, buf)
    if err != nil {
        pw.errCh <- &WalkError{Path: item.path, Err: err}
        break
    }
    if n == 0 {
        break
    }

    dirents = ParseDirents(buf, n, dirents)
    // ... process entries
}
```

### The linux_dirent64 Kernel Structure

`getdents64` (syscall number 217 on x86-64) fills the user-space buffer with
a packed array of `linux_dirent64` structs. The kernel defines this in
`include/linux/dirent.h`:

```c
struct linux_dirent64 {
    ino64_t        d_ino;    /* 8 bytes: 64-bit inode number */
    off64_t        d_off;    /* 8 bytes: offset to next dirent (for seekdir) */
    unsigned short d_reclen; /* 2 bytes: total size of this record */
    unsigned char  d_type;   /* 1 byte: file type (DT_REG, DT_DIR, etc.) */
    char           d_name[]; /* variable: null-terminated filename */
};
```

The struct is **variable-length** because `d_name` has no fixed size. The
`d_reclen` field tells you the total size of the record, including any padding
the kernel adds for alignment. This is why you cannot simply cast the buffer
to an array of fixed-size structs -- you must walk the buffer manually using
`d_reclen` as the stride.

Layout in memory (for a file named "hello.txt"):

```
Offset  Field       Bytes   Value
0       d_ino       8       inode number
8       d_off       8       directory stream offset
16      d_reclen    2       32 (padded to alignment)
18      d_type      1       8 (DT_REG)
19      d_name      10      "hello.txt\0"
29      (padding)   3       (to reach reclen=32)
```

### Parsing the Raw Buffer

**Source: `internal/walker/dirent.go`**

```go
// File type constants from dirent.h
const (
    DT_UNKNOWN = 0
    DT_FIFO    = 1
    DT_CHR     = 2
    DT_DIR     = 4
    DT_BLK     = 6
    DT_REG     = 8
    DT_LNK     = 10
    DT_SOCK    = 12
)

// Dirent represents a parsed Linux directory entry.
type Dirent struct {
    Name string
    Type uint8
}

// ParseDirents parses raw getdents64 output into Dirent structs.
// buf must contain the raw bytes returned by unix.Getdents.
// dst is reused to avoid per-call slice allocation; pass nil on first call.
func ParseDirents(buf []byte, n int, dst []Dirent) []Dirent {
    entries := dst[:0]
    offset := 0

    for offset < n {
        // Ensure we have at least the fixed header (19 bytes minimum)
        if offset+19 > n {
            break
        }

        // Parse fields from the raw buffer (skip d_ino at offset+0, d_off at offset+8)
        reclen := *(*uint16)(unsafe.Pointer(&buf[offset+16]))
        dtype := buf[offset+18]

        if reclen == 0 {
            break // prevent infinite loop
        }

        // d_name starts at offset+19, null-terminated
        nameStart := offset + 19
        nameEnd := offset + int(reclen)
        if nameEnd > n {
            nameEnd = n
        }

        // Find the null terminator
        nameBytes := buf[nameStart:nameEnd]
        nameLen := 0
        for nameLen < len(nameBytes) && nameBytes[nameLen] != 0 {
            nameLen++
        }
        name := string(nameBytes[:nameLen])

        // Skip . and ..
        if name != "." && name != ".." {
            entries = append(entries, Dirent{
                Name: name,
                Type: dtype,
            })
        }

        offset += int(reclen)
    }

    return entries
}
```

There are several important details here:

**`unsafe.Pointer` for field access**: The `d_reclen` field is at offset 16 in
the struct. Rather than doing bit-shifting with `buf[offset+16] | buf[offset+17]<<8`
(which would also work and be safe), the code casts directly to `*uint16` via
`unsafe.Pointer`. This is safe because the buffer is properly aligned (it is a
`[]byte` from `make`, and x86-64 allows unaligned reads), and it generates a
single `MOVZX` instruction instead of two loads and a shift.

**The `d_type` field**: This is the key advantage of raw `getdents64`. The
kernel provides the file type (regular file, directory, symlink, etc.) as a
single byte in each dirent entry. This means agrep can distinguish files from
directories without making a separate `stat` or `lstat` syscall per entry. For
a directory with 1,000 files, that eliminates 1,000 syscalls.

**`DT_UNKNOWN` fallback**: Some filesystems (notably NFS, and some older
filesystems) do not populate `d_type`, returning `DT_UNKNOWN` instead. In this
case, agrep falls back to `unix.Stat` to determine the file type:

```go
case DT_UNKNOWN:
    var stat unix.Stat_t
    if err := unix.Stat(fullPath, &stat); err != nil {
        pw.errCh <- &WalkError{Path: fullPath, Err: err}
        continue
    }
    mode := stat.Mode & unix.S_IFMT
    if mode == unix.S_IFREG {
        // ... handle regular file
    } else if mode == unix.S_IFDIR {
        // ... handle directory
    }
```

On modern local filesystems like ext4, btrfs, and XFS, `d_type` is always
populated. The `DT_UNKNOWN` path is rarely taken on a typical developer
workstation.

**Slice reuse with `dst[:0]`**: The `dst` parameter is the dirent slice from
the previous call. Setting `entries := dst[:0]` resets the length to zero
while preserving the backing array. This means the slice memory is allocated
once per worker and reused for every directory. Only the filename strings
(which must be copied out of the buffer before the next `Getdents` call
overwrites it) require per-entry allocation.

**Buffer sizing**: The 32KB buffer was chosen empirically. It is large enough
to hold hundreds of directory entries per `getdents64` call (reducing the
number of syscalls for large directories) but small enough that each worker's
buffer does not waste memory. The kernel will fill as many entries as fit in
the buffer per call, so a larger buffer means fewer round-trips to the kernel.

### Allocation Comparison

| Operation | os.ReadDir (per directory of N files) | agrep (per directory of N files) |
|---|---|---|
| Directory handle | 1 `*os.File` + finalizer (heap) | 1 `int` fd (stack) |
| Getdents buffer | Internal, reallocated | 1 per worker, reused forever |
| Per-entry struct | N `os.DirEntry` (heap) | 0 (reused slice) |
| Per-entry name | N strings (heap) | N strings (heap, unavoidable) |
| Type information | 0-N `Lstat` calls | 0 (from `d_type`, except DT_UNKNOWN) |

The only unavoidable per-entry allocation is the filename string, since the
raw bytes in the getdents buffer will be overwritten on the next call. agrep
uses `string(nameBytes[:nameLen])` which copies the bytes into a new string
allocation. This is the theoretical minimum for any approach that needs to
pass filenames to other goroutines.

---

## O_NOATIME -- Avoiding Inode Writes

### The Atime Problem

Every time a file is read on Linux, the kernel updates the file's **access time**
(atime) in its inode metadata. This means that a pure read operation triggers a
write to the filesystem. For a grep tool scanning 37,000 files, that is 37,000
unnecessary inode writes.

The impact of these writes:

- **Disk I/O**: Each atime update dirties the inode, which must eventually be
  flushed to disk. Even with modern filesystems that batch inode updates, this
  generates write I/O that competes with the read workload.
- **SSD wear**: On solid-state drives, every write contributes to flash cell wear.
  Atime updates for read-only operations are pure waste.
- **Inode lock contention**: In parallel workloads, multiple threads updating
  inodes can create lock contention in the filesystem layer.
- **Journal overhead**: On journaling filesystems like ext4, atime updates must
  also be journaled, doubling the write amplification.

Most Linux systems mitigate this with the `relatime` mount option (the default
since kernel 2.6.30), which only updates atime when it is older than mtime or
once per day. But even `relatime` still triggers atime writes for files that
have not been read today, and the first access of each file after midnight will
still cause an inode write.

### The O_NOATIME Flag

`O_NOATIME` (value `0x40000`, defined in `<fcntl.h>`) is a Linux-specific flag
for `open(2)` that instructs the kernel to skip atime updates entirely for all
subsequent reads through this file descriptor. It eliminates the problem at the
source.

However, there is a security restriction: `O_NOATIME` requires that the process
either **owns the file** (the effective UID matches the file's UID) or has the
`CAP_FOWNER` capability. Without this, `open(..., O_NOATIME)` fails with
`EPERM`.

This means:
- When searching your own home directory, `O_NOATIME` works for all your files.
- When searching `/usr/lib` or `/etc` (files owned by root), `O_NOATIME` fails
  with `EPERM` for every file.

### agrep's Atomic Fallback Caching

agrep implements a "try once, cache the result" strategy using `atomic.Int32`.
This pattern appears in two places because the input and walker packages are
independent (they do not share state by design).

**Source: `internal/input/mmap.go`, lines 106-124**

```go
// noatimeWorks tracks whether O_NOATIME is usable (requires file ownership or CAP_FOWNER).
// Starts as 1 (try it); set to 0 after the first EPERM, avoiding repeated failed syscalls.
var noatimeWorks atomic.Int32

func init() { noatimeWorks.Store(1) }

// openFile opens a file with O_NOATIME, falling back without it.
// After the first EPERM, all subsequent opens skip O_NOATIME entirely.
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

**Source: `internal/walker/walker.go`, lines 14-33**

```go
// noatimeWorks tracks whether O_NOATIME is usable for directory opens.
// Starts as 1 (try it); set to 0 after the first EPERM.
var noatimeWorks atomic.Int32

func init() { noatimeWorks.Store(1) }

// openDir opens a directory with O_NOATIME, falling back without it.
func openDir(path string) (int, error) {
    flags := unix.O_RDONLY | unix.O_DIRECTORY
    if noatimeWorks.Load() != 0 {
        fd, err := unix.Open(path, flags|unix.O_NOATIME, 0)
        if err == nil {
            return fd, nil
        }
        if err == unix.EPERM {
            noatimeWorks.Store(0)
        }
    }
    return unix.Open(path, flags, 0)
}
```

### Why Atomic? Why Not a Regular Bool?

Multiple goroutines call `openFile` and `openDir` concurrently (the walker has
`runtime.NumCPU()` workers, the scheduler has `NumCPU*2` workers). A regular
`bool` would create a data race. `atomic.Int32` provides lock-free concurrent
access:

- `Load()` is a single atomic read instruction (`MOVL` on x86-64).
- `Store(0)` is a single atomic write. Multiple goroutines may write 0
  simultaneously -- that is fine, since they all write the same value.

There is no lock, no mutex, no CAS loop. The fast path (after the first EPERM)
is a single atomic load that evaluates to false, skipping the failed open
entirely.

### The Strace Evidence

Before implementing this caching, a strace analysis of searching `/usr/lib`
(162,000 files, mostly owned by root) revealed:

```
% time     seconds  usecs/call     calls    errors syscall
------ ----------- ----------- --------- --------- ----------------
 26.87    0.267     1           162000    162000 open (EPERM attempts)
 25.13    0.250     1           162000           open (successful retries)
```

That is 162,000 wasted syscalls -- open attempts that fail with `EPERM`, each
followed by a retry without `O_NOATIME`. The atomic caching reduces this to a
single failed attempt, eliminating 161,999 wasted syscalls.

### Why Two Separate Variables?

The `input` and `walker` packages each have their own `noatimeWorks` variable.
This is intentional for two reasons:

1. **Package independence**: The packages do not import each other and share no
   state. This is a design principle of agrep -- pure dependency injection,
   no global mutable state crossing package boundaries.

2. **Different file types**: The walker opens directories; the input package
   opens regular files. While `O_NOATIME` permission depends on file ownership
   (not file type), they execute at different times and one could conceivably
   fail while the other succeeds (e.g., if the user owns the directory but not
   the files inside it, though this is unusual).

---

## openat and O_DIRECTORY -- Directory Opens

The walker's `openDir` function uses `unix.Open` with the `O_DIRECTORY` flag:

```go
flags := unix.O_RDONLY | unix.O_DIRECTORY
```

`O_DIRECTORY` tells the kernel to fail with `ENOTDIR` if the path is not a
directory. This is a defensive measure: without it, if a race condition causes
a directory to be replaced with a regular file between the time the walker
discovers it and the time it tries to open it, the `Getdents` call would fail
with a confusing error. With `O_DIRECTORY`, the failure happens at open time
with a clear error.

Note: agrep currently uses `unix.Open` (which maps to the `open` syscall on
amd64) rather than `unix.Openat` (which maps to `openat`). The `openat` syscall
takes an additional `dirfd` parameter for relative path resolution. agrep uses
absolute paths throughout (constructed by `joinPath`), so `openat` with
`AT_FDCWD` would be functionally equivalent. However, `openat` is the foundation
of the io_uring integration (see the [io_uring section](#io_uring--when-faster-syscalls-are-actually-slower)),
where `PrepOpenat` uses `AT_FDCWD` as the directory file descriptor.

---

## fstat -- Stat Without Path Resolution

A recurring pattern in agrep is: open a file, then immediately stat it using
the file descriptor rather than the path.

**Source: `internal/input/mmap.go`, lines 47-65**

```go
func (r *MmapReader) Read(path string) (ReadResult, error) {
    fd, err := openFile(path)
    if err != nil {
        return ReadResult{}, fmt.Errorf("open %s: %w", path, err)
    }

    var stat unix.Stat_t
    if err := unix.Fstat(fd, &stat); err != nil {
        unix.Close(fd)
        return ReadResult{}, fmt.Errorf("stat %s: %w", path, err)
    }

    if stat.Size == 0 {
        unix.Close(fd)
        return ReadResult{Data: nil, Closer: noopCloser}, nil
    }

    return readMmap(fd, stat.Size, path)
}
```

### Why fstat Instead of stat?

`stat(path, &stat)` and `fstat(fd, &stat)` return the same information. The
difference is in path resolution:

**`stat(path, &stat)`:**
1. The kernel receives a path string pointer.
2. It must convert the user-space pointer to a kernel string
   (`copy_from_user`).
3. It walks the path components: for `/home/user/project/src/file.go`, that
   is resolving `/`, `home`, `user`, `project`, `src`, and `file.go` -- each
   component requires a directory lookup in the dentry cache.
4. At each component, it checks permissions.
5. Finally, it reads the inode metadata.

**`fstat(fd, &stat)`:**
1. The kernel receives an integer file descriptor.
2. It looks up the fd in the process's file descriptor table (an array index
   operation, O(1)).
3. It reads the inode metadata directly from the already-resolved `struct file`.

Since agrep has already opened the file (and thus already paid for path
resolution), calling `fstat` avoids repeating that work. The saving is small
per call (perhaps a few hundred nanoseconds), but across 37,000+ files it adds
up.

### Stack-Allocated Struct

The `unix.Stat_t` struct is declared as a local variable:

```go
var stat unix.Stat_t
```

This allocates the struct on the stack frame. It is 144 bytes on amd64 and
contains all the file metadata fields:

```go
type Stat_t struct {
    Dev     uint64
    Ino     uint64
    Nlink   uint64
    Mode    uint32
    Uid     uint32
    Gid     uint32
    _       int32
    Rdev    uint64
    Size    int64    // <-- this is what agrep reads
    Blksize int64
    Blocks  int64
    Atim    Timespec
    Mtim    Timespec
    Ctim    Timespec
    _       [3]int64
}
```

Compare this to `os.File.Stat()`, which returns an `os.FileInfo` interface:

```go
type FileInfo interface {
    Name() string
    Size() int64
    Mode() FileMode
    ModTime() time.Time
    IsDir() bool
    Sys() any
}
```

Returning an interface causes the concrete `fileStat` struct to escape to the
heap. The `Sys()` method returns `any` (another interface), which further
complicates escape analysis. The result: `os.File.Stat()` always allocates.

agrep only needs `stat.Size` (to know how many bytes to read) and `stat.Mode`
(in the walker's DT_UNKNOWN fallback path). Direct field access on a
stack-allocated struct is zero-cost.

---

## pread -- Seekless I/O

`pread(fd, buf, count, offset)` reads `count` bytes from file descriptor `fd`
at byte position `offset` into `buf`, without modifying the file descriptor's
current position.

### Why pread Instead of read?

The standard `read(fd, buf, count)` syscall has implicit state: it reads from
the file descriptor's current position and advances that position by the number
of bytes read. This means:

1. If you want to read from a specific offset, you need `lseek(fd, offset,
   SEEK_SET)` followed by `read(fd, buf, count)` -- that is two syscalls.
2. The file descriptor's position is shared state. If multiple threads read
   from the same fd, their reads interfere with each other (this is not a
   concern for agrep since each worker opens its own fd, but it is a general
   advantage of pread).

`pread` combines seek and read into a single atomic syscall and does not modify
the fd's position. This saves one syscall compared to `lseek + read`.

### agrep's pread Loop

**Source: `internal/input/buffered.go`, lines 51-76**

```go
// readBuffered reads a file from an already-open fd into a pooled buffer.
// Takes ownership of fd -- caller must not close it.
func readBuffered(fd int, size int64) (ReadResult, error) {
    // Get a pooled buffer and grow it to fit the file
    bp := bufPool.Get().(*[]byte)
    buf := *bp
    if cap(buf) < int(size) {
        buf = make([]byte, size)
    } else {
        buf = buf[:size]
    }

    // Read the entire file using pread (no seek state)
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

    unix.Close(fd)
    // ...
}
```

### The Retry Loop

The loop `for totalRead < int(size)` is necessary because `pread` (like all
POSIX read operations) is not guaranteed to return all requested bytes in a
single call. The kernel may return a **short read** for several reasons:

- The read crosses a page boundary in the page cache and the next page is not
  yet loaded (the kernel returns what it has rather than blocking).
- A signal interrupted the read (though `pread` will typically restart
  automatically on Linux, unlike `read`).
- The file is on a network filesystem and the server returned a partial
  response.
- The requested size exceeds the kernel's maximum single-read limit
  (typically 2GB on 64-bit systems, but can be lower).

For local files that fit in memory, the loop typically executes only once. But
correctness demands handling the partial-read case.

### pread in the Watch Module

The watch module also uses `pread` to read only newly appended content:

**Source: `internal/watch/watch.go`, lines 198-238**

```go
func (w *Watcher) ReadNew(path string) ([]byte, error) {
    fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOATIME, 0)
    if err != nil {
        fd, err = unix.Open(path, unix.O_RDONLY, 0)
        if err != nil {
            return nil, err
        }
    }
    defer unix.Close(fd)

    var stat unix.Stat_t
    if err := unix.Fstat(fd, &stat); err != nil {
        return nil, err
    }

    lastOffset := w.offsets[path]
    newSize := stat.Size

    if newSize <= lastOffset {
        // File was truncated or no new data
        if newSize < lastOffset {
            w.offsets[path] = 0
            lastOffset = 0
        } else {
            return nil, nil
        }
    }

    toRead := int(newSize - lastOffset)
    if toRead == 0 {
        return nil, nil
    }

    buf := make([]byte, toRead)
    n, err := unix.Pread(fd, buf, lastOffset)
    if err != nil {
        return nil, err
    }

    w.offsets[path] = lastOffset + int64(n)
    return buf[:n], nil
}
```

Here `pread` is essential: the watcher tracks how far into each file it has
already read (`w.offsets[path]`), and on each modification event, it reads
only the new bytes starting from that offset. `pread` does this in a single
syscall, and it handles the file-truncation case (log rotation) by detecting
when the file shrunk and resetting the offset to 0.

---

## mmap -- Memory-Mapped File Reading

Memory mapping creates a virtual memory region that is backed by a file on
disk. Instead of explicitly reading bytes with `read`/`pread`, the file's
contents appear directly in the process's address space. Accessing a byte
triggers a page fault, and the kernel's page fault handler loads the
corresponding 4KB page from disk (or page cache) into physical memory.

### How mmap Works

```
Process virtual address space:
    0x7f0000000000  +-----------------------+
                    |                       |
                    |  mmap'd file region   |  <- file contents accessible
                    |  (read-only pages)    |     as regular memory
                    |                       |
    0x7f0000100000  +-----------------------+

Accessing byte at offset 50000:
    1. CPU generates virtual address 0x7f000000C350
    2. MMU finds no page table entry -> page fault
    3. Kernel page fault handler:
       a. Identifies this is a file-backed mapping
       b. Computes file page: offset 50000 / 4096 = page 12
       c. Checks page cache for this file's page 12
       d. If not cached: reads 4KB from disk into page cache
       e. Maps the physical page into the process's page table
    4. CPU retries the memory access -> succeeds
    5. Subsequent accesses to the same 4KB page are instant (no fault)
```

### agrep's mmap Implementation

**Source: `internal/input/mmap.go`, lines 20-45**

```go
// readMmap memory-maps an already-opened fd of known size.
func readMmap(fd int, size int64, path string) (ReadResult, error) {
    // Hint kernel: sequential read pattern
    unix.Fadvise(fd, 0, size, unix.FADV_SEQUENTIAL)

    // Memory-map the file. FADV_SEQUENTIAL + MADV_SEQUENTIAL handle readahead;
    // we skip MAP_POPULATE so pages fault in on demand, enabling early exit
    // for -l/MatchExists without reading the entire file.
    data, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE)
    if err != nil {
        // Fall back to buffered read from the already-open fd
        return readBuffered(fd, size)
    }

    // Additional hint: sequential access pattern
    unix.Madvise(data, unix.MADV_SEQUENTIAL)

    return ReadResult{
        Data: data,
        Closer: func() error {
            unix.Madvise(data, unix.MADV_DONTNEED)
            syscall.Munmap(data)
            unix.Close(fd)
            return nil
        },
    }, nil
}
```

### mmap Flags Explained

**`PROT_READ`**: The mapping is read-only. Any attempt to write to it would
cause a `SIGSEGV`. This is all agrep needs -- it searches file contents but
never modifies them.

**`MAP_PRIVATE`**: Changes to the mapping are not written back to the file. For
a read-only mapping this is semantically identical to `MAP_SHARED`, but
`MAP_PRIVATE` has a subtle performance advantage: the kernel can use
copy-on-write optimizations and does not need to track dirty pages for
writeback. Some kernel code paths are simpler for private mappings.

**Why NOT MAP_POPULATE**: `MAP_POPULATE` tells the kernel to pre-fault all pages
at `mmap` time, loading the entire file into memory before returning. agrep
deliberately omits this flag. The reason is the `-l` (files-only) mode: when
agrep only needs to check whether a file contains *any* match, it calls
`MatchExists`, which may find a match in the first few kilobytes. With
`MAP_POPULATE`, the kernel would load the entire file (potentially megabytes)
before the search even starts. Without it, pages fault in on demand, and if the
match is found early, the remaining pages are never loaded.

### The Adaptive Reader

agrep does not always use mmap. The `adaptiveReader` chooses between buffered
pread and mmap based on file size:

**Source: `internal/input/mmap.go`, lines 67-103**

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
    // Single open, single fstat -- no redundant Stat(path) allocation
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

The default threshold is 8MB (configured in `cli.Config`). The rationale:

- **Small files (< 8MB)**: Buffered pread is faster because `mmap` has fixed
  overhead: the `mmap` syscall, page table setup, page faults, and `munmap`
  cleanup. For a 4KB file, a single `pread` completes in one syscall, while
  mmap requires `mmap` + page fault + `munmap` = more total kernel work.
- **Large files (>= 8MB)**: mmap wins because the kernel can load pages on
  demand via readahead, and the data goes directly from page cache to the
  process's address space without an intermediate kernel-to-user copy. With
  `pread`, the kernel must copy data from page cache into the user buffer.

Note how the adaptive reader avoids redundant syscalls: it calls `openFile`
once and `Fstat` once, then branches to either `readMmap` or `readBuffered`,
both of which take ownership of the already-open fd. There is no separate
"should I mmap?" stat call followed by a "now actually read" open call.

### mmap Fallback to Buffered Read

If `syscall.Mmap` fails (which can happen if the system's virtual memory
address space is fragmented or `vm.max_map_count` is exhausted), `readMmap`
falls back to `readBuffered`:

```go
data, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_PRIVATE)
if err != nil {
    // Fall back to buffered read from the already-open fd
    return readBuffered(fd, size)
}
```

This is a graceful degradation: the fd is already open and stat'd, so the
buffered reader can use it directly.

---

## fadvise and madvise -- Kernel Page Cache Hints

These syscalls do not read or write data. They are advisory hints that tell
the kernel about the application's access pattern so it can optimize its
internal behavior. The kernel is free to ignore them, but in practice they
have significant effects.

### fadvise -- File Descriptor Level Hints

`posix_fadvise(fd, offset, len, advice)` advises the kernel about expected
access patterns for a file descriptor.

**Source: `internal/input/mmap.go`, line 22**

```go
unix.Fadvise(fd, 0, size, unix.FADV_SEQUENTIAL)
```

**`FADV_SEQUENTIAL`** tells the kernel: "I will read this file sequentially
from beginning to end." The kernel responds by:

1. **Doubling the readahead window**: The default readahead is typically 128KB
   (32 pages). With `FADV_SEQUENTIAL`, the kernel increases this to 256KB or
   more. This means when you read page N, the kernel speculatively loads pages
   N+1 through N+64 in the background, so they are already in page cache when
   you need them.
2. **Aggressive page reclaim**: Pages behind the read position can be reclaimed
   sooner, since sequential access implies they will not be re-read.

For a grep workload that scans every byte of every file from start to finish,
this is the ideal access pattern hint.

Other `fadvise` values (not used by agrep but worth knowing):

- `FADV_RANDOM`: Disable readahead entirely (for database-style random access).
- `FADV_WILLNEED`: Start loading specified range into page cache now (async
  prefetch).
- `FADV_DONTNEED`: Evict specified range from page cache (useful after
  processing).
- `FADV_NOREUSE`: Hint that data will be accessed only once (kernel may evict
  sooner).

### madvise -- Memory Mapping Level Hints

`madvise(addr, length, advice)` is similar to `fadvise` but operates on memory
mappings rather than file descriptors.

**Source: `internal/input/mmap.go`, line 34**

```go
unix.Madvise(data, unix.MADV_SEQUENTIAL)
```

**`MADV_SEQUENTIAL`** has the same semantic meaning as `FADV_SEQUENTIAL` but
operates on the page fault handler rather than the read syscall handler. When
a page fault occurs in this mapping, the kernel knows to readahead aggressively
and to free pages behind the fault address.

**Source: `internal/input/mmap.go`, line 39 (in the Closer)**

```go
unix.Madvise(data, unix.MADV_DONTNEED)
```

**`MADV_DONTNEED`** tells the kernel: "I am done with these pages. You can free
them immediately." Without this hint, the pages would remain in the page cache
until memory pressure forces eviction (the kernel's LRU algorithm). For a grep
tool scanning thousands of files, leaving every scanned file in page cache
would eventually push out more useful cached data (like your IDE's files, your
browser cache, etc.).

The sequence `MADV_DONTNEED` -> `munmap` -> `close` in the Closer ensures
prompt page release. The order matters slightly: `MADV_DONTNEED` marks pages
for immediate reclaim before `munmap` removes the virtual mapping.

### The Combined Effect

For a file read via mmap, the hint sequence is:

```
1. fadvise(fd, FADV_SEQUENTIAL)    -- readahead hint before mapping
2. mmap(fd, PROT_READ, MAP_PRIVATE) -- create virtual mapping
3. madvise(data, MADV_SEQUENTIAL)   -- readahead hint for page faults
4. ... search the file ...          -- page faults trigger aggressive readahead
5. madvise(data, MADV_DONTNEED)     -- release pages
6. munmap(data)                     -- remove mapping
7. close(fd)                        -- release file descriptor
```

Both `fadvise` and `madvise` are set because they influence different kernel
subsystems. `fadvise` affects the VFS-level readahead (which runs asynchronously
in kernel threads), while `madvise` affects the VM-level page fault handler
(which runs synchronously in the faulting thread's context). Together, they
ensure that no matter which code path triggers the I/O, the kernel knows to
optimize for sequential access.

---

## writev -- Scatter-Gather Output

`writev(fd, iovec[], count)` writes multiple buffers to a file descriptor in a
single syscall. Each buffer is described by an `iovec` struct containing a
pointer and length:

```c
struct iovec {
    void  *iov_base;  /* Starting address */
    size_t iov_len;   /* Number of bytes */
};
```

### agrep's writev Writer

**Source: `internal/output/writer.go`**

```go
// Writer writes formatted output to stdout, using writev for scatter-gather I/O.
type Writer struct {
    fd int
}

// NewWriter creates a Writer that writes to stdout.
func NewWriter() *Writer {
    return &Writer{fd: int(os.Stdout.Fd())}
}

// Write writes the given bytes to stdout using writev for scatter-gather I/O.
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

### Why writev Instead of write?

In the current implementation, writev is called with a single iovec (one
buffer). This might seem pointless -- why not use `write`? Two reasons:

**1. Partial write handling**: Both `write` and `writev` can return short writes
(writing fewer bytes than requested). The retry loop `data = data[n:]` handles
this. While `write` could do the same, `writev` provides the infrastructure
for future optimization.

**2. Future scatter-gather potential**: Each search result consists of multiple
logical components: filename prefix, line number, separator, match content,
newline. Currently, the formatter concatenates these into a single buffer
before writing. With writev, these could be written as separate iovecs:

```go
// Hypothetical future optimization:
iovs := [][]byte{
    filename,       // "/home/user/file.go"
    []byte(":"),    // separator
    lineNum,        // "42"
    []byte(":"),    // separator
    matchedLine,    // "func main() {"
    []byte("\n"),   // newline
}
unix.Writev(w.fd, iovs)
```

This would eliminate the string concatenation/buffer copy step entirely. The
matched line data could be written directly from the mmap'd file buffer without
ever being copied into an intermediate buffer.

### The Partial Write Problem

Why does `writev` (or `write`) not always write all requested bytes? Several
scenarios:

- **Pipe buffer full**: If stdout is piped to another program (e.g.,
  `agrep pattern | head -20`), the pipe has a finite buffer (typically 64KB
  on Linux, configurable via `F_SETPIPE_SZ`). When the pipe buffer fills, the
  write blocks until the reader consumes some data. If a signal interrupts the
  blocked write, it returns with a short write.
- **Non-blocking fd**: If the fd is set to non-blocking mode, writes return
  immediately with however many bytes fit.
- **Disk full**: When writing to a file, the filesystem may accept partial data.
- **Signal interruption**: A signal delivered during the write can cause it to
  return early (though Linux typically restarts writes automatically for regular
  files).

The retry loop ensures all bytes are eventually written regardless of these
conditions.

### The Ordered Writer

Output ordering is critical for deterministic results. When multiple workers
process files in parallel, results arrive out of order. The `OrderedWriter`
buffers out-of-order results and emits them in sequence:

**Source: `internal/output/writer.go`, lines 56-85**

```go
func (ow *OrderedWriter) WriteOrdered(results <-chan Result, onMatch func()) {
    nextSeq := 1
    pending := make(map[int]Result)
    var buf []byte // reused across all writeResult calls

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
            pending[r.SeqNum] = r
        }
    }
}
```

The `buf` byte slice is reused across all writes (`buf = ow.writeResult(buf, r)`
returns the same backing array). This means the format buffer is allocated once
and grows as needed, with no per-file allocation.

---

## inotify + epoll -- Raw File Watching

agrep's `--watch` mode monitors files for changes and searches new content in
real time. It uses two Linux-specific subsystems: **inotify** for filesystem
event notification and **epoll** for efficient I/O multiplexing.

### Why Not fsnotify?

The Go ecosystem has the `fsnotify` package, which provides cross-platform file
watching. agrep avoids it for the same reasons it avoids the `os` package:

- `fsnotify` allocates an `Event` struct per notification (heap).
- It runs background goroutines with channels.
- It normalizes events across platforms, adding overhead for features agrep
  does not need (Windows `ReadDirectoryChangesW`, macOS `kqueue`).
- It does not expose the raw inotify event buffer for zero-copy parsing.

### inotify: Filesystem Event Notification

`inotify` is a Linux kernel subsystem (introduced in kernel 2.6.13) that
allows a process to subscribe to filesystem events. The API has three syscalls:

1. **`inotify_init1(flags)`**: Create an inotify instance, returning a file
   descriptor.
2. **`inotify_add_watch(ifd, path, mask)`**: Subscribe to events on a path,
   returning a watch descriptor.
3. **`read(ifd, buf, len)`**: Read events from the inotify fd (blocking or
   non-blocking depending on flags).

**Source: `internal/watch/watch.go`, lines 38-68**

```go
func New() (*Watcher, error) {
    ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
    if err != nil {
        return nil, fmt.Errorf("inotify_init1: %w", err)
    }

    efd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
    if err != nil {
        unix.Close(ifd)
        return nil, fmt.Errorf("epoll_create1: %w", err)
    }

    // Register inotify fd with epoll
    event := unix.EpollEvent{
        Events: unix.EPOLLIN,
        Fd:     int32(ifd),
    }
    if err := unix.EpollCtl(efd, unix.EPOLL_CTL_ADD, ifd, &event); err != nil {
        unix.Close(efd)
        unix.Close(ifd)
        return nil, fmt.Errorf("epoll_ctl: %w", err)
    }

    return &Watcher{
        inotifyFd: ifd,
        epollFd:   efd,
        watches:   make(map[int]string),
        offsets:   make(map[string]int64),
        done:      make(chan struct{}),
    }, nil
}
```

### inotify_init1 Flags

**`IN_CLOEXEC`**: Sets the close-on-exec flag on the inotify file descriptor.
If agrep ever spawns a child process via `exec`, this fd is automatically
closed. Without it, the child would inherit the fd, and the inotify watches
would remain active in the child process, potentially causing resource leaks
and unexpected behavior.

**`IN_NONBLOCK`**: Makes `read()` on the inotify fd non-blocking. Instead of
blocking when no events are available, `read` returns immediately with
`EAGAIN`. This is essential for use with epoll -- the event loop needs to
check for shutdown (`w.done` channel) between event reads.

### Adding Watches

**Source: `internal/watch/watch.go`, lines 72-94**

```go
func (w *Watcher) Add(path string) error {
    absPath, err := filepath.Abs(path)
    if err != nil {
        return err
    }

    mask := uint32(unix.IN_MODIFY | unix.IN_CREATE | unix.IN_MOVED_TO |
        unix.IN_MOVE_SELF | unix.IN_DELETE_SELF)

    wd, err := unix.InotifyAddWatch(w.inotifyFd, absPath, mask)
    if err != nil {
        return fmt.Errorf("inotify_add_watch %s: %w", absPath, err)
    }

    w.watches[wd] = absPath

    // Initialize offset for files
    info, err := os.Stat(absPath)
    if err == nil && !info.IsDir() {
        w.offsets[absPath] = info.Size()
    }

    return nil
}
```

The event mask specifies which filesystem events to subscribe to:

| Flag | Meaning | Use Case in agrep |
|---|---|---|
| `IN_MODIFY` | File content was modified | Trigger re-search on changed files |
| `IN_CREATE` | File was created in watched dir | Auto-watch newly created files |
| `IN_MOVED_TO` | File was moved into watched dir | Handle `mv` operations |
| `IN_MOVE_SELF` | Watched file/dir was itself moved | Detect log rotation |
| `IN_DELETE_SELF` | Watched file/dir was deleted | Notify user, clean up watch |

The `w.offsets[absPath] = info.Size()` initialization sets the "already read"
offset to the current file size. This means when watch mode starts, it does not
re-search existing content -- it only searches new bytes appended after the
watch began. This is the behavior you want for `tail -f`-style watching.

### epoll: Efficient I/O Multiplexing

epoll is Linux's scalable I/O event notification mechanism. It replaces the
older `select(2)` and `poll(2)` syscalls, which have O(n) performance in the
number of monitored file descriptors.

In agrep's case, epoll monitors exactly one fd (the inotify fd). This might
seem like overkill -- why not just call `read` on the inotify fd directly?
The reason is the shutdown mechanism: the event loop must check `w.done`
between reads. Without epoll, a blocking `read` would hang forever if no
events arrive and the user presses Ctrl+C. With epoll's timeout parameter,
the loop wakes up every 100ms to check for shutdown:

**Source: `internal/watch/watch.go`, lines 97-138**

```go
func (w *Watcher) Events() <-chan Event {
    ch := make(chan Event, 64)
    go func() {
        defer close(ch)
        buf := make([]byte, 4096)
        events := make([]unix.EpollEvent, 1)

        for {
            select {
            case <-w.done:
                return
            default:
            }

            // Wait for events with 100ms timeout
            n, err := unix.EpollWait(w.epollFd, events, 100)
            if err != nil {
                if err == unix.EINTR {
                    continue
                }
                ch <- Event{Err: fmt.Errorf("epoll_wait: %w", err)}
                return
            }
            if n == 0 {
                continue
            }

            // Read inotify events
            nbytes, err := unix.Read(w.inotifyFd, buf)
            if err != nil {
                if err == unix.EAGAIN {
                    continue
                }
                ch <- Event{Err: fmt.Errorf("read inotify: %w", err)}
                return
            }

            // Parse inotify events from buffer
            w.parseEvents(buf[:nbytes], ch)
        }
    }()
    return ch
}
```

**`EpollWait(epollFd, events, 100)`**: The third argument is a timeout in
milliseconds. The syscall returns when either:
- An event is ready on a monitored fd (returns 1+).
- The timeout expires (returns 0).
- A signal interrupts the wait (returns -1 with `EINTR`).

**`EINTR` handling**: Signals (like `SIGWINCH` from terminal resize) can
interrupt `epoll_wait`. The `if err == unix.EINTR { continue }` restarts the
wait. This is a standard pattern for any blocking syscall in a Unix program.

**`EAGAIN` handling**: Since the inotify fd is non-blocking, `read` returns
`EAGAIN` if epoll reported readiness but the events were already consumed
(a race condition that can happen in theory but is rare in practice). The code
handles it by simply continuing the loop.

### Manual inotify Event Parsing

inotify delivers events as a packed byte stream. Each event has a fixed 16-byte
header followed by a variable-length filename:

```c
struct inotify_event {
    int32_t  wd;      /* Watch descriptor (identifies which path) */
    uint32_t mask;    /* Event type flags */
    uint32_t cookie;  /* Cookie for associating rename pairs */
    uint32_t len;     /* Length of name field (including padding) */
    char     name[];  /* Null-terminated filename (only for dir watches) */
};
```

The `cookie` field is used to correlate `IN_MOVED_FROM` and `IN_MOVED_TO`
events (they share the same cookie value). agrep does not use it because it
does not watch for `IN_MOVED_FROM`.

**Source: `internal/watch/watch.go`, lines 141-194**

```go
const inotifyEventSize = 16

func (w *Watcher) parseEvents(buf []byte, ch chan<- Event) {
    offset := 0
    for offset+inotifyEventSize <= len(buf) {
        wd := int32(binary.LittleEndian.Uint32(buf[offset:]))
        mask := binary.LittleEndian.Uint32(buf[offset+4:])
        // cookie at offset+8 (unused)
        nameLen := int(binary.LittleEndian.Uint32(buf[offset+12:]))

        var name string
        if nameLen > 0 {
            nameStart := offset + inotifyEventSize
            nameEnd := nameStart + nameLen
            if nameEnd > len(buf) {
                break
            }
            nameBytes := buf[nameStart:nameEnd]
            // Trim NUL padding
            for i, b := range nameBytes {
                if b == 0 {
                    nameBytes = nameBytes[:i]
                    break
                }
            }
            name = string(nameBytes)
        }

        offset += inotifyEventSize + nameLen

        dirPath := w.watches[int(wd)]
        var path string
        if name != "" {
            path = filepath.Join(dirPath, name)
        } else {
            path = dirPath
        }

        switch {
        case mask&unix.IN_CREATE != 0 || mask&unix.IN_MOVED_TO != 0:
            ch <- Event{Path: path, Type: EventCreated}
        case mask&unix.IN_MODIFY != 0:
            ch <- Event{Path: path, Type: EventModified}
        case mask&unix.IN_DELETE_SELF != 0 || mask&unix.IN_MOVE_SELF != 0:
            ch <- Event{Path: path, Type: EventDeleted}
        }
    }
}
```

Key details about the parsing:

**NUL padding**: The `name` field is padded with NUL bytes to align subsequent
events. The `nameLen` field includes this padding. So for a filename "test.txt"
(8 chars + NUL = 9 bytes), `nameLen` might be 12 or 16 to maintain alignment.
The parsing loop trims trailing NUL bytes by scanning for the first NUL.

**Watch descriptor to path mapping**: inotify identifies watched paths by
integer watch descriptors (wd), not by path string. The `w.watches` map
translates wd back to the original path. When a directory watch fires, the
`name` field contains the filename within that directory. For file watches,
`name` is empty and the path is the watched file itself.

**`binary.LittleEndian.Uint32`**: This is used instead of `unsafe.Pointer`
casting (as in the dirent parser) because inotify events may not be properly
aligned in the buffer. `binary.LittleEndian.Uint32` works regardless of
alignment by reading individual bytes.

---

## io_uring -- When "Faster" Syscalls Are Actually Slower

agrep includes a complete pure-Go io_uring wrapper at
`internal/uring/` (since removed from the tree — see git history; findings preserved below). It was built, benchmarked extensively,
and found to be **1.3x to 3.6x slower** than direct syscalls for grep
workloads. This section explains what io_uring is, how the wrapper works, and
why it lost.

### What io_uring Is

io_uring (introduced in Linux 5.1) is an asynchronous I/O interface that uses
shared memory ring buffers to submit and complete I/O operations. The
architecture:

```
                User Space                    Kernel Space
          +-------------------+          +-------------------+
          |                   |          |                   |
          |   Submission      |  shared  |   Submission      |
          |   Queue (SQ)      |  memory  |   Queue (SQ)      |
          |   +-----------+   |  <---->  |   +-----------+   |
          |   | SQE | SQE |   |          |   | SQE | SQE |   |
          |   +-----------+   |          |   +-----------+   |
          |                   |          |                   |
          |   Completion      |  shared  |   Completion      |
          |   Queue (CQ)      |  memory  |   Queue (CQ)      |
          |   +-----------+   |  <---->  |   +-----------+   |
          |   | CQE | CQE |   |          |   | CQE | CQE |   |
          |   +-----------+   |          |   +-----------+   |
          +-------------------+          +-------------------+

1. User fills SQEs (submission queue entries) in shared memory
2. User calls io_uring_enter() to notify kernel (or kernel polls with SQPOLL)
3. Kernel processes SQEs, performs I/O operations
4. Kernel fills CQEs (completion queue entries) in shared memory
5. User reads CQEs without a syscall (just memory reads with atomic fences)
```

The theoretical advantage: instead of 4 syscalls per file (open + fstat + read
+ close), you submit 4 SQEs in one batch and call `io_uring_enter` once. The
syscall count drops from 4N to approximately N/batch_size.

### The Ring Setup

**Source: `internal/uring/uring.go`, lines 143-162**

```go
func NewRing(entries uint32) (*Ring, error) {
    var p params
    fd, _, errno := syscall.RawSyscall(unix.SYS_IO_URING_SETUP,
        uintptr(entries), uintptr(unsafe.Pointer(&p)), 0)
    if errno != 0 {
        return nil, fmt.Errorf("io_uring_setup: %w", errno)
    }

    r := &Ring{
        fd:      int(fd),
        entries: p.SQEntries,
    }

    if err := r.mmapRings(&p); err != nil {
        unix.Close(r.fd)
        return nil, err
    }

    return r, nil
}
```

`SYS_IO_URING_SETUP` creates the io_uring instance. The `params` struct is
both input and output: the caller specifies desired entries and flags, and the
kernel fills in the actual ring sizes and memory offsets. The kernel returns a
file descriptor that is used for subsequent `io_uring_enter` calls and for
`mmap`-ing the ring memory.

**Note the use of `syscall.RawSyscall`** instead of `unix.Syscall`. The
difference is that `RawSyscall` does not inform the Go scheduler that the
goroutine is about to block. For `io_uring_setup` (which does not block -- it
returns immediately after allocating kernel structures), this avoids the
overhead of notifying the scheduler. `Syscall` (the regular variant) calls
`runtime.entersyscall()` and `runtime.exitsyscall()`, which interact with the
goroutine scheduler's P (processor) management.

### Memory-Mapped Ring Buffers

**Source: `internal/uring/uring.go`, lines 164-219**

The rings are mapped into user space via `mmap`:

```go
func (r *Ring) mmapRings(p *params) error {
    // Map SQ ring
    sqRingSize := p.SQOff.Array + p.SQEntries*4
    sqMem, err := syscall.Mmap(r.fd, offSQRing, int(sqRingSize),
        syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED|syscall.MAP_POPULATE)
    // ...
```

Three separate regions are mapped at fixed offsets from the io_uring fd:

| Offset Constant | Value | Purpose |
|---|---|---|
| `offSQRing` | 0x0 | Submission queue ring (head, tail, flags, array) |
| `offCQRing` | 0x8000000 | Completion queue ring (head, tail, CQE array) |
| `offSQEs` | 0x10000000 | SQE array (actual submission entries) |

**`MAP_SHARED`**: The mapping is shared between user space and kernel. Writes
by user space to SQEs are visible to the kernel, and writes by the kernel to
CQEs are visible to user space. This is the fundamental zero-copy mechanism.

**`MAP_POPULATE`**: For the ring buffers (unlike file mmap), pre-faulting is
desirable. The ring buffers are small (a few KB) and will be accessed
immediately. Pre-faulting avoids a page fault on the first SQE write.

**`FEAT_SINGLE_MMAP`**: Some kernels support a feature where the SQ and CQ
rings share a single mmap region. The wrapper checks for this:

```go
if p.Features&featSingleMmap != 0 {
    r.cqMem = sqMem
} else {
    // Separate mmap for CQ ring
}
```

### Pointer Arithmetic Into Shared Memory

The ring pointers are set up by computing offsets into the mmap'd regions:

```go
// Set up SQ pointers
base := unsafe.Pointer(&sqMem[0])
r.sqHead = (*uint32)(unsafe.Add(base, p.SQOff.Head))
r.sqTail = (*uint32)(unsafe.Add(base, p.SQOff.Tail))
r.sqMask = *(*uint32)(unsafe.Add(base, p.SQOff.RingMask))
r.sqArray = unsafe.Add(base, p.SQOff.Array)
```

`r.sqHead` and `r.sqTail` are pointers directly into the mmap'd memory. The
kernel reads `sqTail` to know how many new SQEs are available, and writes
`sqHead` after processing them. Similarly, the kernel writes `cqTail` after
adding completions, and user space reads it and advances `cqHead`.

This is a **lock-free producer-consumer ring buffer** using atomic operations
as the synchronization mechanism.

### Submitting Operations

**Source: `internal/uring/uring.go`, lines 243-278**

```go
func (r *Ring) SubmitAndWait(count uint32, fn func(cqe *CQE)) error {
    if count == 0 {
        return nil
    }

    // Set up SQ array: SQ[slot] = SQE index
    tail := atomic.LoadUint32(r.sqTail)
    for i := uint32(0); i < count; i++ {
        slot := (tail + i) & r.sqMask
        *(*uint32)(unsafe.Add(r.sqArray, uintptr(slot)*4)) = i
    }

    // Advance SQ tail (release semantics -- kernel reads this)
    atomic.StoreUint32(r.sqTail, tail+count)

    // Submit and wait for all completions
    _, _, errno := syscall.Syscall6(unix.SYS_IO_URING_ENTER,
        uintptr(r.fd), uintptr(count), uintptr(count),
        enterGetEvents, 0, 0)
    if errno != 0 {
        return fmt.Errorf("io_uring_enter: %w", errno)
    }

    // Drain all available CQEs
    head := atomic.LoadUint32(r.cqHead)
    cqTail := atomic.LoadUint32(r.cqTail)
    for head != cqTail {
        idx := head & r.cqMask
        cqe := (*CQE)(unsafe.Add(r.cqes, uintptr(idx)*unsafe.Sizeof(CQE{})))
        fn(cqe)
        head++
    }
    atomic.StoreUint32(r.cqHead, head)

    return nil
}
```

**The SQ indirection array**: There is a level of indirection between the SQ
ring and the SQE array. The SQ ring contains uint32 indices into the SQE
array, not the SQEs themselves. This allows the user to prepare SQEs in any
order and submit them in a different order. agrep fills SQEs 0..count-1 and
maps them directly: `SQ[i] = i`.

**`atomic.StoreUint32(r.sqTail, tail+count)`**: This is the critical release
write. The kernel reads `sqTail` to determine how many new submissions are
available. The atomic store ensures that all SQE writes are visible to the
kernel before the tail advance (release semantics). On x86-64, a regular store
has release semantics by default (x86 does not reorder stores past stores), but
the atomic is correct for all architectures and communicates intent.

**`SYS_IO_URING_ENTER`**: The submission syscall. Arguments:
- `fd`: The io_uring instance.
- `to_submit` (count): Number of new SQEs to submit.
- `min_complete` (count): Wait until at least this many CQEs are available.
- `flags` (`enterGetEvents = 1`): `IORING_ENTER_GETEVENTS` -- wait for
  completions (makes the syscall blocking until `min_complete` CQEs are ready).

### SQE Operation Types

**Source: `internal/uring/ops.go`**

The wrapper supports four io_uring opcodes for the grep workflow:

```go
const (
    OpOpenat = 18  // IORING_OP_OPENAT
    OpClose  = 19  // IORING_OP_CLOSE
    OpStatx  = 21  // IORING_OP_STATX
    OpRead   = 22  // IORING_OP_READ
)
```

Each operation has a prep helper that fills the 64-byte SQE:

```go
// PrepOpenat sets up an SQE for IORING_OP_OPENAT.
func (sqe *SQE) PrepOpenat(dirfd int32, pathPtr *byte, flags uint32, mode uint32) {
    *sqe = SQE{} // zero out
    sqe.Opcode = OpOpenat
    sqe.Fd = dirfd
    sqe.Addr = uint64(uintptr(unsafe.Pointer(pathPtr)))
    sqe.Len = mode
    sqe.OpcodeFlags = flags
}

// PrepRead sets up an SQE for IORING_OP_READ.
func (sqe *SQE) PrepRead(fd int32, buf *byte, nbytes uint32, offset uint64) {
    *sqe = SQE{} // zero out
    sqe.Opcode = OpRead
    sqe.Fd = fd
    sqe.Addr = uint64(uintptr(unsafe.Pointer(buf)))
    sqe.Len = nbytes
    sqe.Off = offset
}
```

**`*sqe = SQE{}`**: Each prep function zeroes the SQE first. This is critical
because the SQE is 64 bytes with many fields, and leftover bits from a previous
operation could cause the kernel to misinterpret the request. The zero-value of
SQE is safe (all zeros is a valid "do nothing" configuration for unused fields).

**Pointer lifetime**: The `pathPtr` and `buf` pointers passed to `PrepOpenat`
and `PrepRead` must remain valid until the corresponding CQE is reaped. If the
Go garbage collector moves or frees the underlying memory before the kernel
processes the SQE, the kernel will read garbage or cause a fault. In practice,
the GC does not move objects (Go has a non-moving collector), but pinning
awareness is important for correctness reasoning.

### Why io_uring Lost: The Benchmark Results

Three architectures were tested, all slower than direct syscalls:

| Architecture | Direct Syscalls | io_uring | Slowdown |
|---|---|---|---|
| 4-Phase Batch (open all, stat all, read all, close all) | 304ms | 414ms | 1.36x |
| State Machine (single ring, per-file state progression) | 345ms | 1238ms | 3.6x |
| Multi-ring (NumCPU rings, inline matching) | 345ms | 545-630ms | 1.5-1.8x |

### Root Cause Analysis

**1. Small batch sizes**: The walker discovers files incrementally through BFS
traversal. By the time an io_uring worker has accumulated files to batch,
it typically has only 1-5 SQEs. The fixed overhead of `io_uring_enter` (~800us
measured via strace) dominates when the batch is small. Direct syscalls
(~40-50us each for cached I/O) are faster in absolute terms when the batch
contains fewer than ~16-20 operations.

**2. Go's M:N scheduler already provides parallelism**: When 24 goroutines
each call `open()`, `fstat()`, `pread()`, and `close()` in parallel, the
Go scheduler distributes these across OS threads. At any given moment, multiple
threads are blocked in different syscalls, and the OS kernel processes them
concurrently. This is *implicit batching* through parallelism. io_uring's
*explicit batching* does not add value when the implicit parallelism is already
sufficient.

**3. Channel contention**: All tested architectures required a channel to feed
files from the walker to the io_uring event loop. The `futex` syscalls for
channel synchronization accounted for ~51-56% of total syscall time (measured
via strace). This overhead exists regardless of whether the I/O uses direct
syscalls or io_uring.

**4. Serialization bottlenecks**: The state machine architecture (approach #2)
funneled all I/O through a single goroutine's event loop. This created a
serialization point that eliminated parallelism entirely, explaining the 3.6x
slowdown.

**5. CQE ordering**: io_uring completions arrive out of order. An `OPENAT` SQE
submitted first may complete after a `STATX` SQE submitted second. The
`UserData` field must be used to correlate CQEs with their original SQEs.
This added complexity without adding performance.

### When io_uring WOULD Help

io_uring is not universally slower. It excels in scenarios agrep does not
encounter:

- **Network filesystems (NFS, FUSE)**: Per-operation latency is milliseconds
  instead of microseconds. Batching saves real time.
- **Cold page cache**: When data must come from physical disk, I/O latency
  dominates. io_uring's asynchronous model can keep the disk queue full.
- **NVMe with high queue depth**: Modern NVMe SSDs have 32+ hardware queues.
  io_uring can saturate them; sequential syscalls cannot.
- **SQPOLL mode**: A kernel-side polling thread processes SQEs without any
  `io_uring_enter` syscall at all. This requires root and a dedicated CPU core.
- **Fixed files and registered buffers**: Pre-registering file descriptors and
  buffers eliminates per-operation setup in the kernel.

agrep's workload -- warm page cache, local filesystem, many small files -- is
the worst case for io_uring and the best case for direct syscalls with a well-
parallelized goroutine pool.

### The Lesson

Do not assume that a newer, more sophisticated API is faster. io_uring is a
powerful tool, but it was designed for high-latency, high-throughput I/O
workloads (databases, network servers). For CPU-bound search with cached I/O,
the simplest approach (direct syscalls from parallel goroutines) wins
convincingly. **Always benchmark before committing to a more complex
architecture.**

---

## Syscall Overhead Accounting

Understanding where time goes requires counting and measuring every syscall.
Here is the per-file syscall breakdown for a typical agrep recursive search:

### Per-File Syscalls (Buffered Read Path)

```
open(path, O_RDONLY|O_NOATIME)    -- 1 syscall (or 2 if O_NOATIME fails)
fstat(fd)                          -- 1 syscall
pread(fd, buf, size, 0)            -- 1 syscall (usually; more for very large files)
close(fd)                          -- 1 syscall
                                   ----------
                                   4 syscalls per file
```

### Per-File Syscalls (mmap Read Path)

```
open(path, O_RDONLY|O_NOATIME)    -- 1 syscall
fstat(fd)                          -- 1 syscall
fadvise(fd, FADV_SEQUENTIAL)       -- 1 syscall
mmap(fd, size, PROT_READ)          -- 1 syscall
madvise(data, MADV_SEQUENTIAL)     -- 1 syscall
  ... page faults (kernel-internal, not syscalls) ...
madvise(data, MADV_DONTNEED)       -- 1 syscall
munmap(data)                       -- 1 syscall
close(fd)                          -- 1 syscall
                                   ----------
                                   8 syscalls per file
```

The mmap path has double the syscall count, which is why it is reserved for
large files where the data-transfer savings outweigh the syscall overhead.

### Per-Directory Syscalls

```
open(dirpath, O_RDONLY|O_DIRECTORY|O_NOATIME)  -- 1 syscall
getdents64(fd, buf, 32768)                      -- 1+ syscalls (1 per buffer-full)
close(fd)                                       -- 1 syscall
                                                ----------
                                                3+ syscalls per directory
```

For a small directory (fewer than ~500 entries), `getdents64` fills the 32KB
buffer in a single call. For larger directories, multiple calls are needed.

### Total for End-to-End Benchmark (37K files)

```
~37,000 files * 4 syscalls/file  = ~148,000 file I/O syscalls
~3,000 dirs * 3 syscalls/dir     = ~9,000 directory syscalls
writev calls for output           = ~1,000 (depends on matches)
futex calls (channel sync)        = ~500,000+ (goroutine scheduling)
                                  -------
                                  ~672,000 total syscalls (measured via strace)
```

The futex calls dominate the count but not the time -- each is ~1us for
an uncontended channel operation, vs ~40-50us for a file I/O syscall.

### Profiling with strace

To analyze syscall overhead on a real run:

```bash
# Count syscalls by type
strace -c -f agrep -r --no-ignore --hidden -l "pattern" /path/to/tree 2>&1 | tail -20

# Trace specific syscalls with timing
strace -f -e trace=open,openat,close,fstat,pread64,getdents64 \
    -T agrep -r -l "pattern" /path/to/tree 2>trace.log

# Count O_NOATIME failures
strace -f -e trace=open,openat agrep -r -l "pattern" /usr/lib 2>&1 | grep EPERM | wc -l
```

The `-f` flag is essential: it traces all threads (Go goroutines run on
multiple OS threads). The `-T` flag appends the time spent in each syscall,
which is invaluable for identifying slow operations.

---

## Cross-references

- See `03-memory-management.md` for mmap details and the adaptive reader's
  threshold tuning, sync.Pool buffer reuse patterns, and zero-allocation
  strategies.
- See `05-concurrency-patterns.md` for how the walker parallelizes directory
  traversal with the `parallelWalker` work-stealing design, and how the
  scheduler distributes file processing across workers.
- See `07-benchmarking-and-profiling.md` for using strace to analyze syscall
  overhead, `perf` for kernel-side profiling, and Go's `pprof` for
  user-space analysis.
