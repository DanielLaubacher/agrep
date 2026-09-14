// Package cli wires the gogrep pipeline together: it turns a parsed
// Config into a running search, selecting one of four modes (stdin,
// explicit files, recursive walk, watch) and connecting the walker,
// scheduler, matchers, and output stages. Large single files are searched
// in parallel line-aligned chunks (see parallel.go).
package cli

import (
	"fmt"
	"os"
	"sync/atomic"
	"unicode"

	"github.com/dl/gogrep/internal/input"
	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
	"github.com/dl/gogrep/internal/scheduler"
	"github.com/dl/gogrep/internal/walker"
	"github.com/dl/gogrep/internal/watch"
)

// logWarn writes a warning to stderr.
func logWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gogrep: "+format+"\n", args...)
}

// searchMode determines the fast path in searchReader.
type searchMode int

const (
	searchFull      searchMode = iota // full match extraction
	searchFilesOnly                   // just check if any match exists
	searchCountOnly                   // count matching lines, skip line extraction
)

// effectiveMaxCols resolves the display-column limit: 0 means the 75-col
// default, negative means unlimited.
func effectiveMaxCols(cfg Config) int {
	maxCols := cfg.MaxColumns
	if maxCols == 0 {
		maxCols = 75
	}
	if maxCols < 0 {
		maxCols = 0
	}
	return maxCols
}

// Run executes the search with the given config.
// Returns exit code: 0 = match found, 1 = no match, 2 = error.
func Run(cfg Config) int {
	// --get-region: fetch bytes for a span id; no search at all.
	if cfg.GetRegion != "" {
		return runGetRegion(cfg.GetRegion, output.NewWriter())
	}

	// --clear-index: index management; no search at all.
	if cfg.ClearIndex != "" {
		return runClearIndex(cfg.ClearIndex)
	}

	// Normalize pipelines from legacy fields if needed
	cfg.NormalizePipelines()

	// --ident: rewrite patterns as word-bounded case-convention regexes.
	if cfg.Ident {
		applyIdent(&cfg)
	}

	// Smart case: if enabled and all patterns are lowercase, enable case-insensitive
	if cfg.SmartCase && !cfg.IgnoreCase {
		allLower := true
		for _, pipeline := range cfg.Pipelines {
			for _, stage := range pipeline {
				for _, r := range stage.Pattern {
					if unicode.IsUpper(r) {
						allLower = false
						break
					}
				}
				if !allLower {
					break
				}
			}
			if !allLower {
				break
			}
		}
		if allLower {
			cfg.IgnoreCase = true
		}
	}

	maxCols := effectiveMaxCols(cfg)

	opts := matcher.MatcherOpts{
		MaxCols: maxCols,
		// JSON consumers (agents) always need real line numbers.
		NeedLineNums: cfg.LineNumbers || cfg.JSONOutput,
	}
	// Histogram counts matched spans — never truncate the lines they
	// live in.
	if cfg.Histogram {
		opts.MaxCols = 0
	}

	// Create matcher from pipelines (--batch builds its own matchers).
	var m matcher.Matcher
	onlyMatch := false
	if cfg.BatchFile == "" {
		var err error
		m, err = matcher.NewMatcherFromPipelines(
			convertPipelines(cfg.Pipelines), cfg.IgnoreCase, cfg.Invert, opts,
		)
		if err != nil {
			logWarn("invalid pattern: %v", err)
			return 2
		}

		// Detect if we need only-matched output mode
		onlyMatch = hasOnlyMatch(m)

		// Wrap with context if needed (not for watch mode — watch handles context via streaming)
		if !cfg.WatchMode {
			m = matcher.NewContextMatcher(m, cfg.ContextBefore, cfg.ContextAfter)
		}
	}

	// Determine color mode
	useColor := false
	switch cfg.Color {
	case ColorAlways:
		useColor = true
	case ColorNever:
		useColor = false
	case ColorAuto:
		useColor = output.StdoutIsTerminal()
	}

	// Create formatter and writer
	w := output.NewWriter()
	var formatter output.Formatter
	if cfg.JSONOutput {
		jf := output.NewJSONFormatter()
		jf.Sections = cfg.Sections
		jf.Scope = cfg.Scope
		jf.CountOnly = cfg.CountOnly
		jf.FilesOnly = cfg.FileNamesOnly
		formatter = jf
	} else {
		tf := output.NewTextFormatter(cfg.LineNumbers, cfg.CountOnly, cfg.FileNamesOnly, useColor, maxCols, onlyMatch)
		tf.Sections = cfg.Sections
		tf.Scope = cfg.Scope
		formatter = tf
	}
	if cfg.MaxTokens > 0 {
		formatter = output.NewBudgetFormatter(formatter, cfg.MaxTokens, cfg.JSONOutput)
	}

	reader := input.NewAdaptiveReader(cfg.MmapThreshold)
	stdinReader := input.NewStdinReader()

	// Determine search mode
	mode := searchFull
	if cfg.FileNamesOnly {
		mode = searchFilesOnly
	} else if cfg.CountOnly {
		mode = searchCountOnly
	}

	// Determine input sources
	paths := cfg.Paths
	readFromStdin := len(paths) == 0 && cfg.FilesFrom == "" && cfg.ChangedSince == ""

	if cfg.WatchMode {
		return runWatch(paths, m, formatter, w, cfg)
	}

	if cfg.BatchFile != "" {
		return runBatch(cfg, reader, stdinReader, formatter, w, mode)
	}

	if cfg.Outline && !readFromStdin {
		return runOutline(paths, m, reader, w, cfg, cfg.JSONOutput)
	}

	if cfg.Histogram {
		return runHistogram(paths, m, reader, stdinReader, w, cfg)
	}

	var exitCode int
	switch {
	case readFromStdin:
		exitCode = runStdin(stdinReader, m, formatter, w, cfg.LineNumbers)
	case cfg.Recursive || cfg.FilesFrom != "" || cfg.ChangedSince != "":
		exitCode = runRecursive(paths, m, reader, formatter, w, cfg, mode)
	default:
		exitCode = runFiles(paths, m, reader, formatter, w, mode, cfg.LineNumbers)
	}

	// Zero hits: probe derived variants so the agent's next query is
	// informed instead of guessed.
	if exitCode == 1 && cfg.Suggest {
		if readFromStdin {
			logWarn("--suggest needs file paths to probe; ignored for stdin")
		} else if len(cfg.Patterns) > 0 {
			runSuggest(cfg.Patterns, paths, reader, w, cfg)
		}
	}
	return exitCode
}

