package matcher

import (
	"bytes"
	"regexp"

	"github.com/DanielLaubacher/agrep/internal/simd"
)

// RegexMatcher uses Go's RE2 regexp engine with optional SIMD literal prefiltering.
// When required literal substrings are extracted from the regex AST, the matcher
// first scans the buffer with SIMD for the primary literal, then verifies
// additional literals on candidate lines before running the regex engine.
type RegexMatcher struct {
	re           *regexp.Regexp
	invert       bool
	maxCols      int
	needLineNums bool
	prefilter    []byte   // primary extracted literal for SIMD prefilter (nil = no prefilter)
	prefilterCI  bool     // use case-insensitive SIMD scan for primary
	extraFilters [][]byte // additional required literals for cascaded filtering
	extraCI      []bool   // case-insensitive flags for each extra filter
}

// NewRegexMatcher creates a RegexMatcher for the given pattern.
func NewRegexMatcher(pattern string, ignoreCase bool, invert bool) (*RegexMatcher, error) {
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}

	m := &RegexMatcher{re: re, invert: invert}

	// Extract literal prefilters from the regex AST.
	// Invert mode checks every line, so prefilter doesn't help.
	if !invert {
		lits := extractLiterals(pattern, ignoreCase)
		if len(lits) > 0 {
			// Primary prefilter: first literal in source order. This enables
			// position-aware cascaded checking — each extra literal is verified
			// to appear after the previous one, which is critical for single-line
			// files (minified code) where line-level filtering is meaningless.
			m.prefilter = []byte(lits[0].literal)
			m.prefilterCI = lits[0].ignoreCase

			// Extra filters: subsequent literals in source order. Only include
			// literals >= 4 bytes to avoid overhead from low-selectivity checks.
			for _, l := range lits[1:] {
				if len(l.literal) < 4 {
					continue
				}
				m.extraFilters = append(m.extraFilters, []byte(l.literal))
				m.extraCI = append(m.extraCI, l.ignoreCase)
			}
		}
	}

	return m, nil
}

func (m *RegexMatcher) hasPrefilter() bool {
	return len(m.prefilter) > 0
}

// lineContainsAllAfter checks whether a line contains all extra filter literals
// at or after the given start position. This ordering constraint is critical for
// minified files where the entire file is one line — it ensures the extra literals
// appear in the correct order relative to the primary prefilter match, avoiding
// false positives from literals that appear earlier in the line.
func (m *RegexMatcher) lineContainsAllAfter(line []byte, after int) bool {
	pos := after
	for i, pat := range m.extraFilters {
		remaining := line[pos:]
		var idx int
		if m.extraCI[i] {
			idx = simd.IndexCaseInsensitive(remaining, pat)
		} else {
			idx = simd.Index(remaining, pat)
		}
		if idx < 0 {
			return false
		}
		// Advance past this match for ordered checking
		pos += idx + len(pat)
	}
	return true
}

func (m *RegexMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}

	if !m.hasPrefilter() {
		return m.re.Match(data)
	}

	// SIMD scan for literal candidates one at a time, verify with regex.
	off := 0
	for off < len(data) {
		var idx int
		if m.prefilterCI {
			idx = simd.IndexCaseInsensitive(data[off:], m.prefilter)
		} else {
			idx = simd.Index(data[off:], m.prefilter)
		}
		if idx < 0 {
			return false
		}

		absOff := off + idx

		// Find containing line boundaries.
		lineStart := 0
		if absOff > 0 {
			if i := bytes.LastIndexByte(data[:absOff], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[absOff:], '\n'); i >= 0 {
			lineEnd = absOff + i
		}

		line := data[lineStart:lineEnd]
		posInLine := absOff - lineStart + len(m.prefilter)

		// Check extra literals after primary match before running regex
		if m.lineContainsAllAfter(line, posInLine) && m.re.Match(line) {
			return true
		}

		// Advance past this line.
		if lineEnd >= len(data) {
			return false
		}
		off = lineEnd + 1
	}
	return false
}

func (m *RegexMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			return !m.re.Match(line)
		})
	}

	if !m.hasPrefilter() {
		return countLocsUniqueLines(data, toLocs2(m.re.FindAllIndex(data, -1)))
	}

	// SIMD prefilter: find literal candidates, deduplicate by line, regex-verify.
	var offsets []int
	if m.prefilterCI {
		offsets = simd.IndexAllCaseInsensitive(data, m.prefilter)
	} else {
		offsets = simd.IndexAll(data, m.prefilter)
	}
	if len(offsets) == 0 {
		return 0
	}

	count := 0
	lastLineEnd := -1

	for _, off := range offsets {
		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}

		if lineStart <= lastLineEnd {
			continue // same line as previous candidate
		}

		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}
		lastLineEnd = lineEnd

		line := data[lineStart:lineEnd]
		posInLine := off - lineStart + len(m.prefilter)
		if m.lineContainsAllAfter(line, posInLine) && m.re.Match(line) {
			count++
		}
	}

	return count
}

func (m *RegexMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	if !m.hasPrefilter() {
		locs := toLocs2(m.re.FindAllIndex(data, -1))
		if len(locs) == 0 {
			return MatchSet{}
		}
		return matchSetFromLocs(data, locs, m.maxCols, m.needLineNums)
	}

	return m.findAllPrefiltered(data)
}

// findAllPrefiltered scans the buffer with SIMD for literal candidates,
// verifies extra literals, then runs the regex on surviving lines.
func (m *RegexMatcher) findAllPrefiltered(data []byte) MatchSet {
	// Step 1: SIMD scan for all primary literal occurrences.
	var offsets []int
	if m.prefilterCI {
		offsets = simd.IndexAllCaseInsensitive(data, m.prefilter)
	} else {
		offsets = simd.IndexAll(data, m.prefilter)
	}
	if len(offsets) == 0 {
		return MatchSet{}
	}

	// Step 2: Resolve candidate lines, check extra literals, run regex on survivors.
	var allLocs [][2]int
	lastLineEnd := -1

	for _, off := range offsets {
		// Find line start.
		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}

		// Deduplicate: skip if same line as previous candidate.
		if lineStart <= lastLineEnd {
			continue
		}

		// Find line end.
		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}
		lastLineEnd = lineEnd

		// Check extra literals after primary match before running regex.
		line := data[lineStart:lineEnd]
		posInLine := off - lineStart + len(m.prefilter)
		if !m.lineContainsAllAfter(line, posInLine) {
			continue
		}

		// Run regex on this candidate line.
		lineLocs := m.re.FindAllIndex(line, -1)
		for _, loc := range lineLocs {
			allLocs = append(allLocs, [2]int{lineStart + loc[0], lineStart + loc[1]})
		}
	}

	if len(allLocs) == 0 {
		return MatchSet{}
	}

	return matchSetFromLocs(data, allLocs, m.maxCols, m.needLineNums)
}

func (m *RegexMatcher) findAllInvert(data []byte) MatchSet {
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

func (m *RegexMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
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
			ms.Positions[i] = [2]int{loc[0], loc[1]}
		}
	}
	ms.Matches = []Match{match}

	return ms, true
}
