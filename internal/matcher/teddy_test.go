package matcher

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func multiPatternBenchData() []byte {
	rng := rand.New(rand.NewSource(11))
	words := []string{"the", "quick", "brown", "fox", "jumps", "over", "lazy",
		"request", "handler", "timeout", "server", "client", "buffer", "stream",
		"kernel", "driver", "parse", "token"}
	var buf bytes.Buffer
	for buf.Len() < 4<<20 {
		if rng.Float64() < 0.01 {
			buf.WriteString("error errno errcode ")
		}
		fmt.Fprintf(&buf, "%s ", words[rng.Intn(len(words))])
		if rng.Float64() < 0.12 {
			buf.WriteByte('\n')
		}
	}
	return buf.Bytes()
}

// TestTeddyMatcherEquivalence verifies TeddyMatcher and AhoCorasickMatcher
// agree on counts, existence, and per-line match positions.
func TestTeddyMatcherEquivalence(t *testing.T) {
	data := multiPatternBenchData()

	sets := [][]string{
		{"error", "errno", "errcode"},
		{"cat", "dog", "elephant"},
		{"the", "server"},
	}
	for _, patterns := range sets {
		for _, ci := range []bool{false, true} {
			tm := NewTeddyMatcher(patterns, ci, false)
			if tm == nil {
				t.Fatalf("NewTeddyMatcher(%v, ci=%v) nil", patterns, ci)
			}
			ac := NewAhoCorasickMatcher(patterns, ci, false)

			if got, want := tm.CountAll(data), ac.CountAll(data); got != want {
				t.Errorf("%v ci=%v: CountAll teddy=%d ac=%d", patterns, ci, got, want)
			}
			if got, want := tm.MatchExists(data), ac.MatchExists(data); got != want {
				t.Errorf("%v ci=%v: MatchExists teddy=%v ac=%v", patterns, ci, got, want)
			}
			tms, acs := tm.FindAll(data), ac.FindAll(data)
			if len(tms.Matches) != len(acs.Matches) {
				t.Errorf("%v ci=%v: FindAll lines teddy=%d ac=%d",
					patterns, ci, len(tms.Matches), len(acs.Matches))
				continue
			}
			for i := range tms.Matches {
				if tms.Matches[i].LineStart != acs.Matches[i].LineStart ||
					tms.Matches[i].PosCount != acs.Matches[i].PosCount {
					t.Errorf("%v ci=%v: line %d differs (start %d/%d, posCount %d/%d)",
						patterns, ci, i,
						tms.Matches[i].LineStart, acs.Matches[i].LineStart,
						tms.Matches[i].PosCount, acs.Matches[i].PosCount)
					break
				}
			}
		}
	}
}

func benchMulti(b *testing.B, mk func() Matcher) {
	data := multiPatternBenchData()
	m := mk()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		m.CountAll(data)
	}
}

var errSet = []string{"error", "errno", "errcode"}

func BenchmarkMultiPattern_Teddy(b *testing.B) {
	benchMulti(b, func() Matcher { return NewTeddyMatcher(errSet, false, false) })
}
func BenchmarkMultiPattern_AhoCorasick(b *testing.B) {
	benchMulti(b, func() Matcher { return NewAhoCorasickMatcher(errSet, false, false) })
}
func BenchmarkMultiPattern_TeddyCI(b *testing.B) {
	benchMulti(b, func() Matcher { return NewTeddyMatcher(errSet, true, false) })
}
func BenchmarkMultiPattern_AhoCorasickCI(b *testing.B) {
	benchMulti(b, func() Matcher { return NewAhoCorasickMatcher(errSet, true, false) })
}
