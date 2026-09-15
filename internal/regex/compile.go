package regex

// Compiler: regexp/syntax AST → NFA (Thompson's construction).
//
// The compiler transforms Go's parsed regex AST into a flat NFA state array.
// It detects ASCII-only patterns and emits byte-level instructions for the
// fast path, falling back to UTF-8 byte automaton fragments for Unicode.

import (
	"regexp/syntax"
	"unicode"
)

// compile transforms a parsed regex AST into an NFA.
func compile(re *syntax.Regexp) *NFA {
	nfa := &NFA{
		States: make([]State, 0, estimateStates(re)),
	}

	// Add match state first (always index 0)
	matchState := nfa.addState(State{Op: OpMatch, Next: -1, Alt: -1})
	nfa.Match = matchState

	// Compile the AST
	frag := compileNode(nfa, re)

	// Patch all dangling transitions to the match state
	nfa.patch(frag.dangles, matchState)
	nfa.Start = frag.start

	// Detect flags
	nfa.Flags = detectFlags(re)
	nfa.NumCap = countCaptures(re) + 1 // +1 for implicit group 0

	return nfa
}

// compileNode recursively compiles an AST node into an NFA fragment.
func compileNode(nfa *NFA, re *syntax.Regexp) fragment {
	switch re.Op {
	case syntax.OpLiteral:
		return compileLiteral(nfa, re)
	case syntax.OpCharClass:
		return compileCharClass(nfa, re.Rune)
	case syntax.OpAnyCharNotNL:
		return compileDotNotNL(nfa)
	case syntax.OpAnyChar:
		return compileDotAll(nfa)
	case syntax.OpConcat:
		return compileConcat(nfa, re.Sub)
	case syntax.OpAlternate:
		return compileAlternate(nfa, re.Sub)
	case syntax.OpCapture:
		return compileCapture(nfa, re)
	case syntax.OpStar:
		return compileStar(nfa, re)
	case syntax.OpPlus:
		return compilePlus(nfa, re)
	case syntax.OpQuest:
		return compileQuest(nfa, re)
	case syntax.OpRepeat:
		return compileRepeat(nfa, re)
	case syntax.OpBeginLine:
		return compileAssert(nfa, OpAssertBOL)
	case syntax.OpEndLine:
		return compileAssert(nfa, OpAssertEOL)
	case syntax.OpBeginText:
		return compileAssert(nfa, OpAssertBOT)
	case syntax.OpEndText:
		return compileAssert(nfa, OpAssertEOT)
	case syntax.OpWordBoundary:
		return compileAssert(nfa, OpAssertWord)
	case syntax.OpNoWordBoundary:
		return compileAssert(nfa, OpAssertNotWord)
	case syntax.OpEmptyMatch:
		// Empty match: just a pass-through split
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	case syntax.OpNoMatch:
		// Dead state
		s := nfa.addState(State{Op: OpByte, ByteVal: 0xFF, Next: -1, Alt: -1})
		return fragment{start: s, dangles: nil}
	default:
		// Fallback: treat as empty
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}
}

// compileLiteral compiles a literal string into a chain of byte-match states.
func compileLiteral(nfa *NFA, re *syntax.Regexp) fragment {
	if len(re.Rune) == 0 {
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	foldCase := re.Flags&syntax.FoldCase != 0

	var frags []fragment
	for _, r := range re.Rune {
		if foldCase {
			f := compileFoldedRune(nfa, r)
			frags = append(frags, f)
		} else if r < 0x80 {
			// ASCII: single byte match
			s := nfa.addState(State{Op: OpByte, ByteVal: byte(r), Next: -1, Alt: -1})
			frags = append(frags, fragment{start: s, dangles: []int32{s}})
		} else {
			// Multi-byte UTF-8
			f := compileRuneBytes(nfa, r)
			frags = append(frags, f)
		}
	}

	return concatFragments(nfa, frags)
}

// compileFoldedRune compiles a rune with case folding into an alternation
// of all case variants.
func compileFoldedRune(nfa *NFA, r rune) fragment {
	// Collect all case variants
	variants := caseVariants(r)

	if len(variants) == 1 {
		if variants[0] < 0x80 {
			s := nfa.addState(State{Op: OpByte, ByteVal: byte(variants[0]), Next: -1, Alt: -1})
			return fragment{start: s, dangles: []int32{s}}
		}
		return compileRuneBytes(nfa, variants[0])
	}

	// Build character class from variants
	var runes []rune
	for _, v := range variants {
		runes = append(runes, v, v) // [lo, hi] pair where lo == hi
	}
	return compileCharClass(nfa, runes)
}

// caseVariants returns all Unicode case variants of a rune.
// SimpleFold produces a unique cycle, so no dedup needed.
func caseVariants(r rune) []rune {
	variants := []rune{r}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		variants = append(variants, f)
	}
	return variants
}

