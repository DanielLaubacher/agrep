package regex

// Lazy DFA with two operating modes:
//
// 1. Forward DFA (match-only): single-pass O(n). Every DFA state includes
//    the NFA start states. Used by match().
//
// 2. Search DFA (findIndex): SIMD-accelerated first-byte scan skips to
//    candidate positions (32 bytes/iteration via AVX2 range check), then
//    runs the anchored DFA from each candidate. The SIMD scan automatically
//    handles UTF-8 rune boundaries since continuation bytes (0x80-0xBF)
//    never appear in the start state's transition table.

import (
	"slices"

	"github.com/DanielLaubacher/agrep/internal/simd"
)

const (
	dfaDeadState    int32 = 0
	dfaDefaultCache       = 4096
	dfaUncomputed   int32 = -1
)

type stateKey string

// ---------------- search DFA (for findIndex) ----------------

// searchDFA uses flat transition array with match-bit encoding.
// trans[state*256+byte] holds destination state in bits 0-29, match flag in bit 30.
type searchDFA struct {
	nfa          *NFA
	trans        []int32 // flat: trans[state*256 + byte]
	isMatch      []bool
	stateMap     map[stateKey]int32
	nfaSets      [][]int32
	numStates    int
	cacheLimit   int
	startState   int32
	canStart     [256]bool
	startRanges  [][2]byte
	scanner      simd.ByteRangeScanner      // pre-computed SIMD vectors (single range)
	multiScanner simd.MultiByteRangeScanner // pre-computed SIMD vectors (multi range)
	startIsMatch bool
	warmed       bool
	precomputed  bool
}

const sMatchBit int32 = 1 << 30
const sStateMask int32 = sMatchBit - 1

func newSearchDFA(nfa *NFA) *searchDFA {
	dfa := &searchDFA{
		nfa:        nfa,
		trans:      make([]int32, 256), // state 0 (dead)
		isMatch:    []bool{false},
		stateMap:   make(map[stateKey]int32, 64),
		nfaSets:    [][]int32{nil},
		numStates:  1,
		cacheLimit: dfaDefaultCache,
	}

	startSet := epsilonClosure(nfa, []int32{nfa.Start})
	dfa.startState = dfa.getOrCreateState(startSet)

	return dfa
}

// warmStart pre-computes all 256 transitions from the start state,
// builds the canStart bitmap, and extracts contiguous byte ranges
// for SIMD-accelerated first-byte scanning.
func (dfa *searchDFA) warmStart() {
	if dfa.warmed {
		return
	}
	dfa.warmed = true
	ss := dfa.startState
	ss256 := int(ss) * 256
	for b := 0; b < 256; b++ {
		if dfa.trans[ss256+b] == dfaUncomputed {
			dfa.computeTransition(ss, byte(b))
		}
		v := dfa.trans[ss256+b]
		dfa.canStart[b] = v != dfaDeadState && (v&sStateMask) != dfaDeadState
	}

	dfa.startIsMatch = dfa.isMatch[ss]

	// Extract contiguous ranges from canStart for SIMD scanning
	dfa.startRanges = dfa.startRanges[:0]
	inRange := false
	var lo byte
	for b := 0; b < 256; b++ {
		if dfa.canStart[b] {
			if !inRange {
				lo = byte(b)
				inRange = true
			}
		} else if inRange {
			dfa.startRanges = append(dfa.startRanges, [2]byte{lo, byte(b - 1)})
			inRange = false
		}
	}
	if inRange {
		dfa.startRanges = append(dfa.startRanges, [2]byte{lo, 255})
	}

	// Pre-compute SIMD broadcast vectors once (eliminates per-call overhead)
	if len(dfa.startRanges) == 1 {
		dfa.scanner = simd.NewByteRangeScanner(dfa.startRanges[0][0], dfa.startRanges[0][1])
	} else if len(dfa.startRanges) > 1 {
		dfa.multiScanner = simd.NewMultiByteRangeScanner(dfa.startRanges)
	}
}

