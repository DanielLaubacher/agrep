package matcher

import (
	"fmt"
	"strings"
)

// MatcherOpts holds display-related options that affect match extraction.
type MatcherOpts struct {
	MaxCols      int  // max columns for snippet extraction (0 = full lines)
	NeedLineNums bool // compute line numbers (false = skip for speed)
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

	if usePCRE {
		// Combine multiple patterns with |
		pattern := patterns[0]
		if len(patterns) > 1 {
			var combined strings.Builder
			for i, p := range patterns {
				if i > 0 {
					combined.WriteString("|")
				}
				combined.WriteString("(?:" + p + ")")
			}
			pattern = combined.String()
		}
		m, err := NewPCREMatcher(pattern, ignoreCase, invert)
		if err != nil {
			return nil, err
		}
		m.maxCols = opts.MaxCols
		m.needLineNums = opts.NeedLineNums
		return m, nil
	}

	if fixed {
		if len(patterns) == 1 {
			m := NewBoyerMooreMatcher(patterns[0], ignoreCase, invert)
			m.maxCols = opts.MaxCols
			m.needLineNums = opts.NeedLineNums
			return m, nil
		}
		m := NewAhoCorasickMatcher(patterns, ignoreCase, invert)
		m.maxCols = opts.MaxCols
		m.needLineNums = opts.NeedLineNums
		return m, nil
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
			m.maxCols = opts.MaxCols
			m.needLineNums = opts.NeedLineNums
			return m, nil
		}
		m := NewAhoCorasickMatcher(patterns, ignoreCase, invert)
		m.maxCols = opts.MaxCols
		m.needLineNums = opts.NeedLineNums
		return m, nil
	}

	// Regex mode: combine multiple patterns with |
	pattern := patterns[0]
	if len(patterns) > 1 {
		var combined strings.Builder
		for i, p := range patterns {
			if i > 0 {
				combined.WriteString("|")
			}
			combined.WriteString("(?:" + p + ")")
		}
		pattern = combined.String()
	}

	m, err := NewRegexMatcher(pattern, ignoreCase, invert)
	if err != nil {
		return nil, err
	}
	m.maxCols = opts.MaxCols
	m.needLineNums = opts.NeedLineNums
	return m, nil
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

		// Stage 0 in a multi-stage pipeline must not truncate lines (MaxCols=0)
		// because subsequent stages need the full line content to search.
		// Only a single-stage pipeline (handled by the fast path above) should truncate.
		// All non-first stages also use relaxed opts.
		stageOpts := opts
		if len(stages) > 1 {
			if i == 0 {
				stageOpts.MaxCols = 0 // full lines for pipeline filtering
			} else {
				stageOpts = MatcherOpts{MaxCols: 0, NeedLineNums: false}
			}
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

// isLiteral returns true if the pattern contains no regex metacharacters
// and can be treated as a fixed string.
func isLiteral(pattern string) bool {
	return !strings.ContainsAny(pattern, `\.+*?()|[]{}^$`)
}