// compileRuneBytes compiles a single rune into its UTF-8 byte sequence.
func compileRuneBytes(nfa *NFA, r rune) fragment {
	buf := runeToUTF8(r)
	n := runeUTF8Len(r)

	var firstIdx int32
	var dangleIdx int32
	var prevIdx int32 = -1

	for i := n - 1; i >= 0; i-- {
		s := State{Op: OpByte, ByteVal: buf[i], Next: prevIdx, Alt: -1}
		idx := nfa.addState(s)
		if i == n-1 {
			dangleIdx = idx // last byte has Next=-1 (the dangle)
		}
		prevIdx = idx
		if i == 0 {
			firstIdx = idx
		}
	}

	return fragment{start: firstIdx, dangles: []int32{dangleIdx}}
}

func runeUTF8Len(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case r < 0x800:
		return 2
	case r < 0x10000:
		return 3
	default:
		return 4
	}
}

// compileConcat compiles a concatenation of sub-expressions.
func compileConcat(nfa *NFA, subs []*syntax.Regexp) fragment {
	if len(subs) == 0 {
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	frags := make([]fragment, len(subs))
	for i, sub := range subs {
		frags[i] = compileNode(nfa, sub)
	}

	return concatFragments(nfa, frags)
}

// concatFragments chains fragments sequentially.
func concatFragments(nfa *NFA, frags []fragment) fragment {
	if len(frags) == 0 {
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}
	if len(frags) == 1 {
		return frags[0]
	}

	// Chain: patch each fragment's dangles to the next fragment's start
	for i := 0; i < len(frags)-1; i++ {
		nfa.patch(frags[i].dangles, frags[i+1].start)
	}

	return fragment{
		start:   frags[0].start,
		dangles: frags[len(frags)-1].dangles,
	}
}

// compileAlternate compiles an alternation (|) using balanced split tree.
func compileAlternate(nfa *NFA, subs []*syntax.Regexp) fragment {
	if len(subs) == 0 {
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	frags := make([]fragment, len(subs))
	for i, sub := range subs {
		frags[i] = compileNode(nfa, sub)
	}

	return alternateFragments(nfa, frags)
}

// compileCapture compiles a capture group.
func compileCapture(nfa *NFA, re *syntax.Regexp) fragment {
	if len(re.Sub) == 0 {
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	capIdx := uint16(re.Cap * 2) // open slot

	// Capture open
	open := nfa.addState(State{Op: OpCapture, CaptIdx: capIdx, Next: -1, Alt: -1})

	// Compile body
	body := compileNode(nfa, re.Sub[0])
	nfa.States[open].Next = body.start

	// Capture close
	close := nfa.addState(State{Op: OpCapture, CaptIdx: capIdx + 1, Next: -1, Alt: -1})
	nfa.patch(body.dangles, close)

	return fragment{start: open, dangles: []int32{close}}
}

// compileStar compiles e* (zero or more).
func compileStar(nfa *NFA, re *syntax.Regexp) fragment {
	body := compileNode(nfa, re.Sub[0])

	s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})

	if re.Flags&syntax.NonGreedy != 0 {
		// Non-greedy: try skip first
		nfa.States[s].Alt = body.start
		// Next is the dangle (skip path)
	} else {
		// Greedy: try body first
		nfa.States[s].Next = body.start
		// Alt is the dangle (skip path)
	}

	// Body loops back to split
	nfa.patch(body.dangles, s)

	return fragment{start: s, dangles: []int32{s}}
}

// compilePlus compiles e+ (one or more).
func compilePlus(nfa *NFA, re *syntax.Regexp) fragment {
	body := compileNode(nfa, re.Sub[0])

	s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})

	if re.Flags&syntax.NonGreedy != 0 {
		nfa.States[s].Alt = body.start
	} else {
		nfa.States[s].Next = body.start
	}

	nfa.patch(body.dangles, s)

	return fragment{start: body.start, dangles: []int32{s}}
}

// compileQuest compiles e? (zero or one).
func compileQuest(nfa *NFA, re *syntax.Regexp) fragment {
	body := compileNode(nfa, re.Sub[0])

	s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})

	if re.Flags&syntax.NonGreedy != 0 {
		nfa.States[s].Alt = body.start
	} else {
		nfa.States[s].Next = body.start
	}

	// Dangles: the skip path from split + body's dangles
	dangles := append(body.dangles, s)
	return fragment{start: s, dangles: dangles}
}

