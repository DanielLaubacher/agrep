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

func TestJSONFormatter_SummaryTrailer(t *testing.T) {
	f := NewJSONFormatter()
	data := []byte("hello world\n")
	result := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 11, PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}},
		},
	}
	buf := f.Format(nil, result, false)
	got := string(f.Finish(buf))
	if !strings.HasSuffix(strings.TrimSpace(got), `{"type":"summary","files":1,"lines":1}`) {
		t.Errorf("missing summary trailer, got %q", got)
	}
}

func TestJSONFormatter_EmptySummary(t *testing.T) {
	f := NewJSONFormatter()
	got := strings.TrimSpace(string(f.Finish(nil)))
	if got != `{"type":"summary","files":0,"lines":0}` {
		t.Errorf("zero-result summary = %q", got)
	}
}

func TestJSONFormatter_CountMode(t *testing.T) {
	f := NewJSONFormatter()
	f.CountOnly = true
	buf := f.Format(nil, Result{FilePath: "a.txt", MatchCount: 7}, true)
	// Zero-count results are skipped, like text -c.
	buf = f.Format(buf, Result{FilePath: "b.txt"}, true)
	got := string(f.Finish(buf))
	want := `{"type":"count","file":"a.txt","count":7}` + "\n" +
		`{"type":"summary","files":1,"lines":7}` + "\n"
	if got != want {
		t.Errorf("count mode = %q, want %q", got, want)
	}
}

func TestJSONFormatter_FilesMode(t *testing.T) {
	f := NewJSONFormatter()
	f.FilesOnly = true
	r := Result{FilePath: "a.txt", MatchSet: matcher.MatchSet{Matches: []matcher.Match{{}}}}
	got := string(f.Finish(f.Format(nil, r, true)))
	// No degenerate match object, no fabricated line counts.
	want := `{"type":"file","file":"a.txt"}` + "\n" +
		`{"type":"summary","files":1}` + "\n"
	if got != want {
		t.Errorf("files mode = %q, want %q", got, want)
	}
	if strings.Contains(got, `"span"`) || strings.Contains(got, `"lines"`) {
		t.Errorf("files mode leaked span/lines: %q", got)
	}
}

func TestJSONFormatter_BatchQueryTotals(t *testing.T) {
	f := NewJSONFormatter()
	f.CountOnly = true
	// Registration makes zero-hit queries appear explicitly.
	f.RegisterQueries([]string{"alpha", "beta"})
	buf := f.Format(nil, Result{FilePath: "a.txt", MatchCount: 3, Query: "alpha"}, true)
	got := string(f.Finish(buf))
	if !strings.Contains(got, `"queries":[{"query":"alpha","files":1,"lines":3},{"query":"beta","files":0,"lines":0}]`) {
		t.Errorf("batch summary missing per-query totals: %q", got)
	}
}

func TestBudgetWrapEmitsSingleSummary(t *testing.T) {
	inner := NewJSONFormatter()
	bf := NewBudgetFormatter(inner, 1000, true)
	data := []byte("hello world\n")
	r := Result{
		FilePath: "test.txt",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 11, PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}},
		},
	}
	got := string(bf.Finish(bf.Format(nil, r, false)))
	if strings.Count(got, `"type":"summary"`) != 1 {
		t.Errorf("budget-wrapped output must emit exactly one summary: %q", got)
	}
	if !strings.Contains(got, `"shown_lines"`) {
		t.Errorf("budget summary shape lost: %q", got)
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
