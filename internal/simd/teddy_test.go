package simd

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"
)

// naiveMultiFind is the reference: every (pos, pattern) where pattern
// occurs at pos, sorted by (pos, pattern).
func naiveMultiFind(data []byte, patterns [][]byte, ci bool) [][2]int {
	var out [][2]int
	for pi, p := range patterns {
		for i := 0; i+len(p) <= len(data); i++ {
			var ok bool
			if ci {
				ok = matchCaseInsensitive(data[i:i+len(p)], p)
			} else {
				ok = bytes.Equal(data[i:i+len(p)], p)
			}
			if ok {
				out = append(out, [2]int{i, pi})
			}
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a][0] != out[b][0] {
			return out[a][0] < out[b][0]
		}
		return out[a][1] < out[b][1]
	})
	return out
}

func teddyCollect(t *Teddy, data []byte) [][2]int {
	var out [][2]int
	t.Scan(data, func(pos, pat int) bool {
		out = append(out, [2]int{pos, pat})
		return true
	})
	return out
}

func TestTeddyEquivalence(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		ci       bool
	}{
		{"errset", []string{"error", "errno", "errcode"}, false},
		{"errset-ci", []string{"error", "errno", "errcode"}, true},
		{"diverse", []string{"cat", "dog", "fox", "elephant"}, false},
		{"overlap", []string{"aa", "aaa"}, false},
		{"substr", []string{"error", "rror", "or"}, false},
		{"short", []string{"ab", "ba"}, false},
		{"eight", []string{"one", "two", "three", "four", "five", "sixx", "seven", "eight"}, false},
		{"keywords-ci", []string{"define", "include", "ifndef"}, true},
	}

	rng := rand.New(rand.NewSource(3))
	words := []string{"error", "errno", "errcode", "cat", "dog", "fox", "elephant",
		"aaa", "aaaa", "ab", "ba", "one", "seven", "the", "quick", "brown",
		"DEFINE", "Include", "IFNDEF", "ERROR", "ErrNo"}
	var corpus bytes.Buffer
	for corpus.Len() < 100000 {
		fmt.Fprintf(&corpus, "%s ", words[rng.Intn(len(words))])
		if rng.Float64() < 0.1 {
			corpus.WriteByte('\n')
		}
	}
	data := corpus.Bytes()

	// Include boundary-sized inputs to exercise the scalar tail.
	inputs := [][]byte{data, data[:33], data[:32], data[:31], data[:7], data[:2], nil}

	for _, tc := range cases {
		pats := make([][]byte, len(tc.patterns))
		for i, p := range tc.patterns {
			b := []byte(p)
			if tc.ci {
				b = bytes.ToLower(b)
			}
			pats[i] = b
		}
		td := NewTeddy(pats, tc.ci)
		if td == nil {
			t.Fatalf("%s: NewTeddy returned nil", tc.name)
		}
		for ii, in := range inputs {
			got := teddyCollect(td, in)
			want := naiveMultiFind(in, pats, tc.ci)
			if len(got) != len(want) {
				t.Fatalf("%s input %d: got %d matches, want %d", tc.name, ii, len(got), len(want))
			}
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("%s input %d: match %d = %v, want %v", tc.name, ii, i, got[i], want[i])
				}
			}
		}
	}
}

func TestTeddyRejects(t *testing.T) {
	if NewTeddy([][]byte{[]byte("ab")}, false) != nil {
		t.Error("single pattern should be rejected")
	}
	if NewTeddy([][]byte{[]byte("ab"), []byte("x")}, false) != nil {
		t.Error("1-byte pattern should be rejected")
	}
	nine := make([][]byte, 9)
	for i := range nine {
		nine[i] = []byte(fmt.Sprintf("pat%d", i))
	}
	if NewTeddy(nine, false) != nil {
		t.Error("9 patterns should be rejected")
	}
}

// --- Benchmarks: Teddy vs the byte-at-a-time Aho-Corasick it replaces ---

func teddyBenchData() []byte {
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

func benchTeddy(b *testing.B, patterns []string, ci bool) {
	data := teddyBenchData()
	pats := make([][]byte, len(patterns))
	for i, p := range patterns {
		pats[i] = []byte(p)
		if ci {
			pats[i] = bytes.ToLower(pats[i])
		}
	}
	td := NewTeddy(pats, ci)
	if td == nil {
		b.Fatal("NewTeddy nil")
	}
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		n := 0
		td.Scan(data, func(_, _ int) bool { n++; return true })
	}
}

func BenchmarkTeddy_ErrSet(b *testing.B)    { benchTeddy(b, []string{"error", "errno", "errcode"}, false) }
func BenchmarkTeddy_ErrSetCI(b *testing.B)  { benchTeddy(b, []string{"error", "errno", "errcode"}, true) }
func BenchmarkTeddy_Diverse(b *testing.B)   { benchTeddy(b, []string{"cat", "dog", "elephant"}, false) }
func BenchmarkTeddy_Keywords(b *testing.B)  { benchTeddy(b, []string{"define", "include", "ifndef", "pragma"}, false) }
