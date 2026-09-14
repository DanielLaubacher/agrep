package matcher

import (
	"bytes"

	"github.com/dl/gogrep/internal/simd"
)

// BoyerMooreMatcher uses SIMD-accelerated fixed string matching for single patterns.
// Uses the SIMD-friendly Horspool algorithm for whole-buffer search.
type BoyerMooreMatcher struct {
	pattern      []byte
	patternLow   []byte // lowered pattern for case-insensitive
	ignoreCase   bool
	invert       bool
	maxCols      int
	needLineNums bool
}

// NewBoyerMooreMatcher creates a BoyerMooreMatcher for a single fixed pattern.
func NewBoyerMooreMatcher(pattern string, ignoreCase bool, invert bool) *BoyerMooreMatcher {
	p := []byte(pattern)
	var pLow []byte
	if ignoreCase {
		pLow = bytes.ToLower(p)
	} else {
		pLow = p
	}

	return &BoyerMooreMatcher{
		pattern:    p,
		patternLow: pLow,
		ignoreCase: ignoreCase,
		invert:     invert,
	}
}

// LineBounded reports that no match spans a newline (the fixed pattern
// contains none), enabling parallel line-aligned chunked search.
func (m *BoyerMooreMatcher) LineBounded() bool {
	return !bytes.ContainsRune(m.pattern, '\n')
}

func (m *BoyerMooreMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}
	if m.ignoreCase {
		return simd.IndexCaseInsensitive(data, m.patternLow) >= 0
	}
	return simd.Index(data, m.patternLow) >= 0
}

func (m *BoyerMooreMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			if m.ignoreCase {
				return simd.IndexCaseInsensitive(line, m.patternLow) < 0
			}
			return simd.Index(line, m.patternLow) < 0
		})
	}

	if m.ignoreCase {
		return countUniqueLines(data, simd.IndexAllCaseInsensitive(data, m.patternLow))
	}
	return countUniqueLines(data, simd.IndexAll(data, m.patternLow))
}

func (m *BoyerMooreMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	var offsets []int
	if m.ignoreCase {
		offsets = simd.IndexAllCaseInsensitive(data, m.patternLow)
	} else {
		offsets = simd.IndexAll(data, m.patternLow)
	}
	return matchSetFromOffsets(data, offsets, len(m.patternLow), m.maxCols, m.needLineNums)
}

// findAllInvert returns lines that do NOT contain the pattern.
func (m *BoyerMooreMatcher) findAllInvert(data []byte) MatchSet {
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

		var found int
		if m.ignoreCase {
			found = simd.IndexCaseInsensitive(line, m.patternLow)
		} else {
			found = simd.Index(line, m.patternLow)
		}
		if found < 0 {
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

func (m *BoyerMooreMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	var offsets []int
	if m.ignoreCase {
		offsets = simd.IndexAllCaseInsensitive(line, m.patternLow)
	} else {
		offsets = simd.IndexAll(line, m.patternLow)
	}
	hasMatch := len(offsets) > 0

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
		pLen := len(m.patternLow)
		match.PosIdx = 0
		match.PosCount = len(offsets)
		ms.Positions = make([][2]int, len(offsets))
		for i, off := range offsets {
			ms.Positions[i] = [2]int{off, off + pLen}
		}
	}
	ms.Matches = []Match{match}

	return ms, true
}

// toLower converts an ASCII byte to lowercase.
func toLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}
