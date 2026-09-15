package matcher

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"

	"github.com/DanielLaubacher/agrep/internal/lang"
)

// MatcherOpts holds display-related options that affect match extraction.
type MatcherOpts struct {
	NeedLineNums bool      // compute line numbers (false = skip for speed)
	Multiline    bool      // -U: patterns may match across line boundaries
	Structural   bool      // -S: pattern is a structural template with :[name] holes
	Lang         lang.Lang // --lang: language family for -S string/comment atoms
}

// StageConfig describes one stage in a match pipeline.
type StageConfig struct {
	Pattern   string
	Fixed     bool
	PCRE      bool
	OnlyMatch bool
}

// NewMatcher creates the appropriate Matcher based on the provided options.
// Selection logic:
//   - PCRE flag -> PCREMatcher (PCRE2 via pure Go port)
//   - Fixed + 1 pattern -> BoyerMooreMatcher (sublinear search)
//   - Fixed + N patterns -> AhoCorasickMatcher (single-pass multi-pattern)
//   - Otherwise -> RegexMatcher (RE2)
func NewMatcher(patterns []string, fixed bool, usePCRE bool, ignoreCase bool, invert bool, opts MatcherOpts) (Matcher, error) {
	if len(patterns) == 0 {
		return nil, fmt.Errorf("no patterns provided")
	}

	// -S routes to the structural-template matcher. Config validation
	// rejects unsupported combinations before we get here.
	if opts.Structural {
		if usePCRE || invert || fixed || len(patterns) != 1 {
			return nil, fmt.Errorf("--structural takes a single template (no -F/-P/-v)")
		}
		return NewStructuralMatcher(patterns[0], opts.Lang, opts)
	}

	// -U routes to the dedicated cross-line matcher (RE2 on the whole
	// buffer). Config validation rejects the unsupported combinations
	// (-P, -v, -t pipelines, --watch) before we get here.
	if opts.Multiline {
		if usePCRE || invert {
			return nil, fmt.Errorf("-U cannot combine with -P or -v")
		}
		return NewMultilineMatcher(patterns, fixed, ignoreCase, opts)
	}

	if usePCRE {
		m, err := NewPCREMatcher(combinePatterns(patterns), ignoreCase, invert)
		if err != nil {
			return nil, err
		}
		m.needLineNums = opts.NeedLineNums
		return m, nil
	}

	// -i on a non-ASCII pattern needs full Unicode case folding — the
	// SIMD literal engines and the lazy-DFA engine fold ASCII only, so
	// 'müller' would silently miss 'MÜLLER'. Route such patterns to the
	// stdlib regex engine (quoting them first when -F promised literal
	// semantics); its prefilters are ASCII-gated, so they stay safe.
	if ignoreCase && !allASCII(patterns) {
		if fixed {
			quoted := make([]string, len(patterns))
			for i, p := range patterns {
				quoted[i] = regexp.QuoteMeta(p)
			}
			patterns = quoted
		}
		m, err := NewRegexMatcher(combinePatterns(patterns), ignoreCase, invert)
		if err != nil {
			return nil, err
		}
		m.needLineNums = opts.NeedLineNums
		return m, nil
	}

	if fixed {
		if len(patterns) == 1 {
			m := NewBoyerMooreMatcher(patterns[0], ignoreCase, invert)
			m.needLineNums = opts.NeedLineNums
			return m, nil
		}
		return newMultiLiteralMatcher(patterns, ignoreCase, invert, opts), nil
	}

	// Optimization: if all patterns are literal strings (no regex metacharacters),
	// use BoyerMooreMatcher / AhoCorasickMatcher for SIMD-accelerated search.
	allLiteral := true
	for _, p := range patterns {
		if !isLiteral(p) {
			allLiteral = false
			break
		}
	}
	if allLiteral {
		if len(patterns) == 1 {
			m := NewBoyerMooreMatcher(patterns[0], ignoreCase, invert)
			m.needLineNums = opts.NeedLineNums
			return m, nil
		}
		return newMultiLiteralMatcher(patterns, ignoreCase, invert, opts), nil
	}

	// Regex mode: combine multiple patterns with |
	pattern := combinePatterns(patterns)

	// Optimization: detect alternation-of-literals in a single regex pattern
	// (e.g., "ERROR|INFO|function") and route to Aho-Corasick for SIMD search.
	// This is what ripgrep does with its Teddy multi-pattern engine.
	if alts := extractAlternationLiterals(pattern); len(alts) > 0 {
		if len(alts) == 1 {
			m := NewBoyerMooreMatcher(alts[0], ignoreCase, invert)
			m.needLineNums = opts.NeedLineNums
			return m, nil
		}
		return newMultiLiteralMatcher(alts, ignoreCase, invert, opts), nil
	}

	return newRegexPathMatcher(pattern, ignoreCase, invert, opts)
}

