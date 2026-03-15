package matcher

import "bytes"

// snippetFromOffset extracts a line snippet around a match at off in data.
// Instead of resolving full line boundaries (which may be thousands of bytes
// away), it looks at most maxCols bytes in each direction and clamps at '\n'.
// Returns the snippet start offset and length within data.
//
// When maxCols <= 0, full line boundaries are resolved (no truncation).
func snippetFromOffset(data []byte, off int, maxCols int) (snippetStart int, snippetLen int, posInSnippet int) {
	n := len(data)

	// Determine search bounds
	var lo, hi int
	if maxCols > 0 {
		lo = max(off-maxCols, 0)
		hi = min(off+maxCols, n)
	} else {
		lo = 0
		hi = n
	}

	// Find line start: last '\n' before off within [lo, off)
	lineStart := lo
	if i := bytes.LastIndexByte(data[lo:off], '\n'); i >= 0 {
		lineStart = lo + i + 1
	}

	// Find line end: first '\n' at or after off within [off, hi)
	lineEnd := hi
	if i := bytes.IndexByte(data[off:hi], '\n'); i >= 0 {
		lineEnd = off + i
	}

	return lineStart, lineEnd - lineStart, off - lineStart
}

// matchSetFromOffsets converts fixed-length match offsets to a MatchSet.
// Uses window-based snippet extraction (bounded by maxCols) and incremental
// bytes.Count for line numbers. O(1) pointer overhead, O(n) total time.
func matchSetFromOffsets(data []byte, offsets []int, patternLen int, maxCols int, needLineNums bool) MatchSet {
	if len(offsets) == 0 {
		return MatchSet{}
	}

	matches := make([]Match, 0, len(offsets))
	positions := make([][2]int, 0, len(offsets))
	lastSnippetStart := -1
	lineNum := 1
	prevOff := 0

	for _, off := range offsets {
		snippetStart, snippetLen, posInSnippet := snippetFromOffset(data, off, maxCols)

		if needLineNums {
			lineNum += bytes.Count(data[prevOff:off], []byte{'\n'})
			prevOff = off
		}

		posIdx := len(positions)
		positions = append(positions, [2]int{posInSnippet, posInSnippet + patternLen})

		if snippetStart == lastSnippetStart {
			// Same line as previous match — extend its position range
			last := &matches[len(matches)-1]
			last.PosCount = posIdx - last.PosIdx + 1
		} else {
			matches = append(matches, Match{
				LineNum:    lineNum,
				LineStart:  snippetStart,
				LineLen:    snippetLen,
				ByteOffset: int64(snippetStart),
				PosIdx:     posIdx,
				PosCount:   1,
			})
			lastSnippetStart = snippetStart
		}
	}

	return MatchSet{Data: data, Matches: matches, Positions: positions}
}

// matchSetFromLocs converts match locations (as [2]int{start, end}) to a MatchSet.
// It reuses the locs slice in-place for positions (converting buffer-absolute offsets
// to snippet-relative offsets), eliminating one allocation.
func matchSetFromLocs(data []byte, locs [][2]int, maxCols int, needLineNums bool) MatchSet {
	if len(locs) == 0 {
		return MatchSet{}
	}

	matches := make([]Match, 0, len(locs))
	lastSnippetStart := -1
	lineNum := 1
	prevOff := 0

	for i, loc := range locs {
		matchStart, matchEnd := loc[0], loc[1]

		snippetStart, snippetLen, posInSnippet := snippetFromOffset(data, matchStart, maxCols)

		if needLineNums {
			lineNum += bytes.Count(data[prevOff:matchStart], []byte{'\n'})
			prevOff = matchStart
		}

		posEnd := min(posInSnippet+(matchEnd-matchStart), snippetLen)

		// Overwrite locs[i] in-place with snippet-relative position.
		// Safe because we already read loc above and iteration is forward-only.
		locs[i] = [2]int{posInSnippet, posEnd}

		if snippetStart == lastSnippetStart {
			last := &matches[len(matches)-1]
			last.PosCount = i - last.PosIdx + 1
		} else {
			matches = append(matches, Match{
				LineNum:    lineNum,
				LineStart:  snippetStart,
				LineLen:    snippetLen,
				ByteOffset: int64(snippetStart),
				PosIdx:     i,
				PosCount:   1,
			})
			lastSnippetStart = snippetStart
		}
	}

	return MatchSet{Data: data, Matches: matches, Positions: locs}
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
