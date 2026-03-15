package simd

import (
	"bytes"
	"testing"
)

func TestIndex(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		pattern string
		want    int
	}{
		{"empty pattern", "hello", "", 0},
		{"empty data", "", "abc", -1},
		{"single byte", "hello", "l", 2},
		{"at start", "abcdef", "abc", 0},
		{"at end", "abcdef", "def", 3},
		{"middle", "abcdef", "cde", 2},
		{"not found", "abcdef", "xyz", -1},
		{"full match", "abc", "abc", 0},
		{"two byte", "abcdef", "cd", 2},
		{"pattern longer than data", "ab", "abcdef", -1},
		{"repeated", "ababab", "ab", 0},
		{"across 32 boundary", string(make([]byte, 30)) + "XY" + string(make([]byte, 30)), "XY", 30},
		{"at 32", string(make([]byte, 32)) + "XY", "XY", 32},
		{"at 64", string(make([]byte, 64)) + "XY", "XY", 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Index([]byte(tt.data), []byte(tt.pattern))
			want := bytes.Index([]byte(tt.data), []byte(tt.pattern))
			if got != want {
				t.Errorf("Index(%q, %q) = %d, want %d", tt.data, tt.pattern, got, want)
			}
		})
	}
}

func TestIndex_LargeData(t *testing.T) {
	sizes := []int{31, 32, 33, 63, 64, 65, 100, 256, 1000, 4096, 65536}
	patternLens := []int{2, 3, 5, 10, 20}

	for _, size := range sizes {
		for _, plen := range patternLens {
			if plen >= size {
				continue
			}

			data := make([]byte, size)
			for i := range data {
				data[i] = 'a'
			}

			pattern := make([]byte, plen)
			for i := range pattern {
				pattern[i] = 'X'
			}

			// Place pattern at various positions
			positions := []int{0, size/4 - plen/2, size/2 - plen/2, size - plen}
			for _, pos := range positions {
				if pos < 0 {
					continue
				}
				copy(data[pos:], pattern)
				got := Index(data, pattern)
				want := bytes.Index(data, pattern)
				if got != want {
					t.Errorf("size=%d plen=%d pos=%d: Index = %d, want %d", size, plen, pos, got, want)
				}
				// Restore
				for i := pos; i < pos+plen && i < size; i++ {
					data[i] = 'a'
				}
			}

			// Not found
			got := Index(data, pattern)
			if got != -1 {
				t.Errorf("size=%d plen=%d not found: Index = %d, want -1", size, plen, got)
			}
		}
	}
}

