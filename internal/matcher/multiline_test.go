package matcher

import "testing"

func TestMultilineMatcher(t *testing.T) {
	m, err := NewMultilineMatcher([]string{`foo\(\n\s*bar`}, false, false, MatcherOpts{NeedLineNums: true})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("x\nfoo(\n  bar)\ny\n")

	if !m.MatchExists(data) {
		t.Fatal("cross-line match should exist")
	}
	if got := m.CountAll(data); got != 1 {
		t.Errorf("CountAll = %d, want 1", got)
	}

	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms.Matches))
	}
	got := ms.Matches[0]
	// Block spans from start of "foo(" line to end of "  bar)" line.
	if string(data[got.LineStart:got.LineStart+got.LineLen]) != "foo(\n  bar)" {
		t.Errorf("block = %q", data[got.LineStart:got.LineStart+got.LineLen])
	}
	if got.LineNum != 2 {
		t.Errorf("LineNum = %d, want 2 (start line of the block)", got.LineNum)
	}
	// Highlight position is block-relative and crosses the newline.
	pos := ms.MatchPositions(0)
	if len(pos) != 1 || pos[0][0] != 0 || pos[0][1] != 10 {
		t.Errorf("positions = %v, want [[0 10]] (\"foo(\\n  bar\")", pos)
	}
}

func TestMultilinePerLineAnchors(t *testing.T) {
	m, err := NewMultilineMatcher([]string{`^end$\n^begin$`}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.MatchExists([]byte("end\nbegin\n")) {
		t.Error("(?m) anchors should match per line")
	}
}

func TestMultilineTrailingNewlineMatch(t *testing.T) {
	// A match whose last byte is the newline belongs to the line it
	// terminates — the block must not swallow the next line.
	m, err := NewMultilineMatcher([]string{`alpha\n`}, false, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("alpha\nbeta\n")
	ms := m.FindAll(data)
	if len(ms.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(ms.Matches))
	}
	b := ms.Matches[0]
	if string(data[b.LineStart:b.LineStart+b.LineLen]) != "alpha" {
		t.Errorf("block = %q, want just the alpha line", data[b.LineStart:b.LineStart+b.LineLen])
	}
}

func TestMultilineFixedStrings(t *testing.T) {
	m, err := NewMultilineMatcher([]string{"a(\nb"}, true, false, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.MatchExists([]byte("xa(\nby\n")) {
		t.Error("fixed multiline pattern (QuoteMeta) should match literally")
	}
}

// -U with -i must take the internal engine (the stdlib has no literal
// prefilter under (?i)) and still agree with it.
func TestMultilineCaseInsensitiveEngine(t *testing.T) {
	m, err := NewMultilineMatcher([]string{`func main\(\) \{\n\s*defer`}, false, true, MatcherOpts{NeedLineNums: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, std := m.re.(stdMLEngine); std {
		t.Fatal("ASCII -U -i pattern should use the internal engine")
	}
	data := []byte("x\nFUNC MAIN() {\n  defer f()\n}\nfunc main() {\n\tDEFER g()\n")
	if got := m.CountAll(data); got != 2 {
		t.Errorf("CountAll = %d, want 2", got)
	}
	ms := m.FindAll(data)
	if len(ms.Matches) != 2 || ms.Matches[0].LineNum != 2 || ms.Matches[1].LineNum != 5 {
		t.Errorf("matches = %+v", ms.Matches)
	}
	if !m.MatchExists(data) || m.MatchExists([]byte("func main() {\nno defer here\n")) {
		t.Error("MatchExists wrong")
	}
	// Non-ASCII -i keeps the stdlib engine for Unicode folding.
	u, err := NewMultilineMatcher([]string{`müller\n`}, false, true, MatcherOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, std := u.re.(stdMLEngine); !std {
		t.Error("non-ASCII -U -i pattern should use the stdlib engine")
	}
	if !u.MatchExists([]byte("MÜLLER\nx")) {
		t.Error("Unicode fold failed")
	}
}
