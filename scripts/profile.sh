#!/bin/bash
# profile.sh — Build, benchmark vs rg, and CPU/mem profile agrep.
#
# Usage:
#   ./scripts/profile.sh              # run all benchmarks + profile
#   ./scripts/profile.sh bench        # benchmarks only (no profiling)
#   ./scripts/profile.sh prof         # profiling only (no benchmarks)
#   ./scripts/profile.sh prof top     # profile + show pprof top
#   ./scripts/profile.sh prof web     # profile + open pprof in browser
#
# Environment:
#   SEARCH_DIR   override search directory (default: /usr/include)
#   PATTERN      override search pattern  (default: define)
#   RUNS         hyperfine runs           (default: 5)
#   WARMUP       hyperfine warmup runs    (default: 2)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN="$ROOT_DIR/bin/agrep"
PROF_DIR="/tmp/agrep-prof"

SEARCH_DIR="${SEARCH_DIR:-/usr/include}"
PATTERN="${PATTERN:-define}"
RUNS="${RUNS:-5}"
WARMUP="${WARMUP:-2}"

export GOEXPERIMENT="${GOEXPERIMENT:-simd}"

# ---------- helpers ----------

die()  { echo "error: $*" >&2; exit 1; }
bold() { printf '\033[1m%s\033[0m\n' "$*"; }
sep()  { echo "────────────────────────────────────────"; }

build() {
    bold "Building agrep..."
    (cd "$ROOT_DIR" && go build -o "$BIN" ./cmd/agrep)
    echo "built: $BIN"
}

check_deps() {
    command -v rg      >/dev/null || die "rg (ripgrep) not found"
    command -v hyperfine >/dev/null || die "hyperfine not found"
    [ -d "$SEARCH_DIR" ] || die "SEARCH_DIR=$SEARCH_DIR does not exist"
}

# ---------- benchmarks ----------

run_bench() {
    local label="$1"; shift
    bold "$label"
    hyperfine --warmup "$WARMUP" --runs "$RUNS" "$@"
    echo
}

do_bench() {
    bold "=== Benchmarks: agrep vs rg ==="
    echo "dir=$SEARCH_DIR  pattern=$PATTERN  runs=$RUNS  warmup=$WARMUP"
    sep

    # 1. Files-only (-l)
    run_bench "Files only (-l)" \
        -n agrep "$BIN -l --no-ignore --hidden '$PATTERN' $SEARCH_DIR" \
        -n rg     "rg -l --no-ignore --hidden '$PATTERN' $SEARCH_DIR"

    # 2. Full output (default)
    run_bench "Full output" \
        -n agrep "$BIN --no-ignore --hidden '$PATTERN' $SEARCH_DIR" \
        -n rg     "rg --no-ignore --hidden '$PATTERN' $SEARCH_DIR"

    # 3. Full output + line numbers
    run_bench "Full output + line numbers (-n)" \
        -n agrep "$BIN -n --no-ignore --hidden '$PATTERN' $SEARCH_DIR" \
        -n rg     "rg -n --no-ignore --hidden '$PATTERN' $SEARCH_DIR"

    # 4. Regex
    run_bench "Regex (func\\w+)" \
        -n agrep "$BIN -rn --no-ignore --hidden 'func\w+' $SEARCH_DIR" \
        -n rg     "rg -n --no-ignore --hidden 'func\w+' $SEARCH_DIR"

    # 5. Count only
    run_bench "Count only (-c)" \
        -n agrep "$BIN -c --no-ignore --hidden '$PATTERN' $SEARCH_DIR" \
        -n rg     "rg -c --no-ignore --hidden '$PATTERN' $SEARCH_DIR"

    # 6. With gitignore (respects .gitignore)
    if [ -d "$ROOT_DIR/../" ]; then
        run_bench "With gitignore (~/dev/)" \
            -n agrep "$BIN -rn 'import' $ROOT_DIR/../" \
            -n rg     "rg -n 'import' $ROOT_DIR/../"
    fi
}

# ---------- profiling ----------

do_prof() {
    local mode="${1:-top}"
    mkdir -p "$PROF_DIR"

    local cpu_prof="$PROF_DIR/cpu.prof"
    local mem_prof="$PROF_DIR/mem.prof"

    bold "=== CPU + Memory Profile ==="
    echo "dir=$SEARCH_DIR  pattern=$PATTERN"
    echo "output: $PROF_DIR/"
    sep

    # CPU profile
    bold "Recording CPU profile..."
    "$BIN" --cpuprofile "$cpu_prof" --no-ignore --hidden "$PATTERN" "$SEARCH_DIR" > /dev/null 2>&1 || true
    echo "wrote $cpu_prof ($(du -h "$cpu_prof" | cut -f1))"

    # Memory profile
    bold "Recording memory profile..."
    "$BIN" --memprofile "$mem_prof" --no-ignore --hidden "$PATTERN" "$SEARCH_DIR" > /dev/null 2>&1 || true
    echo "wrote $mem_prof ($(du -h "$mem_prof" | cut -f1))"

    sep

    case "$mode" in
        top)
            bold "CPU profile — top 30 (flat):"
            go tool pprof -top -nodecount=30 "$cpu_prof" 2>&1
            echo
            bold "CPU profile — top 20 (cumulative):"
            go tool pprof -top -cum -nodecount=20 "$cpu_prof" 2>&1
            echo
            bold "Memory profile — top 20 (alloc_space):"
            go tool pprof -top -nodecount=20 -alloc_space "$mem_prof" 2>&1
            ;;
        web)
            bold "Opening CPU profile in browser..."
            go tool pprof -http=:6060 "$cpu_prof"
            ;;
        *)
            echo "Profiles written. Analyze with:"
            echo "  go tool pprof -top $cpu_prof"
            echo "  go tool pprof -top -alloc_space $mem_prof"
            echo "  go tool pprof -http=:6060 $cpu_prof"
            ;;
    esac
}

# ---------- main ----------

check_deps
build

case "${1:-all}" in
    bench) do_bench ;;
    prof)  do_prof "${2:-top}" ;;
    all)
        do_bench
        sep
        do_prof top
        ;;
    *)
        echo "Usage: $0 [bench|prof|all] [top|web]"
        exit 1
        ;;
esac
