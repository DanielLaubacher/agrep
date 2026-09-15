package matcher

import (
	"strings"
	"testing"
)

// Line-accuracy contract (report bugs 3+4): every Match covers its full
// line regardless of any display-column setting, matches on the same line
// collapse into one record, and FindAll/CountAll agree.
func TestLineAccurateRecords(t *testing.T) {
	// A long line with two matches far apart, surrounded by short lines.
	long := strings.Repeat("x", 300) + " needle " + strings.Repeat("y", 300) + " needle tail"
	data := []byte("first needle line\n" + long + "\nlast line no match\n")

	for _, tc := range []struct {
		name    string
		pattern string
		fixed   bool
	}{
		{"boyer-moore", "needle", true},
		{"regex", "need.e", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewMatcher([]string{tc.pattern}, tc.fixed, false, false, false,
				MatcherOpts{NeedLineNums: true})
			if err != nil {
				t.Fatal(err)
			}

			ms := m.FindAll(data)
			if got, want := len(ms.Matches), 2; got != want {
				t.Fatalf("got %d match records, want %d (one per line)", got, want)
			}
			if got := m.CountAll(data); got != len(ms.Matches) {
				t.Errorf("CountAll = %d, FindAll lines = %d; totals must agree", got, len(ms.Matches))
			}

			// Second record: the long line, full bounds, both positions.
			lm := &ms.Matches[1]
			if lm.LineNum != 2 {
				t.Errorf("long line LineNum = %d, want 2", lm.LineNum)
			}
			if got := string(ms.LineBytes(1)); got != long {
				t.Errorf("long line record covers %d bytes, want full line (%d bytes)", len(got), len(long))
			}
			if lm.PosCount != 2 {
				t.Errorf("long line PosCount = %d, want 2 (both matches on one record)", lm.PosCount)
			}
			for _, pos := range ms.MatchPositions(1) {
				if got := string(ms.LineBytes(1)[pos[0]:pos[1]]); got != "needle" {
					t.Errorf("position %v resolves to %q, want %q", pos, got, "needle")
				}
			}
		})
	}
}

func TestLineBoundsFromOffset(t *testing.T) {
	data := []byte("abc\ndefg\nhi")
	for _, tc := range []struct {
		off, start, end int
	}{
		{0, 0, 3},  // first line
		{2, 0, 3},  // still first line
		{4, 4, 8},  // second line start
		{7, 4, 8},  // second line last byte
		{9, 9, 11}, // final line, no trailing newline
	} {
		s, e := lineBoundsFromOffset(data, tc.off)
		if s != tc.start || e != tc.end {
			t.Errorf("off %d: got [%d,%d), want [%d,%d)", tc.off, s, e, tc.start, tc.end)
		}
	}
}

// -i must fold Unicode, not just ASCII (report bug 7): non-ASCII
// case-insensitive patterns route to the stdlib regex engine.
func TestUnicodeCaseFold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		fixed   bool
		data    string
		want    int
	}{
		{"latin", "müller", false, "MÜLLER\nmüller\n", 2},
		{"latin-fixed", "müller", true, "MÜLLER\nmüller\n", 2},
		{"greek", "σίσυφος", false, "ΣΊΣΥΦΟΣ\nσίσυφος\n", 2},
		{"regex", `mül\..`, false, "MÜL.X\nmül.x\n", 2},
		{"ascii-still-fast", "hello", false, "HELLO\nhello\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewMatcher([]string{tc.pattern}, tc.fixed, false, true, false, MatcherOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if got := m.CountAll([]byte(tc.data)); got != tc.want {
				t.Errorf("CountAll = %d, want %d", got, tc.want)
			}
			if got := len(m.FindAll([]byte(tc.data)).Matches); got != tc.want {
				t.Errorf("FindAll lines = %d, want %d", got, tc.want)
			}
		})
	}
}
