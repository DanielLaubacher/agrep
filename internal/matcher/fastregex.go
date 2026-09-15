package matcher

import (
	"bytes"

	"github.com/DanielLaubacher/agrep/internal/regex"
)

// FastRegexMatcher uses the internal/regex lazy DFA engine.
// All SIMD prefiltering, rare-byte extraction, and DFA optimization
// is encapsulated inside the regex package. This is a thin adapter
// that converts regex.Regexp results to the Matcher interface.
type FastRegexMatcher struct {
	re           *regex.Regexp
	invert       bool
	needLineNums bool
}

// NewFastRegexMatcher creates a FastRegexMatcher using the internal/regex engine.
func NewFastRegexMatcher(pattern string, ignoreCase bool, invert bool) (*FastRegexMatcher, error) {
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	// Line mode: ^ and $ hold at every line, and literal prefilters verify
	// candidates within their line.
	re, err := regex.CompileMode(pattern, regex.ModeLine)
	if err != nil {
		return nil, err
	}
	return &FastRegexMatcher{re: re, invert: invert}, nil
}

// LineBounded reports that no match spans a newline, so line-aligned chunks
// of a buffer can be searched independently (and concurrently — the
// underlying Regexp is immutable after compile).
func (m *FastRegexMatcher) LineBounded() bool {
	return !m.re.CanMatchNewline()
}

func (m *FastRegexMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}
	return m.re.Match(data)
}

func (m *FastRegexMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			return !m.re.Match(line)
		})
	}

	// Stream: count distinct matched lines without materializing locations,
	// resolving each line end while the region is cache-hot.
	count := 0
	lineEnd := -1
	m.re.FindAllIndexFunc(data, func(start, _ int) bool {
		if start > lineEnd {
			count++
			if i := bytes.IndexByte(data[start:], '\n'); i >= 0 {
				lineEnd = start + i
			} else {
				lineEnd = len(data)
			}
		}
		return true
	})
	return count
}

func (m *FastRegexMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	b := newMatchSetBuilder(data, m.needLineNums)
	m.re.FindAllIndexFunc(data, b.add)
	return b.finish()
}

func (m *FastRegexMatcher) findAllInvert(data []byte) MatchSet {
	ms := MatchSet{Data: data}
	var offset int64
	lineNum := 1
	remaining := data

	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		var lineLen int
		if idx >= 0 {
			lineLen = idx
		} else {
			lineLen = len(remaining)
		}
		lineStart := int(offset)
		line := remaining[:lineLen]

		if !m.re.Match(line) {
			ms.Matches = append(ms.Matches, Match{
				LineNum:    lineNum,
				LineStart:  lineStart,
				LineLen:    lineLen,
				ByteOffset: offset,
			})
		}

		if idx >= 0 {
			remaining = remaining[idx+1:]
		} else {
			remaining = nil
		}
		offset += int64(lineLen) + 1
		lineNum++
	}

	return ms
}

func (m *FastRegexMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	locs := m.re.FindAllIndex(line, -1)
	hasMatch := len(locs) > 0

	if m.invert {
		hasMatch = !hasMatch
	}

	if !hasMatch {
		return MatchSet{}, false
	}

	ms := MatchSet{Data: line}
	match := Match{
		LineNum:    lineNum,
		LineStart:  0,
		LineLen:    len(line),
		ByteOffset: byteOffset,
	}

	if !m.invert {
		match.PosIdx = 0
		match.PosCount = len(locs)
		ms.Positions = make([][2]int, len(locs))
		for i, loc := range locs {
			ms.Positions[i] = loc
		}
	}
	ms.Matches = []Match{match}

	return ms, true
}

// countLocsUniqueLines2 counts distinct lines containing at least one loc.
func countLocsUniqueLines2(data []byte, locs [][2]int) int {
	if len(locs) == 0 {
		return 0
	}
	count := 0
	lineEnd := -1
	for _, loc := range locs {
		off := loc[0]
		if off > lineEnd {
			count++
			i := bytes.IndexByte(data[off:], '\n')
			if i >= 0 {
				lineEnd = off + i
			} else {
				lineEnd = len(data)
			}
		}
	}
	return count
}
