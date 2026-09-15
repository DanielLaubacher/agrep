package walker

import (
	"os"
	"path/filepath"
	"slices"
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
