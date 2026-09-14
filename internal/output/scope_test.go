package output

import "testing"

func TestEnclosingScopeGo(t *testing.T) {
	data := []byte(`package x

// doc comment for Foo
func Foo(a int) error {
	if a > 0 {
		return work(a)
	}
	return nil
}

var topLevel = 3

func Bar() {}
`)
	find := func(needle string) int {
		return indexOfLineStart(data, needle, t)
	}
	cases := []struct {
		needle string
		want   string
	}{
		{"return work(a)", "func Foo(a int) error"},
		{"if a > 0 {", "func Foo(a int) error"},
		{"var topLevel = 3", ""}, // top-level: no enclosing definition
		{"func Bar() {}", "func Bar() {}"},
		{"// doc comment for Foo", ""}, // comment at col 0 above the func
	}
	for _, tc := range cases {
		got := string(enclosingScope(data, find(tc.needle), "main.go"))
		if got != tc.want {
			t.Errorf("scope of %q = %q, want %q", tc.needle, got, tc.want)
		}
	}
}

func TestEnclosingScopePython(t *testing.T) {
	data := []byte(`import os

class Retry:
    """doc"""

    def backoff(self, n):
        # comment
        delay = 2 ** n
        return delay

def top():
    pass
`)
	find := func(needle string) int {
		return indexOfLineStart(data, needle, t)
	}
	cases := []struct {
		needle string
		want   string
	}{
		{"delay = 2 ** n", "def backoff(self, n):"},
		{`"""doc"""`, "class Retry:"},
		{"pass", "def top():"},
		{"import os", ""},
	}
	for _, tc := range cases {
		got := string(enclosingScope(data, find(tc.needle), "retry.py"))
		if got != tc.want {
			t.Errorf("scope of %q = %q, want %q", tc.needle, got, tc.want)
		}
	}
}

func TestEnclosingScopeMarkdownFallback(t *testing.T) {
	data := []byte("# Title\n\n## Backoff\n\njittered delay here\n")
	pos := indexOfLineStart(data, "jittered delay here", t)
	got := string(enclosingScope(data, pos, "book.md"))
	if got != "## Backoff" {
		t.Errorf("markdown scope = %q, want ## Backoff", got)
	}
}

func TestEnclosingScopeUnknownExtension(t *testing.T) {
	data := []byte("a\n\tb\n")
	if s := enclosingScope(data, 2, "data.bin"); s != nil {
		t.Errorf("unknown extension should have no scope, got %q", s)
	}
}

// indexOfLineStart returns the byte offset of the line containing needle.
func indexOfLineStart(data []byte, needle string, t *testing.T) int {
	t.Helper()
	idx := -1
	for i := 0; i+len(needle) <= len(data); i++ {
		if string(data[i:i+len(needle)]) == needle {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("needle %q not found", needle)
	}
	for idx > 0 && data[idx-1] != '\n' {
		idx--
	}
	return idx
}
