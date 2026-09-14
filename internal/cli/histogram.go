package cli

// --histogram: aggregate distinct matched texts with occurrence and file
// counts — a built-in `... | sort | uniq -c | sort -rn` over the matched
// spans. Composes with -o pipelines: the final stage's spans are what
// get counted ("what error codes exist?", "which config keys are
// referenced?"). Like every aggregation mode, the search runs to
// completion and totals are exact.

import (
	"sort"
	"strconv"

	"github.com/dl/gogrep/internal/input"
	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
	"github.com/dl/gogrep/internal/scheduler"
	"github.com/dl/gogrep/internal/walker"
)

const histogramTextMax = 120

type histEntry struct {
	count int // total occurrences
	files int // files containing at least one occurrence
}

// histAccum aggregates matched-span texts across files.
type histAccum struct {
	counts map[string]*histEntry
}

func newHistAccum() *histAccum {
	return &histAccum{counts: make(map[string]*histEntry)}
}

// addResult folds one file's matches into the histogram.
func (h *histAccum) addResult(r *output.Result) {
	ms := &r.MatchSet
	var inFile map[string]bool
	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext || m.LineStart < 0 || m.PosCount == 0 {
			continue
		}
		line := ms.Data[m.LineStart : m.LineStart+m.LineLen]
		for _, pos := range ms.MatchPositions(i) {
			s, e := pos[0], pos[1]
			if s < 0 || s >= len(line) || e <= s {
				continue
			}
			if e > len(line) {
				e = len(line)
			}
			text := line[s:e]
			if len(text) > histogramTextMax {
				text = text[:histogramTextMax]
			}
			ent := h.counts[string(text)]
			if ent == nil {
				ent = &histEntry{}
				h.counts[string(text)] = ent
			}
			ent.count++
			if inFile == nil {
				inFile = make(map[string]bool)
			}
			if !inFile[string(text)] {
				inFile[string(text)] = true
				ent.files++
			}
		}
	}
}

// runHistogram searches like a normal query but reports the distinct
// matched texts with counts, most frequent first.
func runHistogram(paths []string, m matcher.Matcher, reader input.Reader, stdinReader input.Reader, w *output.Writer, cfg Config) int {
	acc := newHistAccum()

	switch {
	case len(paths) == 0:
		r := searchReader(stdinReader, "", m, searchFull, false)
		if r.Err != nil {
			logWarn("stdin: %v", r.Err)
			return 2
		}
		acc.addResult(&r)
		if r.Closer != nil {
			r.Closer()
		}
	case cfg.Recursive:
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
			acc.addResult(&r)
			if r.Closer != nil {
				r.Closer()
			}
		}
	default:
		for _, path := range paths {
			r := searchReader(reader, path, m, searchFull, false)
			if r.Err != nil {
				logWarn("%s: %v", path, r.Err)
				continue
			}
			acc.addResult(&r)
			if r.Closer != nil {
				r.Closer()
			}
		}
	}

	w.Write(appendHistogramReport(nil, acc, cfg.TopK, cfg.JSONOutput))
	if len(acc.counts) > 0 {
		return 0
	}
	return 1
}

// appendHistogramReport renders the histogram, most frequent first
// (ties alphabetical), limited to the top K distinct texts if K > 0.
func appendHistogramReport(buf []byte, acc *histAccum, topK int, jsonOut bool) []byte {
	type row struct {
		text string
		histEntry
	}
	rows := make([]row, 0, len(acc.counts))
	total := 0
	for text, e := range acc.counts {
		rows = append(rows, row{text, *e})
		total += e.count
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].text < rows[j].text
	})
	distinct := len(rows)
	shown := rows
	if topK > 0 && len(shown) > topK {
		shown = shown[:topK]
	}

	if jsonOut {
		for _, r := range shown {
			buf = append(buf, `{"type":"variant","text":`...)
			buf = appendJSONString(buf, r.text)
			buf = append(buf, `,"count":`...)
			buf = strconv.AppendInt(buf, int64(r.count), 10)
			buf = append(buf, `,"files":`...)
			buf = strconv.AppendInt(buf, int64(r.files), 10)
			buf = append(buf, "}\n"...)
		}
		buf = append(buf, `{"type":"summary","distinct":`...)
		buf = strconv.AppendInt(buf, int64(distinct), 10)
		buf = append(buf, `,"total":`...)
		buf = strconv.AppendInt(buf, int64(total), 10)
		buf = append(buf, `,"shown":`...)
		buf = strconv.AppendInt(buf, int64(len(shown)), 10)
		buf = append(buf, "}\n"...)
		return buf
	}

	buf = strconv.AppendInt(buf, int64(distinct), 10)
	buf = append(buf, " distinct match texts; "...)
	buf = strconv.AppendInt(buf, int64(total), 10)
	buf = append(buf, " total occurrences"...)
	if len(shown) < distinct {
		buf = append(buf, " (top "...)
		buf = strconv.AppendInt(buf, int64(len(shown)), 10)
		buf = append(buf, " shown)"...)
	}
	buf = append(buf, '\n')
	for _, r := range shown {
		buf = strconv.AppendInt(buf, int64(r.count), 10)
		buf = append(buf, '\t')
		buf = strconv.AppendInt(buf, int64(r.files), 10)
		buf = append(buf, '\t')
		buf = append(buf, r.text...)
		buf = append(buf, '\n')
	}
	return buf
}
