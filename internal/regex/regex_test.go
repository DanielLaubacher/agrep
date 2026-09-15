package regex

import (
	"fmt"
	"regexp"
	"testing"
)

// TestBasicMatch tests basic match functionality against stdlib.
func TestBasicMatch(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		// Literals
		{"hello", "hello world", true},
		{"hello", "goodbye", false},
		{"abc", "xabcy", true},

		// Character classes
		{`[a-z]+`, "hello", true},
		{`[0-9]+`, "abc", false},
		{`[0-9]+`, "abc123", true},
		{`[a-zA-Z]`, "5", false},
		{`[a-zA-Z]`, "a", true},

		// Dot
		{`a.c`, "abc", true},
		{`a.c`, "ac", false},
		{`a.c`, "a\nc", false}, // dot doesn't match \n

		// Repetition
		{`ab*c`, "ac", true},
		{`ab*c`, "abc", true},
		{`ab*c`, "abbc", true},
		{`ab+c`, "ac", false},
		{`ab+c`, "abc", true},
		{`ab?c`, "ac", true},
		{`ab?c`, "abc", true},
		{`ab?c`, "abbc", false},

		// Alternation
		{`cat|dog`, "I have a cat", true},
		{`cat|dog`, "I have a dog", true},
		{`cat|dog`, "I have a bird", false},

		// Anchors
		{`^hello`, "hello world", true},
		{`^hello`, "say hello", false},
		{`world$`, "hello world", true},
		{`world$`, "world cup", false},

		// Bounded repetition
		{`a{3}`, "aa", false},
		{`a{3}`, "aaa", true},
		{`a{2,4}`, "a", false},
		{`a{2,4}`, "aa", true},
		{`a{2,4}`, "aaaa", true},

		// Grouping
		{`(abc)+`, "abcabc", true},
		{`(abc)+`, "ab", false},

		// Word boundary
		{`\bword\b`, "a word here", true},
		{`\bword\b`, "password", false},

		// Escape sequences
		{`\d+`, "abc123", true},
		{`\d+`, "abc", false},
		{`\w+`, "hello", true},
		{`\s+`, "hello world", true},
		{`\s+`, "hello", false},

		// Empty pattern
		{"", "anything", true},
		{"", "", true},

		// Complex patterns
		{`\d{4}-\d{2}-\d{2}`, "Date: 2024-01-15 ok", true},
		{`\d{4}-\d{2}-\d{2}`, "no date here", false},
		{`[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`, "user@host.com", true},
		{`(?:error|warn|fatal)`, "an error occurred", true},
		{`(?:error|warn|fatal)`, "all good", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.pattern, tt.input), func(t *testing.T) {
			re, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tt.pattern, err)
			}

			got := re.Match([]byte(tt.input))
			if got != tt.want {
				t.Errorf("Match(%q) = %v, want %v (engine=%d)", tt.input, got, tt.want, re.engineType)
			}

			// Cross-check with stdlib
			stdRe := regexp.MustCompile(tt.pattern)
			stdGot := stdRe.Match([]byte(tt.input))
			if got != stdGot {
				t.Errorf("Match(%q) = %v, stdlib = %v", tt.input, got, stdGot)
			}
		})
	}
}

// TestFindIndex tests FindIndex against stdlib.
func TestFindIndex(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
	}{
		{"hello", "hello world"},
		{"hello", "goodbye"},
		{`\d+`, "abc123def"},
		{`\d+`, "no digits"},
		{`[a-z]+`, "ABC123abc"},
		{`(foo)(bar)`, "foobar"},
		{`^start`, "start here"},
		{`^start`, "not start"},
		{`end$`, "the end"},
		{`a{2,4}`, "xaaaay"},
		{`a*`, "bbb"},
		{`x`, "abcxdef"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.pattern, tt.input), func(t *testing.T) {
			re, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tt.pattern, err)
			}

			got := re.FindIndex([]byte(tt.input))

			stdRe := regexp.MustCompile(tt.pattern)
			stdLoc := stdRe.FindIndex([]byte(tt.input))

			if stdLoc == nil {
				if got[0] != -1 {
					t.Errorf("FindIndex(%q) = %v, want [-1, -1]", tt.input, got)
				}
			} else {
				want := [2]int{stdLoc[0], stdLoc[1]}
				if got != want {
					t.Errorf("FindIndex(%q) = %v, want %v", tt.input, got, want)
				}
			}
		})
	}
}

