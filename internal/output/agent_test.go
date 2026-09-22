package output

import (
	"strconv"
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

func TestSectionHeading(t *testing.T) {
	data := []byte("# Title\nintro text\n## Section A\nalpha line\nbeta line\n## Section B\ngamma line\n")

	cases := []struct {
		needle string
		want   string
	}{
		{"intro text", "# Title"},
		{"alpha line", "## Section A"},
		{"beta line", "## Section A"},
		{"gamma line", "## Section B"},
		{"# Title", "# Title"}, // heading is its own section
	}
	for _, tc := range cases {
		start := strings.Index(string(data), tc.needle)
		if start < 0 {
			t.Fatalf("needle %q not in data", tc.needle)
		}
		got := sectionHeading(data, start)
		if string(got) != tc.want {
			t.Errorf("sectionHeading(%q) = %q, want %q", tc.needle, got, tc.want)
		}
	}

	if h := sectionHeading([]byte("no headings here\nat all\n"), 20); h != nil {
		t.Errorf("expected nil for heading-less data, got %q", h)
	}
}

// fakeResult builds a single-line match result over data.
func fakeResult(path string, data []byte, lineStart, lineLen int) Result {
	return Result{
		FilePath: path,
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{{
				LineNum:    1,
				LineStart:  lineStart,
				LineLen:    lineLen,
				ByteOffset: int64(lineStart),
				PosCount:   0,
			}},
		},
	}
}

func TestBudgetFormatter(t *testing.T) {
	inner := NewTextFormatter(false, false, false, false, 0, false)
	// ~10 token budget = 40 bytes.
	bf := NewBudgetFormatter(inner, 10, false)

	line := []byte("this line is definitely longer than forty bytes of output text\n")
	var buf []byte
	shown := 0
	for i := 0; i < 5; i++ {
		before := len(buf)
		buf = bf.Format(buf, fakeResult("f.txt", line, 0, len(line)-1), false)
		if len(buf) > before {
			shown++
		}
	}
	buf = bf.Finish(buf)

	if shown != 1 {
		t.Errorf("expected exactly 1 result within budget, got %d", shown)
	}
	out := string(buf)
	if !strings.Contains(out, "omitted 4 lines in 4 more files") {
		t.Errorf("missing omission summary, got:\n%s", out)
	}
	if !strings.Contains(out, "showing 1 matching lines in 1 files") {
		t.Errorf("missing shown summary, got:\n%s", out)
	}
}

