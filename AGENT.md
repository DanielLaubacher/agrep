# agrep — Agent Instructions

## Quick Reference

- **Language**: Go 1.26+, Linux-only, AMD64
- **Module**: `github.com/DanielLaubacher/agrep`
- **Build**: `GOEXPERIMENT=simd go build -o bin/agrep ./cmd/agrep` (or `make build`)
- **Test**: `GOEXPERIMENT=simd AGREP_SKIP_PCRE=1 go test -race ./...` (or `make test`)
- **Go deps**: `golang.org/x/sys`, `go.elara.ws/pcre`, `github.com/sabhiram/go-gitignore`, `simd/archsimd` (stdlib experimental)

## Critical Rules

1. **Linux-only**: Use `golang.org/x/sys/unix` syscalls. Do not use `os.Open`, `os.ReadFile`, `os.Stat`, `filepath.WalkDir`, or any portable abstraction when a direct Linux syscall exists.
2. **[]byte everywhere**: Never convert `[]byte` to `string` on the hot path. Pattern matching, line scanning, and output formatting all operate on `[]byte`.
3. **No cgo, no C bindings**: Everything is pure Go. Approved deps: `golang.org/x/sys`, `go.elara.ws/pcre`, `github.com/sabhiram/go-gitignore`, `simd/archsimd`. No external CLI framework (manual flag parsing). Do not add others without approval.
3b. **GOEXPERIMENT=simd**: Required for all build/test commands. The `internal/simd/` package uses `simd/archsimd` for AVX2 intrinsics.
4. **O_NOATIME**: Every file open must include `unix.O_NOATIME`.
5. **Pool buffers**: Use `sync.Pool` for any buffer allocated in a loop or per-file.
6. **Interfaces**: Matchers implement `Matcher` (internal/matcher/match.go), readers implement `Reader` (internal/input/reader.go).
7. **Per-stage flags**: `-F`, `-P`, `-t`, `-o` are per-stage modifiers on `-e`. They accumulate and attach to the next `-e`, then reset. `-t` chains stages (AND pipeline), bare `-e` starts a new OR branch. `-o` outputs only matched text.

## Key Syscalls Used

| Syscall | Go API | Used For |
|---|---|---|
| `getdents64` | `unix.Getdents` | Directory traversal without lstat |
| `open` | `unix.Open` | File and directory opens with O_NOATIME |
| `stat` | `unix.Stat` | Stat only when d_type is DT_UNKNOWN |
| `pread64` | `unix.Pread` | Positional read into pooled buffers |
| `mmap` | `syscall.Mmap` | Memory-map large files |
| `fadvise64` | `unix.Fadvise` | Hint sequential access to kernel |
| `madvise` | `unix.Madvise` | MADV_SEQUENTIAL, MADV_DONTNEED |
| `writev` | `unix.Writev` | Scatter-gather output batching |
| `inotify_init1` | `unix.InotifyInit1` | File watch setup |
| `inotify_add_watch` | `unix.InotifyAddWatch` | Watch specific files/dirs |
| `epoll_create1` | `unix.EpollCreate1` | Efficient event loop for inotify |

## Testing

- Run `GOEXPERIMENT=simd AGREP_SKIP_PCRE=1 go test -race ./...` after every change
- Run `GOEXPERIMENT=simd go test ./internal/matcher/ -run "PCRE"` separately (PCRE incompatible with -race)
- Use `testdata/` fixtures for integration tests
- Table-driven tests for matchers
- Always test: empty input, single-char pattern, no match, case-insensitive