// TestFindAllIndex tests FindAllIndex against stdlib.
func TestFindAllIndex(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		n       int
	}{
		{"ab", "ababab", -1},
		{`\d+`, "a1b22c333", -1},
		{`\w+`, "hello world foo", -1},
		{"x", "xxxx", -1},
		{"x", "xxxx", 2},
		{`[a-z]+`, "abc123def456ghi", -1},
		{"hello", "no match here", -1},
		{`a{2}`, "aaaaaa", -1},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s/%d", tt.pattern, tt.input, tt.n), func(t *testing.T) {
			re, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tt.pattern, err)
			}

			got := re.FindAllIndex([]byte(tt.input), tt.n)

			stdRe := regexp.MustCompile(tt.pattern)
			stdLocs := stdRe.FindAllIndex([]byte(tt.input), tt.n)

			if len(got) != len(stdLocs) {
				t.Fatalf("FindAllIndex(%q, %d): got %d matches, want %d\ngot:  %v\nwant: %v",
					tt.input, tt.n, len(got), len(stdLocs), got, formatStdLocs(stdLocs))
			}

			for i := range got {
				want := [2]int{stdLocs[i][0], stdLocs[i][1]}
				if got[i] != want {
					t.Errorf("FindAllIndex(%q, %d)[%d] = %v, want %v",
						tt.input, tt.n, i, got[i], want)
				}
			}
		})
	}
}

// TestMatchString tests MatchString.
func TestMatchString(t *testing.T) {
	re := MustCompile(`\d+`)
	if !re.MatchString("abc123") {
		t.Error("MatchString should match")
	}
	if re.MatchString("abcdef") {
		t.Error("MatchString should not match")
	}
}

// TestFindString tests FindString.
func TestFindString(t *testing.T) {
	re := MustCompile(`\d+`)
	got := re.FindString("abc123def456")
	if got != "123" {
		t.Errorf("FindString = %q, want %q", got, "123")
	}
}

