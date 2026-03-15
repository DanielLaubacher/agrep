// Package regex provides a high-performance regular expression engine with
// lazy DFA, SIMD literal prefiltering, and rune-aware search.
// It is a drop-in replacement for Go's regexp package with significantly
// better performance on common patterns.
package regex

// OpCode represents an NFA instruction type.
type OpCode uint8

const (
	OpByte       OpCode = iota // match a single byte
	OpByteRange                // match a byte in [Lo, Hi]
	OpByteRanges               // match a byte in one of several ranges (dense)
	OpSplit                    // non-deterministic branch (Next or Alt)
	OpMatch                    // accept state
	OpAny                      // match any byte except \n
	OpAnyByte                  // match any byte including \n
	OpCapture                  // capture group boundary (for PikeVM)
	OpAssertBOL                // assert beginning of line
	OpAssertEOL                // assert end of line
	OpAssertBOT                // assert beginning of text
	OpAssertEOT                // assert end of text
	OpAssertWord               // assert word boundary \b
	OpAssertNotWord            // assert non-word boundary \B
)

// State is a single NFA state. Designed for cache-friendly iteration:
// - No pointers (indices only), so []State doesn't cause GC scanning.
// - 32 bytes per state for good cache line packing (2 states per line).
type State struct {
	Op      OpCode  // instruction type
	ByteVal byte    // for OpByte: the byte to match
	Lo, Hi  byte    // for OpByteRange: inclusive range [Lo, Hi]
	CaptIdx uint16  // for OpCapture: capture group index * 2 + (0=open,1=close)
	Next    int32   // primary transition target (state index)
	Alt     int32   // alternate transition (for OpSplit), -1 if unused
	Ranges  []ByteRange // for OpByteRanges: sorted byte ranges
}

// ByteRange is an inclusive byte range [Lo, Hi].
type ByteRange struct {
	Lo, Hi byte
}

// NFA represents a compiled non-deterministic finite automaton.
// States are stored in a flat array for cache-friendly access.
type NFA struct {
	States   []State
	Start    int32 // start state index
	Match    int32 // match/accept state index
	NumCap   int   // number of capture groups (including group 0)
	Flags    NFAFlags
}

// NFAFlags holds properties detected during compilation.
type NFAFlags uint8

const (
	FlagASCIIOnly  NFAFlags = 1 << iota // pattern is ASCII-only (byte DFA safe)
	FlagAnchored                         // pattern is anchored at start (^)
	FlagLiteral                          // pattern is a pure literal string
	FlagHasCapture                       // pattern has capture groups
	FlagHasAssert                        // pattern has zero-width assertions
)

// addState appends a state and returns its index.
func (nfa *NFA) addState(s State) int32 {
	idx := int32(len(nfa.States))
	nfa.States = append(nfa.States, s)
	return idx
}

// patch sets the Next field of all states at indices in list to target.
func (nfa *NFA) patch(list []int32, target int32) {
	for _, idx := range list {
		s := &nfa.States[idx]
		if s.Next == -1 {
			s.Next = target
		}
		if s.Op == OpSplit && s.Alt == -1 {
			s.Alt = target
		}
	}
}

// fragment is a partial NFA under construction.
// start is the entry state; dangles are states with unpatched transitions.
type fragment struct {
	start   int32
	dangles []int32 // states whose Next or Alt needs patching
}