// compileRepeat compiles e{n,m} bounded repetition.
func compileRepeat(nfa *NFA, re *syntax.Regexp) fragment {
	minRep := re.Min
	maxRep := re.Max // -1 means unbounded

	if maxRep == 0 {
		// e{0}: empty match
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	var frags []fragment
	var optDangles []int32

	// Required copies: e{min}
	for i := 0; i < minRep; i++ {
		f := compileNode(nfa, re.Sub[0])
		frags = append(frags, f)
	}

	if maxRep < 0 {
		// e{n,}: min copies then e*
		lastBody := compileNode(nfa, re.Sub[0])
		s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})

		if re.Flags&syntax.NonGreedy != 0 {
			nfa.States[s].Alt = lastBody.start
		} else {
			nfa.States[s].Next = lastBody.start
		}
		nfa.patch(lastBody.dangles, s)

		frags = append(frags, fragment{start: s, dangles: []int32{s}})
	} else {
		// e{n,m}: min copies then (m-n) optional copies
		for i := minRep; i < maxRep; i++ {
			body := compileNode(nfa, re.Sub[0])
			s := nfa.addState(State{Op: OpSplit, Next: -1, Alt: -1})

			if re.Flags&syntax.NonGreedy != 0 {
				nfa.States[s].Alt = body.start
			} else {
				nfa.States[s].Next = body.start
			}

			optDangles = append(optDangles, s)
			frags = append(frags, fragment{start: s, dangles: body.dangles})
		}
	}

	result := concatFragments(nfa, frags)
	result.dangles = append(result.dangles, optDangles...)
	return result
}

// compileAssert compiles a zero-width assertion.
func compileAssert(nfa *NFA, op OpCode) fragment {
	s := nfa.addState(State{Op: op, Next: -1, Alt: -1})
	return fragment{start: s, dangles: []int32{s}}
}

// estimateStates gives a rough estimate of NFA states needed.
func estimateStates(re *syntax.Regexp) int {
	n := 2 // match state + at least one
	n += estimateNodeStates(re)
	if n < 16 {
		n = 16
	}
	return n
}

func estimateNodeStates(re *syntax.Regexp) int {
	n := 1
	for _, sub := range re.Sub {
		n += estimateNodeStates(sub)
	}
	if re.Op == syntax.OpCharClass {
		n += len(re.Rune) // rough: each range pair may need states
	}
	if re.Op == syntax.OpRepeat {
		n *= max(re.Max, re.Min+1)
	}
	return n
}

// detectFlags analyzes the AST to set NFA flags.
func detectFlags(re *syntax.Regexp) NFAFlags {
	var flags NFAFlags
	if isASCIIPattern(re) {
		flags |= FlagASCIIOnly
	}
	if isAnchored(re) {
		flags |= FlagAnchored
	}
	if re.Op == syntax.OpLiteral {
		flags |= FlagLiteral
	}
	if hasCaptures(re) {
		flags |= FlagHasCapture
	}
	if hasAssertions(re) {
		flags |= FlagHasAssert
	}
	return flags
}

// hasAssertions returns true if the pattern has any zero-width assertions.
func hasAssertions(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return true
	}
	for _, sub := range re.Sub {
		if hasAssertions(sub) {
			return true
		}
	}
	return false
}

// isASCIIPattern returns true if the pattern only matches ASCII bytes.
func isASCIIPattern(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if r > unicode.MaxASCII {
				return false
			}
		}
		return true
	case syntax.OpCharClass:
		return isASCIIClass(re.Rune)
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return false // can match non-ASCII
	default:
		for _, sub := range re.Sub {
			if !isASCIIPattern(sub) {
				return false
			}
		}
		return true
	}
}

// isAnchored returns true if the pattern is anchored at the start of the
// text. A per-line ^ (OpBeginLine) is not anchoring: it can hold at every
// line start, so the engines must keep scanning past position 0.
func isAnchored(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginText:
		return true
	case syntax.OpConcat:
		return len(re.Sub) > 0 && isAnchored(re.Sub[0])
	case syntax.OpCapture:
		return len(re.Sub) > 0 && isAnchored(re.Sub[0])
	default:
		return false
	}
}

// hasCaptures returns true if the pattern has capture groups.
func hasCaptures(re *syntax.Regexp) bool {
	if re.Op == syntax.OpCapture && re.Cap > 0 {
		return true
	}
	for _, sub := range re.Sub {
		if hasCaptures(sub) {
			return true
		}
	}
	return false
}

// countCaptures returns the number of capture groups.
func countCaptures(re *syntax.Regexp) int {
	n := 0
	if re.Op == syntax.OpCapture && re.Cap > 0 {
		n = re.Cap
	}
	for _, sub := range re.Sub {
		if c := countCaptures(sub); c > n {
			n = c
		}
	}
	return n
}
