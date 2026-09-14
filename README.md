# agrep

> **v0.0.1** | **Experimental** — APIs and flags may change without notice.

A high-performance grep alternative written in pure Go, designed exclusively for Linux AMD64. Uses raw Linux syscalls, AVX2 SIMD acceleration, and memory-mapped I/O to maximize search throughput.

<p align="center">
  <img src="https://static.wikia.nocookie.net/arrow/images/2/24/Vibe_with_his_powers_restored.png" alt="Vibe" width="300">
</p>

Built vibe coding with [Claude Code](https://claude.com/claude-code).

## Features

- **AVX2 SIMD search** — fixed-string patterns use a SIMD-friendly Horspool algorithm that compares 32 byte positions per iteration
- **Search-then-split** — searches the entire file buffer first, then extracts line boundaries only around matches (avoids per-line overhead)
- **Memory-mapped I/O** — large files are mmap'd with `MADV_SEQUENTIAL` + `FADV_SEQUENTIAL` for zero-copy search (demand-paged, no `MAP_POPULATE`, enabling early exit for `-l` mode)
- **Raw syscalls** — `getdents64`, `open`, `pread`, `mmap`, `writev`, `inotify`, `epoll` — no portable Go abstractions
- **Parallel recursive search** — worker pool distributes files across `NumCPU * 2` goroutines with deterministic output ordering
- **Multiple pattern engines** — custom lazy-DFA regex engine with SIMD prefilters, Boyer-Moore with SIMD, rare-pair Teddy multi-pattern (2-8 fixed patterns), Aho-Corasick for larger sets, optional PCRE2 (pure Go port, `make build-pcre`)
- **Parallel everywhere** — recursive searches fan out across a worker pool, and large single files are searched in parallel line-aligned chunks
- **Regex pipeline** (`-t`/`-o`) — chain patterns with different engines; SIMD fixed-string stages eliminate lines before expensive regex runs
- **Multi-literal prefilter** — regex AST analysis extracts all required literals for cascaded SIMD rejection before the regex engine runs
- **Watch mode** — inotify + epoll file watching with log rotation handling
- **JSON output** — JSON Lines format for programmatic consumption
- **Pure Go, no cgo** — no C bindings or assembly files

## Requirements

- **Go 1.26+** with `GOEXPERIMENT=simd`
- **Linux AMD64** (x86-64 with AVX2 — Intel Haswell+ or AMD Excavator+)
- `golang.org/x/sys` for Linux syscall bindings

## Building

```sh
# Build
GOEXPERIMENT=simd go build -o bin/agrep ./cmd/agrep

# Or use make
make build
```

## Installation

```sh
GOEXPERIMENT=simd go install github.com/DanielLaubacher/agrep/cmd/agrep@latest
```

Or with make:

```sh
make install
```

## Testing

```sh
# Run all tests (PCRE tests run separately due to -race incompatibility)
make test

# Run benchmarks
make bench

# Profile against ripgrep
make profile

# Lint
make lint
```

## Usage

```
agrep [OPTIONS] PATTERN [FILE...]
agrep [OPTIONS] -e PATTERN [-e PATTERN...] [FILE...]
```

If no files are given, reads from stdin.

```sh
# Basic search
agrep "error" app.log

# Case-insensitive, recursive, with line numbers
agrep -rin "fixme" ./src/

# Fixed string with SIMD acceleration
agrep -F "[ERROR]" app.log

# Multiple patterns (one SIMD Teddy scan)
agrep -F -e "timeout" -e "refused" -e "EOF" app.log

# PCRE2 regex with lookbehind (requires the pcre build: make build-pcre)
agrep-pcre -P '(?<=error:\s)\w+' app.log

# Regex pipeline: SIMD prefilter → extract digits (like grep|grep -o)
agrep -Fe 'ERROR' -toe '\d+' app.log

# Only-matching (like grep -o)
agrep -oe '\d+' app.log

# Three-stage pipeline mixing engines
agrep -Fe 'HTTP' -Fte 'status' -toe '\d+' access.log

# Context lines
agrep -C3 "panic" app.log

# Watch mode
agrep --watch "ERROR" /var/log/syslog

# JSON output
agrep --json "error" app.log
```

See [help.md](help.md) for the full flag reference and more examples.

## Performance

agrep wins or ties ripgrep on every workload in its benchmark suite (up
to 10.9x faster on single-file counts, ~1.5x on recursive `-l`). See the
measured table in [architecture.md](architecture.md#performance-vs-ripgrep)
and the full optimization history in `education/`.

```sh
# Build the pcre-less binary and race through a tree
make build
./bin/agrep -rn 'ERROR.*timeout' /var/log
```

## Agent mode

agrep is purpose-built to serve AI agents as a sensing API: output token
budgets (`--max-tokens`), corpus surveys (`--outline --top K`), Markdown
section context (`--sections`), one-pass multi-query execution
(`--batch`), zero-hit variant guidance (`--suggest`), and verifiable
citation spans (`--json` + `--get-region`). Run `agrep --skill` to get
the agent operating manual (~850 tokens) for loading into an agent's
context. See [agent-mode.md](agent-mode.md) for the design and examples.

A daemonless trigram index (`--use-index`) prunes repeated searches over
slowly-changing corpora: candidates come from the index, a per-query
stat sweep guarantees freshness, and reindexing is incremental via
per-file digests (only changed files are ever re-read). The index is a
prefilter, never an answerer — every match is verified against disk
bytes. See [indexing-daemon.md](indexing-daemon.md).

## Architecture

See [architecture.md](architecture.md) for detailed design documentation covering the pipeline, syscall usage, SIMD algorithms, concurrency model, and key constants. The `education/` directory contains in-depth writeups of every subsystem and the ripgrep-gap optimization campaign.

## License

MIT
