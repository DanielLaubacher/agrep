package matcher

import (
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"
)

const minPrefilterLen = 3

// literalInfo holds a literal substring extracted from a regex AST that is
// guaranteed to appear in any match of the regex.
type literalInfo struct {
	literal    string
	ignoreCase bool
}

// extractLiteral parses a regex pattern and extracts the longest required
// literal substring that must appear in any match. Returns the literal info
// and true if a usable literal was found (length >= minPrefilterLen).
func extractLiteral(pattern string, ignoreCase bool) (literalInfo, bool) {
	lits := extractLiterals(pattern, ignoreCase)
	if len(lits) == 0 {
		return literalInfo{}, false
	}
	// Pick longest
	best := lits[0]
	for _, l := range lits[1:] {
		if len(l.literal) > len(best.literal) {
			best = l
		}
	}
	return best, true
}

// extractLiterals parses a regex pattern and extracts all required literal
// substrings that must appear in any match. Returns them in source order.
// Each literal has length >= minPrefilterLen and is all-ASCII.
func extractLiterals(pattern string, ignoreCase bool) []literalInfo {
	flags := syntax.Perl
	if ignoreCase {
		flags |= syntax.FoldCase
	}

	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		return nil
	}
	re = re.Simplify()

	// Safety: if any node uses DotNL ((?s) flag), matches can span lines.
	if hasDotNL(re) {
		return nil
	}

	candidates := extractFromNode(re)
	if len(candidates) == 0 {
		return nil
	}

	// Convert all qualifying candidates to literalInfo, preserving source order.
	var result []literalInfo
	for _, c := range candidates {
		if len(c.runes) < minPrefilterLen || !isASCIIRunes(c.runes) {
			continue
		}
		ci := c.foldCase || ignoreCase
		lit := string(c.runes)
		if ci {
			lit = strings.ToLower(lit)
		}
		result = append(result, literalInfo{literal: lit, ignoreCase: ci})
	}

	return result
}

// candidate is a literal substring found in the regex AST.
type candidate struct {
	runes    []rune
	foldCase bool
}

// extractFromNode walks the AST and returns all required literal substrings.
func extractFromNode(re *syntax.Regexp) []candidate {
	switch re.Op {
	case syntax.OpLiteral:
		if len(re.Rune) == 0 {
			return nil
		}
		return []candidate{{
			runes:    re.Rune,
			foldCase: re.Flags&syntax.FoldCase != 0,
		}}

	case syntax.OpConcat:
		return extractFromConcat(re.Sub)

	case syntax.OpCapture:
		if len(re.Sub) > 0 {
			return extractFromNode(re.Sub[0])
		}
		return nil

	case syntax.OpPlus:
		// Must match at least once — child is required.
		if len(re.Sub) > 0 {
			return extractFromNode(re.Sub[0])
		}
		return nil

	case syntax.OpRepeat:
		// Required only if Min >= 1.
		if re.Min >= 1 && len(re.Sub) > 0 {
			return extractFromNode(re.Sub[0])
		}
		return nil

	case syntax.OpStar, syntax.OpQuest:
		// Zero occurrences is valid — child is NOT required.
		return nil

	case syntax.OpAlternate:
		// None of the branches is individually required.
		return nil

	default:
		// OpCharClass, OpAnyChar, OpAnyCharNotNL, anchors, etc.
		return nil
	}
}

// extractFromConcat handles OpConcat by collecting candidates from children
// and merging adjacent OpLiteral nodes into longer candidates.
func extractFromConcat(subs []*syntax.Regexp) []candidate {
	var results []candidate

	// First pass: merge adjacent OpLiteral children.
	var currentRunes []rune
	var currentFold bool
	flushMerged := func() {
		if len(currentRunes) > 0 {
			results = append(results, candidate{
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
				// FoldCase flag changed — flush and start new run.
				flushMerged()
			}
			currentFold = fc
			currentRunes = append(currentRunes, sub.Rune...)
		} else {
			flushMerged()
			// Recurse into non-literal required children.
			results = append(results, extractFromNode(sub)...)
		}
	}
	flushMerged()

	return results
}

// hasDotNL returns true if any node in the tree has the DotNL flag set,
// meaning the pattern uses (?s) and matches can span lines.
func hasDotNL(re *syntax.Regexp) bool {
	if re.Op == syntax.OpAnyChar {
		// OpAnyChar means dot-matches-newline is active.
		return true
	}
	return slices.ContainsFunc(re.Sub, hasDotNL)
}

// isASCIIRunes returns true if all runes are ASCII.
func isASCIIRunes(runes []rune) bool {
	for _, r := range runes {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
