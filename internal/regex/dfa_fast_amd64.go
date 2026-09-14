package regex

// precomputeAll performs BFS over all reachable DFA states, computing
// every transition eagerly. After this, no dfaUncomputed entries remain
// for any reachable state. This allows the inner loop to use `next <= 0`
// as a combined dead+uncomputed check (since uncomputed never occurs).
func (dfa *searchDFA) precomputeAll() bool {
	classOf, reps := computeByteClasses(dfa.nfa)
	visited := make([]bool, dfa.cacheLimit)
	queue := make([]int32, 1, 64)
	queue[0] = dfa.startState
	visited[dfa.startState] = true

	for qi := 0; qi < len(queue); qi++ {
		state := queue[qi]
		base := int(state) * 256

		// Compute one transition per byte class, then fan the value out to
		// every byte in the class (bytes in a class are indistinguishable
		// to all NFA states, so their transitions are identical).
		for _, rep := range reps {
			if dfa.trans[base+int(rep)] == dfaUncomputed {
				if dfa.computeTransition(state, rep) < 0 {
					return false // cache overflow
				}
			}
		}
		for b := 0; b < 256; b++ {
			val := dfa.trans[base+int(reps[classOf[b]])]
			dfa.trans[base+b] = val
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
	if len(data) < 4096 {
		return dfa.findAllIndexSafe(data, n)
	}

	estMatches := len(data)/300 + 64
	if n >= 0 && n < estMatches {
		estMatches = n
	}
	results := make([][2]int, 0, estMatches)
	dfa.findAllIndexFastFunc(data, func(s, e int) bool {
		results = append(results, [2]int{s, e})
		return n < 0 || len(results) < n
	})
	return results
}

// findAllIndexFunc streams matches to yield in order (yield false = stop).
func (dfa *searchDFA) findAllIndexFunc(data []byte, yield func(s, e int) bool) {
	if dfa.nfa.Flags&FlagAnchored != 0 || dfa.startIsMatch || len(data) < 4096 {
		// Rare/small paths: collect then replay.
		var locs [][2]int
		if dfa.nfa.Flags&FlagAnchored != 0 || dfa.startIsMatch {
			locs = dfa.findAllIndexSlow(data, -1)
		} else {
			locs = dfa.findAllIndexSafe(data, -1)
		}
		for _, loc := range locs {
			if !yield(loc[0], loc[1]) {
				return
			}
		}
		return
	}
	dfa.findAllIndexFastFunc(data, yield)
}

// findAllIndexFastFunc is the batch-SIMD + precomputed-DFA streaming core.
// Requires: not anchored, start not matching, len(data) >= 4096.
func (dfa *searchDFA) findAllIndexFastFunc(data []byte, yield func(s, e int) bool) {
	dfa.warmStart()
	if !dfa.precomputed {
		if !dfa.precomputeAll() {
			for _, loc := range dfa.findAllIndexSafe(data, -1) {
				if !yield(loc[0], loc[1]) {
					return
				}
			}
			return
		}
		dfa.precomputed = true
	}

	ss := dfa.startState
	trans := dfa.trans
	dataLen := len(data)

	const batchSize = 4096
	var candBuf [batchSize]int
	ssBase := int(ss) * 256

	pos := 0
	for pos < dataLen {
		// Batch SIMD: collect up to 4096 candidate positions in one scan
		nCand := dfa.batchNextCandidates(data, pos, dataLen, candBuf[:])
		if nCand == 0 {
			break
		}

		for ci := 0; ci < nCand; {
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
				if !yield(startPos, lastMatch) {
					return
				}
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
