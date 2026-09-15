package cli

import (
	"fmt"
	"regexp"
)

// ColorMode controls when colored output is used.
type ColorMode int

const (
	ColorAuto   ColorMode = iota // color when stdout is a terminal
	ColorAlways                  // always use color
	ColorNever                   // never use color
)

// StageConfig holds per-pattern configuration for a pipeline stage.
type StageConfig struct {
	Pattern   string
	Fixed     bool // -F for this stage
	PCRE      bool // -P for this stage
	Pipe      bool // -t: receive matched text from previous stage
	OnlyMatch bool // -o: output only matched portion
}

// Config holds all configuration for a agrep search.
type Config struct {
	// Pipelines holds one or more match pipelines, OR'd together.
	// Each pipeline is a chain of stages; -t chains stages, bare -e starts a new pipeline.
	// Legacy fields (Patterns, Fixed, PCRE) are still populated for backwards compatibility
	// and converted to Pipelines in Validate().
	Pipelines [][]StageConfig

	// Legacy pattern fields — used when Pipelines is nil (backwards compat).
	Patterns []string
	Fixed    bool
	PCRE     bool

	IgnoreCase    bool
	Recursive     bool
	LineNumbers   bool
	CountOnly     bool
	Invert        bool
	FileNamesOnly bool
	OnlyMatch     bool // global -o flag (when no pipelines)
	ContextBefore int
	ContextAfter  int
	WatchMode     bool
	JSONOutput    bool
	Compact       bool // --compact: lean JSON match records (no span/byte_offset/matches)
	// CaseFromConfig marks that the effective -S/-i came from the config
	// file with no command-line case flag — worth a stderr note when it
	// changes counts versus grep.
	CaseFromConfig bool
	ConfigPath     string // config file that supplied injected flags ("" if none)
	Color          ColorMode
	Workers        int
	NoIgnore       bool
	Hidden         bool
	FollowSymlinks bool
	SmartCase      bool
	Globs          []string
	MaxColumns     int
	MmapThreshold  int64
	Paths          []string

	// Agent-oriented options (see agent-mode.md).
	Ident        bool     // --ident: match all case conventions of an identifier, word-bounded
	MaxTokens    int      // --max-tokens: output token budget (0 = unlimited)
	Outline      bool     // --outline: per-file survey instead of match lines
	Histogram    bool     // --histogram: distinct matched texts with counts
	Rank         string   // --rank: outline order — "count" (default), "density", or "defs"
	Collapse     bool     // --collapse: suppress repeats of identical match lines
	Multiline    bool     // -U/--multiline: patterns may match across lines
	WordRegexp   bool     // -w/--word-regexp: match only whole words
	Structural   bool     // -S/--structural: pattern is a template with :[name] holes
	Lang         string   // --lang: language family for -S (default: generic)
	Capture      string   // --capture: hole name for --histogram aggregation
	Block        bool     // --block: emit the whole enclosing definition block
	TopK         int      // --top: limit outline/histogram to the K busiest entries
	Sections     bool     // --sections: annotate matches with Markdown headings
	Scope        bool     // --scope: annotate matches with the enclosing definition
	BatchFile    string   // --batch: file of patterns, one per line
	FilesFrom    string   // --files-from: file of paths to search ('-' = stdin)
	ChangedSince string   // --changed-since REF: only files changed since the git ref
	WithFile     []string // --with-file: file must also contain each of these
	WithoutFile  []string // --without-file: file must not contain any of these
	Suggest      bool     // --suggest: on zero hits, probe derived variants
	GetRegion    string   // --get-region: print bytes for a "path@start-end" span
	ExpandLines  int      // --expand N: widen --get-region by N lines each side

	// Index options (see index.md).
	UseIndex   bool   // --use-index: build/use the trigram index for recursive search
	ClearIndex string // --clear-index PATH: delete index state at/under PATH
}

