package cli

// Agent-oriented commands and modes: --get-region, --outline, --suggest.
// See agent-mode.md for the design.

import (
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"

	"github.com/DanielLaubacher/agrep/internal/input"
	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/output"
	"github.com/DanielLaubacher/agrep/internal/scheduler"
)

// ---------------- --get-region ----------------

// runGetRegion prints the exact bytes for a region id and exits.
// Two forms: "path@start-end" (byte span, as emitted in JSON "region"
// fields) and "path@:start-end" (1-based inclusive line range — the
// dialect of compilers and stack traces). expand widens the region by
// N whole lines on each side. Region ids are self-contained, so an
// agent can cite a span and later re-fetch or read around it without
// re-running the search.
func runGetRegion(region string, expand int, w *output.Writer) int {
	at := strings.LastIndexByte(region, '@')
	if at <= 0 {
		logWarn("invalid region %q (want path@start-end or path@:line-line)", region)
		return 2
	}
	path := region[:at]
	rangeStr := region[at+1:]

	// Scope-addressable forms: "path@func:Name" returns the whole
	// definition block, "path@section:Name" the whole Markdown section
	// — the unit the agent would otherwise read by guessed line range.
	if kind, name, found := strings.Cut(rangeStr, ":"); found && (kind == "func" || kind == "section") && name != "" {
		return runGetNamedRegion(path, kind, name, expand, w)
	}

	lineMode := strings.HasPrefix(rangeStr, ":")
	if lineMode {
		rangeStr = rangeStr[1:]
	}
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
	if lineMode && start < 1 {
		logWarn("invalid region %q: line numbers are 1-based", region)
		return 2
	}

	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		logWarn("%s: %v", path, err)
		return 2
	}
	defer unix.Close(fd)

	// Strict bounds: a cited region must lie entirely within the file.
	// A stale or fabricated citation must fail loudly, never "verify"
	// as silently empty output (report bug 9).
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		logWarn("%s: %v", path, err)
		return 2
	}
	if !lineMode && (start > st.Size || end > st.Size) {
		logWarn("region %q out of range: %s is %d bytes", region, path, st.Size)
		return 2
	}

	// Fast path: exact byte span, no expansion — pread just the range.
	if !lineMode && expand == 0 {
		buf := make([]byte, end-start)
		total := 0
		for total < len(buf) {
			n, err := unix.Pread(fd, buf[total:], start+int64(total))
			if err != nil {
				logWarn("%s: read: %v", path, err)
				return 2
			}
			if n == 0 {
				break
			}
			total += n
		}
		w.Write(buf[:total])
		return 0
	}

	// Line mode or expansion: read the file and resolve line bounds.
	data, err := readAllFd(fd)
	if err != nil {
		logWarn("%s: read: %v", path, err)
		return 2
	}
	var s, e int
	if lineMode {
		total := countLines(data)
		if int(start) > total || int(end) > total {
			logWarn("region %q out of range: %s has %d lines", region, path, total)
			return 2
		}
		s, e = lineRangeToBytes(data, int(start), int(end))
	} else {
		s, e = int(start), int(end)
	}
	if expand > 0 {
		s, e = expandByLines(data, s, e, expand)
	}
	w.Write(data[s:e])
	return 0
}

// runGetNamedRegion resolves "path@func:Name" / "path@section:Name"
// and prints the whole named block. Ambiguity is never silent: the
// first candidate is printed and the rest are listed on stderr.
func runGetNamedRegion(path, kind, name string, expand int, w *output.Writer) int {
	fd, err := unix.Open(path, unix.O_RDONLY, 0)
	if err != nil {
		logWarn("%s: %v", path, err)
		return 2
	}
	defer unix.Close(fd)
	data, err := readAllFd(fd)
	if err != nil {
		logWarn("%s: read: %v", path, err)
		return 2
	}

	s, e, candidates, ok := output.FindNamedBlock(data, path, kind, name)
	if !ok {
		logWarn("no %s matching %q in %s", kind, name, path)
		return 2
	}
	if len(candidates) > 1 {
		var lines []string
		for _, ln := range candidates[1:] {
			lines = append(lines, strconv.Itoa(ln))
		}
		logWarn("%d candidates for %s:%s; showing line %d (others at lines %s — cite %s@:N-M to disambiguate)",
			len(candidates), kind, name, candidates[0], strings.Join(lines, ", "), path)
	}
	if expand > 0 {
		s, e = expandByLines(data, s, e, expand)
	}
	w.Write(data[s:e])
	if e > s && data[e-1] != '\n' {
		w.Write([]byte{'\n'})
	}
	return 0
}

