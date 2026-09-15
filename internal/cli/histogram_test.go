package cli

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/output"
)

func histResult(path, data string, matches []matcher.Match, positions [][2]int) output.Result {
	return output.Result{
		FilePath: path,
		MatchSet: matcher.MatchSet{
			Data:      []byte(data),
			Matches:   matches,
			Positions: positions,
		},
	}
}

func TestHistogramAggregation(t *testing.T) {
	acc := newHistAccum("")

	// File 1: ERR_A twice on one line, ERR_B once.
	acc.addResult(&output.Result{
		FilePath: "a.log",
		MatchSet: matcher.MatchSet{
			Data: []byte("ERR_A x ERR_A\nERR_B\n"),
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 13, PosIdx: 0, PosCount: 2},
				{LineNum: 2, LineStart: 14, LineLen: 5, PosIdx: 2, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}, {8, 13}, {0, 5}},
		},
	})
	// File 2: ERR_A once.
	acc.addResult(&output.Result{
		FilePath: "b.log",
		MatchSet: matcher.MatchSet{
			Data: []byte("ERR_A\n"),
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 5, PosIdx: 0, PosCount: 1},
			},
			Positions: [][2]int{{0, 5}},
		},
	})

	got := string(appendHistogramReport(nil, acc, 0, true))
	for _, want := range []string{
		`{"type":"variant","text":"ERR_A","count":3,"files":2}`,
		`{"type":"variant","text":"ERR_B","count":1,"files":1}`,
		`{"type":"summary","distinct":2,"total":4,"shown":2}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("histogram JSON missing %s in %q", want, got)
		}
	}

	// Most frequent first in text mode; --top applies.
	text := string(appendHistogramReport(nil, acc, 1, false))
	if !strings.Contains(text, "2 distinct match texts; 4 total occurrences (top 1 shown)") ||
		!strings.Contains(text, "3\t2\tERR_A") || strings.Contains(text, "ERR_B") {
		t.Errorf("histogram text = %q", text)
	}
}

func TestHistogramCaptures(t *testing.T) {
	// Matches carrying captures aggregate the selected hole's text.
	acc := newHistAccum("arg")
	data := []byte("f(x)\nf(y)\nf(x)\n")
	ms := matcher.MatchSet{
		Data: data,
		Matches: []matcher.Match{
			{LineNum: 1, LineStart: 0, LineLen: 4, PosIdx: 0, PosCount: 1, CapIdx: 0, CapCount: 1},
			{LineNum: 2, LineStart: 5, LineLen: 4, PosIdx: 1, PosCount: 1, CapIdx: 1, CapCount: 1},
			{LineNum: 3, LineStart: 10, LineLen: 4, PosIdx: 2, PosCount: 1, CapIdx: 2, CapCount: 1},
		},
		Positions: [][2]int{{0, 4}, {0, 4}, {0, 4}},
		Captures: []matcher.Capture{
			{Name: "arg", Start: 2, End: 3},   // x
			{Name: "arg", Start: 7, End: 8},   // y
			{Name: "arg", Start: 12, End: 13}, // x
		},
	}
	acc.addResult(&output.Result{FilePath: "t.go", MatchSet: ms})
	got := string(appendHistogramReport(nil, acc, 0, true))
	if !strings.Contains(got, `{"type":"variant","text":"x","count":2,"files":1}`) ||
		!strings.Contains(got, `{"type":"variant","text":"y","count":1,"files":1}`) {
		t.Errorf("capture histogram = %q", got)
	}
}
