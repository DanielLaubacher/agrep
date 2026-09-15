package walker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Walk must emit files in a deterministic sorted-DFS order (report bug 2):
// budgeted and truncated output derive their stability from it.
func TestWalkDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	// Create files in a deliberately unsorted creation order across
	// nested directories.
	paths := []string{
		"zeta.txt", "alpha.txt", "mid/inner/deep.txt", "mid/b.txt",
		"mid/a.txt", "beta/x.txt", "beta/a/y.txt",
	}
	for _, p := range paths {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	collect := func() []string {
		fileCh, errCh := Walk([]string{dir}, WalkOptions{Recursive: true, NoIgnore: true})
		var got []string
		for f := range fileCh {
			rel, _ := filepath.Rel(dir, f.Path)
			got = append(got, rel)
		}
		for err := range errCh {
			t.Fatalf("walk error: %v", err)
		}
		return got
	}

	want := []string{
		"alpha.txt", "beta/a/y.txt", "beta/x.txt",
		"mid/a.txt", "mid/b.txt", "mid/inner/deep.txt", "zeta.txt",
	}
	first := collect()
	if !slices.Equal(first, want) {
		t.Fatalf("walk order = %v, want sorted DFS %v", first, want)
	}
	for i := 0; i < 5; i++ {
		if got := collect(); !slices.Equal(got, first) {
			t.Fatalf("run %d order = %v, differs from first run %v", i+2, got, first)
		}
	}
}

// Globs: an include glob names files, so it must not prune the
// directories that lead to them (report bug 4); '/' globs match the
// printed path with '**' spanning directories.
func TestWalkGlobs(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		"a.py", "a.txt", "sub/b.py", "sub/d.txt", "sub/deep/c.py", "build/e.py",
	} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("data\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Path globs match the path as printed, so walk a relative root.
	t.Chdir(dir)
	walk := func(globs ...string) []string {
		fileCh, errCh := Walk([]string{"."}, WalkOptions{Recursive: true, NoIgnore: true, Globs: globs})
		var got []string
		for f := range fileCh {
			got = append(got, strings.TrimPrefix(f.Path, "./"))
		}
		for err := range errCh {
			t.Fatalf("walk error: %v", err)
		}
		return got
	}
	cases := []struct {
		globs []string
		want  []string
	}{
		{[]string{"*.py"}, []string{"a.py", "build/e.py", "sub/b.py", "sub/deep/c.py"}},
		{[]string{"**/*.py"}, []string{"a.py", "build/e.py", "sub/b.py", "sub/deep/c.py"}},
		{[]string{"*.py", "!build"}, []string{"a.py", "sub/b.py", "sub/deep/c.py"}},
		{[]string{"sub/*.py"}, []string{"sub/b.py"}},
		{[]string{"sub/**/*.py"}, []string{"sub/b.py", "sub/deep/c.py"}},
		{[]string{"./sub/**"}, []string{"sub/b.py", "sub/d.txt", "sub/deep/c.py"}},
		{[]string{"!*.txt"}, []string{"a.py", "build/e.py", "sub/b.py", "sub/deep/c.py"}},
		{[]string{"!sub/deep"}, []string{"a.py", "a.txt", "build/e.py", "sub/b.py", "sub/d.txt"}},
		{[]string{"*.{py,txt}", "!**/d.txt"}, []string{"a.py", "a.txt", "build/e.py", "sub/b.py", "sub/deep/c.py"}},
	}
	for _, c := range cases {
		if got := walk(c.globs...); !slices.Equal(got, c.want) {
			t.Errorf("globs %v: got %v, want %v", c.globs, got, c.want)
		}
	}
	if !MatchesGlobs([]string{"src/**/*.go"}, "src/a/b.go") || MatchesGlobs([]string{"src/**/*.go"}, "lib/b.go") {
		t.Error("MatchesGlobs should apply path globs to the relative path")
	}
}
