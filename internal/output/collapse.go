package output

// --collapse: near-duplicate suppression. Generated files, lock files
// and vendored code repeat the same matching line hundreds of times;
// after the first few, repeats spend context budget without adding
// information. CollapseFormatter passes the first collapseThreshold
// occurrences of each distinct (trimmed) line text through and tallies
// the rest, reporting exactly what was suppressed. Wraps OUTSIDE
// BudgetFormatter so collapsed lines never spend budget.

import (
	"bytes"
	"strconv"
)

// collapseThreshold is how many occurrences of an identical line are
// shown before further repeats are suppressed.
const collapseThreshold = 3

// collapseKeyMax bounds the dedup key length so pathological lines
// don't bloat the seen map.
const collapseKeyMax = 256

type CollapseFormatter struct {
	inner Formatter
	json  bool

	seen           map[string]int
	collapsedLines int
	collapsedTexts int // distinct texts that crossed the threshold
}

// NewCollapseFormatter wraps inner with near-duplicate suppression.
func NewCollapseFormatter(inner Formatter, json bool) *CollapseFormatter {
	return &CollapseFormatter{inner: inner, json: json, seen: make(map[string]int)}
}

func (f *CollapseFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	ms := &result.MatchSet
	if result.Err != nil || len(ms.Matches) == 0 {
		return f.inner.Format(buf, result, multiFile)
	}

	kept := ms.Matches[:0:0]
	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			kept = append(kept, *m)
			continue
		}
		line := bytes.TrimSpace(ms.Data[m.LineStart : m.LineStart+m.LineLen])
		if len(line) > collapseKeyMax {
			line = line[:collapseKeyMax]
		}
		n := f.seen[string(line)] + 1
		if n == 1 || n <= collapseThreshold {
			f.seen[string(line)] = n
			kept = append(kept, *m)
			continue
		}
		f.seen[string(line)] = n
		if n == collapseThreshold+1 {
			f.collapsedTexts++
		}
		f.collapsedLines++
	}
	if len(kept) == len(ms.Matches) {
		return f.inner.Format(buf, result, multiFile)
	}
	filtered := result
	filtered.MatchSet = ms.WithMatches(kept)
	return f.inner.Format(buf, filtered, multiFile)
}

// Finish reports what was suppressed, then finishes the inner chain.
func (f *CollapseFormatter) Finish(buf []byte) []byte {
	if f.collapsedLines > 0 {
		if f.json {
			buf = append(buf, `{"type":"collapsed","lines":`...)
			buf = strconv.AppendInt(buf, int64(f.collapsedLines), 10)
			buf = append(buf, `,"texts":`...)
			buf = strconv.AppendInt(buf, int64(f.collapsedTexts), 10)
			buf = append(buf, "}\n"...)
		} else {
			buf = append(buf, "[agrep] collapsed "...)
			buf = strconv.AppendInt(buf, int64(f.collapsedLines), 10)
			buf = append(buf, " repeats of "...)
			buf = strconv.AppendInt(buf, int64(f.collapsedTexts), 10)
			buf = append(buf, " line texts already shown 3x\n"...)
		}
	}
	if fin, ok := f.inner.(Finisher); ok {
		buf = fin.Finish(buf)
	}
	return buf
}

// RegisterQueries forwards through the wrapper chain.
func (f *CollapseFormatter) RegisterQueries(queries []string) {
	if qr, ok := f.inner.(QueryRegistrar); ok {
		qr.RegisterQueries(queries)
	}
}

var _ Formatter = (*CollapseFormatter)(nil)
var _ Finisher = (*CollapseFormatter)(nil)
