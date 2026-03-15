# gogrep

Linux-only, high-performance grep alternative in Go with AVX2 SIMD acceleration.

## Build & Test

Requires Go 1.26+ with `GOEXPERIMENT=simd` (set automatically by Makefile).

- `make build` — build to `bin/gogrep`
- `make test` — `go test -race ./...` (skips PCRE under race) + PCRE tests separately
- `make bench` — run benchmarks (matchers, input, SIMD)
- `make lint` — `go vet ./...`
- `GOEXPERIMENT=simd go test -race ./internal/matcher/` — test just matchers
- `GOEXPERIMENT=simd go test -bench=. -benchmem ./internal/matcher/` — benchmark matchers
- `GOEXPERIMENT=simd go test -bench=. -benchmem ./internal/simd/` — benchmark SIMD primitives

## Architecture

    CLI (cmd/gogrep/main.go) — manual flag parsing, no external framework
      -> Config (internal/cli/config.go)
      -> Walker (internal/walker/) — raw getdents64, .gitignore support
      -> Scheduler (internal/scheduler/) — worker pool
      -> Input (internal/input/) — mmap / pread
      -> Matcher (internal/matcher/) — regex / boyer-moore / aho-corasick / pcre
         -> SIMD (internal/simd/) — AVX2-accelerated search via simd/archsimd
      -> Output (internal/output/) — text / json, writev batching
      -> Watch (internal/watch/) — raw inotify + epoll

Orchestration: `internal/cli/run.go`

## Design Principles

- **Linux-only**: Use Linux syscalls directly (`getdents64`, `open`, `mmap`, `fadvise`, `madvise`, `writev`, `inotify`, `epoll`). Never use portable abstractions when a Linux-specific path is faster.
- **SIMD-accelerated**: Fixed-string search uses AVX2 SIMD-friendly Horspool (first+last byte prefilter, 32 positions/iteration) via Go 1.26 `simd/archsimd`. Regex patterns auto-extract multiple required literals for cascaded SIMD prefiltering.
- **Search-then-split**: All matchers search the whole buffer first, then extract line boundaries around matches. Only lines with matches are processed.
- **Zero allocations on hot path**: Use `sync.Pool`, `[]byte` everywhere, no `string` conversions during search.
- **Pure Go, no cgo**: `golang.org/x/sys`, `go.elara.ws/pcre`, `github.com/sabhiram/go-gitignore`, `simd/archsimd`. No C bindings, no external CLI frameworks.
- **`O_NOATIME`** on every file open to eliminate atime inode writes.

## Conventions

- All file I/O through `golang.org/x/sys/unix` syscalls, not `os` package
- Work with `[]byte` — never convert to `string` in hot paths
- Matchers implement `Matcher` interface (internal/matcher/match.go)
- Readers implement `Reader` interface (internal/input/reader.go)
- Exit codes: 0 = match found, 1 = no match, 2 = error
- Tests use `testdata/` fixtures
- Benchmarks compare against `bytes.Index` baseline
- `GOEXPERIMENT=simd` required for all go commands

## File Layout

- `cmd/gogrep/` — entry point, manual flag parsing (no cobra)
- `internal/cli/` — config + orchestration (`run.go` wires everything)
- `internal/walker/` — directory traversal (getdents64 + dirent parsing)
- `internal/scheduler/` — worker pool + concurrency
- `internal/input/` — file reading strategies (buffered, mmap, streaming)
- `internal/matcher/` — pattern matching (regex, fixed, boyer-moore, aho-corasick, pcre, pipeline)
- `internal/simd/` — AVX2 SIMD primitives (IndexByte, IndexAll, Count, ToLowerASCII via archsimd)
- `internal/output/` — formatting + ordered writing
- `internal/watch/` — inotify file watching
