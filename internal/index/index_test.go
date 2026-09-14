package index

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func candidatePaths(t *testing.T, ix *Index, lit string) map[string]bool {
	t.Helper()
	ids, constrained := ix.CandidateIDs([][]byte{[]byte(lit)})
	if !constrained {
		t.Fatalf("literal %q should constrain the index", lit)
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[ix.Entries[id].Path] = true
	}
	return out
}

func TestBuildCandidatesSuperset(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	files := map[string]string{
		"a.txt":       "the quick brown fox\njumps over ConnectTimeout\n",
		"sub/b.txt":   "nothing interesting here\n",
		"sub/c.go":    "func ConnectTimeout() int { return 42 }\n",
		"d.md":        "CONNECTTIMEOUT in caps\n",
		"bin.dat":     "prefix\x00binary",
		"e.txt":       "connect timeout separately\n",
	}
	writeTree(t, root, files)

	if _, _, err := Build(root, WalkOpts{}); err != nil {
		t.Fatal(err)
	}
	ix, err := Load(Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	if ix.Meta.Files != 6 {
		t.Fatalf("expected 6 manifest files, got %d", ix.Meta.Files)
	}

	// Superset property: every file whose content contains the literal
	// (case-insensitively — the index is lowercased) must be a candidate.
	for _, lit := range []string{"ConnectTimeout", "quick brown", "interesting"} {
		cands := candidatePaths(t, ix, lit)
		for p, content := range files {
			if bytes.Contains(bytes.ToLower([]byte(content)), bytes.ToLower([]byte(lit))) && !bytes.Contains([]byte(content), []byte{0}) {
				if !cands[p] {
					t.Errorf("literal %q: file %s contains it but is not a candidate", lit, p)
				}
			}
		}
	}

	// Selectivity sanity: "quick brown" appears only in a.txt.
	if cands := candidatePaths(t, ix, "quick brown"); len(cands) != 1 || !cands["a.txt"] {
		t.Errorf("expected only a.txt for 'quick brown', got %v", cands)
	}

	// Absent literal → empty candidates, still constrained.
	if cands := candidatePaths(t, ix, "zzqxjvzz"); len(cands) != 0 {
		t.Errorf("expected no candidates for absent literal, got %v", cands)
	}

	// Short literal → unconstrained.
	if _, constrained := ix.CandidateIDs([][]byte{[]byte("ab")}); constrained {
		t.Error("2-byte literal must not constrain")
	}

	// Binary file recorded but never a candidate.
	for _, e := range ix.Entries {
		if e.Path == "bin.dat" && !e.Binary {
			t.Error("bin.dat should be flagged binary")
		}
	}
}

func TestSweepDetectsChanges(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt": "alpha\n",
		"b.txt": "beta\n",
	})
	if _, _, err := Build(root, WalkOpts{}); err != nil {
		t.Fatal(err)
	}
	ix, err := Load(Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()

	sweep, err := ix.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if len(sweep.Dirty) != 0 || len(sweep.Stale) != 0 {
		t.Fatalf("fresh index should sweep clean, got dirty=%v stale=%v", sweep.Dirty, sweep.Stale)
	}

	// Modify, add, delete — backdating not needed: size changes count.
	writeTree(t, root, map[string]string{"a.txt": "alpha modified\n", "new.txt": "gamma\n"})
	if err := os.Remove(filepath.Join(root, "b.txt")); err != nil {
		t.Fatal(err)
	}

	sweep, err = ix.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	dirty := map[string]bool{}
	for _, p := range sweep.Dirty {
		rel, _ := filepath.Rel(root, p)
		dirty[rel] = true
	}
	if !dirty["a.txt"] || !dirty["new.txt"] || len(dirty) != 2 {
		t.Errorf("expected dirty {a.txt,new.txt}, got %v", dirty)
	}
	stalePaths := map[string]bool{}
	for id := range sweep.Stale {
		stalePaths[ix.Entries[id].Path] = true
	}
	if !stalePaths["a.txt"] || !stalePaths["b.txt"] || len(stalePaths) != 2 {
		t.Errorf("expected stale {a.txt,b.txt}, got %v", stalePaths)
	}
}

