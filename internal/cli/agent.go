package cli

// Agent-oriented commands and modes: --get-region, --outline, --suggest.
// See agent-mode.md for the design.

import (
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"

	"golang.org/x/sys/unix"

	"github.com/dl/gogrep/internal/input"
	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
	"github.com/dl/gogrep/internal/scheduler"
	"github.com/dl/gogrep/internal/walker"
)

// ---------------- --get-region ----------------

// runGetRegion prints the exact bytes for a "path@start-end" span id (as
// emitted in JSON output's "region" field) and exits. Region ids are
// self-contained, so an agent can cite a span and later re-fetch it
// without re-running the search.
func runGetRegion(region string, w *output.Writer) int {
	at := strings.LastIndexByte(region, '@')
	if at <= 0 {
		logWarn("invalid region %q (want path@start-end)", region)
		return 2
	}
	path := region[:at]
	rangeStr := region[at+1:]
	dash := strings.IndexByte(rangeStr, '-')
	if dash <= 0 {
		logWarn("invalid region range %q (want start-end)", rangeStr)
		return 2
	}
	start, err1 := strconv.ParseInt(rangeStr[:dash], 10, 64)
	end, err2 := strconv.ParseInt(rangeStr[dash+1:], 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		logWarn("invalid region range %q", rangeStr)
		return 2
	}

	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		logWarn("%s: %v", path, err)
		return 2
	}
	defer unix.Close(fd)

	buf := make([]byte, end-start)
	total := 0
	for total < len(buf) {
		n, err := unix.Pread(fd, buf[total:], start+int64(total))
		if err != nil {
			logWarn("%s: read: %v", path, err)
			return 2
		}
		if n == 0 {
			break // region extends past EOF: return what exists
		}
		total += n
	}
	w.Write(buf[:total])
	return 0
}

// ---------------- --outline ----------------

// outlineEntry is one row of the per-file survey.
type outlineEntry struct {
	path  string
	count int
	first string
}

const outlineExemplarMax = 120

// outlineFromResult extracts (count, first matching line) from a full
// search result. The exemplar is copied before the result's buffer is
// released.
func outlineFromResult(r *output.Result) (outlineEntry, bool) {
	entry := outlineEntry{path: r.FilePath}
	for i := range r.MatchSet.Matches {
		m := &r.MatchSet.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			continue
		}
		entry.count++
		if entry.first == "" {
			line := r.MatchSet.Data[m.LineStart : m.LineStart+m.LineLen]
			if len(line) > outlineExemplarMax {
				line = line[:outlineExemplarMax]
			}
			entry.first = strings.TrimSpace(string(line))
		}
	}
	return entry, entry.count > 0
}

// runOutline surveys the corpus: one row per matching file (count + first
// matching line), sorted by count descending, optionally limited to the
// top K files.
func runOutline(paths []string, m matcher.Matcher, reader input.Reader, w *output.Writer, cfg Config, jsonOut bool) int {
	var entries []outlineEntry

	if cfg.Recursive {
		fileCh, errCh := walker.Walk(paths, walker.WalkOptions{
			Recursive:      true,
			NoIgnore:       cfg.NoIgnore,
			Hidden:         cfg.Hidden,
			FollowSymlinks: cfg.FollowSymlinks,
			Globs:          cfg.Globs,
		})
		go func() {
			for err := range errCh {
				logWarn("walk: %v", err)
			}
		}()
		sched := scheduler.New(cfg.Workers, m, reader, false, false)
		for r := range sched.Run(fileCh) {
			if r.Err != nil {
				logWarn("%s: %v", r.FilePath, r.Err)
				continue
			}
			if e, ok := outlineFromResult(&r); ok {
				entries = append(entries, e)
			}
			if r.Closer != nil {
				r.Closer()
			}
		}
	} else {
		for _, path := range paths {
			r := searchReader(reader, path, m, searchFull, false)
			if r.Err != nil {
				logWarn("%s: %v", path, r.Err)
				continue
			}
			if e, ok := outlineFromResult(&r); ok {
				entries = append(entries, e)
			}
			if r.Closer != nil {
				r.Closer()
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].path < entries[j].path
	})

	totalFiles := len(entries)
	totalLines := 0
	for _, e := range entries {
		totalLines += e.count
	}
	shown := entries
	if cfg.TopK > 0 && len(shown) > cfg.TopK {
		shown = shown[:cfg.TopK]
	}

	var buf []byte
	if jsonOut {
		for _, e := range shown {
			buf = append(buf, `{"type":"outline","file":`...)
			buf = appendJSONString(buf, e.path)
			buf = append(buf, `,"count":`...)
			buf = strconv.AppendInt(buf, int64(e.count), 10)
			buf = append(buf, `,"first":`...)
			buf = appendJSONString(buf, e.first)
			buf = append(buf, "}\n"...)
		}
		buf = append(buf, `{"type":"summary","files":`...)
		buf = strconv.AppendInt(buf, int64(totalFiles), 10)
		buf = append(buf, `,"lines":`...)
		buf = strconv.AppendInt(buf, int64(totalLines), 10)
		buf = append(buf, `,"shown":`...)
		buf = strconv.AppendInt(buf, int64(len(shown)), 10)
		buf = append(buf, "}\n"...)
	} else {
		buf = strconv.AppendInt(buf, int64(totalFiles), 10)
		buf = append(buf, " files match; "...)
		buf = strconv.AppendInt(buf, int64(totalLines), 10)
		buf = append(buf, " matching lines"...)
		if len(shown) < totalFiles {
			buf = append(buf, " (top "...)
			buf = strconv.AppendInt(buf, int64(len(shown)), 10)
			buf = append(buf, " shown)"...)
		}
		buf = append(buf, '\n')
		for _, e := range shown {
			buf = strconv.AppendInt(buf, int64(e.count), 10)
			buf = append(buf, '\t')
			buf = append(buf, e.path...)
			buf = append(buf, '\t')
			buf = append(buf, e.first...)
			buf = append(buf, '\n')
		}
	}
	w.Write(buf)

	if totalFiles > 0 {
		return 0
	}
	return 1
}