// countLines returns the number of lines in data (a trailing byte
// without a newline still counts as a line).
func countLines(data []byte) int {
	n := bytes.Count(data, []byte{'\n'})
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// readAllFd reads the remaining contents of fd from offset 0.
func readAllFd(fd int) ([]byte, error) {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, err
	}
	buf := make([]byte, st.Size)
	total := 0
	for total < len(buf) {
		n, err := unix.Pread(fd, buf[total:], int64(total))
		if err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		total += n
	}
	return buf[:total], nil
}

// lineRangeToBytes converts a 1-based inclusive line range to a byte
// span covering those whole lines (including the final newline).
func lineRangeToBytes(data []byte, startLine, endLine int) (int, int) {
	line := 1
	s, e := 0, len(data)
	pos := 0
	for pos < len(data) {
		if line == startLine {
			s = pos
			break
		}
		nl := indexByteFrom(data, pos, '\n')
		if nl < 0 {
			return len(data), len(data) // start past EOF
		}
		pos = nl + 1
		line++
	}
	if line < startLine {
		return len(data), len(data)
	}
	for pos < len(data) && line <= endLine {
		nl := indexByteFrom(data, pos, '\n')
		if nl < 0 {
			return s, len(data)
		}
		pos = nl + 1
		line++
	}
	e = pos
	return s, e
}

// expandByLines widens byte span [s,e) to whole lines plus n extra
// lines on each side. A span already at a line boundary is not
// re-snapped, so expansion is exact regardless of the input form.
func expandByLines(data []byte, s, e, n int) (int, int) {
	if s > len(data) {
		s = len(data)
	}
	if e > len(data) {
		e = len(data)
	}
	// Snap s back to its line start (no-op at a boundary), then walk n
	// whole lines further back.
	for s > 0 && data[s-1] != '\n' {
		s--
	}
	for i := 0; i < n && s > 0; i++ {
		s-- // step onto the previous line's newline
		for s > 0 && data[s-1] != '\n' {
			s--
		}
	}
	// Snap e forward through the rest of its line unless it already
	// sits at a boundary, then walk n whole lines further.
	if e > 0 && e < len(data) && data[e-1] != '\n' {
		for e < len(data) && data[e] != '\n' {
			e++
		}
		if e < len(data) {
			e++ // include the newline
		}
	}
	for i := 0; i < n && e < len(data); i++ {
		for e < len(data) && data[e] != '\n' {
			e++
		}
		if e < len(data) {
			e++
		}
	}
	return s, e
}

// indexByteFrom returns the index of c in data at or after pos, or -1.
func indexByteFrom(data []byte, pos int, c byte) int {
	for ; pos < len(data); pos++ {
		if data[pos] == c {
			return pos
		}
	}
	return -1
}

// ---------------- --outline ----------------

// outlineEntry is one row of the per-file survey.
type outlineEntry struct {
	path     string
	count    int
	defs     int // matching lines that are definition-shaped (--rank defs)
	size     int // file size in bytes, for density ranking
	exemplar string
}

const outlineExemplarMax = 120

