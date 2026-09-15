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

func TestBudgetFormatterJSONTrailer(t *testing.T) {
	bf := NewBudgetFormatter(NewJSONFormatter(), 1, true)
	line := []byte("some matching line content that exceeds four bytes\n")
	var buf []byte
	buf = bf.Format(buf, fakeResult("f.txt", line, 0, len(line)-1), false)
	buf = bf.Format(buf, fakeResult("g.txt", line, 0, len(line)-1), false)
	buf = bf.Finish(buf)
	out := string(buf)
	if !strings.Contains(out, `"type":"summary"`) || !strings.Contains(out, `"omitted_files":1`) {
		t.Errorf("bad JSON trailer:\n%s", out)
	}
}

func TestJSONSpanAndSection(t *testing.T) {
	data := []byte("## Heading\nneedle line here\n")
	lineStart := strings.Index(string(data), "needle")
	jf := NewJSONFormatter()
	jf.Sections = true
	r := fakeResult("doc.md", data, lineStart, len("needle line here"))
	out := string(jf.Format(nil, r, true))

	for _, want := range []string{
		`"span":[11,27]`,
		`"region":"doc.md@11-27"`,
		`"section":"## Heading"`,
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
