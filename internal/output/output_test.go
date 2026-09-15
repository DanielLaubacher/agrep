package output

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

// makeMatchSet creates a MatchSet from line content strings and positions for testing.
func makeMatchSet(data []byte, matches []matcher.Match, positions [][2]int) matcher.MatchSet {
	return matcher.MatchSet{
		Data:      data,
		Matches:   matches,
		Positions: positions,
	}
}

func TestTextFormatter_SingleFile(t *testing.T) {
	f := NewTextFormatter(true, false, false, false, 0, false)
	data := []byte("hello world\n???\nhello again\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 11, PosIdx: 0, PosCount: 1},
				{LineNum: 3, LineStart: 16, LineLen: 11, PosIdx: 1, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}, {0, 5}},
		},
	}

	got := string(f.Format(nil, result, false))
	want := "1:hello world\n3:hello again\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTextFormatter_MultiFile(t *testing.T) {
	f := NewTextFormatter(true, false, false, false, 0, false)
	data := []byte("?????\n?????\n?????\n?????\nmatch line\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 5, LineStart: 24, LineLen: 10},
			},
		},
	}

	got := string(f.Format(nil, result, true))
	want := "test.txt:5:match line\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTextFormatter_CountOnly(t *testing.T) {
	f := NewTextFormatter(false, true, false, false, 0, false)
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Matches: make([]matcher.Match, 3),
		},
	}

	// Single file
	got := string(f.Format(nil, result, false))
	if got != "3\n" {
		t.Errorf("count single: got %q, want %q", got, "3\n")
	}

	// Multi file
	got = string(f.Format(nil, result, true))
	if got != "test.txt:3\n" {
		t.Errorf("count multi: got %q, want %q", got, "test.txt:3\n")
	}
}

func TestTextFormatter_FilesOnly(t *testing.T) {
	f := NewTextFormatter(false, false, true, false, 0, false)

	// Has matches
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Matches: make([]matcher.Match, 1),
		},
	}
	got := string(f.Format(nil, result, true))
	if got != "test.txt\n" {
		t.Errorf("got %q, want %q", got, "test.txt\n")
	}

	// No matches
	result.MatchSet.Matches = nil
	got = string(f.Format(nil, result, true))
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestTextFormatter_MaxColumns(t *testing.T) {
	f := NewTextFormatter(true, false, false, false, 20, false)
	data := []byte("short\nthis is a very long line that exceeds the max columns limit\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 5, PosIdx: 0, PosCount: 1},
				{LineNum: 2, LineStart: 6, LineLen: 59, PosIdx: 1, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}, {0, 4}},
		},
	}

	got := string(f.Format(nil, result, false))
	// Match at [0,4], center=2, window starts at 0; the right edge
	// snaps back to the word boundary (no mid-word cut, no trailing
	// space), and a "..." marks the cut so it never reads as a short line.
	want := "1:short\n2:this is a very long...\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTextFormatter_MaxColumnsClipsPositions(t *testing.T) {
	// Match at [6,11] in a 26-char line, maxColumns=10
	// center=8, window centered: start=3, end=13
	f := NewTextFormatter(false, false, false, false, 10, false)
	data := []byte("hello world and more stuff\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 26, PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{6, 11}},
		},
	}

	got := string(f.Format(nil, result, false))
	// Window [3,13) with the right edge snapped to the word boundary
	// after "world" — the trailing " a" fragment is dropped, and both
	// cut edges (window starts and ends mid-line) get a "..." marker.
	want := "...lo world...\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestTextFormatter_MaxColumnsCentered(t *testing.T) {
	// Match deep in a long line — should be centered in the window
	f := NewTextFormatter(false, false, false, false, 60, false)
	line := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa benchmark bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	data := []byte(line + "\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: len(line), PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{43, 52}},
		},
	}

	got := string(f.Format(nil, result, false))
	if len(got) == 0 {
		t.Fatal("no output")
	}
	// Output should contain "benchmark" centered in 60-char window
	if !strings.Contains(got, "benchmark") {
		t.Errorf("output %q does not contain match word 'benchmark'", got)
	}
	// Output line (minus newline) should be at most maxColumns, plus up
	// to two 3-byte "..." truncation markers.
	line2 := strings.TrimSuffix(got, "\n")
	if len(line2) > 60+2*len("...") {
		t.Errorf("output line length %d exceeds maxColumns 60 (plus markers)", len(line2))
	}
}
