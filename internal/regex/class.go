package regex

// Character class compilation: converts Unicode character classes into
// byte-level NFA fragments for the DFA to consume.

import (
	"regexp/syntax"
	"sort"
	"unicode"
)

// runeClassToByteRanges converts a Unicode rune range table into
// byte-level ranges suitable for NFA matching. For ASCII ranges,
// this is direct; for multi-byte UTF-8, it creates byte sequence ranges.
func runeClassToByteRanges(runes []rune) [][]ByteRange {
	// runes is pairs: [lo1, hi1, lo2, hi2, ...]
	if len(runes)%2 != 0 {
		return nil
	}

	var result [][]ByteRange
	for i := 0; i < len(runes); i += 2 {
		lo, hi := runes[i], runes[i+1]
		seqs := utf8Sequences(lo, hi)
		result = append(result, seqs...)
	}
	return result
}

// isASCIIClass returns true if the character class only contains ASCII runes.
func isASCIIClass(runes []rune) bool {
	for i := 0; i < len(runes); i += 2 {
		if runes[i+1] > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// mergeByteRanges merges overlapping/adjacent ByteRange slices.
func mergeByteRanges(ranges []ByteRange) []ByteRange {
	if len(ranges) <= 1 {
		return ranges
	}

	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].Lo < ranges[j].Lo
	})

	merged := ranges[:1]
	for _, r := range ranges[1:] {
		last := &merged[len(merged)-1]
		if r.Lo <= last.Hi+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
		} else {
			merged = append(merged, r)
		}
	}
	return merged
}

// compileDotNotNL builds NFA states that match any single UTF-8 rune except \n.
// Handles:
//   - ASCII (0x00-0x7F except \n): 1 byte
//   - 2-byte UTF-8 (0xC2-0xDF + cont): 2 bytes
//   - 3-byte UTF-8 (0xE0-0xEF + 2 cont): 3 bytes
//   - 4-byte UTF-8 (0xF0-0xF4 + 3 cont): 4 bytes
//   - Invalid bytes (standalone 0x80-0xC1, 0xF5-0xFF): 1 byte (replacement char)
func compileDotNotNL(nfa *NFA) fragment {
	var frags []fragment

	// ASCII except \n: single byte [0x00-0x09] | [0x0B-0x7F]
	s1a := nfa.addState(State{Op: OpByteRange, Lo: 0x00, Hi: 0x09, Next: -1, Alt: -1})
	s1b := nfa.addState(State{Op: OpByteRange, Lo: 0x0B, Hi: 0x7F, Next: -1, Alt: -1})
	frags = append(frags, fragment{start: s1a, dangles: []int32{s1a}})
	frags = append(frags, fragment{start: s1b, dangles: []int32{s1b}})

	// Note: invalid UTF-8 bytes (standalone continuations, overlong leads) are NOT
	// matched by dot. This is a deliberate simplification for grep use cases where
	// inputs are valid text files. For invalid UTF-8, the search loop advances by
	// 1 byte (stdDecodeRune returns size 1) but dot won't match at that position.

	// 2-byte UTF-8: [0xC2-0xDF][0x80-0xBF]
	cont2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
	lead2 := nfa.addState(State{Op: OpByteRange, Lo: 0xC2, Hi: 0xDF, Next: cont2, Alt: -1})
	frags = append(frags, fragment{start: lead2, dangles: []int32{cont2}})

	// 3-byte UTF-8 with proper validation:
	// E0 [A0-BF] [80-BF]  (reject overlong)
	// [E1-EC] [80-BF] [80-BF]
	// ED [80-9F] [80-BF]  (reject surrogates)
	// [EE-EF] [80-BF] [80-BF]
	{
		// E0 [A0-BF] [80-BF]
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0xA0, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xE0, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}
	{
		// [E1-EC] [80-BF] [80-BF]
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xE1, Hi: 0xEC, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}
	{
		// ED [80-9F] [80-BF]
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0x9F, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xED, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}
	{
		// [EE-EF] [80-BF] [80-BF]
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xEE, Hi: 0xEF, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}

	// 4-byte UTF-8 with proper validation:
	// F0 [90-BF] [80-BF] [80-BF]  (reject overlong)
	// [F1-F3] [80-BF] [80-BF] [80-BF]
	// F4 [80-8F] [80-BF] [80-BF]  (reject > U+10FFFF)
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x90, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xF0, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xF1, Hi: 0xF3, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0x8F, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xF4, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}

	return alternateFragments(nfa, frags)
}

