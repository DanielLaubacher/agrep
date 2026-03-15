package regex

// PikeVM: Thompson/Pike NFA simulation.
// Always-correct baseline engine with guaranteed linear time.
// Used as fallback when the lazy DFA can't handle a pattern,
// and for capture group extraction.
//
// Key optimizations:
// - Flat arrays instead of linked lists for thread sets
// - Generation-counter sparse set to avoid clearing visited bitset
// - Stack-allocated work buffers for small NFAs (≤64 states)
// - No allocations on the hot path after warmup

// pikeVM is the NFA simulation engine.
type pikeVM struct {
	nfa *NFA
}

// thread represents a single NFA simulation thread.
// Tracks the start position of the potential match.
type thread struct {
	pc    int32 // current NFA state
	start int   // byte position where this match attempt started
}

// threadSet is a deduplicated set of threads for one step.
// Uses a generation counter to avoid clearing the visited bitset.
type threadSet struct {
	threads []thread
	sparse  []int32 // sparse[state] = generation if visited
	gen     int32
}

func newThreadSet(numStates int) *threadSet {
	return &threadSet{
		threads: make([]thread, 0, min(numStates, 64)),
		sparse:  make([]int32, numStates),
		gen:     1,
	}
}

func (ts *threadSet) reset() {
	ts.threads = ts.threads[:0]
	ts.gen++
	if ts.gen < 0 {
		for i := range ts.sparse {
			ts.sparse[i] = 0
		}
		ts.gen = 1
	}
}

func (ts *threadSet) contains(pc int32) bool {
	return ts.sparse[pc] == ts.gen
}

func (ts *threadSet) add(t thread) {
	if ts.sparse[t.pc] == ts.gen {
		return
	}
	ts.sparse[t.pc] = ts.gen
	ts.threads = append(ts.threads, t)
}