func TestIncrementalReuse(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt": "alpha content\n",
		"b.txt": "beta content\n",
		"c.txt": "gamma content\n",
	})
	if _, stats, err := Build(root, WalkOpts{}); err != nil || stats.Read != 3 {
		t.Fatalf("first build: err=%v stats=%+v", err, stats)
	}

	// Unchanged rebuild: zero reads.
	if _, stats, err := Build(root, WalkOpts{}); err != nil || stats.Read != 0 || stats.Reused != 3 {
		t.Fatalf("unchanged rebuild should reuse all: err=%v stats=%+v", err, stats)
	}

	// One edit: exactly one read. Ensure mtime moves even on coarse clocks.
	time.Sleep(10 * time.Millisecond)
	writeTree(t, root, map[string]string{"b.txt": "beta edited\n"})
	_, stats, err := Build(root, WalkOpts{})
	if err != nil || stats.Read != 1 || stats.Reused != 2 {
		t.Fatalf("one-edit rebuild: err=%v stats=%+v", err, stats)
	}

	// Verify the rebuilt index reflects the edit.
	ix, err := Load(Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if cands := candidatePaths(t, ix, "edited"); len(cands) != 1 || !cands["b.txt"] {
		t.Errorf("expected b.txt for 'edited', got %v", cands)
	}
	if cands := candidatePaths(t, ix, "beta content"); len(cands) != 0 {
		t.Errorf("stale trigrams should be gone, got %v", cands)
	}
}

func TestParentReusesChildDigests(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	parent := t.TempDir()
	writeTree(t, parent, map[string]string{
		"child/x.txt": "xylophone content\n",
		"child/y.txt": "yesterday content\n",
		"top.txt":     "toplevel content\n",
	})

	child := filepath.Join(parent, "child")
	if _, stats, err := Build(child, WalkOpts{}); err != nil || stats.Read != 2 {
		t.Fatalf("child build: err=%v stats=%+v", err, stats)
	}

	// Parent build reads only the file the child didn't cover.
	_, stats, err := Build(parent, WalkOpts{})
	if err != nil || stats.Read != 1 || stats.Reused != 2 {
		t.Fatalf("parent build should reuse child digests: err=%v stats=%+v", err, stats)
	}
	ix, err := Load(Dir(parent))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if cands := candidatePaths(t, ix, "xylophone"); len(cands) != 1 || !cands["child/x.txt"] {
		t.Errorf("expected child/x.txt for 'xylophone', got %v", cands)
	}
}

func TestClearUnder(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	base := t.TempDir()
	r1 := filepath.Join(base, "one")
	r2 := filepath.Join(base, "two")
	writeTree(t, r1, map[string]string{"a.txt": "alpha\n"})
	writeTree(t, r2, map[string]string{"b.txt": "beta\n"})
	if _, _, err := Build(r1, WalkOpts{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Build(r2, WalkOpts{}); err != nil {
		t.Fatal(err)
	}

	cleared, err := ClearUnder(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 2 {
		t.Fatalf("expected 2 roots cleared, got %v", cleared)
	}
	if _, err := ReadMeta(Dir(r1)); err == nil {
		t.Error("r1 index should be gone")
	}
	if again, _ := ClearUnder(base); len(again) != 0 {
		t.Errorf("second clear should find nothing, got %v", again)
	}
}

func TestVocabLookup(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"a.txt": "timeout timeout retry\n",
		"b.txt": "timeout only\n",
	})
	if _, _, err := Build(root, WalkOpts{}); err != nil {
		t.Fatal(err)
	}
	files, occs, ok := VocabLookup(Dir(root), "timeout")
	if !ok || files != 2 || occs != 3 {
		t.Errorf("timeout: got files=%d occs=%d ok=%v, want 2/3/true", files, occs, ok)
	}
	if _, _, ok := VocabLookup(Dir(root), "absentword"); ok {
		t.Error("absent token should not be found")
	}
}

func TestOptsEqual(t *testing.T) {
	a := WalkOpts{Hidden: true, Globs: []string{"*.go", "!vendor"}}
	b := WalkOpts{Hidden: true, Globs: []string{"!vendor", "*.go"}}
	if !a.Equal(b) {
		t.Error("glob order must not matter")
	}
	if a.Equal(WalkOpts{Hidden: false, Globs: a.Globs}) {
		t.Error("hidden mismatch must fail")
	}
}