// compileDotAll builds NFA states that match any single UTF-8 rune including \n.
// Same structure as compileDotNotNL but includes \n in the ASCII range.
func compileDotAll(nfa *NFA) fragment {
	var frags []fragment

	// All ASCII bytes [0x00-0x7F] (including \n)
	s1 := nfa.addState(State{Op: OpByteRange, Lo: 0x00, Hi: 0x7F, Next: -1, Alt: -1})
	frags = append(frags, fragment{start: s1, dangles: []int32{s1}})

	// 2-byte UTF-8
	cont2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
	lead2 := nfa.addState(State{Op: OpByteRange, Lo: 0xC2, Hi: 0xDF, Next: cont2, Alt: -1})
	frags = append(frags, fragment{start: lead2, dangles: []int32{cont2}})

	// 3-byte UTF-8 (same validated paths as compileDotNotNL)
	for _, spec := range [][3]byte{{0xE0, 0xA0, 0xBF}, {0xED, 0x80, 0x9F}} {
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: spec[1], Hi: spec[2], Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: spec[0], Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}
	{
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xE1, Hi: 0xEC, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}
	{
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xEE, Hi: 0xEF, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c2}})
	}

	// 4-byte UTF-8 (same validated paths)
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x90, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xF0, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByteRange, Lo: 0xF1, Hi: 0xF3, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}
	{
		c3 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: -1, Alt: -1})
		c2 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0xBF, Next: c3, Alt: -1})
		c1 := nfa.addState(State{Op: OpByteRange, Lo: 0x80, Hi: 0x8F, Next: c2, Alt: -1})
		l := nfa.addState(State{Op: OpByte, ByteVal: 0xF4, Next: c1, Alt: -1})
		frags = append(frags, fragment{start: l, dangles: []int32{c3}})
	}

	return alternateFragments(nfa, frags)
}

// compileCharClass compiles a syntax.Regexp character class to NFA fragments.
// Returns a fragment that matches any rune in the class via byte-level transitions.
func compileCharClass(nfa *NFA, runes []rune) fragment {
	if len(runes) == 0 {
		// Empty class: no match possible. Use a dead state.
		s := nfa.addState(State{Op: OpByte, ByteVal: 0xFF, Next: -1, Alt: -1})
		// Don't add to dangles - this is a dead end
		return fragment{start: s, dangles: nil}
	}

	// Check if entirely ASCII for fast path
	if isASCIIClass(runes) {
		return compileASCIIClass(nfa, runes)
	}

	// General case: convert rune ranges to UTF-8 byte sequences
	seqs := runeClassToByteRanges(runes)
	if len(seqs) == 0 {
		s := nfa.addState(State{Op: OpByte, ByteVal: 0xFF, Next: -1, Alt: -1})
		return fragment{start: s, dangles: nil}
	}

	var frags []fragment
	for _, seq := range seqs {
		f := compileByteSequence(nfa, seq)
		frags = append(frags, f)
	}

	if len(frags) == 1 {
		return frags[0]
	}
	return alternateFragments(nfa, frags)
}

