package matcher

import (
	"bytes"

	"github.com/dl/gogrep/internal/simd"
)

// TeddyMatcher matches 2..8 fixed patterns using the rare-pair Teddy SIMD
// prefilter (internal/simd/teddy.go) — same observable semantics as
// AhoCorasickMatcher (all matches reported, including overlapping ones),
// but candidates are found 32 positions per SIMD iteration probing the two
// rarest byte positions of the set instead of walking a trie byte-by-byte.
type TeddyMatcher struct {
	teddy        *simd.Teddy
	patterns     [][]byte // lowered when ignoreCase
	ignoreCase   bool
	invert       bool
	maxCols      int
	needLineNums bool
}

// NewTeddyMatcher returns a TeddyMatcher, or nil if the pattern set is
// unsuitable (wrong count, a pattern shorter than 2 bytes, or non-ASCII
// bytes under ignoreCase where ASCII case folding is insufficient).
func NewTeddyMatcher(patterns []string, ignoreCase bool, invert bool) *TeddyMatcher {
	pats := make([][]byte, 0, len(patterns))
	for _, p := range patterns {
		b := []byte(p)
		if ignoreCase {
			for _, c := range b {
				if c >= 0x80 {
					return nil // ASCII-only case folding
				}
			}
			b = bytes.ToLower(b)
		}
		pats = append(pats, b)
	}
	t := simd.NewTeddy(pats, ignoreCase)
	if t == nil {
		return nil
	}
	return &TeddyMatcher{
		teddy:      t,
		patterns:   pats,
		ignoreCase: ignoreCase,
		invert:     invert,
	}
}

// LineBounded reports that no match spans a newline, enabling parallel
// line-aligned chunked search.
func (m *TeddyMatcher) LineBounded() bool {
	for _, p := range m.patterns {
		if bytes.ContainsRune(p, '\n') {
			return false
		}
	}
	return true
}

func (m *TeddyMatcher) matchExists(data []byte) bool {
	found := false
	m.teddy.Scan(data, func(_, _ int) bool {
		found = true
		return false
	})
	return found
}

func (m *TeddyMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}
	return m.matchExists(data)
}

func (m *TeddyMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			return !m.matchExists(line)
		})
	}

	count := 0
	lineEnd := -1
	m.teddy.Scan(data, func(pos, _ int) bool {
		if pos > lineEnd {
			count++
			if j := bytes.IndexByte(data[pos:], '\n'); j >= 0 {
				lineEnd = pos + j
			} else {
				lineEnd = len(data)
			}
		}
		return true
	})
	return count
}

func (m *TeddyMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	// Stream matches straight into the MatchSet builder (line extraction
	// and newline counting happen while the region is cache-hot).
	b := newMatchSetBuilder(data, m.maxCols, m.needLineNums)
	m.teddy.Scan(data, func(pos, pat int) bool {
		return b.add(pos, pos+len(m.patterns[pat]))
	})
	return b.finish()
}

func (m *TeddyMatcher) findAllInvert(data []byte) MatchSet {
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

		if !m.matchExists(line) {
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

func (m *TeddyMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	var locs [][2]int
	m.teddy.Scan(line, func(pos, pat int) bool {
		locs = append(locs, [2]int{pos, pos + len(m.patterns[pat])})
		return true
	})
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
		ms.Positions = locs
	}
	ms.Matches = []Match{match}
	return ms, true
}
