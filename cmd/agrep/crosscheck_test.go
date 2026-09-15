package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Cross-checks: agrep's grep-compatible flags should agree with the real
// tools they mimic (grep, ripgrep) on both matched output and exit codes,
// and its plain line-selection behavior should agree with the narrower
// slice of that job sed and awk do natively. Every external tool is
// looked up by absolute-ish exec.LookPath, which resolves the real binary
// directly (never a shell alias/function), so results reflect the actual
// tool, not an interactive shell wrapper.

var (
	agrepBinOnce sync.Once
	agrepBinPath string
	agrepBinErr  error
)

// buildAgrep compiles this package's current source to a temp binary once
// per test process, so cross-checks exercise the code as it stands rather
// than a possibly-stale bin/agrep.
func buildAgrep(t *testing.T) string {
	t.Helper()
	agrepBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "agrep-crosscheck")
		if err != nil {
			agrepBinErr = err
			return
		}
		agrepBinPath = filepath.Join(dir, "agrep")
		cmd := exec.Command("go", "build", "-o", agrepBinPath, ".")
		cmd.Env = append(os.Environ(), "GOEXPERIMENT=simd")
		if out, err := cmd.CombinedOutput(); err != nil {
			agrepBinErr = &buildError{err, out}
		}
	})
	if agrepBinErr != nil {
		t.Fatalf("building agrep: %v", agrepBinErr)
	}
	return agrepBinPath
}

type buildError struct {
	err error
	out []byte
}

func (b *buildError) Error() string { return b.err.Error() + "\n" + string(b.out) }

// requireTool skips the test if name isn't installed, so this file stays
// portable to environments without the comparison tools.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not found in PATH: %v", name, err)
	}
	return path
}

// runTool runs an already-resolved binary path with args in dir and
// returns combined stdout and the process exit code (0/1/2 like grep).
func runTool(t *testing.T, bin, dir string, extraEnv []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{} // agrep's config-source notices are noise here
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("running %s %v: %v", bin, args, err)
		}
	}
	return out.String(), code
}

// runAgrep runs the freshly built agrep binary with a neutral environment:
// AGREP_CONFIG_PATH=/dev/null so ~/.agrep (smart-case, hidden, follow,
// glob excludes) never changes behavior underneath a comparison, per the
// agrep-benchmark-setup memory.
func runAgrep(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	return runTool(t, buildAgrep(t), dir, []string{"AGREP_CONFIG_PATH=/dev/null"}, args...)
}

// initGitRepo makes dir a minimal git repo with everything committed, with
// a fixed identity so it doesn't depend on the host's git config.
func initGitRepo(t *testing.T, git, dir string) {
	t.Helper()
	env := []string{
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "commit.gpgsign", "false"},
		{"add", "."},
		{"commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

func writeFixtures(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// sampleText is a small, deterministic fixture exercising mixed case,
// digits, repeated lines (for context tests), and punctuation.
const sampleText = `apple pie
Banana Bread
foo123bar
hello world
HELLO AGAIN
error: disk full
warning: low memory
error: disk full
retry count=3
done.
`

func sortedLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	sort.Strings(lines)
	return lines
}

// --- grep ---

func TestCrossCheckGrepDefaultMatch(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	// agrep's regex syntax is always extended (Go regexp/RE2-flavored,
	// like grep -E), never POSIX BRE. Patterns using +, |, etc. must be
	// compared against `grep -E`, not bare grep, which defaults to BRE
	// and would treat "+"/"|" as literal characters.
	cases := []struct {
		name      string
		pattern   string
		extendedE bool
	}{
		{"literal", "apple", false},
		{"regex-class", "[0-9]+", true},
		{"regex-alternation", "error|warning", true},
		{"anchor", "^error", false},
		{"case-sensitive-miss", "HELLO", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grepArgs := []string{tc.pattern, "sample.txt"}
			if tc.extendedE {
				grepArgs = []string{"-E", tc.pattern, "sample.txt"}
			}
			got, gotCode := runAgrep(t, dir, tc.pattern, "sample.txt")
			want, wantCode := runTool(t, grep, dir, nil, grepArgs...)
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\ngrep:  %q", got, want)
			}
			if gotCode != wantCode {
				t.Errorf("exit code mismatch: agrep=%d grep=%d", gotCode, wantCode)
			}
		})
	}
}

func TestCrossCheckGrepIgnoreCaseAndInvert(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	cases := [][]string{
		{"-i", "hello"},
		{"-v", "error"},
		{"-vi", "ERROR"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			full := append(append([]string{}, args...), "sample.txt")
			got, gotCode := runAgrep(t, dir, full...)
			want, wantCode := runTool(t, grep, dir, nil, full...)
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\ngrep:  %q", got, want)
			}
			if gotCode != wantCode {
				t.Errorf("exit code mismatch: agrep=%d grep=%d", gotCode, wantCode)
			}
		})
	}
}