// NormalizePipelines converts legacy Patterns/Fixed/PCRE fields into the
// Pipelines representation if Pipelines is not already set.
func (c *Config) NormalizePipelines() {
	if len(c.Pipelines) > 0 {
		return
	}
	if len(c.Patterns) == 0 {
		return
	}
	// Legacy mode: all patterns form a single pipeline with one stage each (OR'd).
	// Multiple patterns with same Fixed/PCRE → single pipeline with one stage
	// containing all patterns (handled by matcher factory as before).
	pipeline := []StageConfig{{
		Pattern:   c.Patterns[0],
		Fixed:     c.Fixed,
		PCRE:      c.PCRE,
		OnlyMatch: c.OnlyMatch,
	}}
	c.Pipelines = [][]StageConfig{pipeline}

	// If multiple patterns, they are OR'd — but the matcher factory already
	// handles that via combining with |. We store them all in the first stage
	// and let the factory deal with it.
	if len(c.Patterns) > 1 {
		// Store extra patterns for the factory to combine
		c.Pipelines[0][0].Pattern = c.Patterns[0]
		// We need all patterns accessible — keep Patterns populated for the factory
	}
}

// Validate checks that the config is valid and returns an error if not.
func (c *Config) Validate() error {
	if c.GetRegion != "" || c.ClearIndex != "" {
		return nil // --get-region / --clear-index need no pattern
	}
	if len(c.Patterns) == 0 && len(c.Pipelines) == 0 && c.BatchFile == "" {
		return fmt.Errorf("no pattern specified")
	}
	if c.BatchFile != "" {
		return nil // batch patterns are loaded and validated at run time
	}

	// Validate pipelines if set
	for i, pipeline := range c.Pipelines {
		if len(pipeline) == 0 {
			return fmt.Errorf("empty pipeline %d", i)
		}
		if pipeline[0].Pipe {
			return fmt.Errorf("-t on first pattern has nothing to pipe from")
		}
		for j, stage := range pipeline {
			if stage.Fixed && stage.PCRE {
				return fmt.Errorf("cannot use -F and -P together on stage %d of pipeline %d", j, i)
			}
		}
	}

	// Legacy validation (when Pipelines not set)
	if len(c.Pipelines) == 0 {
		if c.Fixed && c.PCRE {
			return fmt.Errorf("cannot use -F (fixed) and -P (pcre) together")
		}
	}

	if c.ContextBefore < 0 {
		return fmt.Errorf("invalid context before: %d", c.ContextBefore)
	}
	if c.ContextAfter < 0 {
		return fmt.Errorf("invalid context after: %d", c.ContextAfter)
	}
	if c.CountOnly && c.FileNamesOnly {
		return fmt.Errorf("cannot use -c (count) and -l (files-with-matches) together")
	}
	if c.FilesFrom != "" && c.ChangedSince != "" {
		return fmt.Errorf("cannot use --files-from and --changed-since together")
	}
	if c.Multiline {
		if c.PCRE || c.Invert || c.WatchMode {
			return fmt.Errorf("-U (multiline) cannot combine with -P, -v, or --watch")
		}
		for _, pipeline := range c.Pipelines {
			if len(pipeline) > 1 {
				return fmt.Errorf("-U (multiline) cannot combine with -t pipelines")
			}
			for _, stage := range pipeline {
				if stage.PCRE {
					return fmt.Errorf("-U (multiline) cannot combine with -P")
				}
			}
		}
	}
	if c.Structural {
		if c.PCRE || c.Invert || c.WatchMode || c.Multiline || c.Ident || c.Fixed || c.WordRegexp {
			return fmt.Errorf("--structural cannot combine with -F, -P, -v, -U, -w, --ident, or --watch")
		}
		if c.ContextBefore > 0 || c.ContextAfter > 0 {
			return fmt.Errorf("--structural cannot combine with context lines")
		}
		if len(c.Pipelines) > 1 || (len(c.Pipelines) == 1 && len(c.Pipelines[0]) > 1) {
			return fmt.Errorf("--structural takes a single template")
		}
		for _, pipeline := range c.Pipelines {
			for _, stage := range pipeline {
				if stage.Fixed || stage.PCRE {
					return fmt.Errorf("--structural cannot combine with -F or -P")
				}
			}
		}
	}
	if c.Capture != "" && !(c.Structural && c.Histogram) {
		return fmt.Errorf("--capture requires --structural and --histogram")
	}
	return nil
}

// WordPattern wraps a pattern so it matches only whole words (-w):
// \b(?:PATTERN)\b, quoting a fixed string first. The result is always a
// regex; \bLITERAL\b shapes run on the SIMD literal engine.
func WordPattern(pattern string, fixed bool) string {
	if fixed {
		return `\b` + regexp.QuoteMeta(pattern) + `\b`
	}
	return `\b(?:` + pattern + `)\b`
}