// newRegexPathMatcher builds the regex engine chain: the lazy-DFA
// FastRegexMatcher, falling back to the stdlib RegexMatcher for
// constructs it can't handle.
func newRegexPathMatcher(pattern string, ignoreCase bool, invert bool, opts MatcherOpts) (Matcher, error) {
	fm, err := NewFastRegexMatcher(pattern, ignoreCase, invert)
	if err != nil {
		m, err2 := NewRegexMatcher(pattern, ignoreCase, invert)
		if err2 != nil {
			return nil, err2
		}
		m.needLineNums = opts.NeedLineNums
		return m, nil
	}
	fm.needLineNums = opts.NeedLineNums
	return fm, nil
}

// combinePatterns OR-joins patterns into one regex, each in a
// non-capturing group.
func combinePatterns(patterns []string) string {
	if len(patterns) == 1 {
		return patterns[0]
	}
	var combined strings.Builder
	for i, p := range patterns {
		if i > 0 {
			combined.WriteString("|")
		}
		combined.WriteString("(?:" + p + ")")
	}
	return combined.String()
}

// allASCII reports whether every pattern is pure ASCII (the SIMD literal
// engines can only case-fold ASCII).
func allASCII(patterns []string) bool {
	for _, p := range patterns {
		for i := 0; i < len(p); i++ {
			if p[i] >= 0x80 {
				return false
			}
		}
	}
	return true
}

// NewMatcherFromPipelines creates a Matcher from pipeline stage configurations.
// Each pipeline is a chain of stages (AND via match narrowing).
// Multiple pipelines are OR'd together.
func NewMatcherFromPipelines(pipelines [][]StageConfig, ignoreCase bool, invert bool, opts MatcherOpts) (Matcher, error) {
	if len(pipelines) == 0 {
		return nil, fmt.Errorf("no pipelines provided")
	}

	// Fast path: single pipeline, single stage, no -o → use legacy NewMatcher
	if len(pipelines) == 1 && len(pipelines[0]) == 1 && !pipelines[0][0].OnlyMatch {
		s := pipelines[0][0]
		return NewMatcher([]string{s.Pattern}, s.Fixed, s.PCRE, ignoreCase, invert, opts)
	}

	// Fast path: N single-stage pipelines with identical engine flags and no
	// -o are plain OR'd patterns (`-e a -e b -e c`) — run them as ONE
	// multi-pattern matcher (Teddy/Aho-Corasick for fixed sets) instead of
	// N separate full scans OR'd afterwards. This is also required for
	// correct -v semantics: invert must apply to the OR of the patterns,
	// not per pattern.
	// (-F is per-stage and resets after each -e, so require each pattern to
	// be either explicitly fixed or literal — then all are fixed strings.)
	if len(pipelines) > 1 {
		combinable := true
		for _, pl := range pipelines {
			if len(pl) != 1 || pl[0].OnlyMatch || pl[0].PCRE ||
				(!pl[0].Fixed && !isLiteral(pl[0].Pattern)) {
				combinable = false
				break
			}
		}
		if combinable {
			patterns := make([]string, len(pipelines))
			for i, pl := range pipelines {
				patterns[i] = pl[0].Pattern
			}
			return NewMatcher(patterns, true, false, ignoreCase, invert, opts)
		}
	}

	var pipelineMatchers []*PipelineMatcher
	for _, pipeline := range pipelines {
		pm, err := buildPipeline(pipeline, ignoreCase, invert, opts)
		if err != nil {
			return nil, err
		}
		pipelineMatchers = append(pipelineMatchers, pm)
	}

	if len(pipelineMatchers) == 1 {
		return pipelineMatchers[0], nil
	}
	return NewMultiPipelineMatcher(pipelineMatchers), nil
}

