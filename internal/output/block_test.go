package output

import (
	"strings"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

func TestBlockBoundsGo(t *testing.T) {
	data := []byte(`package x

func Foo() int {
	if a {
		return 1
	}
	return 2
}

func Bar() {}
`)
	pos := indexOfLineStart(data, "return 1", t)
	bs, be, trunc, ok := blockBounds(data, pos, "x.go")
	if !ok || trunc {
		t.Fatalf("blockBounds: ok=%v trunc=%v", ok, trunc)
	}
	got := string(data[bs:be])
	if !strings.HasPrefix(got, "func Foo() int {") || !strings.HasSuffix(got, "}") ||
		strings.Contains(got, "Bar") {
		t.Errorf("block = %q", got)
	}
}

func TestBlockBoundsPython(t *testing.T) {
	data := []byte("class A:\n    def f(self):\n        x = 1\n        return x\n\n    def g(self):\n        pass\n")
	pos := indexOfLineStart(data, "x = 1", t)
	bs, be, _, ok := blockBounds(data, pos, "a.py")
	if !ok {
		t.Fatal("no python block")
	}
	got := string(data[bs:be])
	if !strings.HasPrefix(got, "    def f(self):") || strings.Contains(got, "def g") {
		t.Errorf("python block = %q", got)
	}
}

func TestBlockBoundsMarkdown(t *testing.T) {
	data := []byte("# T\n\n## Backoff\nuse jitter\nmore prose\n\n## Next\nother\n")
	pos := indexOfLineStart(data, "use jitter", t)
	bs, be, _, ok := blockBounds(data, pos, "b.md")
	if !ok {
		t.Fatal("no markdown block")
	}
	got := string(data[bs:be])
	if !strings.HasPrefix(got, "## Backoff") || strings.Contains(got, "## Next") {
		t.Errorf("markdown block = %q", got)
	}
}

func TestBlockBoundsGenericNone(t *testing.T) {
	if _, _, _, ok := blockBounds([]byte("plain\ntext\n"), 0, "notes.txt"); ok {
		t.Error("generic files have no block notion")
	}
}

func TestBlockFormatterRewrite(t *testing.T) {
	data := []byte("func F() {\n\ta()\n\ta()\n}\n")
	ms := matcher.MatchSet{
		Data: data,
		Matches: []matcher.Match{
			{LineNum: 2, LineStart: 11, LineLen: 4, ByteOffset: 11, PosIdx: 0, PosCount: 1},
			{LineNum: 3, LineStart: 16, LineLen: 4, ByteOffset: 16, PosIdx: 1, PosCount: 1},
		},
		Positions: [][2]int{{1, 4}, {1, 4}},
	}
	inner := NewJSONFormatter()
	bf := NewBlockFormatter(inner)
	got := string(bf.Format(nil, Result{FilePath: "f.go", MatchSet: ms}, false))

	// Two matches in one block dedupe to a single emission covering the
	// whole function, with both highlights rebased.
	if n := strings.Count(got, `"type":"match"`); n != 1 {
		t.Fatalf("emitted %d matches, want 1 (deduped block): %s", n, got)
	}
	if !strings.Contains(got, `"text":"func F() {\n\ta()\n\ta()\n}"`) {
		t.Errorf("block text wrong: %s", got)
	}
	if !strings.Contains(got, `"line_number":1`) {
		t.Errorf("line number should be the block's first line: %s", got)
	}
	if !strings.Contains(got, `"span":[0,22]`) {
		t.Errorf("span must cover the block: %s", got)
	}
	if !strings.Contains(got, `"matches":[{"start":12,"end":15},{"start":17,"end":20}]`) {
		t.Errorf("positions not rebased: %s", got)
	}
}

// Scope-addressable regions (critique): file@func:Name and
// file@section:Name resolve whole blocks.
func TestFindNamedBlock(t *testing.T) {
	goSrc := []byte("package x\n\nfunc alpha() {\n\treturn\n}\n\nfunc beta(n int) int {\n\treturn n\n}\n")
	s, e, cands, ok := FindNamedBlock(goSrc, "x.go", "func", "beta", 0)
	if !ok || len(cands) != 1 {
		t.Fatalf("beta: ok=%v cands=%v", ok, cands)
	}
	if got := string(goSrc[s:e]); got != "func beta(n int) int {\n\treturn n\n}" {
		t.Errorf("beta block = %q", got)
	}

	// Word-bounded: "beta" must not match "betamax".
	src2 := []byte("func betamax() {\n\treturn\n}\n")
	if _, _, _, ok := FindNamedBlock(src2, "x.go", "func", "beta", 0); ok {
		t.Error("beta matched betamax")
	}

	md := []byte("# Intro\ntext\n\n## Gob Encoding\nbody line\nmore\n\n## Next\nother\n")
	s, e, _, ok = FindNamedBlock(md, "b.md", "section", "gob", 0)
	if !ok {
		t.Fatal("section gob not found")
	}
	if got := string(md[s:e]); got != "## Gob Encoding\nbody line\nmore\n" && got != "## Gob Encoding\nbody line\nmore" {
		t.Errorf("section = %q", got)
	}

	if _, _, _, ok := FindNamedBlock(md, "b.md", "func", "gob", 0); ok {
		t.Error("func: should not resolve in Markdown")
	}
}

// Ordinal and parent-path disambiguation for named sections
// (issues.txt #4).
func TestFindNamedBlockDisambiguation(t *testing.T) {
	md := []byte("# Top\n## Recipe 8\na\n### Discussion\nfirst\n## Recipe 9\n### Discussion\nsecond\n")

	_, _, cands, ok := FindNamedBlock(md, "r.md", "section", "Discussion", 0)
	if !ok || len(cands) != 2 {
		t.Fatalf("plain: ok=%v cands=%v", ok, cands)
	}

	s, e, _, ok := FindNamedBlock(md, "r.md", "section", "Discussion", 2)
	if !ok || !strings.Contains(string(md[s:e]), "second") {
		t.Errorf("ordinal 2 = %q", md[s:e])
	}

	if _, _, _, ok := FindNamedBlock(md, "r.md", "section", "Discussion", 3); ok {
		t.Error("ordinal 3 should not resolve (only 2 candidates)")
	}

	s, e, cands, ok = FindNamedBlock(md, "r.md", "section", "Recipe 9/Discussion", 0)
	if !ok || len(cands) != 1 || !strings.Contains(string(md[s:e]), "second") {
		t.Errorf("parent path: ok=%v cands=%v block=%q", ok, cands, md[s:e])
	}
}