func runStdin(reader input.Reader, m matcher.Matcher, formatter output.Formatter, w *output.Writer, lineNums bool) int {
	result := searchReader(reader, "", m, searchFull, lineNums)
	hasMatch := result.HasMatch()
	var buf []byte
	if hasMatch {
		buf = formatter.Format(buf, result, false)
	}
	if result.Closer != nil {
		result.Closer()
	}
	if fin, ok := formatter.(output.Finisher); ok {
		buf = fin.Finish(buf)
	}
	if len(buf) > 0 {
		w.Write(buf)
	}
	if hasMatch {
		return 0
	}
	return 1
}

func runFiles(paths []string, m matcher.Matcher, reader input.Reader, formatter output.Formatter, w *output.Writer, mode searchMode, lineNums bool) int {
	multiFile := len(paths) > 1
	hasMatch := false
	var buf []byte

	for _, path := range paths {
		result := searchReader(reader, path, m, mode, lineNums)
		if result.Err != nil {
			logWarn("%s: %v", path, result.Err)
			continue
		}
		if result.HasMatch() {
			hasMatch = true
		}
		buf = formatter.Format(buf, result, multiFile)
		if result.Closer != nil {
			result.Closer()
		}
		if len(buf) >= 256*1024 {
			w.Write(buf)
			buf = buf[:0]
		}
	}
	if fin, ok := formatter.(output.Finisher); ok {
		buf = fin.Finish(buf)
	}
	if len(buf) > 0 {
		w.Write(buf)
	}

	if hasMatch {
		return 0
	}
	return 1
}

