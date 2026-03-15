package output

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dl/gogrep/internal/matcher"
)

func TestJSONFormatter_BasicMatch(t *testing.T) {
	f := NewJSONFormatter()
	data := []byte("hello world\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 11, ByteOffset: 0, PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}},
		},
	}

	got := string(f.Format(nil, result, false))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var jm map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &jm); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	if jm["type"] != "match" {
		t.Errorf("type = %v, want match", jm["type"])
	}
	if jm["file"] != "test.txt" {
		t.Errorf("file = %v, want test.txt", jm["file"])
	}
	if jm["text"] != "hello world" {
		t.Errorf("text = %v, want hello world", jm["text"])
	}
	if jm["line_number"].(float64) != 1 {
		t.Errorf("line_number = %v, want 1", jm["line_number"])
	}
}

func TestJSONFormatter_MultipleMatches(t *testing.T) {
	f := NewJSONFormatter()
	data := []byte("first\n???????????????\nthird\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 5, ByteOffset: 0, PosIdx: 0, PosCount: 1},
				{LineNum: 3, LineStart: 22, LineLen: 5, ByteOffset: 20, PosIdx: 1, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}, {0, 5}},
		},
	}

	got := string(f.Format(nil, result, true))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}

	// Verify each line is valid JSON
	for i, line := range lines {
		var jm map[string]any
		if err := json.Unmarshal([]byte(line), &jm); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
		}
	}
}

func TestJSONFormatter_ContextLinesSkipped(t *testing.T) {
	f := NewJSONFormatter()
	data := []byte("context\nmatch\ncontext\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 7, IsContext: true},
				{LineNum: 2, LineStart: 8, LineLen: 5, PosIdx: 0, PosCount: 1},
				{LineNum: 3, LineStart: 14, LineLen: 7, IsContext: true},
			},
			Positions: [][2]int{{0, 5}},
		},
	}

	got := string(f.Format(nil, result, false))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1 (context should be skipped)", len(lines))
	}
}

func TestJSONFormatter_NoMatches(t *testing.T) {
	f := NewJSONFormatter()
	result := Result{
		FilePath: "test.txt",
	}

	got := f.Format(nil, result, false)
	if got != nil {
		t.Errorf("got %q, want nil for no matches", got)
	}
}

func TestJSONFormatter_MatchPositions(t *testing.T) {
	f := NewJSONFormatter()
	data := []byte("hello world hello\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 17, ByteOffset: 0, PosIdx: 0, PosCount: 2},
			},
			Positions: [][2]int{{0, 5}, {12, 17}},
		},
	}

	got := string(f.Format(nil, result, false))
	var jm map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &jm); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	matches := jm["matches"].([]any)
	if len(matches) != 2 {
		t.Fatalf("got %d match positions, want 2", len(matches))
	}

	pos0 := matches[0].(map[string]any)
	if pos0["start"].(float64) != 0 || pos0["end"].(float64) != 5 {
		t.Errorf("position[0] = %v, want {start:0, end:5}", pos0)
	}
}
