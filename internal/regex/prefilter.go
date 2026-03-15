package regex

// SIMD literal prefilter integration.
// Extracts required literals from the regex AST and uses SIMD to scan
// for candidates before engaging the DFA/PikeVM.

import (
	"regexp/syntax"
	"strings"
	"unicode"
)

const minLiteralLen = 3

// prefilter holds extracted literal information for SIMD acceleration.
type prefilter struct {
	// primary is the best literal for SIMD scanning (longest, most selective)
	primary    []byte
	primaryCI  bool // case-insensitive

	// extras are additional required literals verified after primary hits
	extras   [][]byte
	extrasCI []bool

	// rareByte is a single required byte extracted from the pattern when no
	// multi-byte literal is available. Used as a memchr prefilter: SIMD scan
	// for this byte, then DFA verify at each hit. Effective for patterns like
	// \d{4}-\d{2}-\d{2} (rareByte='-') or [a-zA-Z]+@... (rareByte='@').
	rareByte    byte
	hasRareByte bool
}

// extractPrefilter analyzes a regex AST and extracts required literals.
func extractPrefilter(pattern string, flags syntax.Flags) *prefilter {
	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		return nil
	}
	re = re.Simplify()

	// Don't extract if pattern uses (?s) — matches can span lines
	if hasDotNLFlag(re) {
		return nil
	}

	ci := flags&syntax.FoldCase != 0
	candidates := extractLiteralsFromAST(re)
	if len(candidates) == 0 {
		return nil
	}

	// Filter: must be ASCII, >= minLiteralLen
	var valid []litCandidate
	for _, c := range candidates {
		if len(c.runes) < minLiteralLen {
			continue
		}
		if !allASCII(c.runes) {
			continue
		}
		valid = append(valid, c)
	}

	if len(valid) == 0 {
		// No multi-byte literals. Try extracting a rare single byte.
		if rb, ok := extractRareByte(re); ok {
			return &prefilter{hasRareByte: true, rareByte: rb}
		}
		return nil
	}

	pf := &prefilter{}

	// First literal is primary (source order for cascaded checking)
	lit := string(valid[0].runes)
	isCI := valid[0].foldCase || ci
	if isCI {
		lit = strings.ToLower(lit)
	}
	pf.primary = []byte(lit)
	pf.primaryCI = isCI

	// Remaining literals >= 4 bytes are extras
	for _, c := range valid[1:] {
		if len(c.runes) < 4 {
			continue
		}
		l := string(c.runes)
		lci := c.foldCase || ci
		if lci {
			l = strings.ToLower(l)
		}
		pf.extras = append(pf.extras, []byte(l))
		pf.extrasCI = append(pf.extrasCI, lci)
	}

	return pf
}

type litCandidate struct {
	runes    []rune
	foldCase bool
}

func extractLiteralsFromAST(re *syntax.Regexp) []litCandidate {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return nil
		}
		return []litCandidate{{
			runes:    re.Rune,
			foldCase: re.Flags&syntax.FoldCase != 0,
		}}

	case syntax.OpConcat:
		return extractFromConcatAST(re.Sub)

	case syntax.OpCapture:
		if len(re.Sub) > 0 {
			return extractLiteralsFromAST(re.Sub[0])
		}
		return nil

	case syntax.OpPlus:
		if len(re.Sub) > 0 {
			return extractLiteralsFromAST(re.Sub[0])
		}
		return nil

	case syntax.OpRepeat:
		if re.Min >= 1 && len(re.Sub) > 0 {
			return extractLiteralsFromAST(re.Sub[0])
		}
		return nil

	case syntax.OpStar, syntax.OpQuest, syntax.OpAlternate:
		return nil

	default:
		return nil
	}
}

func extractFromConcatAST(subs []*syntax.Regexp) []litCandidate {
	var results []litCandidate

	var currentRunes []rune
	var currentFold bool
	flush := func() {
		if len(currentRunes) > 0 {
			results = append(results, litCandidate{
				runes:    currentRunes,
				foldCase: currentFold,
			})
			currentRunes = nil
		}
	}

	for _, sub := range subs {
		if sub.Op == syntax.OpLiteral && len(sub.Rune) > 0 {
			fc := sub.Flags&syntax.FoldCase != 0
			if len(currentRunes) > 0 && fc != currentFold {
				flush()
			}
			currentFold = fc
			currentRunes = append(currentRunes, sub.Rune...)
		} else {
			flush()
			results = append(results, extractLiteralsFromAST(sub)...)
		}
	}
	flush()

	return results
}

func hasDotNLFlag(re *syntax.Regexp) bool {
	if re.Op == syntax.OpAnyChar {
		return true
	}
	for _, sub := range re.Sub {
		if hasDotNLFlag(sub) {
			return true
		}
	}
	return false
}

func allASCII(runes []rune) bool {
	for _, r := range runes {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

// byteRarity scores how rare a byte is in typical English text.
// Lower score = rarer = better prefilter candidate.
// Punctuation and special chars are rare; letters and digits are common.
var byteRarity = func() [256]byte {
	var r [256]byte
	for i := range r {
		r[i] = 50 // default: moderately rare
	}
	// Very common: lowercase letters, space, newline
	for c := byte('a'); c <= 'z'; c++ {
		r[c] = 200
	}
	r[' '] = 250
	r['\n'] = 240
	r['\t'] = 230
	// Common: uppercase letters, digits
	for c := byte('A'); c <= 'Z'; c++ {
		r[c] = 150
	}
	for c := byte('0'); c <= '9'; c++ {
		r[c] = 160
	}
	// Rare: punctuation and special characters
	for _, c := range []byte("@#$%^&*~`|\\<>{}[]") {
		r[c] = 10
	}
	for _, c := range []byte("!?;:") {
		r[c] = 20
	}
	for _, c := range []byte("+-=_") {
		r[c] = 30
	}
	for _, c := range []byte(".,/()\"'") {
		r[c] = 60
	}
	return r
}()

// extractRareByte finds the rarest required literal byte in the pattern.
// Walks the AST looking for single-byte literals in required positions
// (concat children that aren't optional).
func extractRareByte(re *syntax.Regexp) (byte, bool) {
	var candidates []byte
	collectRequiredBytes(re, &candidates)

	if len(candidates) == 0 {
		return 0, false
	}

	// Pick the rarest byte
	best := candidates[0]
	bestScore := byteRarity[best]
	for _, c := range candidates[1:] {
		if byteRarity[c] < bestScore {
			best = c
			bestScore = byteRarity[c]
		}
	}

	// Only use if reasonably rare (not a letter/digit/space)
	if bestScore > 100 {
		return 0, false
	}

	return best, true
}

// collectRequiredBytes collects all literal bytes from required positions in the AST.
func collectRequiredBytes(re *syntax.Regexp, out *[]byte) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if r < 128 && re.Flags&syntax.FoldCase == 0 {
				*out = append(*out, byte(r))
			}
		}

	case syntax.OpConcat:
		for _, sub := range re.Sub {
			collectRequiredBytes(sub, out)
		}

	case syntax.OpCapture:
		if len(re.Sub) > 0 {
			collectRequiredBytes(re.Sub[0], out)
		}

	case syntax.OpPlus:
		if len(re.Sub) > 0 {
			collectRequiredBytes(re.Sub[0], out)
		}

	case syntax.OpRepeat:
		if re.Min >= 1 && len(re.Sub) > 0 {
			collectRequiredBytes(re.Sub[0], out)
		}

	// OpStar, OpQuest, OpAlternate: not required, skip
	}
}
