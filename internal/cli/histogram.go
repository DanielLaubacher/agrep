package cli

// --histogram: aggregate distinct matched texts with occurrence and file
// counts — a built-in `... | sort | uniq -c | sort -rn` over the matched
// spans. Composes with -o pipelines: the final stage's spans are what
// get counted ("what error codes exist?", "which config keys are
// referenced?"). Like every aggregation mode, the search runs to
// completion and totals are exact.

import (
	"bytes"
	"sort"
	"strconv"

	"github.com/DanielLaubacher/agrep/internal/input"
	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/output"
	"github.com/DanielLaubacher/agrep/internal/scheduler"
)

const histogramTextMax = 120

type histEntry struct {
	count int // total occurrences
	files int // files containing at least one occurrence
}

// histAccum aggregates matched-span texts across files. With a capture
// selector (-S holes), the named hole's bound text is counted instead
// of the full matched span — "histogram of the first argument to
// NewClient(...)" as one query.
type histAccum struct {
	counts  map[string]*histEntry
	capture string // hole name to aggregate ("" = first named hole, else spans)
}

func newHistAccum(capture string) *histAccum {
	return &histAccum{counts: make(map[string]*histEntry), capture: capture}
}

func (h *histAccum) add(text []byte, inFile map[string]bool) {
	if len(text) > histogramTextMax {
		text = text[:histogramTextMax]
	}
	ent := h.counts[string(text)]
	if ent == nil {
		ent = &histEntry{}
		h.counts[string(text)] = ent
	}
	ent.count++
	if !inFile[string(text)] {
		inFile[string(text)] = true
		ent.files++
	}
}

// captureText picks the selected hole's binding for match i, or nil.
func (h *histAccum) captureText(ms *matcher.MatchSet, i int) []byte {
	for _, c := range ms.MatchCaptures(i) {
		if h.capture != "" {
			if c.Name == h.capture {
				return ms.Data[c.Start:c.End]
			}
			continue
		}
		if c.Name != "_" {
			return ms.Data[c.Start:c.End] // first named hole
		}
	}
	return nil
}

// addResult folds one file's matches into the histogram.
func (h *histAccum) addResult(r *output.Result) {
	ms := &r.MatchSet
	inFile := make(map[string]bool)
	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext || m.LineStart < 0 || m.PosCount == 0 {
			continue
		}
		if m.CapCount > 0 {
			if text := h.captureText(ms, i); text != nil {
				h.add(bytes.TrimSpace(text), inFile)
				continue
			}
			continue // capture selected but this match lacks it
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
			h.add(line[s:e], inFile)
		}
	}
}

// runHistogram searches like a normal query but reports the distinct
// matched texts with counts, most frequent first.
func runHistogram(paths []string, m matcher.Matcher, reader input.Reader, stdinReader input.Reader, w *output.Writer, cfg Config) int {
	acc := newHistAccum(cfg.Capture)

	if len(paths) == 0 && cfg.FilesFrom == "" && cfg.ChangedSince == "" {
		r := searchReader(stdinReader, "", m, searchFull, false)
		if r.Err != nil {
			logWarn("stdin: %v", r.Err)
			return 2
		}
		acc.addResult(&r)
		if r.Closer != nil {
			r.Closer()
		}
	} else {
		fileCh, err := fileSource(cfg, paths)
		if err != nil {
			logWarn("files-from: %v", err)
			return 2
		}
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