// TestFindAllString tests FindAllString.
func TestFindAllString(t *testing.T) {
	re := MustCompile(`\d+`)
	got := re.FindAllString("a1b22c333", -1)
	want := []string{"1", "22", "333"}
	if len(got) != len(want) {
		t.Fatalf("FindAllString: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("FindAllString[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestReplaceAll tests replacement.
func TestReplaceAll(t *testing.T) {
	re := MustCompile(`\d+`)
	got := string(re.ReplaceAll([]byte("a1b22c333"), []byte("X")))
	want := "aXbXcX"
	if got != want {
		t.Errorf("ReplaceAll = %q, want %q", got, want)
	}
}

// TestReplaceAllString tests string replacement.
func TestReplaceAllString(t *testing.T) {
	re := MustCompile(`\bfoo\b`)
	got := re.ReplaceAllString("foo bar foo baz", "qux")
	want := "qux bar qux baz"
	if got != want {
		t.Errorf("ReplaceAllString = %q, want %q", got, want)
	}
}

// TestSplit tests Split.
func TestSplit(t *testing.T) {
	re := MustCompile(`[,;]+`)
	got := re.Split("a,b;;c,d", -1)
	want := []string{"a", "b", "c", "d"}
	if len(got) != len(want) {
		t.Fatalf("Split: got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Split[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestCaseInsensitive tests case-insensitive patterns.
func TestCaseInsensitive(t *testing.T) {
	re := MustCompile(`(?i)hello`)
	tests := []struct {
		input string
		want  bool
	}{
		{"hello", true},
		{"Hello", true},
		{"HELLO", true},
		{"hElLo", true},
		{"goodbye", false},
	}
	for _, tt := range tests {
		got := re.Match([]byte(tt.input))
		if got != tt.want {
			t.Errorf("(?i)hello Match(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// TestUnicode tests Unicode pattern matching.
func TestUnicode(t *testing.T) {
	tests := []struct {
		pattern string
		input   string
		want    bool
	}{
		{`café`, "I love café", true},
		{`\w+`, "héllo", true},
		{`.`, "日", true},
		{`.`, "\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			re, err := Compile(tt.pattern)
			if err != nil {
				t.Fatalf("Compile(%q): %v", tt.pattern, err)
			}
			got := re.Match([]byte(tt.input))
			if got != tt.want {
				t.Errorf("Match(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestMustCompilePanic tests that MustCompile panics on invalid patterns.
func TestMustCompilePanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("MustCompile should panic on invalid pattern")
		}
	}()
	MustCompile(`[invalid`)
}

// TestCompileError tests that Compile returns errors for invalid patterns.
func TestCompileError(t *testing.T) {
	invalid := []string{
		`[unclosed`,
		`(unclosed`,
		`*invalid`,
		`+invalid`,
		`?invalid`,
	}
	for _, p := range invalid {
		_, err := Compile(p)
		if err == nil {
			t.Errorf("Compile(%q) should return error", p)
		}
	}
}

// TestStdlibCompat runs comprehensive cross-validation against stdlib.
func TestStdlibCompat(t *testing.T) {
	patterns := []string{
		`\w+`,
		`\d{3}-\d{4}`,
		`[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`,
		`(?:a|b|c|d)+`,
		`(.{0,5})needle`,
		`^start.*end$`,
		`\bword\b`,
		`[^aeiou]+`,
		`a+b+c+`,
		`(?:foo|bar|baz)`,
		`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}`,
	}

	inputs := []string{
		"hello world",
		"abc123def456",
		"user@host.com is an email",
		"aabbccdd",
		"find the needle here",
		"start something at end",
		"a word boundary",
		"bcdfgh vowels aeiou",
		"aaabbbccc",
		"I like foo and bar",
		"IP is 192.168.1.1 ok",
		"555-1234 phone number",
		"no match here",
		"",
		"single",
	}

	for _, pattern := range patterns {
		stdRe := regexp.MustCompile(pattern)
		re, err := Compile(pattern)
		if err != nil {
			t.Fatalf("Compile(%q): %v", pattern, err)
		}

		for _, input := range inputs {
			b := []byte(input)

			// Match
			gotMatch := re.Match(b)
			wantMatch := stdRe.Match(b)
			if gotMatch != wantMatch {
				t.Errorf("Match(%q, %q) = %v, want %v", pattern, input, gotMatch, wantMatch)
			}

			// FindAllIndex
			gotAll := re.FindAllIndex(b, -1)
			wantAll := stdRe.FindAllIndex(b, -1)
			if len(gotAll) != len(wantAll) {
				t.Errorf("FindAllIndex(%q, %q): got %d, want %d\ngot:  %v\nwant: %v",
					pattern, input, len(gotAll), len(wantAll), gotAll, formatStdLocs(wantAll))
				continue
			}
			for i := range gotAll {
				w := [2]int{wantAll[i][0], wantAll[i][1]}
				if gotAll[i] != w {
					t.Errorf("FindAllIndex(%q, %q)[%d] = %v, want %v",
						pattern, input, i, gotAll[i], w)
				}
			}
		}
	}
}

func formatStdLocs(locs [][]int) [][2]int {
	result := make([][2]int, len(locs))
	for i, l := range locs {
		result[i] = [2]int{l[0], l[1]}
	}
	return result
}

// Line mode: ^ and $ hold at every line (report: '^b' on "a\nb\n" found
// nothing), and assertion patterns keep a literal prefilter whose
// per-line verification must agree with the stdlib engine.
func TestCompileModeLineAnchors(t *testing.T) {
	data := []byte("a\nb\nab\nba\n")
	cases := []struct {
		pat  string
		want [][2]int
	}{
		{`^b`, [][2]int{{2, 3}, {7, 8}}},
		{`b$`, [][2]int{{2, 3}, {5, 6}}},
		{`^a$`, [][2]int{{0, 1}}},
		{`^ab$`, [][2]int{{4, 6}}},
		{`\Aa`, [][2]int{{0, 1}}},
	}
	for _, c := range cases {
		re, err := CompileMode(c.pat, ModeLine)
		if err != nil {
			t.Fatal(err)
		}
		got := re.FindAllIndex(data, -1)
		if !equalLocs(got, c.want) {
			t.Errorf("ModeLine %q: got %v, want %v", c.pat, got, c.want)
		}
		if (len(c.want) > 0) != re.Match(data) {
			t.Errorf("ModeLine %q: Match disagrees with FindAllIndex", c.pat)
		}
	}
}

func TestWordLiteralFastPath(t *testing.T) {
	data := []byte("error errors _error error_ (error) ERROR\nerror\nxerror error")
	cases := []struct {
		pat   string
		count int
	}{
		{`\berror\b`, 4},
		{`(?i)\berror\b`, 5},
		{`\berror`, 6},
		{`error\b`, 6},
		{`\b\(error\)\b`, 0}, // '(' is non-word and is preceded by a space
		{`\bx`, 1},
	}
	for _, c := range cases {
		re, err := CompileMode(c.pat, ModeLine)
		if err != nil {
			t.Fatal(err)
		}
		if re.engineType != engineLiteral {
			t.Errorf("%q: engine %v, want the literal engine", c.pat, re.engineType)
		}
		got := re.FindAllIndex(data, -1)
		want := toLocs(regexp.MustCompile(c.pat).FindAllIndex(data, -1))
		if len(want) != c.count {
			t.Fatalf("%q: test expectation wrong, stdlib finds %d", c.pat, len(want))
		}
		if !equalLocs(got, want) {
			t.Errorf("%q: got %v, want %v", c.pat, got, want)
		}
		if re.Match(data) != (c.count > 0) {
			t.Errorf("%q: Match = %v", c.pat, re.Match(data))
		}
		if fi := re.FindIndex(data); (c.count == 0 && fi[0] >= 0) || (c.count > 0 && fi != want[0]) {
			t.Errorf("%q: FindIndex = %v", c.pat, fi)
		}
	}
}

// The relaxed DFA (assertions stripped) must reject candidate lines that
// lack the pattern's shape before the PikeVM runs.
func TestRelaxedGate(t *testing.T) {
	re, err := CompileMode(`\bgo\s+func\s*\(`, ModeLine)
	if err != nil {
		t.Fatal(err)
	}
	if re.engineType != enginePikeVM || re.prefilter == nil {
		t.Fatalf("engine %v prefilter %v", re.engineType, re.prefilter != nil)
	}
	if re.relaxed == nil {
		t.Fatal("relaxed DFA not built")
	}
	if re.lineMayMatch([]byte("a func b")) {
		t.Error("gate admitted a line without the shape")
	}
	if !re.lineMayMatch([]byte("x go  func (")) {
		t.Error("gate rejected a matching line")
	}
}

// Assertion patterns that are not a bare word literal keep the PikeVM but
// gain a per-line literal prefilter; results must equal the stdlib's
// multi-line-anchored results.
func TestAssertionPrefilterMatchesStdlib(t *testing.T) {
	data := []byte("go func(\ngo  func (x)\nlogo func(\n#define X\n  #define Y\nfoo_bar baz\nbaz foo_bar\n")
	for _, pat := range []string{
		`\bgo\s+func\s*\(`, `^#define`, `^\s*#define \w`, `\bfoo_bar\b baz`, `baz \bfoo`, `func\b\(`, `\Bo func`,
	} {
		re, err := CompileMode(pat, ModeLine)
		if err != nil {
			t.Fatal(err)
		}
		if re.engineType != enginePikeVM {
			t.Errorf("%q: expected PikeVM, got %v", pat, re.engineType)
		}
		if re.prefilter == nil {
			t.Errorf("%q: expected a literal prefilter", pat)
		}
		got := re.FindAllIndex(data, -1)
		want := toLocs(regexp.MustCompile(`(?m)` + pat).FindAllIndex(data, -1))
		if !equalLocs(got, want) {
			t.Errorf("%q: got %v, want %v", pat, got, want)
		}
	}
	// \A refers to the whole text: no line prefilter may be used.
	re, _ := CompileMode(`\Afoo_bar`, ModeLine)
	if re.prefilter != nil {
		t.Error(`\A pattern must not get a per-line prefilter`)
	}
	if got := re.FindAllIndex(data, -1); len(got) != 0 {
		t.Errorf(`\Afoo_bar matched %v`, got)
	}
}

// Multiline mode: a prefilter may anchor at the match start or gate the
// buffer, but never confine verification to one line.
func TestCompileModeMultiline(t *testing.T) {
	data := []byte("x\nfoo(\n  bar)\ny\n  Foo(\nBAR)\n")
	cases := []struct {
		pat  string
		want [][2]int
	}{
		{`foo\(\n\s*bar`, [][2]int{{2, 12}}},               // prefix literal: anchored verify across lines
		{`(?i)foo\(\n\s*bar`, [][2]int{{2, 12}, {18, 26}}},  // case-insensitive anchored
		{`\s*foo\(\n\s*bar`, [][2]int{{1, 12}}},             // non-prefix literal: gate only
		{`(?i)\s*foo\(\n\s*bar`, [][2]int{{1, 12}, {15, 26}}},
		{`^foo\($\n^\s*bar`, [][2]int{{2, 12}}},             // assertions: PikeVM, gated
		{`(?i)\s*foo\(\n\s*zzz`, nil},                       // gate literal present, no match
		{`(?i)qqq\(\n\s*bar`, nil},                          // gate literal absent
	}
	for _, c := range cases {
		re, err := CompileMode(c.pat, ModeMultiline)
		if err != nil {
			t.Fatal(err)
		}
		got := re.FindAllIndex(data, -1)
		if !equalLocs(got, c.want) {
			t.Errorf("ModeMultiline %q: got %v, want %v", c.pat, got, c.want)
		}
		want := toLocs(regexp.MustCompile(`(?m)` + c.pat).FindAllIndex(data, -1))
		if !equalLocs(got, want) {
			t.Errorf("ModeMultiline %q: disagrees with stdlib %v", c.pat, want)
		}
		if re.Match(data) != (len(c.want) > 0) {
			t.Errorf("ModeMultiline %q: Match = %v", c.pat, re.Match(data))
		}
	}
}

func toLocs(locs [][]int) [][2]int {
	out := make([][2]int, len(locs))
	for i, l := range locs {
		out[i] = [2]int{l[0], l[1]}
	}
	return out
}

func equalLocs(a, b [][2]int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