func TestIndexAll(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		pattern string
		want    []int
	}{
		{"empty", "hello", "", nil},
		{"single byte all", "ababa", "a", []int{0, 2, 4}},
		{"not found", "abcdef", "xyz", nil},
		{"one match", "hello world", "world", []int{6}},
		{"multiple", "abXabXab", "ab", []int{0, 3, 6}},
		{"non-overlapping", "aaa", "aa", []int{0}},
		{"adjacent", "abab", "ab", []int{0, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IndexAll([]byte(tt.data), []byte(tt.pattern))
			if len(got) != len(tt.want) {
				t.Errorf("IndexAll(%q, %q) = %v, want %v", tt.data, tt.pattern, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("IndexAll(%q, %q)[%d] = %d, want %d", tt.data, tt.pattern, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestIndexAll_LargeData(t *testing.T) {
	// 10K lines, pattern every 100th line
	var buf []byte
	var expected []int
	pattern := []byte("MARKER")
	for i := range 10000 {
		if i%100 == 0 {
			pos := len(buf)
			expected = append(expected, pos)
			buf = append(buf, pattern...)
			buf = append(buf, '\n')
		} else {
			buf = append(buf, []byte("the quick brown fox\n")...)
		}
	}

	got := IndexAll(buf, pattern)
	if len(got) != len(expected) {
		t.Fatalf("got %d matches, want %d", len(got), len(expected))
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Errorf("match[%d] = %d, want %d", i, got[i], expected[i])
		}
	}
}

func TestIndexCaseInsensitive(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		pattern string
		want    int
	}{
		{"exact", "hello world", "hello", 0},
		{"upper data", "HELLO WORLD", "hello", 0},
		{"mixed", "hElLo WoRlD", "hello", 0},
		{"not found", "abcdef", "xyz", -1},
		{"middle", "xxxHELLOxxx", "hello", 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IndexCaseInsensitive([]byte(tt.data), []byte(tt.pattern))
			if got != tt.want {
				t.Errorf("IndexCaseInsensitive(%q, %q) = %d, want %d", tt.data, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestIndexAllCaseInsensitive(t *testing.T) {
	data := []byte("Hello HELLO hElLo world")
	got := IndexAllCaseInsensitive(data, []byte("hello"))
	want := []int{0, 6, 12}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

// Benchmarks

func BenchmarkIndex_SIMD_Short(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		Index(data, pattern)
	}
}

func BenchmarkIndex_Stdlib_Short(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		bytes.Index(data, pattern)
	}
}

func BenchmarkIndexAll_SIMD(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		IndexAll(data, pattern)
	}
}

func BenchmarkIndexAll_BoyerMoore(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	bm := &boyerMooreForBench{}
	bm.init(pattern)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		bm.search(data)
	}
}

func BenchmarkIndexAll_BytesIndex(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		d := data
		for {
			idx := bytes.Index(d, pattern)
			if idx < 0 {
				break
			}
			d = d[idx+len(pattern):]
		}
	}
}

// Sparse match benchmark: pattern only appears rarely
func BenchmarkIndexAll_SIMD_Sparse(b *testing.B) {
	var buf []byte
	for i := range 10000 {
		if i%1000 == 0 {
			buf = append(buf, []byte("ERROR: connection refused\n")...)
		} else {
			buf = append(buf, []byte("the quick brown fox jumps over the lazy dog\n")...)
		}
	}
	pattern := []byte("ERROR")
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for b.Loop() {
		IndexAll(buf, pattern)
	}
}

func BenchmarkIndexAll_BytesIndex_Sparse(b *testing.B) {
	var buf []byte
	for i := range 10000 {
		if i%1000 == 0 {
			buf = append(buf, []byte("ERROR: connection refused\n")...)
		} else {
			buf = append(buf, []byte("the quick brown fox jumps over the lazy dog\n")...)
		}
	}
	pattern := []byte("ERROR")
	b.SetBytes(int64(len(buf)))
	b.ResetTimer()
	for b.Loop() {
		d := buf
		for {
			idx := bytes.Index(d, pattern)
			if idx < 0 {
				break
			}
			d = d[idx+len(pattern):]
		}
	}
}

func BenchmarkIndex_SIMD_NoMatch(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("ZZZZZ")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		Index(data, pattern)
	}
}

func BenchmarkIndex_Stdlib_NoMatch(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("ZZZZZ")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		bytes.Index(data, pattern)
	}
}

func BenchmarkIndexCaseInsensitive_SIMD(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	pattern := []byte("lazy")
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for b.Loop() {
		IndexCaseInsensitive(data, pattern)
	}
}

// Simple Boyer-Moore for benchmark comparison (avoids import cycle)
type boyerMooreForBench struct {
	pattern []byte
	badChar [256]int
}

func (m *boyerMooreForBench) init(pattern []byte) {
	m.pattern = pattern
	pLen := len(pattern)
	for i := range m.badChar {
		m.badChar[i] = pLen
	}
	for i := 0; i < pLen-1; i++ {
		m.badChar[pattern[i]] = pLen - 1 - i
	}
}

func (m *boyerMooreForBench) search(text []byte) []int {
	var offsets []int
	tLen := len(text)
	pLen := len(m.pattern)
	if pLen == 0 || tLen < pLen {
		return offsets
	}

	i := pLen - 1
	for i < tLen {
		j := pLen - 1
		k := i
		for j >= 0 && text[k] == m.pattern[j] {
			j--
			k--
		}
		if j < 0 {
			offsets = append(offsets, k+1)
			i += pLen
		} else {
			shift := max(m.badChar[text[i]], 1)
			i += shift
		}
	}
	return offsets
}
