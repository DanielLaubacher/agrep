package matcher

// MultilineMatcher (-U) lets a pattern match across line boundaries.
// The search side needs nothing new — every matcher already scans the
// whole buffer — the work is in extraction: each match's snippet spans
// from the start of its first line to the end of its last, so text
// output prints the whole block and JSON spans/regions cover it (a
// multiline citation re-fetches exactly like a single-line one).
//
// (?m) is enabled so ^ and $ anchor per line. Deliberately does NOT
// implement LineBounded: a chunked parallel scan could split a match.

import (
	"bytes"
	"regexp"
	"strings"
)

type MultilineMatcher struct {
	re           *regexp.Regexp
	needLineNums bool
}

// NewMultilineMatcher compiles patterns (OR'd) for cross-line matching.
func NewMultilineMatcher(patterns []string, fixed bool, ignoreCase bool, opts MatcherOpts) (*MultilineMatcher, error) {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		if fixed {
			parts[i] = regexp.QuoteMeta(p)
		} else {
			parts[i] = "(?:" + p + ")"
		}
	}
	pattern := "(?m)" + strings.Join(parts, "|")
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	return &MultilineMatcher{re: re, needLineNums: opts.NeedLineNums}, nil
}

func (m *MultilineMatcher) MatchExists(data []byte) bool {
	return m.re.Match(data)
}

// CountAll counts matches (not lines): a block match is one result.
func (m *MultilineMatcher) CountAll(data []byte) int {
	return len(m.re.FindAllIndex(data, -1))
}

func (m *MultilineMatcher) FindAll(data []byte) MatchSet {
	locs := m.re.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return MatchSet{}
	}

	ms := MatchSet{Data: data}
	lineNum := 1
	prevOff := 0
	lastBlockStart := -1

	for _, loc := range locs {
		start, end := loc[0], loc[1]

		// Block start: beginning of the line containing the match start.
		blockStart := 0
		if i := bytes.LastIndexByte(data[:start], '\n'); i >= 0 {
			blockStart = i + 1
		}
		// Block end: end of the line containing the last matched byte.
		// A match whose final byte is the newline itself belongs to the
		// line it terminates, not the next one.
		scan := end
		if end > start && data[end-1] == '\n' {
			scan = end - 1
		}
		blockEnd := len(data)
		if i := bytes.IndexByte(data[scan:], '\n'); i >= 0 {
			blockEnd = scan + i
		}
		blockLen := blockEnd - blockStart

		if m.needLineNums {
			lineNum += bytes.Count(data[prevOff:start], []byte{'\n'})
			prevOff = start
		}

		pos := [2]int{start - blockStart, min(end-blockStart, blockLen)}
		if blockStart == lastBlockStart {
			last := &ms.Matches[len(ms.Matches)-1]
			ms.Positions = append(ms.Positions, pos)
			last.PosCount++
			// Later match may extend the shared block.
			if blockLen > last.LineLen {
				last.LineLen = blockLen
			}
			continue
		}
		ms.Positions = append(ms.Positions, pos)
		ms.Matches = append(ms.Matches, Match{
			LineNum:    lineNum,
			LineStart:  blockStart,
			LineLen:    blockLen,
			ByteOffset: int64(blockStart),
			PosIdx:     len(ms.Positions) - 1,
			PosCount:   1,
		})
		lastBlockStart = blockStart
	}
	return ms
}

func (m *MultilineMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	locs := m.re.FindAllIndex(line, -1)
	if len(locs) == 0 {
		return MatchSet{}, false
	}
	ms := MatchSet{Data: line}
	ms.Positions = make([][2]int, len(locs))
	for i, loc := range locs {
		ms.Positions[i] = [2]int{loc[0], loc[1]}
	}
	ms.Matches = []Match{{
		LineNum:    lineNum,
		LineStart:  0,
		LineLen:    len(line),
		ByteOffset: byteOffset,
		PosIdx:     0,
		PosCount:   len(locs),
	}}
	return ms, true
}

var _ Matcher = (*MultilineMatcher)(nil)
