#!/bin/bash
# benchmark.sh — Compare agrep vs GNU grep vs ripgrep
# Usage: ./scripts/benchmark.sh [SEARCH_DIR]
#
# Generates a large test file if none exists, then runs comparative benchmarks.
# Uses hyperfine if available, otherwise falls back to shell-based timing.

set -euo pipefail

AGREP="./bin/agrep"
GREP="/usr/bin/grep"
RG="/usr/bin/rg"

SEARCH_DIR="${1:-/usr/include}"
TESTFILE="/tmp/agrep_bench_data.txt"

# Build agrep if needed
if [ ! -f "$AGREP" ]; then
    echo "Building agrep..."
    go build -o "$AGREP" ./cmd/agrep
fi

# Generate test data if needed
if [ ! -f "$TESTFILE" ]; then
    echo "Generating test data..."
    for i in $(seq 1 100000); do
        echo "line $i: the quick brown fox jumps over the lazy dog"
        echo "line $i: Lorem ipsum dolor sit amet, consectetur adipiscing elit"
        echo "line $i: ERROR: connection refused at port 8080"
        echo "line $i: 2024-01-15T10:30:00Z INFO request processed successfully"
        echo "line $i: function calculate(a, b) { return a + b; }"
    done > "$TESTFILE"
    echo "Generated $(wc -l < "$TESTFILE") lines ($(du -h "$TESTFILE" | cut -f1))"
fi

echo ""
echo "=== agrep Benchmark Suite ==="
echo "agrep: $AGREP"
echo "grep:   $GREP ($(grep --version 2>&1 | head -1))"
echo "rg:     $RG ($(rg --version | head -1))"
echo "Test file: $TESTFILE ($(wc -l < "$TESTFILE") lines, $(du -h "$TESTFILE" | cut -f1))"
echo "Search dir: $SEARCH_DIR"
echo ""

run_bench() {
    local name="$1"
    shift
    local cmds=("$@")

    echo "--- $name ---"

    if command -v hyperfine &>/dev/null; then
        hyperfine --warmup 3 "${cmds[@]}"
    else
        # Fallback: manual timing
        for cmd in "${cmds[@]}"; do
            echo "  $cmd"
            # Warmup
            eval "$cmd" > /dev/null 2>&1 || true
            eval "$cmd" > /dev/null 2>&1 || true

            # Timed runs
            local total=0
            local runs=5
            for i in $(seq 1 $runs); do
                local start end elapsed
                start=$(date +%s%N)
                eval "$cmd" > /dev/null 2>&1 || true
                end=$(date +%s%N)
                elapsed=$(( (end - start) / 1000000 ))
                total=$((total + elapsed))
            done
            local avg=$((total / runs))
            echo "    avg: ${avg}ms ($runs runs)"
        done
    fi
    echo ""
}

# Benchmark 1: Simple fixed string search in large file
run_bench "Fixed string search (large file)" \
    "$AGREP -F 'connection refused' $TESTFILE" \
    "$GREP -F 'connection refused' $TESTFILE" \
    "$RG -F 'connection refused' $TESTFILE"

# Benchmark 2: Regex search in large file
run_bench "Regex search (large file)" \
    "$AGREP 'ERROR.*port [0-9]+' $TESTFILE" \
    "$GREP -E 'ERROR.*port [0-9]+' $TESTFILE" \
    "$RG 'ERROR.*port [0-9]+' $TESTFILE"

# Benchmark 3: Case-insensitive search
run_bench "Case-insensitive search (large file)" \
    "$AGREP -i 'lorem ipsum' $TESTFILE" \
    "$GREP -i 'lorem ipsum' $TESTFILE" \
    "$RG -i 'lorem ipsum' $TESTFILE"

# Benchmark 4: No-match search (worst case for many tools)
run_bench "No-match search (large file)" \
    "$AGREP 'ZZZZNOTFOUND' $TESTFILE" \
    "$GREP 'ZZZZNOTFOUND' $TESTFILE" \
    "$RG 'ZZZZNOTFOUND' $TESTFILE"

# Benchmark 5: Count only
run_bench "Count matches (large file)" \
    "$AGREP -c 'fox' $TESTFILE" \
    "$GREP -c 'fox' $TESTFILE" \
    "$RG -c 'fox' $TESTFILE"

# Benchmark 6: Recursive search (directory tree)
if [ -d "$SEARCH_DIR" ]; then
    run_bench "Recursive search (directory tree: $SEARCH_DIR)" \
        "$AGREP -r 'include' $SEARCH_DIR" \
        "$GREP -r 'include' $SEARCH_DIR" \
        "$RG 'include' $SEARCH_DIR"

    run_bench "Recursive + line numbers (directory tree)" \
        "$AGREP -rn 'define' $SEARCH_DIR" \
        "$GREP -rn 'define' $SEARCH_DIR" \
        "$RG -n 'define' $SEARCH_DIR"
fi

# Benchmark 7: Multiple patterns
run_bench "Multiple patterns (large file)" \
    "$AGREP -F -e 'ERROR' -e 'INFO' -e 'function' $TESTFILE" \
    "$GREP -F -e 'ERROR' -e 'INFO' -e 'function' $TESTFILE" \
    "$RG -F -e 'ERROR' -e 'INFO' -e 'function' $TESTFILE"

echo "=== Benchmark Complete ==="
