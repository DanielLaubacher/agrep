//go:build pcre

package matcher

import (
	"bytes"

	"go.elara.ws/pcre"
)

// PCREAvailable reports whether this build includes the PCRE engine.
const PCREAvailable = true

// PCREMatcher matches using PCRE2-compatible regexes via the pure Go pcre package.
// Supports lookahead, lookbehind, backreferences, atomic groups, and all PCRE2 features.
type PCREMatcher struct {
	re           *pcre.Regexp
	ignoreCase   bool
	invert       bool
	needLineNums bool
}

// NewPCREMatcher creates a PCREMatcher from a PCRE2 pattern string.
func NewPCREMatcher(pattern string, ignoreCase bool, invert bool) (*PCREMatcher, error) {
	var opts pcre.CompileOption
	if ignoreCase {
		opts |= pcre.Caseless
	}

	re, err := pcre.CompileOpts(pattern, opts)
	if err != nil {
		return nil, err
	}

	return &PCREMatcher{
		re:         re,
		ignoreCase: ignoreCase,
		invert:     invert,
	}, nil
}

func (m *PCREMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}
	return m.re.Match(data)
}

func (m *PCREMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			return len(m.re.FindAllIndex(line, -1)) == 0
		})
	}

	locs := toLocs2(m.re.FindAllIndex(data, -1))
	return countLocsUniqueLines(data, locs)
}

func (m *PCREMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	locs := toLocs2(m.re.FindAllIndex(data, -1))
	if len(locs) == 0 {
		return MatchSet{}
	}

	return matchSetFromLocs(data, locs, m.needLineNums)
}

func (m *PCREMatcher) findAllInvert(data []byte) MatchSet {
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

		locs := m.re.FindAllIndex(line, -1)
		if len(locs) == 0 {
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

func (m *PCREMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
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
	if !m.invert && len(locs) > 0 {
		match.PosIdx = 0
		match.PosCount = len(locs)
		ms.Positions = make([][2]int, len(locs))
		for i, loc := range locs {
			ms.Positions[i] = [2]int{loc[0], loc[1]}
		}
	}
	ms.Matches = []Match{match}

	return ms, true
}

// Close releases the compiled PCRE regex resources.
func (m *PCREMatcher) Close() {
	if m.re != nil {
		m.re.Close()
	}
}