func (dfa *searchDFA) findIndex(data []byte) [2]int {
	if dfa.nfa.Flags&FlagAnchored != 0 {
		return dfa.findIndexAnchored(data)
	}
	dfa.warmStart()
	return dfa.findIndexUnanchored(data)
}

func (dfa *searchDFA) findIndexAnchored(data []byte) [2]int {
	state := dfa.startState
	if state == dfaDeadState {
		return [2]int{-1, -1}
	}
	trans := dfa.trans
	ss256 := int(state) * 256
	lastMatch := -1
	if dfa.isMatch[state] {
		lastMatch = 0
	}
	for i := 0; i < len(data); i++ {
		next := trans[ss256+int(data[i])]
		if next == dfaUncomputed {
			next = dfa.computeTransition(state, data[i])
			if next < 0 {
				return (&pikeVM{nfa: dfa.nfa}).findIndex(data)
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
		return [2]int{0, lastMatch}
	}
	return [2]int{-1, -1}
}

func (dfa *searchDFA) findIndexUnanchored(data []byte) [2]int {
	ss := dfa.startState
	if ss == dfaDeadState {
		return [2]int{-1, -1}
	}

	if dfa.startIsMatch {
		return dfa.findIndexSlow(data, 0)
	}

	// SIMD-accelerated: scan for candidate bytes, try DFA at each
	startPos := 0
	for startPos < len(data) {
		startPos = dfa.nextCandidateFrom(data, startPos)
		if startPos < 0 {
			break
		}

		if match := dfa.tryMatchAt(data, startPos); match[0] >= 0 {
			return match
		}
		startPos++
	}
	return [2]int{-1, -1}
}

// nextCandidateFrom returns the absolute position of the next byte in data
// starting at `from` that could start a match. Uses pre-computed SIMD
// vectors — no per-call broadcast overhead.
func (dfa *searchDFA) nextCandidateFrom(data []byte, from int) int {
	if len(dfa.startRanges) == 0 {
		return -1
	}
	if len(dfa.startRanges) == 1 {
		return dfa.scanner.Next(data, from)
	}
	return dfa.multiScanner.Next(data, from)
}

// tryMatchAt runs the DFA starting at position startPos.
func (dfa *searchDFA) tryMatchAt(data []byte, startPos int) [2]int {
	state := dfa.startState
	trans := dfa.trans
	ss256 := int(state) * 256
	lastMatch := -1
	if dfa.startIsMatch {
		lastMatch = startPos
	}

	for i := startPos; i < len(data); i++ {
		next := trans[ss256+int(data[i])]
		if next == dfaUncomputed {
			next = dfa.computeTransition(state, data[i])
			if next < 0 {
				return (&pikeVM{nfa: dfa.nfa}).findIndex(data)
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
		return [2]int{startPos, lastMatch}
	}
	return [2]int{-1, -1}
}

func (dfa *searchDFA) findIndexSlow(data []byte, startSearch int) [2]int {
	ss := dfa.startState
	trans := dfa.trans
	for startPos := startSearch; startPos <= len(data); startPos++ {
		state := ss
		ss256 := int(state) * 256
		lastMatch := startPos

		for i := startPos; i < len(data); i++ {
			next := trans[ss256+int(data[i])]
			if next == dfaUncomputed {
				next = dfa.computeTransition(state, data[i])
				if next < 0 {
					return (&pikeVM{nfa: dfa.nfa}).findIndex(data)
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
		return [2]int{startPos, lastMatch}
	}
	return [2]int{-1, -1}
}

func (dfa *searchDFA) findAllIndex(data []byte, n int) [][2]int {
	// Delegates to findAllIndexFast (batch SIMD + precomputed DFA + unsafe).
	// findAllIndexFast handles anchored/startIsMatch checks internally.
	return dfa.findAllIndexFast(data, n)
}

// findAllIndexSlow handles empty-match-capable patterns.
func (dfa *searchDFA) findAllIndexSlow(data []byte, n int) [][2]int {
	var results [][2]int
	pos := 0
	prevEnd := -1

	for (n < 0 || len(results) < n) && pos <= len(data) {
		match := dfa.findIndexFrom(data, pos)
		if match[0] < 0 {
			break
		}
		if match[0] == match[1] && match[1] == prevEnd {
			if pos < len(data) {
				_, size := stdDecodeRune(data[pos:])
				if size <= 0 {
					size = 1
				}
				pos += size
			} else {
				break
			}
			continue
		}
		results = append(results, match)
		prevEnd = match[1]
		if match[1] > match[0] {
			pos = match[1]
		} else {
			if pos < len(data) {
				_, size := stdDecodeRune(data[pos:])
				if size <= 0 {
					size = 1
				}
				pos += size
			} else {
				break
			}
		}
	}
	return results
}

func (dfa *searchDFA) findIndexFrom(data []byte, startSearch int) [2]int {
	if dfa.nfa.Flags&FlagAnchored != 0 {
		if startSearch > 0 {
			return [2]int{-1, -1}
		}
		return dfa.findIndexAnchored(data)
	}

	dfa.warmStart()
	ss := dfa.startState
	if ss == dfaDeadState {
		return [2]int{-1, -1}
	}

	if dfa.startIsMatch {
		return dfa.findIndexSlow(data, startSearch)
	}

	// SIMD-accelerated scan
	startPos := startSearch
	for startPos < len(data) {
		startPos = dfa.nextCandidateFrom(data, startPos)
		if startPos < 0 {
			break
		}

		if match := dfa.tryMatchAt(data, startPos); match[0] >= 0 {
			return match
		}
		startPos++
	}
	return [2]int{-1, -1}
}

func (dfa *searchDFA) computeTransition(stateIdx int32, b byte) int32 {
	if dfa.numStates >= dfa.cacheLimit {
		dfa.flush()
		return -1
	}

	off := int(stateIdx)*256 + int(b)
	nfaStates := dfa.nfaSets[stateIdx]

	var reachBuf [32]int32
	reachable := reachBuf[:0]
	for _, nfaIdx := range nfaStates {
		s := &dfa.nfa.States[nfaIdx]
		if byteMatchesState(s, b) && s.Next >= 0 {
			reachable = append(reachable, s.Next)
		}
	}

	if len(reachable) == 0 {
		dfa.trans[off] = dfaDeadState
		return dfaDeadState
	}

	closed := epsilonClosure(dfa.nfa, reachable)
	if len(closed) == 0 {
		dfa.trans[off] = dfaDeadState
		return dfaDeadState
	}

	nextState := dfa.getOrCreateState(closed)
	val := nextState
	if dfa.isMatch[nextState] {
		val |= sMatchBit
	}
	dfa.trans[off] = val
	return val
}

func (dfa *searchDFA) getOrCreateState(nfaStates []int32) int32 {
	key := makeStateKey(nfaStates)
	if idx, ok := dfa.stateMap[key]; ok {
		return idx
	}

	match := false
	for _, s := range nfaStates {
		if dfa.nfa.States[s].Op == OpMatch {
			match = true
			break
		}
	}

	idx := int32(dfa.numStates)
	dfa.numStates++

	needed := int(idx+1) * 256
	if needed > cap(dfa.trans) {
		newCap := cap(dfa.trans) * 2
		if newCap < needed {
			newCap = needed
		}
		newTrans := make([]int32, needed, newCap)
		copy(newTrans, dfa.trans)
		dfa.trans = newTrans
	} else {
		dfa.trans = dfa.trans[:needed]
	}
	for i := int(idx) * 256; i < needed; i++ {
		dfa.trans[i] = dfaUncomputed
	}

	dfa.isMatch = append(dfa.isMatch, match)
	dfa.stateMap[key] = idx
	dfa.nfaSets = append(dfa.nfaSets, nfaStates)
	return idx
}

func (dfa *searchDFA) flush() {
	dfa.trans = dfa.trans[:256]
	dfa.isMatch = dfa.isMatch[:1]
	dfa.nfaSets = dfa.nfaSets[:1]
	dfa.numStates = 1
	for k := range dfa.stateMap {
		delete(dfa.stateMap, k)
	}
	startSet := epsilonClosure(dfa.nfa, []int32{dfa.nfa.Start})
	dfa.startState = dfa.getOrCreateState(startSet)
	dfa.warmed = false
	dfa.precomputed = false
}

// ---------------- forward DFA (for match-only) ----------------

// forwardDFA is a single-pass DFA for match-only queries.
// Cache-optimized: flat []int32 array with match bit encoded in bit 30.
type forwardDFA struct {
	nfa         *NFA
	trans       []int32
	isMatch     []bool
	stateMap    map[stateKey]int32
	nfaSets     [][]int32
	numStates   int
	cacheLimit  int
	startState  int32
	startNFASet []int32
	precomputed bool
}

const fwdMatchBit int32 = 1 << 30
const fwdStateMask int32 = fwdMatchBit - 1

func newForwardDFA(nfa *NFA) *forwardDFA {
	fd := &forwardDFA{
		nfa:        nfa,
		trans:      make([]int32, 256),
		isMatch:    []bool{false},
		stateMap:   make(map[stateKey]int32, 64),
		nfaSets:    [][]int32{nil},
		numStates:  1,
		cacheLimit: dfaDefaultCache,
	}

	startSet := epsilonClosure(nfa, []int32{nfa.Start})
	fd.startNFASet = startSet
	fd.startState = fd.getOrCreateState(startSet)

	return fd
}

func (fd *forwardDFA) match(data []byte) bool {
	state := fd.startState
	if fd.isMatch[state] {
		return true
	}

	trans := fd.trans
	ss256 := int(state) * 256

	if fd.precomputed {
		// Read-only fast path: every reachable transition exists, and the
		// forward DFA has no dead state (misses map back to the start state),
		// so the loop is a single load + compare per byte.
		for _, b := range data {
			next := trans[ss256+int(b)]
			if next >= fwdMatchBit {
				return true
			}
			ss256 = int(next) * 256
		}
		return false
	}

	for _, b := range data {
		next := trans[ss256+int(b)]
		if next < 0 {
			next = fd.computeTransition(state, b)
			if next < 0 {
				return (&pikeVM{nfa: fd.nfa}).match(data)
			}
			trans = fd.trans
		}
		if next >= fwdMatchBit {
			return true
		}
		state = next & fwdStateMask
		ss256 = int(state) * 256
	}
	return false
}

// precomputeAll eagerly computes every transition reachable from the start
// state. On success the DFA becomes immutable, making it safe for concurrent
// use by multiple goroutines. Returns false on state-cache overflow.
func (fd *forwardDFA) precomputeAll() bool {
	classOf, reps := computeByteClasses(fd.nfa)
	visited := make([]bool, fd.cacheLimit)
	queue := make([]int32, 1, 64)
	queue[0] = fd.startState
	visited[fd.startState] = true

	for qi := 0; qi < len(queue); qi++ {
		state := queue[qi]
		base := int(state) * 256
		for _, rep := range reps {
			if fd.trans[base+int(rep)] == dfaUncomputed {
				if fd.computeTransition(state, rep) < 0 {
					return false // cache overflow
				}
			}
		}
		for b := 0; b < 256; b++ {
			val := fd.trans[base+int(reps[classOf[b]])]
			fd.trans[base+b] = val
			dest := val & fwdStateMask
			if int(dest) < len(visited) && !visited[dest] {
				visited[dest] = true
				queue = append(queue, dest)
			}
		}
	}
	fd.precomputed = true
	return true
}

func (fd *forwardDFA) computeTransition(stateIdx int32, b byte) int32 {
	if fd.numStates >= fd.cacheLimit {
		fd.flush()
		return -1
	}

	nfaStates := fd.nfaSets[stateIdx]
	off := int(stateIdx)*256 + int(b)

	var reachBuf [32]int32
	reachable := reachBuf[:0]

	for _, nfaIdx := range nfaStates {
		s := &fd.nfa.States[nfaIdx]
		if byteMatchesState(s, b) && s.Next >= 0 {
			reachable = append(reachable, s.Next)
		}
	}

	if stateIdx != fd.startState {
		for _, nfaIdx := range fd.startNFASet {
			s := &fd.nfa.States[nfaIdx]
			if byteMatchesState(s, b) && s.Next >= 0 {
				reachable = append(reachable, s.Next)
			}
		}
	}

	if len(reachable) == 0 {
		fd.trans[off] = fd.startState
		return fd.startState
	}

	closed := epsilonClosure(fd.nfa, reachable)
	closed = unionSorted(closed, fd.startNFASet)

	if len(closed) == 0 {
		fd.trans[off] = fd.startState
		return fd.startState
	}

	nextState := fd.getOrCreateState(closed)

	val := nextState
	if fd.isMatch[nextState] {
		val |= fwdMatchBit
	}
	fd.trans[off] = val
	return val
}

func (fd *forwardDFA) getOrCreateState(nfaStates []int32) int32 {
	key := makeStateKey(nfaStates)
	if idx, ok := fd.stateMap[key]; ok {
		return idx
	}

	match := false
	for _, s := range nfaStates {
		if fd.nfa.States[s].Op == OpMatch {
			match = true
			break
		}
	}

	idx := int32(fd.numStates)
	fd.numStates++

	needed := int(idx+1) * 256
	if needed > cap(fd.trans) {
		newCap := cap(fd.trans) * 2
		if newCap < needed {
			newCap = needed
		}
		newTrans := make([]int32, needed, newCap)
		copy(newTrans, fd.trans)
		fd.trans = newTrans
	} else {
		fd.trans = fd.trans[:needed]
	}
	for i := int(idx) * 256; i < needed; i++ {
		fd.trans[i] = dfaUncomputed
	}

	fd.isMatch = append(fd.isMatch, match)
	fd.stateMap[key] = idx
	fd.nfaSets = append(fd.nfaSets, nfaStates)
	return idx
}

func (fd *forwardDFA) flush() {
	fd.trans = fd.trans[:256]
	fd.isMatch = fd.isMatch[:1]
	fd.nfaSets = fd.nfaSets[:1]
	fd.numStates = 1
	for k := range fd.stateMap {
		delete(fd.stateMap, k)
	}
	startSet := epsilonClosure(fd.nfa, []int32{fd.nfa.Start})
	fd.startNFASet = startSet
	fd.startState = fd.getOrCreateState(startSet)
}

// ---------------- shared helpers ----------------

// computeByteClasses partitions the byte alphabet into equivalence classes:
// bytes in the same class are indistinguishable to every consuming NFA state,
// so they always produce identical DFA transitions. Precomputation then runs
// the (expensive) epsilon-closure work once per class representative instead
// of once per byte — typically ~5-20 classes instead of 256.
func computeByteClasses(nfa *NFA) (classOf [256]int32, reps []byte) {
	var boundary [257]bool
	boundary[0] = true
	mark := func(lo, hi byte) {
		boundary[lo] = true
		boundary[int(hi)+1] = true
	}
	for i := range nfa.States {
		s := &nfa.States[i]
		switch s.Op {
		case OpByte:
			mark(s.ByteVal, s.ByteVal)
		case OpByteRange:
			mark(s.Lo, s.Hi)
		case OpByteRanges:
			for _, r := range s.Ranges {
				mark(r.Lo, r.Hi)
			}
		case OpAny:
			mark('\n', '\n')
		}
	}
	cls := int32(-1)
	for b := 0; b < 256; b++ {
		if boundary[b] {
			cls++
			reps = append(reps, byte(b))
		}
		classOf[b] = cls
	}
	return classOf, reps
}

// computeAllowedBytes returns the set of bytes consumable by any NFA state.
// Every byte inside any match is consumed by some transition, so a byte
// outside this set can never appear within a match. The rare-byte prefilter
// uses this to bound how far left of a hit a match could begin.
func computeAllowedBytes(nfa *NFA) (allowed [256]bool) {
	for i := range nfa.States {
		s := &nfa.States[i]
		switch s.Op {
		case OpByte:
			allowed[s.ByteVal] = true
		case OpByteRange:
			for b := int(s.Lo); b <= int(s.Hi); b++ {
				allowed[b] = true
			}
		case OpByteRanges:
			for _, r := range s.Ranges {
				for b := int(r.Lo); b <= int(r.Hi); b++ {
					allowed[b] = true
				}
			}
		case OpAny:
			for b := 0; b < 256; b++ {
				if b != '\n' {
					allowed[b] = true
				}
			}
		case OpAnyByte:
			for b := 0; b < 256; b++ {
				allowed[b] = true
			}
		}
	}
	return allowed
}

func byteMatchesState(s *State, b byte) bool {
	switch s.Op {
	case OpByte:
		return b == s.ByteVal
	case OpByteRange:
		return b >= s.Lo && b <= s.Hi
	case OpByteRanges:
		return byteInRanges(b, s.Ranges)
	case OpAny:
		return b != '\n'
	case OpAnyByte:
		return true
	default:
		return false
	}
}

func epsilonClosure(nfa *NFA, seeds []int32) []int32 {
	numStates := len(nfa.States)

	var stackVisited [256]bool
	var visited []bool
	if numStates <= 256 {
		visited = stackVisited[:numStates]
	} else {
		visited = make([]bool, numStates)
	}

	var result []int32
	var stackBuf [64]int32
	stack := stackBuf[:0]

	for i := len(seeds) - 1; i >= 0; i-- {
		stack = append(stack, seeds[i])
	}

	for len(stack) > 0 {
		idx := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if idx < 0 || int(idx) >= numStates || visited[idx] {
			continue
		}
		visited[idx] = true

		s := &nfa.States[idx]
		switch s.Op {
		case OpSplit:
			if s.Alt >= 0 {
				stack = append(stack, s.Alt)
			}
			if s.Next >= 0 {
				stack = append(stack, s.Next)
			}
		case OpCapture:
			if s.Next >= 0 {
				stack = append(stack, s.Next)
			}
		default:
			result = append(result, idx)
		}
	}

	slices.Sort(result)
	return result
}

func makeStateKey(states []int32) stateKey {
	buf := make([]byte, len(states)*4)
	for i, s := range states {
		buf[i*4] = byte(s)
		buf[i*4+1] = byte(s >> 8)
		buf[i*4+2] = byte(s >> 16)
		buf[i*4+3] = byte(s >> 24)
	}
	return stateKey(buf)
}

func unionSorted(a, b []int32) []int32 {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	result := make([]int32, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] < b[j] {
			result = append(result, a[i])
			i++
		} else if a[i] > b[j] {
			result = append(result, b[j])
			j++
		} else {
			result = append(result, a[i])
			i++
			j++
		}
	}
	result = append(result, a[i:]...)
	result = append(result, b[j:]...)
	return result
}

func runeSize(data []byte) int {
	if len(data) == 0 {
		return 1
	}
	_, n := stdDecodeRune(data)
	if n <= 0 {
		return 1
	}
	return n
}
