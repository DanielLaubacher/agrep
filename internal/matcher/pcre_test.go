//go:build pcre

package matcher

import (
	"bytes"
	"os"
	"testing"
)

// skipIfRace skips PCRE tests when running with -race.
// The go.elara.ws/pcre library uses modernc.org/libc which has pointer
// arithmetic that triggers checkptr (enabled by -race). This is an
// upstream issue, not a real race condition.
func skipIfRace(t *testing.T) {
	t.Helper()
	if os.Getenv("GOGREP_SKIP_PCRE") == "1" {
		t.Skip("skipping PCRE test: checkptr incompatible with modernc.org/libc")
	}
}

func TestPCREMatcher_FindAll(t *testing.T) {
	skipIfRace(t)
	tests := []struct {
		name       string
		pattern    string
		ignoreCase bool
		invert     bool
		input      string
		wantCount  int
		wantLines  []int
	}{
		{
			name:      "simple match",
			pattern:   "hello",
			input:     "hello world\ngoodbye world\n",
			wantCount: 1,
			wantLines: []int{1},
		},
		{
			name:      "no match",
			pattern:   "xyz",
			input:     "hello world\ngoodbye world\n",
			wantCount: 0,
		},
		{
			name:       "case insensitive",
			pattern:    "hello",
			ignoreCase: true,
			input:      "Hello World\nhello world\nHELLO\n",
			wantCount:  3,
			wantLines:  []int{1, 2, 3},
		},
		{
			name:      "lookahead",
			pattern:   `\w+(?=\s+world)`,
			input:     "hello world\ngoodbye world\nfoo bar\n",
			wantCount: 2,
			wantLines: []int{1, 2},
		},
		{
			name:      "lookbehind",
			pattern:   `(?<=hello\s)\w+`,
			input:     "hello world\ngoodbye world\nhello again\n",
			wantCount: 2,
			wantLines: []int{1, 3},
		},
		{
			name:      "backreference",
			pattern:   `(\w+)\s+\1`,
			input:     "the the\nhello world\nbye bye\n",
			wantCount: 2,
			wantLines: []int{1, 3},
		},
		{
			name:      "invert",
			pattern:   "hello",
			invert:    true,
			input:     "hello\nworld\nhello again\n",
			wantCount: 1,
			wantLines: []int{2},
		},
		{
			name:      "regex metacharacters",
			pattern:   `\d+`,
			input:     "abc\n123\ndef456\n",
			wantCount: 2,
			wantLines: []int{2, 3},
		},
		{
			name:      "empty input",
			pattern:   "hello",
			input:     "",
			wantCount: 0,
		},
		{
			name:      "atomic group",
			pattern:   `(?>a+)b`,
			input:     "aab\nab\nbb\n",
			wantCount: 2,
			wantLines: []int{1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := NewPCREMatcher(tt.pattern, tt.ignoreCase, tt.invert)
			if err != nil {
				t.Fatalf("NewPCREMatcher() error: %v", err)
			}
			defer m.Close()
			m.needLineNums = true

			ms := m.FindAll([]byte(tt.input))
			if len(ms.Matches) != tt.wantCount {
				t.Errorf("got %d matches, want %d", len(ms.Matches), tt.wantCount)
			}
			for i, wantLine := range tt.wantLines {
				if i >= len(ms.Matches) {
					break
				}
				if ms.Matches[i].LineNum != wantLine {
					t.Errorf("match[%d].LineNum = %d, want %d", i, ms.Matches[i].LineNum, wantLine)
				}
			}
		})
	}
}

func TestPCREMatcher_Positions(t *testing.T) {
	skipIfRace(t)
	m, err := NewPCREMatcher("ab", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ms := m.FindAll([]byte("xabcabd\n"))
	if len(ms.Matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(ms.Matches))
	}
	positions := ms.MatchPositions(0)
	if len(positions) != 2 {
		t.Fatalf("got %d positions, want 2", len(positions))
	}
	if positions[0] != [2]int{1, 3} {
		t.Errorf("position[0] = %v, want [1,3]", positions[0])
	}
	if positions[1] != [2]int{4, 6} {
		t.Errorf("position[1] = %v, want [4,6]", positions[1])
	}
}

func TestPCREMatcher_FindLine(t *testing.T) {
	skipIfRace(t)
	m, err := NewPCREMatcher("test", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	ms, ok := m.FindLine([]byte("this is a test"), 5, 100)
	if !ok {
		t.Fatal("expected match")
	}
	if ms.Matches[0].LineNum != 5 {
		t.Errorf("LineNum = %d, want 5", ms.Matches[0].LineNum)
	}
	if ms.Matches[0].ByteOffset != 100 {
		t.Errorf("ByteOffset = %d, want 100", ms.Matches[0].ByteOffset)
	}

	_, ok = m.FindLine([]byte("no match here"), 1, 0)
	if ok {
		t.Error("expected no match")
	}
}

func TestPCREMatcher_InvalidPattern(t *testing.T) {
	skipIfRace(t)
	_, err := NewPCREMatcher("[invalid", false, false)
	if err == nil {
		t.Error("expected error for invalid PCRE pattern")
	}
}

func TestNewMatcher_PCRE(t *testing.T) {
	skipIfRace(t)
	m, err := NewMatcher([]string{`\w+(?=\s+world)`}, false, true, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	ms := m.FindAll([]byte("hello world\nfoo bar\n"))
	if len(ms.Matches) != 1 {
		t.Errorf("got %d matches, want 1", len(ms.Matches))
	}
}

func TestNewMatcher_PCRE_Multi(t *testing.T) {
	skipIfRace(t)
	m, err := NewMatcher([]string{"hello", "world"}, false, true, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}

	ms := m.FindAll([]byte("hello\nfoo\nworld\n"))
	if len(ms.Matches) != 2 {
		t.Errorf("got %d matches, want 2", len(ms.Matches))
	}
}

func BenchmarkPCRE_Simple(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	m, err := NewPCREMatcher("lazy", false, false)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		m.FindAll(data)
	}
}

func BenchmarkPCRE_Lookahead(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	m, err := NewPCREMatcher(`\w+(?=\s+dog)`, false, false)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		m.FindAll(data)
	}
}

func BenchmarkPCRE_NoMatch(b *testing.B) {
	data := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog\n"), 10000)
	m, err := NewPCREMatcher("zzzzz", false, false)
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close()

	b.ResetTimer()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		m.FindAll(data)
	}
}
