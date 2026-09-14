package matcher

import "testing"

func TestFileFilterMatcher(t *testing.T) {
	primary, _ := NewMatcher([]string{"handler"}, true, false, false, false, MatcherOpts{})
	oldAPI, _ := NewMatcher([]string{"oldCall"}, true, false, false, false, MatcherOpts{})
	newAPI, _ := NewMatcher([]string{"newCall"}, true, false, false, false, MatcherOpts{})

	// Files that call handler AND oldCall but NOT newCall.
	ff := NewFileFilter(primary, []Matcher{oldAPI}, []Matcher{newAPI})

	unmigrated := []byte("handler(x)\noldCall(y)\n")
	migrated := []byte("handler(x)\noldCall(y)\nnewCall(z)\n")
	unrelated := []byte("nothing here\n")

	if !ff.MatchExists(unmigrated) {
		t.Error("unmigrated file should match")
	}
	if ms := ff.FindAll(unmigrated); ms.Len() != 1 {
		t.Error("FindAll should return the handler line")
	}
	if ff.MatchExists(migrated) {
		t.Error("migrated file must be suppressed by --without-file")
	}
	if ms := ff.FindAll(migrated); ms.HasMatch() {
		t.Error("FindAll on migrated file must be empty")
	}
	if ff.CountAll(migrated) != 0 {
		t.Error("CountAll on migrated file must be 0")
	}
	if ff.MatchExists(unrelated) {
		t.Error("unrelated file should not match")
	}

	// Excluded counts only files where the primary would have hit:
	// migrated (via MatchExists + FindAll + CountAll = 3), never
	// unrelated.
	if got := ff.Excluded(); got != 3 {
		t.Errorf("Excluded = %d, want 3", got)
	}
}

func TestFileFilterWithCondition(t *testing.T) {
	primary, _ := NewMatcher([]string{"x"}, true, false, false, false, MatcherOpts{})
	need, _ := NewMatcher([]string{"needed"}, true, false, false, false, MatcherOpts{})
	ff := NewFileFilter(primary, []Matcher{need}, nil)
	if ff.MatchExists([]byte("x alone\n")) {
		t.Error("file without the with-file pattern must be suppressed")
	}
	if !ff.MatchExists([]byte("x and needed\n")) {
		t.Error("file with both must match")
	}
}
