package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/lang"
)

// --structural silently defaulted to Generic (delimiters only, no
// string/comment awareness) whenever --lang was omitted, even against a
// single obviously-Go file — a silent trap matching --block/--scope/
// --sections' own per-file auto-detection would have avoided (report
// bug). resolveStructuralLang covers the cases that trap has to handle.
func TestResolveStructuralLang(t *testing.T) {
	dir := t.TempDir()
	goFile := filepath.Join(dir, "a.go")
	pyFile := filepath.Join(dir, "b.py")
	for _, f := range []string{goFile, pyFile} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name string
		cfg  Config
		want lang.Lang
	}{
		{"non-structural ignores paths", Config{Structural: false, Paths: []string{goFile}}, lang.Generic},
		{"explicit --lang wins over path", Config{Structural: true, Lang: "py", Paths: []string{goFile}}, lang.Python},
		{"explicit --lang generic suppresses auto-detect", Config{Structural: true, Lang: "generic", Paths: []string{goFile}}, lang.Generic},
		{"single recognizable file auto-detects", Config{Structural: true, Paths: []string{goFile}}, lang.Go},
		{"a directory can't be auto-detected", Config{Structural: true, Paths: []string{dir}}, lang.Generic},
		{"no paths falls back to generic", Config{Structural: true}, lang.Generic},
		{"files of different families are ambiguous", Config{Structural: true, Paths: []string{goFile, pyFile}}, lang.Generic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStructuralLang(tc.cfg); got != tc.want {
				t.Errorf("resolveStructuralLang(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
