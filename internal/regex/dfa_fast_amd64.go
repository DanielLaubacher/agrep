package regex

// precomputeAll performs BFS over all reachable DFA states, computing
// every transition eagerly. After this, no dfaUncomputed entries remain
// for any reachable state. This allows the inner loop to use `next <= 0`
// as a combined dead+uncomputed check (since uncomputed never occurs).
func (dfa *searchDFA) precomputeAll() bool {
	visited := make([]bool, dfa.cacheLimit)
	queue := make([]int32, 1, 64)
	queue[0] = dfa.startState
	visited[dfa.startState] = true

	for qi := 0; qi < len(queue); qi++ {
		state := queue[qi]

		for b := 0; b < 256; b++ {
			off := int(state)*256 + b
			if dfa.trans[off] == dfaUncomputed {
				next := dfa.computeTransition(state, byte(b))
				if next < 0 {
					return false // cache overflow
				}
			}
			val := dfa.trans[int(state)*256+b]
			destState := val & sStateMask
			if destState != dfaDeadState && int(destState) < len(visited) && !visited[destState] {
				visited[destState] = true
				queue = append(queue, destState)
			}
		}
	}
	return true
}

// findAllIndexFast uses batch SIMD scanning + precomputed DFA transitions
// + unconditional state masking to minimize per-transition overhead.
//
// Three optimizations over the baseline:
//   1. Batch SIMD: one scan per 4096 candidates instead of per-candidate calls
//   2. Precomputed transitions: no dfaUncomputed check in the inner loop
//   3. Unconditional masking: ss256 update has no branch, enabling CMOV
func (dfa *searchDFA) findAllIndexFast(data []byte, n int) [][2]int {
	if dfa.nfa.Flags&FlagAnchored != 0 || dfa.startIsMatch {
		return dfa.findAllIndexSlow(data, n)
	}

	// For small data (per-line calls from prefilter path), skip batch overhead.
	if len(data) < 4096 {
		return dfa.findAllIndexSafe(data, n)
	}

	dfa.warmStart()
	if !dfa.precomputed {
		if !dfa.precomputeAll() {
			return dfa.findAllIndexSafe(data, n)
		}
		dfa.precomputed = true
	}

	ss := dfa.startState
	trans := dfa.trans
	dataLen := len(data)

	estMatches := dataLen/300 + 64
	if n >= 0 && n < estMatches {
		estMatches = n
	}
	results := make([][2]int, 0, estMatches)

	const batchSize = 4096
	var candBuf [batchSize]int
	ssBase := int(ss) * 256

	pos := 0
	for (n < 0 || len(results) < n) && pos < dataLen {
		// Batch SIMD: collect up to 4096 candidate positions in one scan
		nCand := dfa.batchNextCandidates(data, pos, dataLen, candBuf[:])
		if nCand == 0 {
			break
		}

		for ci := 0; ci < nCand && (n < 0 || len(results) < n); {
			startPos := candBuf[ci]
			if startPos < pos {
				ci++
				continue
			}

			// DFA inner loop: precomputed transitions + unconditional masking
			ss256 := ssBase
			lastMatch := -1
			for i := startPos; i < dataLen; i++ {
				next := trans[ss256+int(data[i])]
				if next <= 0 {
					break // dead (precomputed: no uncomputed possible)
				}
				// Unconditional mask — no branch for state update.
				// For non-match: sStateMask is a no-op (bit 30 already clear).
				// For match: clears bit 30. Either way, correct state index.
				ss256 = int(next&sStateMask) * 256
				if next >= sMatchBit {
					lastMatch = i + 1
				}
			}

			if lastMatch >= 0 {
				results = append(results, [2]int{startPos, lastMatch})
				pos = lastMatch
				ci++
				// Skip candidates that fall within the match we just found
				for ci < nCand && candBuf[ci] < pos {
					ci++
				}
			} else {
				pos = startPos + 1
				ci++
			}
		}

		// Advance past the last candidate in this batch
		if nCand > 0 {
			lastCand := candBuf[nCand-1]
			if pos <= lastCand {
				pos = lastCand + 1
			}
		}
	}

	return results
}

// findAllIndexSafe is the non-optimized fallback (small data, cache overflow).
func (dfa *searchDFA) findAllIndexSafe(data []byte, n int) [][2]int {
	dfa.warmStart()
	ss := dfa.startState
	trans := dfa.trans
	var results [][2]int
	pos := 0

	for (n < 0 || len(results) < n) && pos < len(data) {
		startPos := dfa.nextCandidateFrom(data, pos)
		if startPos < 0 {
			break
		}

		state := ss
		ss256 := int(state) * 256
		lastMatch := -1
		for i := startPos; i < len(data); i++ {
			next := trans[ss256+int(data[i])]
			if next == dfaUncomputed {
				next = dfa.computeTransition(state, data[i])
				if next < 0 {
					return (&pikeVM{nfa: dfa.nfa}).findAllIndex(data, n)
				}
				trans = dfa.trans
			}
			if next == dfaDeadState {
				break
			}
			state = next & sStateMask
			ss256 = int(state) * 256
			if next >= sMatchBit {
				lastMatch = i + 1
			}
		}

		if lastMatch >= 0 {
			results = append(results, [2]int{startPos, lastMatch})
			pos = lastMatch
		} else {
			pos = startPos + 1
		}
	}

	return results
}

// batchNextCandidates collects candidate positions using the pre-computed
// SIMD scanner. Returns count of positions written to buf.
func (dfa *searchDFA) batchNextCandidates(data []byte, from, end int, buf []int) int {
	if len(dfa.startRanges) == 0 {
		return 0
	}
	if len(dfa.startRanges) == 1 {
		return dfa.scanner.BatchNext(data, from, end, buf)
	}
	return dfa.multiScanner.BatchNext(data, from, end, buf)
}
