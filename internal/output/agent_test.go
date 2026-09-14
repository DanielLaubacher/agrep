package output

import (
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
