package cli

// --ident: identifier-aware matching. The pattern is treated as an
// identifier name; it matches every case convention of that identifier
// (camelCase, PascalCase, snake_case, kebab-case, SCREAMING_SNAKE, flat)
// with word boundaries, so searching `Match` does not hit `MatchSet`.
// This is the proactive twin of --suggest: instead of proposing variants
// after a zero hit, match them all up front.

import (
	"regexp"
	"strings"
	"unicode"
)

// splitIdentWords splits an identifier into its lowercase word parts on
// case boundaries and non-alphanumerics: ConnectTimeout, connect_timeout
// and connect-timeout all yield [connect, timeout]. minLen filters short
// fragments (0 keeps everything).
func splitIdentWords(pattern string, minLen int) []string {
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= minLen && cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
		}
		cur.Reset()
	}
	runes := []rune(pattern)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) &&
			(unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			flush()
		}
		cur.WriteRune(r)
	}
	flush()
	return words
}

// identRegex builds the case-convention regex for an identifier: word
// parts joined by optional _ or - separators, case-insensitive, bounded
// by \b so substrings of longer identifiers do not match. Returns "" if
// the pattern contains no identifier words.
func identRegex(pattern string) string {
	words := splitIdentWords(pattern, 0)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`(?i)\b`)
	for i, w := range words {
		if i > 0 {
			b.WriteString(`[_-]?`)
		}
		b.WriteString(regexp.QuoteMeta(w))
	}
	b.WriteString(`\b`)
	return b.String()
}

// applyIdent rewrites every pipeline stage pattern as its identifier
// regex. Fixed/PCRE stage flags are cleared: the rewrite is a regex and
// the engine factory routes it. Patterns with no identifier words are
// left untouched.
func applyIdent(cfg *Config) {
	for pi := range cfg.Pipelines {
		for si := range cfg.Pipelines[pi] {
			s := &cfg.Pipelines[pi][si]
			if re := identRegex(s.Pattern); re != "" {
				s.Pattern = re
				s.Fixed = false
				s.PCRE = false
			}
		}
	}
}
