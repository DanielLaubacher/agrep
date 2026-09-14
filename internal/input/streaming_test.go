package input

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

func TestStreamingReader_Lines(t *testing.T) {
	input := "line one\nline two\nline three\n"
	r := NewStreamingReader(strings.NewReader(input))
	lines := r.Lines()

	var collected []StreamLine
	for line := range lines {
		if line.Err != nil {
			t.Fatalf("unexpected error: %v", line.Err)
		}
		collected = append(collected, line)
	}

	if len(collected) != 3 {
		t.Fatalf("got %d lines, want 3", len(collected))
	}

	wantTexts := []string{"line one", "line two", "line three"}
	for i, want := range wantTexts {
		if string(collected[i].Data) != want {
			t.Errorf("line[%d] = %q, want %q", i, collected[i].Data, want)
		}
		if collected[i].LineNum != i+1 {
			t.Errorf("line[%d].LineNum = %d, want %d", i, collected[i].LineNum, i+1)
		}
	}
}

func TestStreamingReader_EmptyInput(t *testing.T) {
	r := NewStreamingReader(strings.NewReader(""))
	lines := r.Lines()

	count := 0
	for range lines {
		count++
	}
	if count != 0 {
		t.Errorf("got %d lines, want 0", count)
	}
}

func TestStreamingReader_NoTrailingNewline(t *testing.T) {
	r := NewStreamingReader(strings.NewReader("no newline"))
	lines := r.Lines()

	var collected []StreamLine
	for line := range lines {
		collected = append(collected, line)
	}

	if len(collected) != 1 {
		t.Fatalf("got %d lines, want 1", len(collected))
	}
	if string(collected[0].Data) != "no newline" {
		t.Errorf("got %q, want %q", collected[0].Data, "no newline")
	}
}

func TestSearchStream_BasicMatch(t *testing.T) {
	input := "hello world\ngoodbye world\nhello again\n"
	m, err := matcher.NewRegexMatcher("hello", false, false)
	if err != nil {
		t.Fatal(err)
	}

	results := SearchStream(strings.NewReader(input), m, 0, 0)

	var collected []matcher.MatchSet
	for ms := range results {
		collected = append(collected, ms)
	}

	if len(collected) != 2 {
		t.Fatalf("got %d matches, want 2", len(collected))
	}
	if collected[0].Matches[0].LineNum != 1 {
		t.Errorf("match[0].LineNum = %d, want 1", collected[0].Matches[0].LineNum)
	}
	if collected[1].Matches[0].LineNum != 3 {
		t.Errorf("match[1].LineNum = %d, want 3", collected[1].Matches[0].LineNum)
	}
}

func TestSearchStream_NoMatch(t *testing.T) {
	input := "abc\ndef\n"
	m, err := matcher.NewRegexMatcher("xyz", false, false)
	if err != nil {
		t.Fatal(err)
	}

	results := SearchStream(strings.NewReader(input), m, 0, 0)

	count := 0
	for range results {
		count++
	}
	if count != 0 {
		t.Errorf("got %d matches, want 0", count)
	}
}

func TestSearchStream_ContextAfter(t *testing.T) {
	input := "match\nafter1\nafter2\nno\n"
	m, err := matcher.NewRegexMatcher("match", false, false)
	if err != nil {
		t.Fatal(err)
	}

	results := SearchStream(strings.NewReader(input), m, 0, 2)

	var collected []matcher.MatchSet
	for ms := range results {
		collected = append(collected, ms)
	}

	// match + 2 context after lines
	if len(collected) != 3 {
		t.Fatalf("got %d results, want 3", len(collected))
	}
	if collected[0].Matches[0].IsContext {
		t.Error("match[0] should not be context")
	}
	if !collected[1].Matches[0].IsContext {
		t.Error("match[1] should be context")
	}
	if !collected[2].Matches[0].IsContext {
		t.Error("match[2] should be context")
	}
}

func TestSearchStream_ContextBefore(t *testing.T) {
	input := "before1\nbefore2\nmatch\nno\n"
	m, err := matcher.NewRegexMatcher("match", false, false)
	if err != nil {
		t.Fatal(err)
	}

	results := SearchStream(strings.NewReader(input), m, 2, 0)

	var collected []matcher.MatchSet
	for ms := range results {
		collected = append(collected, ms)
	}

	// 2 context before lines + match
	if len(collected) != 3 {
		t.Fatalf("got %d results, want 3", len(collected))
	}
	if !collected[0].Matches[0].IsContext {
		t.Error("match[0] should be context")
	}
	if !collected[1].Matches[0].IsContext {
		t.Error("match[1] should be context")
	}
	if collected[2].Matches[0].IsContext {
		t.Error("match[2] should not be context")
	}
}

func TestSearchStream_ContextBeforeAndAfter(t *testing.T) {
	input := "a\nb\nmatch\nd\ne\n"
	m, err := matcher.NewRegexMatcher("match", false, false)
	if err != nil {
		t.Fatal(err)
	}

	results := SearchStream(strings.NewReader(input), m, 1, 1)

	var collected []matcher.MatchSet
	for ms := range results {
		collected = append(collected, ms)
	}

	// b(ctx) + match + d(ctx)
	if len(collected) != 3 {
		t.Fatalf("got %d results, want 3", len(collected))
	}
	lineBytes0 := collected[0].LineBytes(0)
	if string(lineBytes0) != "b" || !collected[0].Matches[0].IsContext {
		t.Errorf("collected[0] = %q (context=%v), want 'b' (context=true)", lineBytes0, collected[0].Matches[0].IsContext)
	}
	lineBytes1 := collected[1].LineBytes(0)
	if string(lineBytes1) != "match" || collected[1].Matches[0].IsContext {
		t.Errorf("collected[1] = %q (context=%v), want 'match' (context=false)", lineBytes1, collected[1].Matches[0].IsContext)
	}
	lineBytes2 := collected[2].LineBytes(0)
	if string(lineBytes2) != "d" || !collected[2].Matches[0].IsContext {
		t.Errorf("collected[2] = %q (context=%v), want 'd' (context=true)", lineBytes2, collected[2].Matches[0].IsContext)
	}
}
