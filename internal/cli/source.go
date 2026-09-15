package cli

// fileSource centralizes where searched files come from: an explicit
// list (--files-from, '-' = stdin), the recursive walk, or literal
// paths. Every aggregation and search mode draws from this one place,
// so list-driven composition (`agrep -l ... | agrep --files-from -`)
// works everywhere.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/DanielLaubacher/agrep/internal/output"
	"github.com/DanielLaubacher/agrep/internal/walker"
)

// walkErrs collects traversal errors from the walk's error stream so
// they can be surfaced after the results — in the JSON stream, the
// summary's errors count, and the exit code. A missing root must never
// read as a clean "no match" (report bug 8).
type walkErrs struct {
	mu   sync.Mutex
	errs []error
	done chan struct{} // closed once the error stream has drained
}

func newWalkErrs() *walkErrs {
	return &walkErrs{done: make(chan struct{})}
}

// collect drains errCh in the background.
func (we *walkErrs) collect(errCh <-chan error) {
	go func() {
		defer close(we.done)
		for err := range errCh {
			we.mu.Lock()
			we.errs = append(we.errs, err)
			we.mu.Unlock()
		}
	}()
}

// wait blocks until the error stream has drained and returns the errors.
// Safe to call on a source with no walk (returns nil immediately).
func (we *walkErrs) wait() []error {
	if we == nil {
		return nil
	}
	<-we.done
	we.mu.Lock()
	defer we.mu.Unlock()
	return we.errs
}

// errResults converts collected walk errors into error Results for the
// formatter (stderr printing happens at the write site).
func (we *walkErrs) errResults() []output.Result {
	var results []output.Result
	for _, err := range we.wait() {
		path := ""
		var werr *walker.WalkError
		if errors.As(err, &werr) {
			path = werr.Path
		}
		results = append(results, output.Result{FilePath: path, Err: err})
	}
	return results
}

// logWalkErrs reports collected walk errors on stderr — for aggregate
// modes (outline, histogram) whose summaries have no error field yet.
func logWalkErrs(we *walkErrs) {
	for _, err := range we.wait() {
		logWarn("walk: %v", err)
	}
}

// fileSource returns the channel of files to search, plus a collector
// for walk errors (nil-safe; nil when the source cannot produce them).
func fileSource(cfg Config, paths []string) (<-chan walker.FileEntry, *walkErrs, error) {
	if cfg.ChangedSince != "" {
		list, err := changedFiles(cfg.ChangedSince, paths, cfg.Globs)
		if err != nil {
			return nil, nil, err
		}
		ch := make(chan walker.FileEntry, len(list))
		for _, p := range list {
			ch <- walker.FileEntry{Path: p}
		}
		close(ch)
		return ch, nil, nil
	}

	if cfg.FilesFrom != "" {
		list, err := loadFileList(cfg.FilesFrom)
		if err != nil {
			return nil, nil, err
		}
		ch := make(chan walker.FileEntry, len(list))
		for _, p := range list {
			ch <- walker.FileEntry{Path: p}
		}
		close(ch)
		return ch, nil, nil
	}

	if cfg.Recursive {
		ch, errCh := walker.Walk(paths, walker.WalkOptions{
			Recursive:      true,
			NoIgnore:       cfg.NoIgnore,
			Hidden:         cfg.Hidden,
			FollowSymlinks: cfg.FollowSymlinks,
			Globs:          cfg.Globs,
		})
		we := newWalkErrs()
		we.collect(errCh)
		return ch, we, nil
	}

	ch := make(chan walker.FileEntry, len(paths))
	for _, p := range paths {
		ch <- walker.FileEntry{Path: p}
	}
	close(ch)
	return ch, nil, nil
}

// loadFileList reads one path per line ('-' = stdin); blank lines are
// skipped.
func loadFileList(from string) ([]string, error) {
	var data []byte
	var err error
	if from == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, err
	}
	var list []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			list = append(list, line)
		}
	}
	return list, nil
}

// changedFiles resolves --changed-since REF: files changed between REF
// and the worktree plus untracked (not ignored) files, restricted to
// the search paths and include/exclude globs. Requires the git binary;
// this is the one stateless place agrep shells out. Deleted files are
// dropped; tracked files bypass ignore rules deliberately (a tracked
// file is searchable even when a .gitignore would hide it from walks).
func changedFiles(ref string, paths []string, globs []string) ([]string, error) {
	root, err := gitOutput("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("--changed-since: not in a git repository (%v)", err)
	}
	rootDir := strings.TrimRight(string(root), "\n")

	diff, err := gitOutput("diff", "--name-only", "-z", ref, "--")
	if err != nil {
		return nil, fmt.Errorf("--changed-since %s: %v", ref, err)
	}
	untracked, err := gitOutput("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("--changed-since: %v", err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var list []string
	for _, chunk := range [][]byte{diff, untracked} {
		for rel := range strings.SplitSeq(string(chunk), "\x00") {
			if rel == "" || seen[rel] {
				continue
			}
			seen[rel] = true
			abs := filepath.Join(rootDir, rel)
			if st, err := os.Stat(abs); err != nil || st.IsDir() {
				continue // deleted since, or a submodule
			}
			// Prefer a cwd-relative path for display parity with walks.
			display := abs
			if r, err := filepath.Rel(cwd, abs); err == nil && !strings.HasPrefix(r, "..") {
				display = r
			}
			if !underAnyPath(display, paths) {
				continue
			}
			if !walker.MatchesGlobs(globs, display) {
				continue
			}
			list = append(list, display)
		}
	}
	return list, nil
}

// underAnyPath reports whether file lies at or under one of the search
// paths. An empty path list means the whole repository.
func underAnyPath(file string, paths []string) bool {
	if len(paths) == 0 {
		return true
	}
	for _, p := range paths {
		cp := filepath.Clean(p)
		if cp == "." || file == cp || strings.HasPrefix(file, cp+"/") {
			return true
		}
	}
	return false
}

// gitOutput runs a git subcommand and returns its stdout.
func gitOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	return out, nil
}
