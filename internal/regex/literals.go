package regex

import "regexp/syntax"

// RequiredLiterals returns ASCII literals (>= 3 bytes) that every match
// of pattern must contain — safe to AND together as an index plan.
// Nil means no usable literal could be extracted and the caller must
// treat the pattern as unconstrained.
func RequiredLiterals(pattern string, caseInsensitive bool) [][]byte {
	flags := syntax.Perl
	if caseInsensitive {
		flags |= syntax.FoldCase
	}
	pf := extractPrefilter(pattern, flags)
	if pf == nil || pf.hasRareByte {
		return nil
	}
	lits := [][]byte{pf.primary}
	lits = append(lits, pf.extras...)
	return lits
}