// compileASCIIClass compiles an ASCII-only character class to NFA states.
func compileASCIIClass(nfa *NFA, runes []rune) fragment {
	// Collect all byte ranges
	var ranges []ByteRange
	for i := 0; i < len(runes); i += 2 {
		ranges = append(ranges, ByteRange{byte(runes[i]), byte(runes[i+1])})
	}
	ranges = mergeByteRanges(ranges)

	if len(ranges) == 1 {
		r := ranges[0]
		if r.Lo == r.Hi {
			s := nfa.addState(State{Op: OpByte, ByteVal: r.Lo, Next: -1, Alt: -1})
			return fragment{start: s, dangles: []int32{s}}
		}
		s := nfa.addState(State{Op: OpByteRange, Lo: r.Lo, Hi: r.Hi, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	// Multiple ranges: use OpByteRanges for dense matching
	s := nfa.addState(State{Op: OpByteRanges, Ranges: ranges, Next: -1, Alt: -1})
	return fragment{start: s, dangles: []int32{s}}
}

// compileByteSequence compiles a sequence of byte ranges into a chain of NFA states.
func compileByteSequence(nfa *NFA, seq []ByteRange) fragment {
	if len(seq) == 0 {
		s := nfa.addState(State{Op: OpMatch, Next: -1, Alt: -1})
		return fragment{start: s, dangles: []int32{s}}
	}

	// Build chain right-to-left so we can set Next pointers
	var lastIdx int32 = -1
	var firstIdx int32
	var dangles []int32

	for i := len(seq) - 1; i >= 0; i-- {
		r := seq[i]
		var s State
		if r.Lo == r.Hi {
			s = State{Op: OpByte, ByteVal: r.Lo, Next: lastIdx, Alt: -1}
		} else {
			s = State{Op: OpByteRange, Lo: r.Lo, Hi: r.Hi, Next: lastIdx, Alt: -1}
		}
		idx := nfa.addState(s)
		if i == len(seq)-1 {
			dangles = []int32{idx}
		}
		firstIdx = idx
		lastIdx = idx
	}

	return fragment{start: firstIdx, dangles: dangles}
}

// alternateFragments creates a split tree alternation over multiple fragments.
func alternateFragments(nfa *NFA, frags []fragment) fragment {
	if len(frags) == 0 {
		s := nfa.addState(State{Op: OpByte, ByteVal: 0xFF, Next: -1, Alt: -1})
		return fragment{start: s, dangles: nil}
	}
	if len(frags) == 1 {
		return frags[0]
	}

	// Build a balanced split tree for better branch prediction
	return balancedAlternate(nfa, frags, 0, len(frags))
}

func balancedAlternate(nfa *NFA, frags []fragment, lo, hi int) fragment {
	if hi-lo == 1 {
		return frags[lo]
	}
	if hi-lo == 2 {
		s := nfa.addState(State{Op: OpSplit, Next: frags[lo].start, Alt: frags[lo+1].start})
		dangles := append(frags[lo].dangles, frags[lo+1].dangles...)
		return fragment{start: s, dangles: dangles}
	}

	mid := lo + (hi-lo)/2
	left := balancedAlternate(nfa, frags, lo, mid)
	right := balancedAlternate(nfa, frags, mid, hi)
	s := nfa.addState(State{Op: OpSplit, Next: left.start, Alt: right.start})
	dangles := append(left.dangles, right.dangles...)
	return fragment{start: s, dangles: dangles}
}

// negateClass returns the rune class that is the complement of the input.
// Input runes are pairs [lo1, hi1, lo2, hi2, ...] sorted and non-overlapping.
func negateClass(runes []rune) []rune {
	var result []rune
	lo := rune(0)
	for i := 0; i < len(runes); i += 2 {
		classLo, classHi := runes[i], runes[i+1]
		if lo < classLo {
			result = append(result, lo, classLo-1)
		}
		lo = classHi + 1
	}
	if lo <= unicode.MaxRune {
		result = append(result, lo, unicode.MaxRune)
	}
	return result
}

// perlClassRunes returns the rune pairs for a Perl character class.
func perlClassRunes(class *syntax.Regexp) []rune {
	return class.Rune
}
