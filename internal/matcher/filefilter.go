package matcher

// FileFilterMatcher gates a primary matcher on file-level content
// conditions: the file must contain every "with" pattern and none of
// the "without" patterns, or the primary's matches are suppressed.
// This expresses "files matching A but not B" — the shape of every
// migration audit — in one pass. Probes use MatchExists (early-exit)
// on the same buffer, so the cost is at most one extra scan per file.
//
// Deliberately does NOT implement LineBounded: file-level conditions
// need the whole buffer, so the parallel chunked path must not split
// it.

import "sync/atomic"

type FileFilterMatcher struct {
	inner   Matcher
	with    []Matcher
	without []Matcher

	// excluded counts files where the primary matched but a file-level
	// condition failed — reported so filtering is never silent.
	excluded atomic.Int64
}

// NewFileFilter wraps inner with file-level with/without conditions.
func NewFileFilter(inner Matcher, with, without []Matcher) *FileFilterMatcher {
	return &FileFilterMatcher{inner: inner, with: with, without: without}
}

// Excluded returns how many files matched the primary pattern but were
// suppressed by a failing condition.
func (f *FileFilterMatcher) Excluded() int64 { return f.excluded.Load() }

// passes reports whether the file-level conditions hold for data.
func (f *FileFilterMatcher) passes(data []byte) bool {
	for _, m := range f.with {
		if !m.MatchExists(data) {
			return false
		}
	}
	for _, m := range f.without {
		if m.MatchExists(data) {
			return false
		}
	}
	return true
}

// noteExcluded tallies a suppressed file if the primary would have hit.
func (f *FileFilterMatcher) noteExcluded(data []byte) {
	if f.inner.MatchExists(data) {
		f.excluded.Add(1)
	}
}

func (f *FileFilterMatcher) FindAll(data []byte) MatchSet {
	if !f.passes(data) {
		f.noteExcluded(data)
		return MatchSet{}
	}
	return f.inner.FindAll(data)
}

func (f *FileFilterMatcher) MatchExists(data []byte) bool {
	if !f.passes(data) {
		f.noteExcluded(data)
		return false
	}
	return f.inner.MatchExists(data)
}

func (f *FileFilterMatcher) CountAll(data []byte) int {
	if !f.passes(data) {
		f.noteExcluded(data)
		return 0
	}
	return f.inner.CountAll(data)
}

// FindLine delegates: a single line cannot carry file-level conditions
// (watch mode streams lines), so conditions are ignored there.
func (f *FileFilterMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	return f.inner.FindLine(line, lineNum, byteOffset)
}

// OnlyMatch forwards -o detection through the wrapper.
func (f *FileFilterMatcher) OnlyMatch() bool {
	type onlyMatcher interface{ OnlyMatch() bool }
	if om, ok := f.inner.(onlyMatcher); ok {
		return om.OnlyMatch()
	}
	return false
}

var _ Matcher = (*FileFilterMatcher)(nil)