// outlineFromResult extracts (count, exemplar line) from a full search
// result. The exemplar is the file's most informative matching line, not
// merely its first: the literal first match in code is often boilerplate
// (imports, a package clause). Preference order: most match occurrences
// on the line, then a line that carries text beyond the matches
// themselves, then earliest. The exemplar is copied before the result's
// buffer is released.
func outlineFromResult(r *output.Result) (outlineEntry, bool) {
	entry := outlineEntry{path: r.FilePath, size: len(r.MatchSet.Data)}
	ms := &r.MatchSet
	bestIdx := -1
	bestOcc := 0
	bestCtx := false
	bestDef := false
	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			continue
		}
		entry.count++

		occ := m.PosCount
		if occ == 0 {
			occ = 1
		}
		matched := 0
		for _, p := range ms.MatchPositions(i) {
			matched += p[1] - p[0]
		}
		rawLine := ms.Data[m.LineStart : m.LineStart+m.LineLen]
		isDef := output.IsDefinitionLine(rawLine, r.FilePath)
		if isDef {
			entry.defs++
		}
		line := strings.TrimSpace(string(rawLine))
		hasCtx := len(line) > matched

		// A definition line is the ideal exemplar; then most
		// occurrences; then a line with text beyond the matches.
		better := bestIdx < 0 ||
			(isDef && !bestDef) ||
			(isDef == bestDef && occ > bestOcc) ||
			(isDef == bestDef && occ == bestOcc && hasCtx && !bestCtx)
		if better {
			bestIdx = i
			bestOcc = occ
			bestCtx = hasCtx
			bestDef = isDef
		}
	}
	if bestIdx >= 0 {
		m := &ms.Matches[bestIdx]
		line := ms.Data[m.LineStart : m.LineStart+m.LineLen]
		if len(line) > outlineExemplarMax {
			line = line[:outlineExemplarMax]
		}
		entry.exemplar = strings.TrimSpace(string(line))
	}
	return entry, entry.count > 0
}

// runOutline surveys the corpus: one row per matching file (count + first
// matching line), sorted by count descending, optionally limited to the
// top K files.
func runOutline(paths []string, m matcher.Matcher, reader input.Reader, w *output.Writer, cfg Config, jsonOut bool) int {
	var entries []outlineEntry

	fileCh, werrs, err := fileSource(cfg, paths)
	if err != nil {
		logWarn("files-from: %v", err)
		return 2
	}
	defer logWalkErrs(werrs)
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

	density := cfg.Rank == "density"
	defsRank := cfg.Rank == "defs"
	scores := make([]float64, len(entries))
	if defsRank {
		// Definitions first: agents ask "where is X defined" far more
		// often than "where is X mentioned". Tests, vendored, and
		// generated files sink to the bottom regardless of counts.
		demoted := func(e *outlineEntry) bool {
			return demotedPath(e.path) || testPath(e.path)
		}
		sort.Slice(entries, func(i, j int) bool {
			di, dj := demoted(&entries[i]), demoted(&entries[j])
			if di != dj {
				return !di
			}
			if entries[i].defs != entries[j].defs {
				return entries[i].defs > entries[j].defs
			}
			if entries[i].count != entries[j].count {
				return entries[i].count > entries[j].count
			}
			return entries[i].path < entries[j].path
		})
	} else if density {
		// Matching lines per KB, with a flat demotion for generated and
		// vendored artifacts — a 200-line file about the concept should
		// outrank a 20k-line file that mentions it as often. The score
		// is emitted with each row so the reader can see why the order
		// changed.
		score := func(e *outlineEntry) float64 {
			kb := float64(e.size) / 1024.0
			if kb < 1 {
				kb = 1
			}
			s := float64(e.count) / kb
			if demotedPath(e.path) {
				s *= 0.25
			}
			return s
		}
		sort.Slice(entries, func(i, j int) bool {
			si, sj := score(&entries[i]), score(&entries[j])
			if si != sj {
				return si > sj
			}
			if entries[i].count != entries[j].count {
				return entries[i].count > entries[j].count
			}
			return entries[i].path < entries[j].path
		})
		for i := range entries {
			scores[i] = score(&entries[i])
		}
	} else {
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].count != entries[j].count {
				return entries[i].count > entries[j].count
			}
			return entries[i].path < entries[j].path
		})
	}

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
		for i, e := range shown {
			buf = append(buf, `{"type":"outline","file":`...)
			buf = appendJSONString(buf, e.path)
			buf = append(buf, `,"count":`...)
			buf = strconv.AppendInt(buf, int64(e.count), 10)
			if density {
				buf = append(buf, `,"score":`...)
				buf = strconv.AppendFloat(buf, scores[i], 'f', 2, 64)
			}
			if defsRank {
				buf = append(buf, `,"defs":`...)
				buf = strconv.AppendInt(buf, int64(e.defs), 10)
			}
			buf = append(buf, `,"exemplar":`...)
			buf = appendJSONString(buf, e.exemplar)
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
		for i, e := range shown {
			buf = strconv.AppendInt(buf, int64(e.count), 10)
			if density {
				buf = append(buf, " ("...)
				buf = strconv.AppendFloat(buf, scores[i], 'f', 2, 64)
				buf = append(buf, "/KB)"...)
			}
			if defsRank {
				buf = append(buf, " ("...)
				buf = strconv.AppendInt(buf, int64(e.defs), 10)
				buf = append(buf, " defs)"...)
			}
			buf = append(buf, '\t')
			buf = append(buf, e.path...)
			buf = append(buf, '\t')
			buf = append(buf, e.exemplar...)
			buf = append(buf, '\n')
		}
	}
	w.Write(buf)

	if totalFiles > 0 {
		return 0
	}
	return 1
}