func runRecursive(paths []string, m matcher.Matcher, reader input.Reader, formatter output.Formatter, w *output.Writer, cfg Config, mode searchMode) int {
	// --use-index: source files from the trigram index (candidates +
	// sweep-dirty) instead of walking. Falls back to fileSource
	// (--files-from list or cold walk) whenever the index doesn't apply.
	var fileCh <-chan walker.FileEntry
	if cfg.UseIndex && cfg.FilesFrom == "" && len(paths) == 1 {
		if ch, ok := indexedFileChannel(cfg, paths[0]); ok {
			fileCh = ch
		}
	}
	if fileCh == nil {
		ch, err := fileSource(cfg, paths)
		if err != nil {
			logWarn("files-from: %v", err)
			return 2
		}
		fileCh = ch
	}

	// Create scheduler and run workers
	sched := scheduler.New(cfg.Workers, m, reader, mode == searchFilesOnly, mode == searchCountOnly)
	resultCh := sched.Run(fileCh)

	// Write results in order
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

func runWatch(paths []string, m matcher.Matcher, formatter output.Formatter, w *output.Writer, cfg Config) int {
	watcher, err := watch.New()
	if err != nil {
		logWarn("failed to create watcher: %v", err)
		return 2
	}
	defer watcher.Close()

	// Add all paths to watch
	for _, path := range paths {
		if err := watcher.Add(path); err != nil {
			logWarn("failed to watch %s: %v", path, err)
			return 2
		}
	}

	hasMatch := false
	events := watcher.Events()

	for evt := range events {
		if evt.Err != nil {
			logWarn("watch: %v", evt.Err)
			continue
		}

		switch evt.Type {
		case watch.EventModified:
			data, err := watcher.ReadNew(evt.Path)
			if err != nil {
				logWarn("%s: read: %v", evt.Path, err)
				continue
			}
			if len(data) == 0 {
				continue
			}

			// Search the new content
			ms := m.FindAll(data)
			if ms.HasMatch() {
				hasMatch = true
				result := output.Result{
					FilePath: evt.Path,
					MatchSet: ms,
				}
				buf := formatter.Format(nil, result, true)
				w.Write(buf)
			}

		case watch.EventCreated:
			// Add newly created files to the watch
			if err := watcher.Add(evt.Path); err != nil {
				logWarn("failed to watch %s: %v", evt.Path, err)
			}

		case watch.EventDeleted:
			logWarn("watched file removed: %s", evt.Path)
		}
	}

	if hasMatch {
		return 0
	}
	return 1
}

// convertPipelines converts cli.StageConfig to matcher.StageConfig.
func convertPipelines(pipelines [][]StageConfig) [][]matcher.StageConfig {
	result := make([][]matcher.StageConfig, len(pipelines))
	for i, pipeline := range pipelines {
		result[i] = make([]matcher.StageConfig, len(pipeline))
		for j, s := range pipeline {
			result[i][j] = matcher.StageConfig{
				Pattern:   s.Pattern,
				Fixed:     s.Fixed,
				PCRE:      s.PCRE,
				OnlyMatch: s.OnlyMatch,
			}
		}
	}
	return result
}

// hasOnlyMatch checks if the matcher is a pipeline with -o on the final stage.
func hasOnlyMatch(m matcher.Matcher) bool {
	type onlyMatcher interface {
		OnlyMatch() bool
	}
	if om, ok := m.(onlyMatcher); ok {
		return om.OnlyMatch()
	}
	return false
}

func searchReader(r input.Reader, path string, m matcher.Matcher, mode searchMode, lineNums bool) output.Result {
	result := output.Result{FilePath: path}

	readResult, err := r.Read(path)
	if err != nil {
		result.Err = err
		return result
	}

	closeReader := func() {
		if readResult.Closer != nil {
			readResult.Closer()
		}
	}

	if readResult.Data == nil {
		closeReader()
		return result
	}

	// Binary detection: skip binary files entirely (like ripgrep)
	if walker.IsBinary(readResult.Data) {
		closeReader()
		return result
	}

	switch mode {
	case searchFilesOnly:
		exists := false
		if useParallelSearch(readResult.Data, m) {
			exists = parallelMatchExists(readResult.Data, m)
		} else {
			exists = m.MatchExists(readResult.Data)
		}
		if exists {
			result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
		}
		closeReader()
	case searchCountOnly:
		var count int
		if useParallelSearch(readResult.Data, m) {
			count = parallelCountAll(readResult.Data, m)
		} else {
			count = m.CountAll(readResult.Data)
		}
		result.MatchCount = count
		closeReader()
	default:
		if useParallelSearch(readResult.Data, m) {
			result.MatchSet = parallelFindAll(readResult.Data, m, lineNums)
		} else {
			result.MatchSet = m.FindAll(readResult.Data)
		}
		// MatchSet.Data is the file buffer — pass Closer
		// to the caller so the buffer stays alive until formatting is done.
		if result.MatchSet.HasMatch() {
			result.Closer = closeReader
		} else {
			closeReader()
		}
	}
	return result
}
