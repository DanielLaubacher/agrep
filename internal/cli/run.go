// Package cli wires the agrep pipeline together: it turns a parsed
// Config into a running search, selecting one of four modes (stdin,
// explicit files, recursive walk, watch) and connecting the walker,
// scheduler, matchers, and output stages. Large single files are searched
// in parallel line-aligned chunks (see parallel.go).
package cli

import (
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/DanielLaubacher/agrep/internal/input"
	"github.com/DanielLaubacher/agrep/internal/lang"
	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/output"
	"github.com/DanielLaubacher/agrep/internal/scheduler"
	"github.com/DanielLaubacher/agrep/internal/walker"
	"github.com/DanielLaubacher/agrep/internal/watch"
)

// resolveStructuralLang picks the language family for --structural's
// string/comment atom awareness. An explicit --lang always wins (this
// includes "--lang generic", which lang.ByName also maps to Generic —
// once given, an explicit choice is indistinguishable from Generic
// itself, so auto-detection/warning below only applies when --lang was
// never given at all). Without one, a single recognizable file
// extension among the search paths is used automatically, matching how
// --block/--scope already auto-detect per file. Anything
// less certain (a directory, files of different families, or no
// recognizable extension) falls back to Generic — but says so: Generic
// only knows balanced delimiters, and can silently misparse ordinary
// code where a string or comment literal contains an unbalanced
// bracket character (self-test finding: --structural without --lang on
// this project's own source).
func resolveStructuralLang(cfg Config) lang.Lang {
	if !cfg.Structural || cfg.Lang != "" {
		return lang.ByName(cfg.Lang)
	}

	detected := lang.Generic
	ambiguous := false
	for _, p := range cfg.Paths {
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			continue
		}
		fam := lang.ByPath(p)
		if fam == lang.Generic {
			continue
		}
		if detected != lang.Generic && detected != fam {
			ambiguous = true
		}
		detected = fam
	}
	if detected != lang.Generic && !ambiguous {
		return detected
	}
	logWarn("--structural has no --lang and no single file language could be inferred (%s); using generic mode — balanced delimiters only, strings/comments are not recognized and an unbalanced bracket inside one can silently misparse real code. Pass --lang go|py|js|rust|c|sh|rb|md.",
		strings.Join(cfg.Paths, " "))
	return lang.Generic
}