func TestCrossCheckGrepLineNumbers(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	got, _ := runAgrep(t, dir, "-n", "error", "sample.txt")
	want, _ := runTool(t, grep, dir, nil, "-n", "error", "sample.txt")
	if got != want {
		t.Errorf("output mismatch\nagrep: %q\ngrep:  %q", got, want)
	}
}

func TestCrossCheckGrepWordRegexp(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	// "foo" appears only glued into "foo123bar" — -w must reject it in
	// both tools (exit 1, no output).
	for _, pattern := range []string{"foo", "error"} {
		t.Run(pattern, func(t *testing.T) {
			got, gotCode := runAgrep(t, dir, "-w", pattern, "sample.txt")
			want, wantCode := runTool(t, grep, dir, nil, "-w", pattern, "sample.txt")
			if got != want || gotCode != wantCode {
				t.Errorf("agrep=(%q,%d) grep=(%q,%d)", got, gotCode, want, wantCode)
			}
		})
	}
}

func TestCrossCheckGrepContext(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	cases := [][]string{
		{"-A1", "error"},
		{"-B1", "error"},
		{"-C1", "error"},
		{"-A2", "-B1", "error"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			full := append(append([]string{}, args...), "sample.txt")
			got, _ := runAgrep(t, dir, full...)
			want, _ := runTool(t, grep, dir, nil, full...)
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\ngrep:  %q", got, want)
			}
		})
	}
}

func TestCrossCheckGrepOnlyMatching(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	cases := []string{`[0-9]+`, `error|warning`, `\w+`}
	for _, pattern := range cases {
		t.Run(pattern, func(t *testing.T) {
			got, _ := runAgrep(t, dir, "-o", pattern, "sample.txt")
			want, _ := runTool(t, grep, dir, nil, "-oE", pattern, "sample.txt")
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\ngrep -oE:  %q", got, want)
			}
		})
	}
}

func TestCrossCheckGrepFixedStrings(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{
		"fixed.txt": "a.b+c price: $3.50 (discount)\na.b+c should not match axbc\n",
	})

	got, _ := runAgrep(t, dir, "-F", "a.b+c", "fixed.txt")
	want, _ := runTool(t, grep, dir, nil, "-F", "a.b+c", "fixed.txt")
	if got != want {
		t.Errorf("output mismatch\nagrep: %q\ngrep:  %q", got, want)
	}
}

// TestCrossCheckGrepCountAndFilesWithMatches covers -c/-l over multiple
// files. Directory read order (agrep's walker uses raw getdents64, not a
// sorted readdir) need not match grep's, so both sides are sorted before
// comparing.
func TestCrossCheckGrepCountAndFilesWithMatches(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{
		"a.txt": "apple pie\n",
		"b.txt": "apple sauce\nsecond apple\n",
		"c.txt": "no fruit here\n",
	})

	t.Run("-c on files with matches", func(t *testing.T) {
		got, _ := runAgrep(t, dir, "-c", "apple", "a.txt", "b.txt")
		want, _ := runTool(t, grep, dir, nil, "-c", "apple", "a.txt", "b.txt")
		if g, w := sortedLines(got), sortedLines(want); !equalSlices(g, w) {
			t.Errorf("agrep=%v grep=%v", g, w)
		}
	})
	// agrep intentionally omits zero-match files from -c output (an
	// agent-oriented, signal-dense choice — see internal/output/text.go's
	// countOnly branch), where grep prints "file:0". Not a bug: pin the
	// documented divergence instead of asserting equality here.
	t.Run("-c omits zero-match files by design", func(t *testing.T) {
		got, _ := runAgrep(t, dir, "-c", "apple", "a.txt", "b.txt", "c.txt")
		want, _ := runTool(t, grep, dir, nil, "-c", "apple", "a.txt", "b.txt", "c.txt")
		if strings.Contains(got, "c.txt") {
			t.Errorf("agrep -c unexpectedly mentions zero-match c.txt: %q", got)
		}
		if !strings.Contains(want, "c.txt:0") {
			t.Fatalf("test assumption broken: grep -c should print c.txt:0, got %q", want)
		}
	})
	t.Run("-l recursive", func(t *testing.T) {
		got, _ := runAgrep(t, dir, "-rl", "apple", ".")
		want, _ := runTool(t, grep, dir, nil, "-rl", "apple", ".")
		if g, w := sortedLines(got), sortedLines(want); !equalSlices(g, w) {
			t.Errorf("agrep=%v grep=%v", g, w)
		}
	})
}

