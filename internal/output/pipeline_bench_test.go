package output

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/regex"
)

// benchCorpus mimics the errport benchmark shape: ~2% ERROR lines among
// filler, ~80-90 byte lines. Deterministic via fixed seed.
func benchCorpus(nLines int) []byte {
	rng := rand.New(rand.NewSource(42))
	words := []string{"the", "quick", "brown", "fox", "jumps", "over", "lazy",
		"request", "handler", "timeout", "server", "client", "buffer", "stream"}
	var buf []byte
	for i := 0; i < nLines; i++ {
		if rng.Float64() < 0.02 {
			buf = append(buf, fmt.Sprintf("2024-01-15T10:30:%02dZ ERROR connection refused at port %d code=500", i%60, 1000+rng.Intn(9000))...)
		} else {
			for w := 0; w < 10; w++ {
				buf = append(buf, words[rng.Intn(len(words))]...)
				buf = append(buf, ' ')
			}
		}
		buf = append(buf, '\n')
	}
	return buf
}

const benchPattern = `ERROR.*port [0-9]+`

// benchLines is sized so the corpus (~29MB) exceeds L3 — cache-cold match
// extraction is exactly the effect the real workload exhibits.
const benchLines = 500000

// Stage 1: raw match location discovery only.
func BenchmarkPipe1_FindAllIndex(b *testing.B) {
	data := benchCorpus(benchLines)
	re := regex.MustCompile(benchPattern)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		re.FindAllIndex(data, -1)
	}
}

// Stage 2: + MatchSet construction (snippet extraction, Match structs).
func BenchmarkPipe2_FindAll(b *testing.B) {
	data := benchCorpus(benchLines)
	m, err := matcher.NewMatcher([]string{benchPattern}, false, false, false, false, matcher.MatcherOpts{})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		m.FindAll(data)
	}
}

// Stage 3: + text formatting (no color, no line numbers — default pipe output).
func BenchmarkPipe3_FindAllFormat(b *testing.B) {
	data := benchCorpus(benchLines)
	m, err := matcher.NewMatcher([]string{benchPattern}, false, false, false, false, matcher.MatcherOpts{})
	if err != nil {
		b.Fatal(err)
	}
	f := NewTextFormatter(false, false, false, false, 0, false)
	var buf []byte
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		ms := m.FindAll(data)
		r := Result{FilePath: "bench.txt", MatchSet: ms}
		buf = f.Format(buf[:0], r, false)
	}
	_ = buf
}

// Stage 3n: same with line numbers (-n).
func BenchmarkPipe3n_FindAllFormatLineNums(b *testing.B) {
	data := benchCorpus(benchLines)
	m, err := matcher.NewMatcher([]string{benchPattern}, false, false, false, false, matcher.MatcherOpts{NeedLineNums: true})
	if err != nil {
		b.Fatal(err)
	}
	f := NewTextFormatter(true, false, false, false, 0, false)
	var buf []byte
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		ms := m.FindAll(data)
		r := Result{FilePath: "bench.txt", MatchSet: ms}
		buf = f.Format(buf[:0], r, false)
	}
	_ = buf
}