// The --max-tokens JSON trailer is a single {"type":"summary",...} record
// carrying true totals plus the shown/omitted breakdown — not a second,
// undocumented "budget_summary" record with its own (shown-only) numbers
// that can disagree with the first (report bug 1).
func TestBudgetFormatterJSONTrailer(t *testing.T) {
	bf := NewBudgetFormatter(NewJSONFormatter(), 1, true)
	line := []byte("some matching line content that exceeds four bytes\n")
	var buf []byte
	buf = bf.Format(buf, fakeResult("f.txt", line, 0, len(line)-1), false)
	buf = bf.Format(buf, fakeResult("g.txt", line, 0, len(line)-1), false)
	buf = bf.Finish(buf)
	out := string(buf)
	if strings.Contains(out, `"type":"budget_summary"`) {
		t.Errorf("undocumented budget_summary record still emitted:\n%s", out)
	}
	if strings.Count(out, `"type":"summary"`) != 1 {
		t.Errorf("want exactly one summary record:\n%s", out)
	}
	// True totals (both files were matched) alongside the shown/omitted
	// breakdown, in the one summary record.
	for _, want := range []string{
		`"files":2`, `"lines":2`, `"errors":0`,
		`"shown_lines":1`, `"shown_files":1`,
		`"omitted_lines":1`, `"omitted_files":1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %s:\n%s", want, out)
		}
	}
}

// Per-query totals in the summary must be TRUE totals (found, not just
// shown) even under a budget that cuts one query's output entirely
// (report bug 1's --batch reproduction).
func TestBudgetFormatterJSONPerQueryTrueTotals(t *testing.T) {
	bf := NewBudgetFormatter(NewJSONFormatter(), 1, true)
	bf.RegisterQueries([]string{"retry", "backoff"})
	var buf []byte
	retryLine := []byte("retry logic goes here in this line\n")
	backoffLine := []byte("backoff logic goes here in this line\n")
	r1 := fakeResult("f.txt", retryLine, 0, len(retryLine)-1)
	r1.Query = "retry"
	r2 := fakeResult("f.txt", backoffLine, 0, len(backoffLine)-1)
	r2.Query = "backoff"
	buf = bf.Format(buf, r1, false)
	buf = bf.Format(buf, r2, false)
	buf = bf.Finish(buf)
	out := string(buf)
	if !strings.Contains(out, `{"query":"retry","files":1,"lines":1}`) {
		t.Errorf("retry query totals wrong (should be true, not shown-only):\n%s", out)
	}
	if !strings.Contains(out, `{"query":"backoff","files":1,"lines":1}`) {
		t.Errorf("backoff query totals wrong — must stay 1/1 (true) even though its output was cut by the budget:\n%s", out)
	}
}

func TestJSONSpanAndScope(t *testing.T) {
	data := []byte("## Heading\nneedle line here\n")
	lineStart := strings.Index(string(data), "needle")
	jf := NewJSONFormatter()
	jf.Scope = true
	r := fakeResult("doc.md", data, lineStart, len("needle line here"))
	out := string(jf.Format(nil, r, true))

	for _, want := range []string{
		`"span":[11,27]`,
		`"region":"doc.md@11-27"`,
		`"scope":"## Heading"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("JSON output missing %s:\n%s", want, out)
		}
	}
}

// multiLineResult builds a result with n matching lines "line0\nline1\n..."
func multiLineResult(path string, n int) Result {
	var data []byte
	matches := make([]matcher.Match, n)
	for i := 0; i < n; i++ {
		start := len(data)
		line := []byte("matching line number " + strconv.Itoa(i))
		data = append(data, line...)
		data = append(data, '\n')
		matches[i] = matcher.Match{
			LineNum:    i + 1,
			LineStart:  start,
			LineLen:    len(line),
			ByteOffset: int64(start),
		}
	}
	return Result{FilePath: path, MatchSet: matcher.MatchSet{Data: data, Matches: matches}}
}

// The budget must cut within a single file, not at file boundaries
// (report bug 1): a file with many matches on a small budget emits only
// the records that fit, and the summary stays exact.
func TestBudgetFormatterCutsWithinFile(t *testing.T) {
	inner := NewJSONFormatter()
	bf := NewBudgetFormatter(inner, 50, true) // 200-byte budget
	var buf []byte
	buf = bf.Format(buf, multiLineResult("big.txt", 100), false)
	buf = bf.Finish(buf)
	out := string(buf)

	if len(buf) > 200+400 { // budget + at most ~1 record + summary of slack
		t.Errorf("output %d bytes far exceeds 200-byte budget:\n%s", len(buf), out)
	}
	if bf.shownLines == 0 || bf.shownLines >= 100 {
		t.Errorf("shownLines = %d, want partial (0 < n < 100)", bf.shownLines)
	}
	if bf.shownLines+bf.omittedLines != 100 {
		t.Errorf("shown %d + omitted %d != 100 total", bf.shownLines, bf.omittedLines)
	}
	if bf.shownFiles != 1 || bf.omittedFiles != 0 {
		t.Errorf("files: shown %d omitted %d, want 1/0 (partially shown file)", bf.shownFiles, bf.omittedFiles)
	}
	// JSON summary from the inner formatter must not double-count the
	// chunk-split file.
	if inner.files != 1 {
		t.Errorf("inner JSON files = %d, want 1 (chunked calls count once)", inner.files)
	}
}