// logWarn writes a warning to stderr.
func logWarn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "agrep: "+format+"\n", args...)
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
		return runGetRegion(cfg.GetRegion, cfg.ExpandLines, cfg.JSONOutput, output.NewWriter())
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
	// A config file silently changing case sensitivity is why counts
	// differ from grep — say so once, so the agent doesn't detour.
	if cfg.IgnoreCase && cfg.CaseFromConfig {
		logWarn("case-insensitive via %s (pass -s to override)", cfg.ConfigPath)
	}

	// Display-column limit (-M) is a text-formatter concern only; matchers
	// always resolve full line bounds so regions and totals stay exact.
	maxCols := effectiveMaxCols(cfg)
	// Multiline, structural, and block output must not be
	// column-truncated unless the user explicitly asked for a limit.
	if (cfg.Multiline || cfg.Structural || cfg.Block) && cfg.MaxColumns == 0 {
		maxCols = 0
	}

	opts := matcher.MatcherOpts{
		// JSON consumers (agents) always need real line numbers.
		NeedLineNums: cfg.LineNumbers || cfg.JSONOutput,
		Multiline:    cfg.Multiline,
		Structural:   cfg.Structural,
		Lang:         resolveStructuralLang(cfg),
	}

	// Create matcher from pipelines (--batch builds its own matchers).
	var m matcher.Matcher
	onlyMatch := false
	if cfg.BatchFile == "" {
		var err error
		m, err = matcher.NewMatcherFromPipelines(
			convertPipelines(cfg.Pipelines, cfg.WordRegexp), cfg.IgnoreCase, cfg.Invert, opts,
		)
		if err != nil {
			logWarn("invalid pattern: %v", err)
			return 2
		}

		// --with-file/--without-file: gate matches on file-level
		// conditions. Filtering is never silent — the excluded count is
		// reported on exit.
		if len(cfg.WithFile)+len(cfg.WithoutFile) > 0 {
			probes := func(patterns []string) ([]matcher.Matcher, error) {
				ms := make([]matcher.Matcher, len(patterns))
				for i, p := range patterns {
					pm, err := matcher.NewMatcher([]string{p}, false, false, cfg.IgnoreCase, false, matcher.MatcherOpts{})
					if err != nil {
						return nil, err
					}
					ms[i] = pm
				}
				return ms, nil
			}
			with, err1 := probes(cfg.WithFile)
			without, err2 := probes(cfg.WithoutFile)
			if err1 != nil || err2 != nil {
				logWarn("invalid file-filter pattern: %v%v", err1, err2)
				return 2
			}
			ff := matcher.NewFileFilter(m, with, without)
			m = ff
			defer func() {
				if n := ff.Excluded(); n > 0 {
					logWarn("%d files matched but were excluded by --with-file/--without-file", n)
				}
			}()
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
	if cfg.Compact && !cfg.JSONOutput {
		logWarn("--compact has no effect without --json")
	}
	if cfg.JSONOutput {
		jf := output.NewJSONFormatter()
		jf.Compact = cfg.Compact
		// Only an explicit positive -M windows JSON text (a triage
		// size opt-in); the default is always the full line.
		if cfg.MaxColumns > 0 {
			jf.MaxColumns = cfg.MaxColumns
		}
		jf.Scope = cfg.Scope
		jf.CountOnly = cfg.CountOnly
		jf.FilesOnly = cfg.FileNamesOnly
		formatter = jf
	} else {
		tf := output.NewTextFormatter(cfg.LineNumbers, cfg.CountOnly, cfg.FileNamesOnly, useColor, maxCols, onlyMatch)
		tf.Scope = cfg.Scope
		formatter = tf
	}
	// --block rewrites matches to their enclosing blocks; innermost so
	// budget and collapse see what will actually be emitted.
	if cfg.Block && !cfg.WatchMode {
		if cfg.CountOnly || cfg.FileNamesOnly || cfg.ContextBefore > 0 || cfg.ContextAfter > 0 {
			logWarn("--block ignored with -c, -l, or context lines")
		} else {
			// A block record should be self-describing: annotate it
			// with its own definition line / heading (the block starts
			// there, so the scope resolver names it directly).
			if jf, ok := formatter.(*output.JSONFormatter); ok {
				jf.Scope = true
			}
			formatter = output.NewBlockFormatter(formatter)
		}
	}
	if cfg.MaxTokens > 0 {
		formatter = output.NewBudgetFormatter(formatter, cfg.MaxTokens, cfg.JSONOutput)
	}
	// Collapse wraps outside the budget so suppressed repeats never
	// spend budget. Needs whole match lines: incompatible with context
	// lines and meaningless for -c/-l.
	if cfg.Collapse {
		if cfg.ContextBefore > 0 || cfg.ContextAfter > 0 || cfg.CountOnly || cfg.FileNamesOnly {
			logWarn("--collapse ignored with context lines, -c, or -l")
		} else {
			formatter = output.NewCollapseFormatter(formatter, cfg.JSONOutput)
		}
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
		// Never silently ignore a flag the user passed.
		if cfg.Outline {
			logWarn("--outline is ignored with --batch")
		}
		if cfg.Histogram {
			logWarn("--histogram is ignored with --batch")
		}
		return runBatch(cfg, reader, stdinReader, formatter, w, mode)
	}

	if cfg.Outline && !readFromStdin {
		if cfg.UseIndex {
			logWarn("--use-index is ignored with --outline (no index is built or read)")
		}
		if cfg.MaxTokens > 0 {
			logWarn("--max-tokens is ignored with --outline; use --top K to bound the survey")
		}
		return runOutline(paths, m, reader, w, cfg, cfg.JSONOutput)
	}

	if cfg.Histogram {
		return runHistogram(paths, m, reader, stdinReader, w, cfg)
	}

	// --suggest: hold the closing summary back so probe records precede
	// it — a JSON consumer reads the summary as the stream's last word.
	var deferred *output.DeferredFinish
	if cfg.Suggest {
		deferred = output.NewDeferredFinish(formatter)
		formatter = deferred
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
	if deferred != nil {
		w.Write(deferred.Final(nil))
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
	hasErr := false
	var buf []byte

	for _, path := range paths {
		result := searchReader(reader, path, m, mode, lineNums)
		if result.Err != nil {
			logWarn("%s: %v", path, result.Err)
			hasErr = true
			// Fall through: JSON mode also records the error in-stream.
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

	// A file that failed to open is an error exit even when other files
	// matched, matching grep's convention and runRecursive's walkFailed
	// handling (report bug: this used to return 0/1 from hasMatch alone,
	// silently masking open errors on explicit file arguments).
	if hasErr {
		return 2
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
	if cfg.UseIndex && !cfg.Structural && cfg.FilesFrom == "" && len(paths) == 1 {
		if ch, ok := indexedFileChannel(cfg, paths[0]); ok {
			fileCh = ch
		}
	}
	var werrs *walkErrs
	if fileCh == nil {
		ch, we, err := fileSource(cfg, paths)
		if err != nil {
			logWarn("files-from: %v", err)
			return 2
		}
		fileCh = ch
		werrs = we
	}

	// Create scheduler and run workers
	sched := scheduler.New(cfg.Workers, m, reader, mode == searchFilesOnly, mode == searchCountOnly)
	resultCh := sched.Run(fileCh)

	// Write results in order; walk errors trail the results so they land
	// in the JSON stream and the summary's errors count.
	var hasMatch atomic.Bool
	walkFailed := false
	ow := output.NewOrderedWriter(w, formatter, true)
	ow.WriteOrderedTail(resultCh, func() {
		hasMatch.Store(true)
	}, func() []output.Result {
		errs := werrs.errResults()
		walkFailed = len(errs) > 0
		return errs
	})

	// A failed walk (missing root, unreadable directory) is an error exit
	// even when other roots matched — absence claims need the caveat.
	if walkFailed {
		return 2
	}
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
func convertPipelines(pipelines [][]StageConfig, word bool) [][]matcher.StageConfig {
	result := make([][]matcher.StageConfig, len(pipelines))
	for i, pipeline := range pipelines {
		result[i] = make([]matcher.StageConfig, len(pipeline))
		for j, s := range pipeline {
			pattern, fixed := s.Pattern, s.Fixed
			if word {
				pattern, fixed = WordPattern(pattern, fixed), false
			}
			result[i][j] = matcher.StageConfig{
				Pattern:   pattern,
				Fixed:     fixed,
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