// appendJSONString appends s as a JSON string literal.
func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			buf = append(buf, '\\', c)
		case c < 0x20:
			buf = append(buf, `\u00`...)
			const hex = "0123456789abcdef"
			buf = append(buf, hex[c>>4], hex[c&0xF])
		default:
			buf = append(buf, c)
		}
	}
	return append(buf, '"')
}

// ---------------- --suggest ----------------

// suggestVariant is a derived probe pattern tried after a zero-hit search.
type suggestVariant struct {
	pattern string
	label   string
}

// suggestVariants derives probes from a failed pattern: its
// case-insensitive form and the word fragments of a split identifier.
func suggestVariants(pattern string) []suggestVariant {
	var out []suggestVariant
	lower := strings.ToLower(pattern)
	if lower != pattern {
		out = append(out, suggestVariant{pattern: lower, label: "case-insensitive"})
	}

	// Split on case boundaries and non-alphanumerics:
	// ConnectTimeout / connect_timeout / connect-timeout → connect, timeout
	var words []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 4 {
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
		if i > 0 && unicode.IsUpper(r) && unicode.IsLower(runes[i-1]) {
			flush()
		}
		cur.WriteRune(r)
	}
	flush()

	seen := map[string]bool{lower: true, pattern: true}
	for _, wd := range words {
		if seen[wd] {
			continue
		}
		seen[wd] = true
		out = append(out, suggestVariant{pattern: wd, label: "fragment"})
		if len(out) >= 5 {
			break
		}
	}
	return out
}

// runSuggest probes derived variants of a zero-hit pattern and reports
// which of them occur in the corpus (case-insensitive fixed-string
// probes), giving the agent its next query instead of an empty result.
func runSuggest(pattern string, paths []string, reader input.Reader, w *output.Writer, cfg Config) {
	variants := suggestVariants(pattern)
	if len(variants) == 0 {
		return
	}

	// Probe all variants first so the report can lead with the most
	// selective (rarest) ones — those are the informative next queries.
	type hit struct {
		v     suggestVariant
		lines int
		files int
	}
	var hits []hit
	for _, v := range variants {
		m, err := matcher.NewMatcher([]string{v.pattern}, true, false, true, false, matcher.MatcherOpts{})
		if err != nil {
			continue
		}
		lines, files := probeCount(paths, m, reader, cfg)
		if lines > 0 {
			hits = append(hits, hit{v, lines, files})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].lines < hits[j].lines })

	var buf []byte
	if !cfg.JSONOutput && len(hits) > 0 {
		buf = append(buf, "[gogrep] no matches for '"...)
		buf = append(buf, pattern...)
		buf = append(buf, "'; variants that do occur (rarest first):\n"...)
	}
	found := 0

	for _, h := range hits {
		v, lines, files := h.v, h.lines, h.files
		found++
		if cfg.JSONOutput {
			buf = append(buf, `{"type":"suggest","variant":`...)
			buf = appendJSONString(buf, v.pattern)
			buf = append(buf, `,"kind":`...)
			buf = appendJSONString(buf, v.label)
			buf = append(buf, `,"lines":`...)
			buf = strconv.AppendInt(buf, int64(lines), 10)
			buf = append(buf, `,"files":`...)
			buf = strconv.AppendInt(buf, int64(files), 10)
			buf = append(buf, "}\n"...)
		} else {
			buf = append(buf, "  "...)
			buf = append(buf, v.pattern...)
			buf = append(buf, " ("...)
			buf = append(buf, v.label...)
			buf = append(buf, "): "...)
			buf = strconv.AppendInt(buf, int64(lines), 10)
			buf = append(buf, " lines in "...)
			buf = strconv.AppendInt(buf, int64(files), 10)
			buf = append(buf, " files\n"...)
		}
	}

	if found > 0 {
		w.Write(buf)
	}
}

// probeCount counts matching lines and files for a variant probe.
func probeCount(paths []string, m matcher.Matcher, reader input.Reader, cfg Config) (lines, files int) {
	var lineCount, fileCount atomic.Int64

	if cfg.Recursive {
		fileCh, errCh := walker.Walk(paths, walker.WalkOptions{
			Recursive:      true,
			NoIgnore:       cfg.NoIgnore,
			Hidden:         cfg.Hidden,
			FollowSymlinks: cfg.FollowSymlinks,
			Globs:          cfg.Globs,
		})
		go func() {
			for range errCh {
			}
		}()
		sched := scheduler.New(cfg.Workers, m, reader, false, true)
		for r := range sched.Run(fileCh) {
			if r.Err == nil && r.MatchCount > 0 {
				lineCount.Add(int64(r.MatchCount))
				fileCount.Add(1)
			}
		}
	} else {
		for _, path := range paths {
			r := searchReader(reader, path, m, searchCountOnly, false)
			if r.Err == nil && r.MatchCount > 0 {
				lineCount.Add(int64(r.MatchCount))
				fileCount.Add(1)
			}
		}
	}
	return int(lineCount.Load()), int(fileCount.Load())
}
