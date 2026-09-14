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

func TestByPathAndName(t *testing.T) {
	if ByPath("a/b.go") != Go || ByPath("x.tsx") != JS || ByPath("noext") != Generic {
		t.Error("ByPath mapping broken")
	}
	if ByName("python") != Python || ByName("weird") != Generic {
		t.Error("ByName mapping broken")
	}
}
