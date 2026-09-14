package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
)

func TestSuggestVariants(t *testing.T) {
	cases := []struct {
		pattern string
		want    []string
	}{
		{"ConnectTimeout", []string{"connecttimeout", "connect", "timeout"}},
		{"retry_backoff_ms", []string{"retry", "backoff"}}, // "ms" too short
		{"epoll", nil}, // lowercase single word: no variants beyond itself
		{"HTTPServer", []string{"httpserver", "httpserver"}[0:1]},
	}
	for _, tc := range cases {
		var got []string
		for _, v := range suggestVariants(tc.pattern) {
			got = append(got, v.pattern)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("suggestVariants(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestAppendJSONString(t *testing.T) {
	got := string(appendJSONString(nil, "a\"b\\c\nd"))
	want := `"a\"b\\c\u000ad"`
	if got != want {
		t.Errorf("appendJSONString = %s, want %s", got, want)
	}
}

func TestLineRangeToBytes(t *testing.T) {
	data := []byte("l1\nl2\nl3\nl4\n")
	cases := []struct {
		start, end int
		want       string
	}{
		{1, 1, "l1\n"},
		{2, 3, "l2\nl3\n"},
		{4, 9, "l4\n"}, // end past EOF: through last line
		{9, 9, ""},     // start past EOF: empty
	}
	for _, tc := range cases {
		s, e := lineRangeToBytes(data, tc.start, tc.end)
		if got := string(data[s:e]); got != tc.want {
			t.Errorf("lines %d-%d = %q, want %q", tc.start, tc.end, got, tc.want)
		}
	}
}

func TestExpandByLines(t *testing.T) {
	data := []byte("a\nbb\nccc\ndddd\ne\n")
	// Span covering just "ccc" (bytes 5-8), expand 1 each side.
	s, e := expandByLines(data, 5, 8, 1)
	if got := string(data[s:e]); got != "bb\nccc\ndddd\n" {
		t.Errorf("expand 1 = %q", got)
	}
	// Already line-snapped span must gain exactly n lines, not n+1.
	s, e = expandByLines(data, 5, 9, 1) // "ccc\n" whole line
	if got := string(data[s:e]); got != "bb\nccc\ndddd\n" {
		t.Errorf("expand snapped = %q", got)
	}
	// Expansion clamps at file bounds.
	s, e = expandByLines(data, 0, 2, 5)
	if s != 0 || e != len(data) {
		t.Errorf("expand clamp = [%d,%d)", s, e)
	}
}

func TestIdentRegex(t *testing.T) {
	cases := []struct {
		pattern string
		want    string
	}{
		{"connectTimeout", `(?i)\bconnect[_-]?timeout\b`},
		{"connect_timeout", `(?i)\bconnect[_-]?timeout\b`},
		{"CONNECT-TIMEOUT", `(?i)\bconnect[_-]?timeout\b`},
		{"Match", `(?i)\bmatch\b`},
		{"utf8Parser", `(?i)\butf8[_-]?parser\b`},
		{"---", ""},
	}
	for _, tc := range cases {
		if got := identRegex(tc.pattern); got != tc.want {
			t.Errorf("identRegex(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

// TestIdentMatchingSemantics compiles the ident regex and checks the
// precision/recall contract: all case conventions match, substrings of
// longer identifiers do not.
func TestIdentMatchingSemantics(t *testing.T) {
	m, err := matcher.NewMatcher([]string{identRegex("connectTimeout")}, false, false, false, false, matcher.MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	shouldMatch := []string{
		"x := connectTimeout + 1",
		"CONNECT_TIMEOUT = 30",
		"set connect-timeout here",
		"ConnectTimeout int",
		"connecttimeout=5",
	}
	shouldNot := []string{
		"xconnectTimeout",
		"connectTimeoutMs",
		"preconnect_timeout",
		"connect timeout", // separated by space: different tokens
	}
	for _, s := range shouldMatch {
		if !m.MatchExists([]byte(s)) {
			t.Errorf("ident should match %q", s)
		}
	}
	for _, s := range shouldNot {
		if m.MatchExists([]byte(s)) {
			t.Errorf("ident should NOT match %q", s)
		}
	}
}

// TestOutlineExemplarSelection verifies the exemplar is the most
// informative matching line, not the literal first one: a boilerplate
// single-occurrence line must lose to a line with more occurrences.
func TestOutlineExemplarSelection(t *testing.T) {
	data := []byte("package matcher\nfunc NewMatcher() Matcher { return matcher{} }\n")
	r := output.Result{
		FilePath: "t.go",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 15, PosIdx: 0, PosCount: 1},
				{LineNum: 2, LineStart: 16, LineLen: 46, PosIdx: 1, PosCount: 3},
			},
			Positions: [][2]int{{8, 15}, {8, 15}, {18, 25}, {36, 43}},
		},
	}
	e, ok := outlineFromResult(&r)
	if !ok || e.count != 2 {
		t.Fatalf("outlineFromResult: ok=%v count=%d, want ok=true count=2", ok, e.count)
	}
	if !strings.HasPrefix(e.exemplar, "func NewMatcher") {
		t.Errorf("exemplar = %q, want the 3-occurrence line, not boilerplate", e.exemplar)
	}
}

// TestOutlineExemplarContextTieBreak: at equal occurrence counts, a line
// carrying text beyond the match beats a line that is only the match.
func TestOutlineExemplarContextTieBreak(t *testing.T) {
	data := []byte("backoff\nuse jittered backoff here\n")
	r := output.Result{
		FilePath: "t.md",
		MatchSet: matcher.MatchSet{
			Data: data,
			Matches: []matcher.Match{
				{LineNum: 1, LineStart: 0, LineLen: 7, PosIdx: 0, PosCount: 1},
				{LineNum: 2, LineStart: 8, LineLen: 25, PosIdx: 1, PosCount: 1},
			},
			Positions: [][2]int{{0, 7}, {13, 20}},
		},
	}
	e, _ := outlineFromResult(&r)
	if e.exemplar != "use jittered backoff here" {
		t.Errorf("exemplar = %q, want the context-bearing line", e.exemplar)
	}
}

// TestSuggestReportNeverSilent: every zero-hit outcome must produce a
// report — hits, no-variant-occurs, and no-derivable-variants alike.
func TestSuggestReportNeverSilent(t *testing.T) {
	probes := []suggestProbe{
		{suggestVariant{pattern: "connect", label: "fragment"}, 0, 0},
		{suggestVariant{pattern: "timeout", label: "fragment"}, 0, 0},
	}

	// Text, zero variants occur.
	got := string(appendSuggestReport(nil, []string{"ConnectTimeout"}, probes, false))
	if !strings.Contains(got, "none of the derived variants occur") ||
		!strings.Contains(got, "connect") {
		t.Errorf("zero-occurrence text report = %q", got)
	}

	// Text, no derivable variants.
	got = string(appendSuggestReport(nil, []string{"qqqq"}, nil, false))
	if !strings.Contains(got, "no derivable variants") {
		t.Errorf("no-variant text report = %q", got)
	}

	// JSON always ends with suggest_summary and lists zero-count probes.
	got = string(appendSuggestReport(nil, []string{"ConnectTimeout"}, probes, true))
	if !strings.Contains(got, `"type":"suggest","variant":"connect","kind":"fragment","lines":0`) {
		t.Errorf("JSON report missing zero-count probe: %q", got)
	}
	if !strings.Contains(got, `{"type":"suggest_summary","patterns":["ConnectTimeout"],"tried":2,"found":0}`) {
		t.Errorf("JSON report missing suggest_summary: %q", got)
	}

	// JSON, no derivable variants: still a suggest_summary.
	got = string(appendSuggestReport(nil, []string{"qqqq"}, nil, true))
	if !strings.Contains(got, `"tried":0,"found":0`) {
		t.Errorf("no-variant JSON report = %q", got)
	}
}

// TestSuggestReportHits mirrors runSuggest's contract: occurring
// variants are listed in text output; zero-count probes are not.
func TestSuggestReportHits(t *testing.T) {
	probes := []suggestProbe{
		{suggestVariant{pattern: "timeout", label: "fragment"}, 3, 2},
		{suggestVariant{pattern: "connect", label: "fragment"}, 0, 0},
	}
	got := string(appendSuggestReport(nil, []string{"ConnectTimeout"}, probes, false))
	if !strings.Contains(got, "variants that do occur") ||
		!strings.Contains(got, "timeout (fragment): 3 lines in 2 files") {
		t.Errorf("hit report = %q", got)
	}
	if strings.Contains(got, "connect (fragment)") {
		t.Errorf("zero-count probe leaked into text hit list: %q", got)
	}
}
