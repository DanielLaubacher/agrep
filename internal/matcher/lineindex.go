package matcher

import "bytes"

// lineBoundsFromOffset resolves the full line containing off: the byte
// after the previous '\n' (or 0) through the next '\n' (or EOF), exclusive.
// Line-accurate bounds are a correctness contract — JSON span/region ids
// and line totals derive from them. Display truncation (-M) happens in the
// output layer only. Callers cache the result per line so a line with many
// matches is resolved once, keeping total cost O(len(data)).
func lineBoundsFromOffset(data []byte, off int) (lineStart, lineEnd int) {
	lineStart = 0
	if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
		lineStart = i + 1
	}
	lineEnd = len(data)
	if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
		lineEnd = off + i
	}
	return lineStart, lineEnd
}

// matchSetFromOffsets converts fixed-length match offsets to a MatchSet.
// One Match per line (full line bounds), incremental bytes.Count for line
// numbers. O(1) pointer overhead, O(n) total time.
func matchSetFromOffsets(data []byte, offsets []int, patternLen int, needLineNums bool) MatchSet {
	if len(offsets) == 0 {
		return MatchSet{}
	}

	matches := make([]Match, 0, len(offsets))
	positions := make([][2]int, 0, len(offsets))
	curLineStart, curLineEnd := 0, -1
	lineNum := 1
	prevOff := 0

	for _, off := range offsets {
		if off > curLineEnd {
			curLineStart, curLineEnd = lineBoundsFromOffset(data, off)
			if needLineNums {
				lineNum += bytes.Count(data[prevOff:off], []byte{'\n'})
				prevOff = off
			}
			matches = append(matches, Match{
				LineNum:    lineNum,
				LineStart:  curLineStart,
				LineLen:    curLineEnd - curLineStart,
				ByteOffset: int64(curLineStart),
				PosIdx:     len(positions),
				PosCount:   1,
			})
		} else {
			matches[len(matches)-1].PosCount++
		}
		pos := off - curLineStart
		positions = append(positions, [2]int{pos, min(pos+patternLen, curLineEnd-curLineStart)})
	}

	return MatchSet{Data: data, Matches: matches, Positions: positions}
}

// matchSetFromLocs converts match locations (as [2]int{start, end}) to a MatchSet.
// It reuses the locs slice in-place for positions (converting buffer-absolute offsets
// to snippet-relative offsets), eliminating one allocation.
func matchSetFromLocs(data []byte, locs [][2]int, needLineNums bool) MatchSet {
	if len(locs) == 0 {
		return MatchSet{}
	}

	matches := make([]Match, 0, len(locs))
	curLineStart, curLineEnd := 0, -1
	lineNum := 1
	prevOff := 0

	for i, loc := range locs {
		matchStart, matchEnd := loc[0], loc[1]

		if matchStart > curLineEnd {
			curLineStart, curLineEnd = lineBoundsFromOffset(data, matchStart)
			if needLineNums {
				lineNum += bytes.Count(data[prevOff:matchStart], []byte{'\n'})
				prevOff = matchStart
			}
			matches = append(matches, Match{
				LineNum:    lineNum,
				LineStart:  curLineStart,
				LineLen:    curLineEnd - curLineStart,
				ByteOffset: int64(curLineStart),
				PosIdx:     i,
				PosCount:   1,
			})
		} else {
			matches[len(matches)-1].PosCount++
		}

		pos := matchStart - curLineStart
		posEnd := min(pos+(matchEnd-matchStart), curLineEnd-curLineStart)

		// Overwrite locs[i] in-place with line-relative position.
		// Safe because we already read loc above and iteration is forward-only.
		locs[i] = [2]int{pos, posEnd}
	}

	return MatchSet{Data: data, Matches: matches, Positions: locs}
}

// matchSetBuilder constructs a MatchSet incrementally as match locations
// stream in from the search. Doing snippet extraction and newline counting
// per match — while the scan front's data is still cache-hot — avoids a
// second cold pass over buffers larger than L3 (the streaming counterpart
// of matchSetFromLocs).
type matchSetBuilder struct {
	data         []byte
	needLineNums bool
	matches      []Match
	positions    [][2]int
	curLineStart int
	curLineEnd   int
	lineNum      int
	prevOff      int
}

func newMatchSetBuilder(data []byte, needLineNums bool) matchSetBuilder {
	return matchSetBuilder{
		data:         data,
		needLineNums: needLineNums,
		curLineEnd:   -1,
		lineNum:      1,
	}
}

// add records one match location; the signature matches regex.FindAllIndexFunc.
func (b *matchSetBuilder) add(matchStart, matchEnd int) bool {
	if matchStart > b.curLineEnd {
		b.curLineStart, b.curLineEnd = lineBoundsFromOffset(b.data, matchStart)
		if b.needLineNums {
			b.lineNum += bytes.Count(b.data[b.prevOff:matchStart], []byte{'\n'})
			b.prevOff = matchStart
		}
		b.matches = append(b.matches, Match{
			LineNum:    b.lineNum,
			LineStart:  b.curLineStart,
			LineLen:    b.curLineEnd - b.curLineStart,
			ByteOffset: int64(b.curLineStart),
			PosIdx:     len(b.positions),
			PosCount:   1,
		})
	} else {
		b.matches[len(b.matches)-1].PosCount++
	}

	pos := matchStart - b.curLineStart
	posEnd := min(pos+(matchEnd-matchStart), b.curLineEnd-b.curLineStart)
	b.positions = append(b.positions, [2]int{pos, posEnd})
	return true
}

func (b *matchSetBuilder) finish() MatchSet {
	if len(b.matches) == 0 {
		return MatchSet{}
	}
	return MatchSet{Data: b.data, Matches: b.matches, Positions: b.positions}
}

// countUniqueLines counts how many distinct lines contain at least one offset.
// Offsets must be sorted ascending.
func countUniqueLines(data []byte, offsets []int) int {
	if len(offsets) == 0 {
		return 0
	}

	count := 0
	lineEnd := -1

	for _, off := range offsets {
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

// countInvert counts lines where matchFunc returns true.
func countInvert(data []byte, matchFunc func(line []byte) bool) int {
	count := 0
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		var line []byte
		if idx >= 0 {
			line = data[:idx]
			data = data[idx+1:]
		} else {
			line = data
			data = nil
		}
		if matchFunc(line) {
			count++
		}
	}
	return count
}

// toLocs2 converts [][]int (as returned by regexp.FindAllIndex / pcre.FindAllIndex)
// to [][2]int value type, eliminating per-element heap allocations.
func toLocs2(locs [][]int) [][2]int {
	if len(locs) == 0 {
		return nil
	}
	result := make([][2]int, len(locs))
	for i, loc := range locs {
		result[i] = [2]int{loc[0], loc[1]}
	}
	return result
}

// countLocsUniqueLines counts how many distinct lines contain at least one loc.
func countLocsUniqueLines(data []byte, locs [][2]int) int {
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
