package matcher

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/lang"
)

func structural(t *testing.T, pattern string, l lang.Lang) *StructuralMatcher {
	t.Helper()
	m, err := NewStructuralMatcher(pattern, l, MatcherOpts{NeedLineNums: true})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func capText(ms *MatchSet, i int, name string) string {
	for _, c := range ms.MatchCaptures(i) {
		if c.Name == name {
			return string(ms.Data[c.Start:c.End])
		}
	}
	return "<missing>"
}

func TestStructuralBalancedHole(t *testing.T) {
	m := structural(t, "foo(:[args])", lang.Go)
	data := []byte("x := foo(bar(1, 2), baz)\nfoo(q)\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(ms.Matches))
	}
	if got := capText(&ms, 0, "args"); got != "bar(1, 2), baz" {
		t.Errorf("capture 0 = %q (nested parens must stay inside the hole)", got)
	}
	if got := capText(&ms, 1, "args"); got != "q" {
		t.Errorf("capture 1 = %q", got)
	}
	if got := m.CountAll(data); got != 2 {
		t.Errorf("CountAll = %d, want 2", got)
	}
}

func TestStructuralMultilineHole(t *testing.T) {
	m := structural(t, "NewClient(:[args])", lang.Go)
	data := []byte("c := NewClient(\n\tctx,\n\topts(1),\n)\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms.Matches))
	}
	if got := capText(&ms, 0, "args"); !strings.Contains(got, "opts(1)") {
		t.Errorf("multiline capture = %q", got)
	}
	// Block spans the whole call.
	b := ms.Matches[0]
	if !strings.Contains(string(ms.Data[b.LineStart:b.LineStart+b.LineLen]), ")") {
		t.Errorf("block = %q must span to the closing line", ms.Data[b.LineStart:b.LineStart+b.LineLen])
	}
}

func TestStructuralAtomSkipping(t *testing.T) {
	// A ')' inside a string or comment must not terminate the hole.
	m := structural(t, "log(:[msg])", lang.Go)
	data := []byte("log(\"smile :)\" + x) // log(unrelated)\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1 (the comment call is inside a skipped atom... found %d)", len(ms.Matches), len(ms.Matches))
	}
	if got := capText(&ms, 0, "msg"); got != "\"smile :)\" + x" {
		t.Errorf("capture = %q", got)
	}
}

// A shebang squashed onto the same physical line as the rest of a script
// (common in PDF-extracted text, which has no real newlines between
// statements) must not be treated as a Python line comment that swallows
// everything after it — that silently hides real code from --lang py
// (report bug 2).
func TestStructuralShebangNotSwallowedAsComment(t *testing.T) {
	m := structural(t, "config.load_kube_config(:[args])", lang.Python)
	data := []byte("#!/usr/bin/env python3 import sys try: pass except: config.load_kube_config() group = 1\n")
	if !m.MatchExists(data) {
		t.Error("match hidden behind a squashed shebang line")
	}

	// A real inline comment (not a shebang) must still swallow to end of
	// line — that part of atom-awareness isn't affected by the fix.
	m2 := structural(t, "foo(:[a])", lang.Python)
	if m2.MatchExists([]byte("# see foo(bar) in the docs\n")) {
		t.Error("ordinary comment should still hide the call inside it")
	}
}

func TestStructuralHoleCannotEscapeRegion(t *testing.T) {
	// foo(:[a]) must not match when foo( is closed before a second ')'.
	m := structural(t, "foo(:[a]))", lang.Go)
	if m.MatchExists([]byte("foo(x)\n")) {
		t.Error("hole escaped its balanced region")
	}
	if !m.MatchExists([]byte("bar(foo(x))\n")) {
		t.Error("nested close should match the double-paren template")
	}
}

func TestStructuralFlexibleWhitespace(t *testing.T) {
	m := structural(t, "if err != nil { return :[e] }", lang.Go)
	data := []byte("if err != nil {\n\treturn fmt.Errorf(\"x: %w\", err)\n}\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("flexible whitespace should match across formatting, got %d", len(ms.Matches))
	}
	if got := capText(&ms, 0, "e"); !strings.Contains(got, "fmt.Errorf") {
		t.Errorf("capture = %q", got)
	}
}

func TestStructuralGenericFamilyIgnoresQuotes(t *testing.T) {
	// Generic (prose) family: quotes are plain bytes, delimiters still balance.
	m := structural(t, "see (:[ref]) for", lang.Generic)
	ms := m.FindAll([]byte("see (chapter 3, \"Retries\") for details\n"))
	if len(ms.Matches) != 1 || capText(&ms, 0, "ref") != "chapter 3, \"Retries\"" {
		t.Fatalf("generic hole failed: %+v", ms.Matches)
	}
}

func TestStructuralParseErrors(t *testing.T) {
	bad := []string{
		"",                 // empty
		":[a] = x",         // must start with literal
		"f(:[a]:[b])",      // adjacent holes
		"f(:[a], :[a])",    // duplicate names
		"f(:[a)",           // unclosed hole
		"f(:[not a name])", // invalid name
	}
	for _, p := range bad {
		if _, err := parseStructural(p); err == nil {
			t.Errorf("parseStructural(%q): expected error", p)
		}
	}
	// Anonymous holes may repeat.
	if _, err := parseStructural("f(:[_], :[_], :[x])"); err != nil {
		t.Errorf("repeated :[_] should parse: %v", err)
	}
}

