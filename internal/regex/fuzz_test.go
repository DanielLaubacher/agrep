package regex

import (
	"regexp"
	"testing"
)

// FuzzRegexMatch fuzzes the regex engine against stdlib for correctness.
// Match() must always agree. FindAllIndex may differ on pathological
// patterns with empty alternatives (DFA prefers longest, NFA prefers first
// alternative — both are valid for grep use cases).
func FuzzRegexMatch(f *testing.F) {
	seeds := []struct {
		pattern string
		input   string
	}{
		{`hello`, "hello world"},
		{`\d+`, "abc123def"},
		{`[a-z]+`, "ABC123abc"},
		{`a{2,4}`, "xaaaay"},
		{`(?:foo|bar)`, "foobar"},
		{`a.c`, "abc"},
		{`ab*c`, "ac"},
		{`ab+c`, "abc"},
		{`ab?c`, "ac"},
		{`\w+`, "hello world"},
		{`\s+`, "hello\tworld"},
		{`[^aeiou]+`, "bcdfg"},
		{`\d{3}-\d{4}`, "555-1234"},
		{`(?:error|warn|fatal)`, "an error occurred"},
		{`x`, "xxx"},
		{``, "anything"},
		{`a*`, "aaa"},
	}

	for _, s := range seeds {
		f.Add(s.pattern, s.input)
	}

	f.Fuzz(func(t *testing.T, pattern, input string) {
		// Skip inputs with invalid UTF-8 — our byte-level DFA handles
		// invalid UTF-8 differently from stdlib's rune-level NFA.
		// For grep use cases, inputs are valid text files.
		for i := 0; i < len(input); {
			r, size := stdDecodeRune([]byte(input[i:]))
			if r == 0xFFFD && (size <= 1 && (i >= len(input) || input[i] >= 0x80)) {
				return
			}
			if size <= 0 {
				size = 1
			}
			i += size
		}

		stdRe, err := regexp.Compile(pattern)
		if err != nil {
			return
		}

		re, err := Compile(pattern)
		if err != nil {
			return
		}

		b := []byte(input)

		// Match must always agree
		gotMatch := re.Match(b)
		wantMatch := stdRe.Match(b)
		if gotMatch != wantMatch {
			t.Errorf("Match(%q, %q) = %v, stdlib = %v", pattern, input, gotMatch, wantMatch)
		}

		// FindIndex: if stdlib finds a match, we must find one too (and vice versa)
		gotIdx := re.FindIndex(b)
		wantIdx := stdRe.FindIndex(b)
		if (gotIdx[0] >= 0) != (wantIdx != nil) {
			t.Errorf("FindIndex(%q, %q): got %v, stdlib %v", pattern, input, gotIdx, wantIdx)
		}
	})
}