// match returns true if the NFA matches anywhere in data.
func (vm *pikeVM) match(data []byte) bool {
	nfa := vm.nfa
	numStates := len(nfa.States)
	curr := newThreadSet(numStates)
	next := newThreadSet(numStates)

	for pos := 0; pos <= len(data); pos++ {
		// Add start state at rune boundaries only
		if pos >= len(data) || !isUTF8Continuation(data, pos) {
			vm.addThread(curr, thread{pc: nfa.Start, start: pos}, data, pos)
		}

		var b byte
		atEnd := pos >= len(data)
		if !atEnd {
			b = data[pos]
		}

		for i := 0; i < len(curr.threads); i++ {
			t := curr.threads[i]
			s := &nfa.States[t.pc]

			switch s.Op {
			case OpMatch:
				return true

			case OpByte:
				if !atEnd && b == s.ByteVal {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpByteRange:
				if !atEnd && b >= s.Lo && b <= s.Hi {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpByteRanges:
				if !atEnd && byteInRanges(b, s.Ranges) {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpAny:
				if !atEnd && b != '\n' {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpAnyByte:
				if !atEnd {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			}
		}

		curr, next = next, curr
		next.reset()

		if len(curr.threads) == 0 {
			// No active threads. For anchored patterns, stop immediately.
			if nfa.Flags&FlagAnchored != 0 {
				return false
			}
		}
	}

	return false
}

// findIndex returns the first match [start, end] or [-1, -1].
// Uses leftmost-longest semantics.
func (vm *pikeVM) findIndex(data []byte) [2]int {
	return vm.findIndexFrom(data, 0)
}

// findIndexFrom finds the first match starting at or after startPos.
func (vm *pikeVM) findIndexFrom(data []byte, startPos int) [2]int {
	nfa := vm.nfa
	numStates := len(nfa.States)
	curr := newThreadSet(numStates)
	next := newThreadSet(numStates)

	bestStart := -1
	bestEnd := -1
	foundMatch := false

	for pos := startPos; pos <= len(data); pos++ {
		// Only add new start threads at rune boundaries and if no match yet
		if !foundMatch && (pos >= len(data) || !isUTF8Continuation(data, pos)) {
			vm.addThread(curr, thread{pc: nfa.Start, start: pos}, data, pos)
		}

		var b byte
		atEnd := pos >= len(data)
		if !atEnd {
			b = data[pos]
		}

		for i := 0; i < len(curr.threads); i++ {
			t := curr.threads[i]
			s := &nfa.States[t.pc]

			switch s.Op {
			case OpMatch:
				// Record match (leftmost-longest)
				if !foundMatch || t.start < bestStart || (t.start == bestStart && pos > bestEnd) {
					bestStart = t.start
					bestEnd = pos
					foundMatch = true
				}
				continue // don't add consuming transitions from match state

			case OpByte:
				if !atEnd && b == s.ByteVal {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpByteRange:
				if !atEnd && b >= s.Lo && b <= s.Hi {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpByteRanges:
				if !atEnd && byteInRanges(b, s.Ranges) {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpAny:
				if !atEnd && b != '\n' {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			case OpAnyByte:
				if !atEnd {
					vm.addThread(next, thread{pc: s.Next, start: t.start}, data, pos+1)
				}
			}
		}

		// If we found a match and no threads can extend it, return
		if foundMatch && len(next.threads) == 0 {
			return [2]int{bestStart, bestEnd}
		}

		curr, next = next, curr
		next.reset()

		// If no match and no active threads, skip ahead
		if !foundMatch && len(curr.threads) == 0 {
			if nfa.Flags&FlagAnchored != 0 {
				return [2]int{-1, -1}
			}
		}
	}

	if foundMatch {
		return [2]int{bestStart, bestEnd}
	}
	return [2]int{-1, -1}
}

// findAllIndex returns all non-overlapping matches.
// Empty matches abutting a preceding match are skipped (Go stdlib semantics).
func (vm *pikeVM) findAllIndex(data []byte, n int) [][2]int {
	var results [][2]int
	pos := 0
	prevEnd := -1

	for (n < 0 || len(results) < n) && pos <= len(data) {
		match := vm.findIndexFrom(data, pos)
		if match[0] < 0 {
			break
		}

		if match[0] == match[1] && match[1] == prevEnd {
			if pos < len(data) {
				_, size := decodeRune(data[pos:])
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
				_, size := decodeRune(data[pos:])
				pos += size
			} else {
				break
			}
		}
	}

	return results
}

// addThread adds a thread with epsilon closure expansion.
// Follows all epsilon transitions immediately, adding reachable consuming states.
func (vm *pikeVM) addThread(ts *threadSet, t thread, data []byte, pos int) {
	type entry struct {
		pc    int32
		start int
	}
	var stackBuf [64]entry
	stack := stackBuf[:0]
	stack = append(stack, entry{t.pc, t.start})

	for len(stack) > 0 {
		e := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if e.pc < 0 || int(e.pc) >= len(vm.nfa.States) {
			continue
		}
		if ts.contains(e.pc) {
			continue
		}

		s := &vm.nfa.States[e.pc]

		switch s.Op {
		case OpSplit:
			ts.add(thread{pc: e.pc, start: e.start})
			// Add both branches. Next first for greedy preference.
			if s.Alt >= 0 {
				stack = append(stack, entry{s.Alt, e.start})
			}
			if s.Next >= 0 {
				stack = append(stack, entry{s.Next, e.start})
			}

		case OpCapture:
			ts.add(thread{pc: e.pc, start: e.start})
			if s.Next >= 0 {
				stack = append(stack, entry{s.Next, e.start})
			}

		case OpAssertBOL:
			if pos == 0 || (pos > 0 && pos <= len(data) && data[pos-1] == '\n') {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		case OpAssertEOL:
			if pos >= len(data) || data[pos] == '\n' {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		case OpAssertBOT:
			if pos == 0 {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		case OpAssertEOT:
			if pos >= len(data) {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		case OpAssertWord:
			if isRuneBoundary(data, pos) && isWordBoundary(data, pos) {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		case OpAssertNotWord:
			if isRuneBoundary(data, pos) && !isWordBoundary(data, pos) {
				if s.Next >= 0 {
					stack = append(stack, entry{s.Next, e.start})
				}
			}

		default:
			// Consuming state (OpByte, OpByteRange, etc.): add to thread set
			ts.add(thread{pc: e.pc, start: e.start})
		}
	}
}

// byteInRanges checks if b is in any of the given byte ranges.
func byteInRanges(b byte, ranges []ByteRange) bool {
	for _, r := range ranges {
		if b >= r.Lo && b <= r.Hi {
			return true
		}
	}
	return false
}

// isWordBoundary returns true if position pos is at a word boundary.
// Only valid at rune boundaries (not at UTF-8 continuation bytes).
func isWordBoundary(data []byte, pos int) bool {
	before := pos > 0 && isWordByte(data[pos-1])
	after := pos < len(data) && isWordByte(data[pos])
	return before != after
}

// isRuneBoundary returns true if pos is at the start of a UTF-8 rune.
func isRuneBoundary(data []byte, pos int) bool {
	if pos <= 0 || pos >= len(data) {
		return true
	}
	return !isUTF8Continuation(data, pos)
}

// isUTF8Continuation returns true if byte at pos is a continuation byte
// that is part of a valid multi-byte UTF-8 sequence.
// Uses unicode/utf8 for correct validation of all edge cases.
func isUTF8Continuation(data []byte, pos int) bool {
	if pos <= 0 || pos >= len(data) {
		return false
	}
	b := data[pos]
	if b&0xC0 != 0x80 {
		return false
	}
	// Find the potential start of the multi-byte sequence
	start := pos - 1
	for start > 0 && start > pos-4 && data[start]&0xC0 == 0x80 {
		start--
	}
	// Decode rune starting at 'start' — if it spans past pos, we're mid-rune
	_, size := stdDecodeRune(data[start:])
	return start+size > pos
}

// stdDecodeRune uses proper UTF-8 decoding with full validation.
func stdDecodeRune(data []byte) (rune, int) {
	if len(data) == 0 {
		return 0xFFFD, 0
	}
	b := data[0]
	if b < 0x80 {
		return rune(b), 1
	}
	// Use the same logic as unicode/utf8.DecodeRune
	if b < 0xC2 || b > 0xF4 {
		return 0xFFFD, 1
	}
	if b < 0xE0 {
		if len(data) < 2 || data[1]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		return rune(b&0x1F)<<6 | rune(data[1]&0x3F), 2
	}
	if b < 0xF0 {
		if len(data) < 3 || data[1]&0xC0 != 0x80 || data[2]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
		r := rune(b&0x0F)<<12 | rune(data[1]&0x3F)<<6 | rune(data[2]&0x3F)
		// Reject overlong and surrogates
		if r < 0x800 || (r >= 0xD800 && r <= 0xDFFF) {
			return 0xFFFD, 1
		}
		return r, 3
	}
	if len(data) < 4 || data[1]&0xC0 != 0x80 || data[2]&0xC0 != 0x80 || data[3]&0xC0 != 0x80 {
		return 0xFFFD, 1
	}
	r := rune(b&0x07)<<18 | rune(data[1]&0x3F)<<12 | rune(data[2]&0x3F)<<6 | rune(data[3]&0x3F)
	if r < 0x10000 || r > 0x10FFFF {
		return 0xFFFD, 1
	}
	return r, 4
}

// isWordByte returns true if b is a word character [0-9A-Za-z_].
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// decodeRune decodes the first UTF-8 rune from data.
// Returns replacement character and size 1 for invalid sequences.
func decodeRune(data []byte) (rune, int) {
	if len(data) == 0 {
		return 0, 0
	}
	b := data[0]
	if b < 0x80 {
		return rune(b), 1
	}
	n := utf8ByteLen(b)
	if n == 0 || n > len(data) {
		return 0xFFFD, 1
	}
	// Validate continuation bytes (must be 10xxxxxx)
	for i := 1; i < n; i++ {
		if data[i]&0xC0 != 0x80 {
			return 0xFFFD, 1
		}
	}
	var r rune
	switch n {
	case 2:
		r = rune(b&0x1F)<<6 | rune(data[1]&0x3F)
	case 3:
		r = rune(b&0x0F)<<12 | rune(data[1]&0x3F)<<6 | rune(data[2]&0x3F)
	case 4:
		r = rune(b&0x07)<<18 | rune(data[1]&0x3F)<<12 | rune(data[2]&0x3F)<<6 | rune(data[3]&0x3F)
	}
	return r, n
}