func TestStructuralTrailingHole(t *testing.T) {
	m := structural(t, "ERROR: :[rest]", lang.Generic)
	ms := m.FindAll([]byte("ERROR: disk full\nok\n"))
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms.Matches))
	}
	got := capText(&ms, 0, "rest")
	if !strings.HasPrefix(got, "disk full") {
		t.Errorf("trailing capture = %q", got)
	}
}

// A template starting with a bare identifier is word-bounded on the
// left: 'Client(' must not match inside 'NewClient(' (issues.txt #1).
func TestStructuralLeftWordBoundary(t *testing.T) {
	src := []byte("x := NewClient(a)\ny := Client(b)\nz := TestNewClient(c)\n")
	m, err := NewStructuralMatcher("Client(:[args])", lang.Go, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.CountAll(src); got != 1 {
		t.Errorf("Client( matched %d times, want 1 (only the bare call)", got)
	}
	m2, err := NewStructuralMatcher("NewClient(:[args])", lang.Go, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if got := m2.CountAll(src); got != 1 {
		t.Errorf("NewClient( matched %d times, want 1 (TestNewClient excluded)", got)
	}
}

// TestStructuralRustNestedComment: a Rust doc/nested block comment must
// hide its whole contents, including a fake call site inside it (report
// bug: the first inner "*/" used to end the comment early, exposing the
// "leftover" text as if it were real code).
func TestStructuralRustNestedComment(t *testing.T) {
	m := structural(t, "leftover(:[x])", lang.Rust)
	data := []byte("/* /* */ leftover(fake_call_site) */\nfn real() {\n    leftover(actual_call)\n}\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1 (only the real call, not the commented-out one)", len(ms.Matches))
	}
	if got := capText(&ms, 0, "x"); got != "actual_call" {
		t.Errorf("capture = %q, want %q", got, "actual_call")
	}
}

// TestStructuralJSRegexLiteral: an unbalanced brace inside a JS regex
// literal (e.g. /\{/ ) must not corrupt the hole's delimiter-depth
// counter (report bug: the hole never closed, so the whole template
// failed to match).
func TestStructuralJSRegexLiteral(t *testing.T) {
	m := structural(t, "target() { :[body] }", lang.JS)
	data := []byte("function target() {\n    const re = /\\{/;\n    return 1;\n}\nfunction after() {\n    return 2;\n}\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms.Matches))
	}
	got := capText(&ms, 0, "body")
	if !strings.Contains(got, "return 1") || strings.Contains(got, "after") {
		t.Errorf("body = %q, want target()'s own body only", got)
	}
}

// TestStructuralHeredocBody: an unbalanced brace inside a shell/Ruby
// heredoc body must not corrupt delimiter-depth tracking for the
// enclosing function/method (report bug: heredoc bodies weren't atoms
// at all).
func TestStructuralHeredocBody(t *testing.T) {
	t.Run("shell", func(t *testing.T) {
		m := structural(t, "target() { :[body] }", lang.Shell)
		data := []byte("target() {\n    cat <<EOF2\nunbalanced brace {\nEOF2\n    echo real\n}\nafter() {\n    echo two\n}\n")
		ms := m.FindAll(data)
		if len(ms.Matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(ms.Matches))
		}
		if got := capText(&ms, 0, "body"); !strings.Contains(got, "echo real") {
			t.Errorf("body = %q, want it to contain %q", got, "echo real")
		}
	})
	t.Run("ruby", func(t *testing.T) {
		m := structural(t, "def target :[body] end", lang.Ruby)
		data := []byte("def target\n  sql = <<~SQL\n    unbalanced brace {\n  SQL\n  1\nend\n\ndef after\n  2\nend\n")
		ms := m.FindAll(data)
		if len(ms.Matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(ms.Matches))
		}
		if got := capText(&ms, 0, "body"); !strings.Contains(got, "1") || strings.Contains(got, "def after") {
			t.Errorf("body = %q, want target's own body only", got)
		}
	})
}

// TestStructuralStringInterpolation: a nested quote/backtick inside a
// Ruby "#{}"/JS "${}" interpolation must not end the outer string early
// (report bug: the outer string closed at the first quote/backtick
// found inside the interpolation, corrupting depth tracking).
func TestStructuralStringInterpolation(t *testing.T) {
	t.Run("ruby", func(t *testing.T) {
		m := structural(t, "def target :[body] end", lang.Ruby)
		data := []byte("def target\n  puts \"value is #{h[\"key\"]}\"\n  1\nend\n")
		ms := m.FindAll(data)
		if len(ms.Matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(ms.Matches))
		}
		if got := capText(&ms, 0, "body"); !strings.Contains(got, "1") {
			t.Errorf("body = %q", got)
		}
	})
	t.Run("js nested backtick", func(t *testing.T) {
		m := structural(t, "target() { :[body] }", lang.JS)
		data := []byte("function target() {\n    const s = `outer ${`inner`} end`;\n    return 1;\n}\n")
		ms := m.FindAll(data)
		if len(ms.Matches) != 1 {
			t.Fatalf("matches = %d, want 1", len(ms.Matches))
		}
		if got := capText(&ms, 0, "body"); !strings.Contains(got, "return 1") {
			t.Errorf("body = %q", got)
		}
	})
}
