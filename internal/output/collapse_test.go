package output

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

func repeatResult(path string, n int) Result {
	data := []byte(strings.Repeat("same line\n", n))
	ms := matcher.MatchSet{Data: data}
	for i := 0; i < n; i++ {
		ms.Matches = append(ms.Matches, matcher.Match{
			LineNum: i + 1, LineStart: i * 10, LineLen: 9, PosIdx: i, PosCount: 1,
		})
		ms.Positions = append(ms.Positions, [2]int{0, 4})
	}
	return Result{FilePath: path, MatchSet: ms}
}

func TestCollapseFormatter(t *testing.T) {
	inner := NewJSONFormatter()
	cf := NewCollapseFormatter(inner, true)

	// 5 identical lines in one file, 2 more in another: 3 shown, 4
	// collapsed, counted across files.
	buf := cf.Format(nil, repeatResult("a.txt", 5), true)
	buf = cf.Format(buf, repeatResult("b.txt", 2), true)
	got := string(cf.Finish(buf))

	if n := strings.Count(got, `"type":"match"`); n != 3 {
		t.Errorf("shown matches = %d, want 3", n)
	}
	if !strings.Contains(got, `{"type":"collapsed","lines":4,"texts":1}`) {
		t.Errorf("missing collapsed trailer: %q", got)
	}
	// The inner chain still finishes: summary present after trailer.
	if !strings.Contains(got, `"type":"summary"`) {
		t.Errorf("inner Finish not chained: %q", got)
	}
}

func TestCollapseFormatterNoRepeats(t *testing.T) {
	inner := NewJSONFormatter()
	cf := NewCollapseFormatter(inner, true)
	buf := cf.Format(nil, repeatResult("a.txt", 2), true)
	got := string(cf.Finish(buf))
	if strings.Contains(got, `"type":"collapsed"`) {
		t.Errorf("no trailer expected below threshold: %q", got)
	}
	if n := strings.Count(got, `"type":"match"`); n != 2 {
		t.Errorf("shown = %d, want 2", n)
	}
}
