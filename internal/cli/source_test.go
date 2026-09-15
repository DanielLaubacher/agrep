package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestGitContextDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := gitContextDir(nil); got != "." {
		t.Errorf("no paths: got %q, want \".\"", got)
	}
	if got := gitContextDir([]string{dir}); got != dir {
		t.Errorf("directory path: got %q, want %q", got, dir)
	}
	if got := gitContextDir([]string{file}); got != dir {
		t.Errorf("file path: got %q, want its parent %q", got, dir)
	}
}

// runGit runs a git command in dir with a fixed test identity, so commits
// don't depend on the host's git config.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// --changed-since must resolve the repo and diff from the searched path,
// not the test process's own working directory — this test's cwd (the
// internal/cli package dir) sits inside the agrep repo itself, a
// completely unrelated git repository from the synthetic one built here
// (report bug: --changed-since previously resolved against cwd's repo).
func TestChangedFilesUsesSearchPathNotCWD(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "commit.gpgsign", "false")

	unchanged := filepath.Join(repo, "unchanged.txt")
	changed := filepath.Join(repo, "changed.txt")
	if err := os.WriteFile(unchanged, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(changed, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "base")

	if err := os.WriteFile(changed, []byte("base\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "changed.txt")
	runGit(t, repo, "commit", "-q", "-m", "second")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(cwd, repo) {
		t.Fatalf("test cwd %q unexpectedly inside synthetic repo %q", cwd, repo)
	}

	list, err := changedFiles("HEAD~1", []string{repo}, nil)
	if err != nil {
		t.Fatalf("changedFiles: %v (cwd=%s, repo=%s)", err, cwd, repo)
	}

	var rels []string
	for _, p := range list {
		rels = append(rels, filepath.Base(p))
	}
	if !slices.Contains(rels, "changed.txt") {
		t.Errorf("changed.txt missing from %v", rels)
	}
	if slices.Contains(rels, "unchanged.txt") {
		t.Errorf("unchanged.txt should not appear in %v", rels)
	}
}
