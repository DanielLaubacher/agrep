package cli

// --batch: execute many queries in one pass. Every file is read once and
// searched by all patterns; results carry their query for attribution.
// This is the execution half of agent-side semantic search: the agent
// expands a concept into N lexical probes and pays one walk for all of
// them. See agent-mode.md.

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/dl/gogrep/internal/input"
	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
	"github.com/dl/gogrep/internal/scheduler"
)

// loadBatchPatterns reads one pattern per line; blank lines and lines
// starting with '#' are ignored.
func loadBatchPatterns(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var patterns []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if len(patterns) == 0 {
		return nil, fmt.Errorf("no patterns in %s", path)
	}
	return patterns, nil
}

// runBatch executes all patterns from cfg.BatchFile over the configured
// paths (or stdin) in a single pass.
func runBatch(cfg Config, reader input.Reader, stdinReader input.Reader, formatter output.Formatter, w *output.Writer, mode searchMode) int {
	patterns, err := loadBatchPatterns(cfg.BatchFile)
	if err != nil {
		logWarn("batch: %v", err)
		return 2
	}

	// Smart case applies across the whole batch: only if every pattern is
	// lowercase (mirrors single-query behavior).
	ignoreCase := cfg.IgnoreCase
	if cfg.SmartCase && !ignoreCase {
		allLower := true
		for _, p := range patterns {
			if strings.ContainsFunc(p, unicode.IsUpper) {
				allLower = false
				break
			}
		}
		ignoreCase = allLower
	}

	opts := matcher.MatcherOpts{MaxCols: effectiveMaxCols(cfg), NeedLineNums: cfg.LineNumbers || cfg.JSONOutput}
	matchers := make([]matcher.Matcher, len(patterns))
	for i, p := range patterns {
		fixed, pcre := cfg.Fixed, cfg.PCRE
		if cfg.Ident {
			if re := identRegex(p); re != "" {
				p = re
				fixed, pcre = false, false
			}
		}
		m, err := matcher.NewMatcher([]string{p}, fixed, pcre, ignoreCase, cfg.Invert, opts)
		if err != nil {
			logWarn("batch pattern %q: %v", p, err)
			return 2
		}
		matchers[i] = m
	}

	// Seed per-query totals so zero-hit queries still appear in the
	// JSON summary.
	if qr, ok := formatter.(output.QueryRegistrar); ok {
		qr.RegisterQueries(patterns)
	}

	// Stdin: search the single buffer with each matcher in turn.
	if len(cfg.Paths) == 0 && cfg.FilesFrom == "" {
		return runBatchStdin(stdinReader, matchers, patterns, formatter, w)
	}

	// Files, recursive and --files-from modes share the scheduler path.
	fileCh, err := fileSource(cfg, cfg.Paths)
	if err != nil {
		logWarn("files-from: %v", err)
		return 2
	}

	sched := scheduler.New(cfg.Workers, nil, reader, mode == searchFilesOnly, mode == searchCountOnly)
	resultCh := sched.RunBatch(fileCh, matchers, patterns)

	var hasMatch atomic.Bool
	ow := output.NewOrderedWriter(w, formatter, true)
	ow.WriteOrdered(resultCh, func() {
		hasMatch.Store(true)
	})

	if hasMatch.Load() {
		return 0
	}
	return 1
}

func runBatchStdin(reader input.Reader, matchers []matcher.Matcher, patterns []string, formatter output.Formatter, w *output.Writer) int {
	readResult, err := reader.Read("")
	if err != nil {
		logWarn("stdin: %v", err)
		return 2
	}
	hasMatch := false
	var buf []byte
	for i, m := range matchers {
		ms := m.FindAll(readResult.Data)
		if !ms.HasMatch() {
			continue
		}
		hasMatch = true
		buf = formatter.Format(buf, output.Result{MatchSet: ms, Query: patterns[i]}, false)
	}
	if fin, ok := formatter.(output.Finisher); ok {
		buf = fin.Finish(buf)
	}
	if len(buf) > 0 {
		w.Write(buf)
	}
	if readResult.Closer != nil {
		readResult.Closer()
	}
	if hasMatch {
		return 0
	}
	return 1
}