func TestCrossCheckGrepExitCodes(t *testing.T) {
	grep := requireTool(t, "grep")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	t.Run("match", func(t *testing.T) {
		_, gotCode := runAgrep(t, dir, "apple", "sample.txt")
		_, wantCode := runTool(t, grep, dir, nil, "apple", "sample.txt")
		if gotCode != wantCode || gotCode != 0 {
			t.Errorf("agrep=%d grep=%d want 0", gotCode, wantCode)
		}
	})
	t.Run("no-match", func(t *testing.T) {
		_, gotCode := runAgrep(t, dir, "nowhere-to-be-found", "sample.txt")
		_, wantCode := runTool(t, grep, dir, nil, "nowhere-to-be-found", "sample.txt")
		if gotCode != wantCode || gotCode != 1 {
			t.Errorf("agrep=%d grep=%d want 1", gotCode, wantCode)
		}
	})
	t.Run("error", func(t *testing.T) {
		_, gotCode := runAgrep(t, dir, "apple", "does-not-exist.txt")
		_, wantCode := runTool(t, grep, dir, nil, "apple", "does-not-exist.txt")
		if gotCode != wantCode || gotCode != 2 {
			t.Errorf("agrep=%d grep=%d want 2", gotCode, wantCode)
		}
	})
}

// --- ripgrep ---

// TestCrossCheckRipgrepGitignore checks the behavior plain grep doesn't
// have: recursive search skips .gitignore'd files by default, same as
// ripgrep, and --no-ignore restores plain-grep-style visibility.
//
// ripgrep only honors .gitignore by default inside an actual git
// repository (--no-require-git lifts that), so the fixture is git-init'd
// to match rg's real default rather than comparing against a mode no one
// runs.
func TestCrossCheckRipgrepGitignore(t *testing.T) {
	rg := requireTool(t, "rg")
	grep := requireTool(t, "grep")
	git := requireTool(t, "git")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{
		".gitignore":      "ignored.txt\n",
		"sub/ignored.txt": "target ignored\n",
		"sub/kept.txt":    "target kept\n",
	})
	initGitRepo(t, git, dir)

	t.Run("default skips gitignored file like rg", func(t *testing.T) {
		got, _ := runAgrep(t, dir, "-r", "target", ".")
		want, _ := runTool(t, rg, dir, []string{"RIPGREP_CONFIG_PATH="}, "--no-config", "target", ".")
		if g, w := sortedLines(got), sortedLines(want); !equalSlices(g, w) {
			t.Errorf("agrep=%v rg=%v", g, w)
		}
	})
	t.Run("--no-ignore sees everything like plain grep -r", func(t *testing.T) {
		got, _ := runAgrep(t, dir, "-r", "--no-ignore", "target", ".")
		want, _ := runTool(t, grep, dir, nil, "-r", "target", ".")
		if g, w := sortedLines(got), sortedLines(want); !equalSlices(g, w) {
			t.Errorf("agrep=%v grep=%v", g, w)
		}
	})
}

// --- sed / awk ---

// TestCrossCheckSedLineSelection: agrep's plain (no -n/-c/-o) output on a
// single file is exactly the set of matching lines, the same job
// `sed -n '/pat/p'` does.
func TestCrossCheckSedLineSelection(t *testing.T) {
	sed := requireTool(t, "sed")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	cases := []struct {
		name   string
		agrep  []string
		sedExp string
	}{
		{"match", []string{"error"}, "/error/p"},
		{"invert", []string{"-v", "error"}, "/error/!p"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := runAgrep(t, dir, append(append([]string{}, tc.agrep...), "sample.txt")...)
			want, _ := runTool(t, sed, dir, nil, "-n", tc.sedExp, "sample.txt")
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\nsed:   %q", got, want)
			}
		})
	}
}

// TestCrossCheckAwkLineSelection: awk '/pat/' and awk '!/pat/' do the same
// line-selection job as agrep's default match/-v output.
func TestCrossCheckAwkLineSelection(t *testing.T) {
	awk := requireTool(t, "awk")
	dir := t.TempDir()
	writeFixtures(t, dir, map[string]string{"sample.txt": sampleText})

	cases := []struct {
		name    string
		agrep   []string
		awkExpr string
	}{
		{"match", []string{"warning"}, "/warning/"},
		{"invert", []string{"-v", "error"}, "!/error/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := runAgrep(t, dir, append(append([]string{}, tc.agrep...), "sample.txt")...)
			want, _ := runTool(t, awk, dir, nil, tc.awkExpr, "sample.txt")
			if got != want {
				t.Errorf("output mismatch\nagrep: %q\nawk:   %q", got, want)
			}
		})
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
