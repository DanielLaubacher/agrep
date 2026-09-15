package lang

import "testing"

func TestSkipAtom(t *testing.T) {
	goSpec := Go.Spec()
	cases := []struct {
		name string
		data string
		i    int
		want int // expected end index (-1 = not an atom)
	}{
		{"line comment to eol", "x // hey (\nz", 2, 10},
		{"block comment", "/* ( */x", 0, 7},
		{"string with escape", `"a\")" x`, 0, 6},
		{"raw backtick string", "`a\")`x", 0, 5},
		{"unterminated string runs to EOF", `"abc`, 0, 4},
		{"not an atom", "abc", 0, -1},
	}
	for _, tc := range cases {
		got, ok := SkipAtom(goSpec, []byte(tc.data), tc.i)
		if tc.want < 0 {
			if ok {
				t.Errorf("%s: unexpectedly skipped to %d", tc.name, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("%s: SkipAtom = (%d,%v), want (%d,true)", tc.name, got, ok, tc.want)
		}
	}

	// Generic family knows no strings: a quote is just a byte.
	if _, ok := SkipAtom(Generic.Spec(), []byte(`"x"`), 0); ok {
		t.Error("generic family must not treat quotes as strings")
	}

	// Python triple quotes are one atom.
	if got, ok := SkipAtom(Python.Spec(), []byte(`"""a"b"""z`), 0); !ok || got != 9 {
		t.Errorf("python triple quote: (%d,%v), want (9,true)", got, ok)
	}
}

// TestNestableBlockComment: Rust's /* */ nests (unlike C/Go/JS), so
// SkipAtom must track depth instead of stopping at the first "*/"
// (report bug: it used to close at the first closer, leaking the
// "still comment" tail as if it were real code).
func TestNestableBlockComment(t *testing.T) {
	rustSpec := Rust.Spec()
	data := []byte("/* outer /* inner */ still comment */after")
	got, ok := SkipAtom(rustSpec, data, 0)
	want := len("/* outer /* inner */ still comment */")
	if !ok || got != want {
		t.Errorf("nested block comment: (%d,%v), want (%d,true)", got, ok, want)
	}

	// C's block comments do NOT nest: the first "*/" ends the comment,
	// same as before this change.
	cSpec := C.Spec()
	data = []byte("/* outer /* inner */ still comment */")
	got, ok = SkipAtom(cSpec, data, 0)
	want = len("/* outer /* inner */")
	if !ok || got != want {
		t.Errorf("C non-nesting block comment: (%d,%v), want (%d,true)", got, ok, want)
	}
}

// TestHeredoc covers shell/Ruby "<<TAG" scanning: the atom must run to
// the closing tag line even when the body contains delimiter-like bytes
// (report bug: heredoc bodies weren't atoms at all, so an unbalanced
// brace/paren in embedded SQL/JSON/HTML corrupted depth tracking).
func TestHeredoc(t *testing.T) {
	shSpec := Shell.Spec()
	cases := []struct {
		name string
		data string
		want int // -1 = not recognized as a heredoc
	}{
		{"plain tag", "<<EOF\nbody { unbalanced\nEOF\nafter", len("<<EOF\nbody { unbalanced\nEOF\n")},
		{"dash strip", "<<-EOF\n  body\n  EOF\nafter", len("<<-EOF\n  body\n  EOF\n")},
		{"tilde strip", "<<~EOF\n  body\nEOF\nafter", len("<<~EOF\n  body\nEOF\n")},
		{"quoted tag", "<<'RAW'\n$not_expanded {\nRAW\nafter", len("<<'RAW'\n$not_expanded {\nRAW\n")},
		{"unterminated runs to EOF", "<<EOF\nbody", len("<<EOF\nbody")},
		{"here-string is not a heredoc", "<<<\"$var\"", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SkipAtom(shSpec, []byte(tc.data), 0)
			if tc.want < 0 {
				if ok {
					t.Errorf("unexpectedly recognized as heredoc, end=%d", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("SkipAtom = (%d,%v), want (%d,true)", got, ok, tc.want)
			}
		})
	}

	// The Ruby "<<" shovel operator (extremely common: `arr << item`)
	// must not be misread as a heredoc opener — lowercase, unquoted
	// tags are deliberately rejected for exactly this reason.
	rbSpec := Ruby.Spec()
	if _, ok := SkipAtom(rbSpec, []byte("<< item\nnot a heredoc body\n"), 0); ok {
		t.Error("shovel operator misread as heredoc opener")
	}
}

// TestStringInterpolation covers JS backtick "${" and Ruby "#{": the
// interpolated region must be scanned as balanced code (recursively
// skipping nested atoms), not treated as opaque string content, so a
// nested quote or bracket inside it can't end the outer string early
// (report bug: absent this, the outer string closed at the first quote
// or bracket found inside the interpolation, wherever that fell).
func TestStringInterpolation(t *testing.T) {
	jsSpec := JS.Spec()
	data := []byte("`outer ${`inner`} end`after")
	want := len("`outer ${`inner`} end`")
	if got, ok := SkipAtom(jsSpec, data, 0); !ok || got != want {
		t.Errorf("JS template literal: (%d,%v), want (%d,true)", got, ok, want)
	}

	rbSpec := Ruby.Spec()
	data = []byte(`"value is #{h["key"]}"after`)
	want = len(`"value is #{h["key"]}"`)
	if got, ok := SkipAtom(rbSpec, data, 0); !ok || got != want {
		t.Errorf("Ruby interpolation: (%d,%v), want (%d,true)", got, ok, want)
	}
}

// TestRegexLiteral covers JS/TS "/regex/flags" recognition and its
// division-vs-regex heuristic (report bug: an unbalanced "{"/"}" inside
// an un-recognized regex literal, e.g. /\{/, corrupted delimiter-depth
// tracking for --structural/--scope/--block).
func TestRegexLiteral(t *testing.T) {
	spec := JS.Spec()
	cases := []struct {
		name string
		data string
		i    int
		want int // -1 = not recognized as a regex literal
	}{
		{"after assignment", `x = /\{/;`, 4, len(`/\{/`) + 4},
		{"after keyword", `return /a{2,}/.test(s)`, 7, len(`/a{2,}/`) + 7},
		{"character class slash", `/[/]/`, 0, len(`/[/]/`)},
		{"with flags", `/abc/gi rest`, 0, len(`/abc/gi`)},
		{"after identifier is division", `width / 2`, 6, -1},
		{"after number is division", `5 / 2`, 2, -1},
		{"after closing paren is division", `(x) / y`, 4, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := SkipAtom(spec, []byte(tc.data), tc.i)
			if tc.want < 0 {
				if ok {
					t.Errorf("unexpectedly recognized as regex literal, end=%d", got)
				}
				return
			}
			if !ok || got != tc.want {
				t.Errorf("SkipAtom = (%d,%v), want (%d,true)", got, ok, tc.want)
			}
		})
	}
}

func TestByPathAndName(t *testing.T) {
	if ByPath("a/b.go") != Go || ByPath("x.tsx") != JS || ByPath("noext") != Generic {
		t.Error("ByPath mapping broken")
	}
	if ByName("python") != Python || ByName("weird") != Generic {
		t.Error("ByName mapping broken")
	}
}
