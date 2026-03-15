package regex

// These are separate functions so we can examine their assembly independently.

//go:noinline
func dfaBareCount(trans []int32, startState int32, data []byte) int {
	state := startState
	ss256 := int(state) * 256
	count := 0
	inMatch := false
	for _, b := range data {
		next := trans[ss256+int(b)]
		if next < 0 { continue }
		if next >= fwdMatchBit {
			if !inMatch { count++; inMatch = true }
			state = next & fwdStateMask
		} else {
			state = next
			inMatch = false
		}
		ss256 = int(state) * 256
	}
	return count
}

//go:noinline
func dfaWithRecording(trans []int32, startState int32, data []byte, ends []int) int {
	state := startState
	ss256 := int(state) * 256
	nEnds := 0
	inMatch := false
	for i, b := range data {
		next := trans[ss256+int(b)]
		if next < 0 { continue }
		if next >= fwdMatchBit {
			state = next & fwdStateMask
			inMatch = true
		} else {
			if inMatch {
				ends[nEnds] = i
				nEnds++
				inMatch = false
			}
			state = next
		}
		ss256 = int(state) * 256
	}
	return nEnds
}