// buildPipeline creates a PipelineMatcher from a single pipeline's stages.
func buildPipeline(stages []StageConfig, ignoreCase bool, invert bool, opts MatcherOpts) (*PipelineMatcher, error) {
	if len(stages) == 0 {
		return nil, fmt.Errorf("empty pipeline")
	}

	matchers := make([]Matcher, len(stages))
	for i, s := range stages {
		// Only the first stage uses invert; intermediate stages don't invert
		stageInvert := false
		if i == 0 {
			stageInvert = invert
		}

		// Non-first stages act as filters over stage 0's lines; they never
		// drive output, so they skip line-number bookkeeping.
		stageOpts := opts
		if len(stages) > 1 && i > 0 {
			stageOpts = MatcherOpts{NeedLineNums: false}
		}

		m, err := NewMatcher([]string{s.Pattern}, s.Fixed, s.PCRE, ignoreCase, stageInvert, stageOpts)
		if err != nil {
			return nil, fmt.Errorf("stage %d: %w", i, err)
		}
		matchers[i] = m
	}

	// The final stage's -o flag determines output mode
	onlyMatch := stages[len(stages)-1].OnlyMatch
	return NewPipelineMatcher(matchers, onlyMatch), nil
}

// newMultiLiteralMatcher picks the best engine for a set of fixed patterns:
// rare-pair Teddy (SIMD, 2-8 patterns) when applicable, Aho-Corasick
// otherwise.
func newMultiLiteralMatcher(patterns []string, ignoreCase bool, invert bool, opts MatcherOpts) Matcher {
	if tm := NewTeddyMatcher(patterns, ignoreCase, invert); tm != nil {
		tm.needLineNums = opts.NeedLineNums
		return tm
	}
	m := NewAhoCorasickMatcher(patterns, ignoreCase, invert)
	m.needLineNums = opts.NeedLineNums
	return m
}

// isLiteral returns true if the pattern contains no regex metacharacters
// and can be treated as a fixed string.
func isLiteral(pattern string) bool {
	return !strings.ContainsAny(pattern, `\.+*?()|[]{}^$`)
}

// extractAlternationLiterals parses a regex and returns the literal strings
// if the pattern is a pure alternation of literals (e.g., "foo|bar|baz").
// Returns nil if the pattern has any non-literal branches or is not
// a simple alternation.
func extractAlternationLiterals(pattern string) []string {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	re = re.Simplify()

	switch re.Op {
	case syntax.OpLiteral:
		// Single literal: return it
		return []string{string(re.Rune)}

	case syntax.OpAlternate:
		// Check if every branch is a literal
		lits := make([]string, 0, len(re.Sub))
		for _, sub := range re.Sub {
			s := extractLiteralFromNode(sub)
			if s == "" {
				return nil
			}
			lits = append(lits, s)
		}
		return lits

	case syntax.OpCapture:
		// Unwrap capture group: (foo|bar|baz) → foo|bar|baz
		if len(re.Sub) == 1 {
			return extractAlternationLiterals(string(re.Sub[0].String()))
		}
		return nil

	default:
		return nil
	}
}

// extractLiteralFromNode returns the literal string from an AST node,
// or "" if the node is not a pure literal (possibly wrapped in a capture).
func extractLiteralFromNode(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		if re.Flags&syntax.FoldCase != 0 {
			return "" // case-folded literals need special handling
		}
		return string(re.Rune)
	case syntax.OpCapture:
		if len(re.Sub) == 1 {
			return extractLiteralFromNode(re.Sub[0])
		}
		return ""
	default:
		return ""
	}
}