// testPath reports whether a path looks like test or spec content,
// which --rank defs sinks below hand-written sources.
func testPath(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "test", "tests", "spec", "specs", "__tests__":
			return true
		}
	}
	base := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		base = path[i+1:]
	}
	return strings.Contains(base, "_test.") || strings.Contains(base, ".test.") ||
		strings.Contains(base, "_spec.") || strings.Contains(base, ".spec.")
}

// demotedPath reports whether a path points at vendored, generated, or
// lock-style content that should rank below hand-written code in
// density mode.
func demotedPath(path string) bool {
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "vendor", "node_modules", "testdata", "third_party", "dist", ".git":
			return true
		}
	}
	base := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		base = path[i+1:]
	}
	switch {
	case strings.HasSuffix(base, ".lock"),
		strings.HasSuffix(base, ".min.js"),
		strings.HasSuffix(base, ".pb.go"),
		strings.HasSuffix(base, ".gen.go"),
		strings.Contains(base, "_generated"),
		base == "package-lock.json",
		base == "go.sum":
		return true
	}
	return false
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
// case-insensitive form and the word fragments of a split identifier
// (ConnectTimeout / connect_timeout / connect-timeout → connect,
// timeout; fragments shorter than 4 bytes are too noisy to probe).
func suggestVariants(pattern string) []suggestVariant {
	var out []suggestVariant
	lower := strings.ToLower(pattern)
	if lower != pattern {
		out = append(out, suggestVariant{pattern: lower, label: "case-insensitive"})
	}

	words := splitIdentWords(pattern, 4)

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

// suggestProbe is one probed variant with its corpus counts.
type suggestProbe struct {
	suggestVariant
	lines int
	files int
}

// runSuggest probes derived variants of the zero-hit pattern(s) and
// reports which of them occur in the corpus (case-insensitive
// fixed-string probes), giving the agent its next query instead of an
// empty result. It always writes a report — "no derivable variants" and
// "no variant occurs" are findings, not silence.
func runSuggest(patterns []string, paths []string, reader input.Reader, w *output.Writer, cfg Config) {
	// Merge variants across patterns, deduplicating probes.
	var variants []suggestVariant
	seen := map[string]bool{}
	for _, p := range patterns {
		for _, v := range suggestVariants(p) {
			if seen[v.pattern] {
				continue
			}
			seen[v.pattern] = true
			variants = append(variants, v)
		}
	}

	probes := make([]suggestProbe, 0, len(variants))
	for _, v := range variants {
		// Word-bounded probes: a fragment like 'herd' must not count
		// every 'shepherd', or rarest-first ordering is meaningless.
		bounded := `\b` + regexp.QuoteMeta(v.pattern) + `\b`
		m, err := matcher.NewMatcher([]string{bounded}, false, false, true, false, matcher.MatcherOpts{})
		if err != nil {
			continue
		}
		lines, files := probeCount(paths, m, reader, cfg)
		probes = append(probes, suggestProbe{v, lines, files})
	}

	// Occurring variants lead, rarest first — those are the most
	// selective next queries. Zero-count probes trail in derivation order.
	sort.SliceStable(probes, func(i, j int) bool {
		if (probes[i].lines > 0) != (probes[j].lines > 0) {
			return probes[i].lines > 0
		}
		return probes[i].lines > 0 && probes[i].lines < probes[j].lines
	})

	w.Write(appendSuggestReport(nil, patterns, probes, cfg.JSONOutput))
}

// appendSuggestReport renders the --suggest report. JSON output lists
// every probe (zero counts included) and ends with a suggest_summary
// object; text output lists occurring variants and always states an
// outcome, so a zero-hit + zero-variant run is never silent.
func appendSuggestReport(buf []byte, patterns []string, probes []suggestProbe, jsonOut bool) []byte {
	found := 0
	for _, p := range probes {
		if p.lines > 0 {
			found++
		}
	}

	if jsonOut {
		for _, p := range probes {
			buf = append(buf, `{"type":"suggest","variant":`...)
			buf = appendJSONString(buf, p.pattern)
			buf = append(buf, `,"kind":`...)
			buf = appendJSONString(buf, p.label)
			buf = append(buf, `,"lines":`...)
			buf = strconv.AppendInt(buf, int64(p.lines), 10)
			buf = append(buf, `,"files":`...)
			buf = strconv.AppendInt(buf, int64(p.files), 10)
			buf = append(buf, "}\n"...)
		}
		buf = append(buf, `{"type":"suggest_summary","patterns":[`...)
		for i, p := range patterns {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendJSONString(buf, p)
		}
		buf = append(buf, `],"tried":`...)
		buf = strconv.AppendInt(buf, int64(len(probes)), 10)
		buf = append(buf, `,"found":`...)
		buf = strconv.AppendInt(buf, int64(found), 10)
		buf = append(buf, "}\n"...)
		return buf
	}

	quoted := "'" + strings.Join(patterns, "', '") + "'"
	switch {
	case len(probes) == 0:
		buf = append(buf, "[agrep] no matches for "...)
		buf = append(buf, quoted...)
		buf = append(buf, "; no derivable variants to probe\n"...)
	case found == 0:
		buf = append(buf, "[agrep] no matches for "...)
		buf = append(buf, quoted...)
		buf = append(buf, "; none of the derived variants occur (tried: "...)
		for i, p := range probes {
			if i > 0 {
				buf = append(buf, ", "...)
			}
			buf = append(buf, p.pattern...)
		}
		buf = append(buf, ")\n"...)
	default:
		buf = append(buf, "[agrep] no matches for "...)
		buf = append(buf, quoted...)
		buf = append(buf, "; variants that do occur (rarest first):\n"...)
		for _, p := range probes {
			if p.lines == 0 {
				continue
			}
			buf = append(buf, "  "...)
			buf = append(buf, p.pattern...)
			buf = append(buf, " ("...)
			buf = append(buf, p.label...)
			buf = append(buf, "): "...)
			buf = strconv.AppendInt(buf, int64(p.lines), 10)
			buf = append(buf, " lines in "...)
			buf = strconv.AppendInt(buf, int64(p.files), 10)
			buf = append(buf, " files\n"...)
		}
	}
	return buf
}

// probeCount counts matching lines and files for a variant probe.
func probeCount(paths []string, m matcher.Matcher, reader input.Reader, cfg Config) (lines, files int) {
	var lineCount, fileCount atomic.Int64

	fileCh, _, err := fileSource(cfg, paths)
	if err != nil {
		return 0, 0
	}
	sched := scheduler.New(cfg.Workers, m, reader, false, true)
	for r := range sched.Run(fileCh) {
		if r.Err == nil && r.MatchCount > 0 {
			lineCount.Add(int64(r.MatchCount))
			fileCount.Add(1)
		}
	}
	return int(lineCount.Load()), int(fileCount.Load())
}
