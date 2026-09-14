package output

// Agent-oriented output shaping: token budgets and Markdown section
// annotation. See agent-mode.md. These features bound and enrich what is
// *emitted* — searches always run to completion so reported totals are
// exact.

import (
	"bytes"
	"strconv"
)

// sectionScanLimit bounds how far back sectionHeading searches for a
// Markdown heading. Applied per printed match only.
const sectionScanLimit = 64 * 1024

// sectionHeading returns the nearest Markdown heading line ("#"-prefixed)
// at or before lineStart in data, or nil if none is found within the scan
// window.
func sectionHeading(data []byte, lineStart int) []byte {
	h, _ := sectionHeadingAt(data, lineStart)
	return h
}

// sectionHeadingAt additionally returns the heading line's byte offset,
// for callers that need the section start (--block).
func sectionHeadingAt(data []byte, lineStart int) ([]byte, int) {
	if lineStart > len(data) {
		lineStart = len(data)
	}
	lo := 0
	if lineStart > sectionScanLimit {
		lo = lineStart - sectionScanLimit
	}
	// Walk line starts backward, beginning with the match's own line
	// (a match on a heading line belongs to that heading).
	cur := lineStart
	for {
		if cur < len(data) && data[cur] == '#' {
			lineEnd := cur
			for lineEnd < len(data) && data[lineEnd] != '\n' {
				lineEnd++
			}
			return bytes.TrimRight(data[cur:lineEnd], " \t\r"), cur
		}
		if cur <= lo {
			return nil, 0
		}
		if i := bytes.LastIndexByte(data[lo:cur-1], '\n'); i >= 0 {
			cur = lo + i + 1
		} else {
			cur = lo
		}
	}
}

// budgetBytesPerToken is the byte→token heuristic (~4 bytes per token for
// English text and code).
const budgetBytesPerToken = 4

// BudgetFormatter wraps a Formatter with an output token budget. Results
// past the budget are tallied instead of formatted; Finish appends an
// exact summary of what was shown and what was omitted. Totals count
// matching lines (context lines excluded).
type BudgetFormatter struct {
	inner      Formatter
	json       bool
	limitBytes int

	spent        int
	shownLines   int
	shownFiles   int
	omittedLines int
	omittedFiles int
}

// NewBudgetFormatter wraps inner with a budget of maxTokens output tokens.
// json selects the summary trailer format.
func NewBudgetFormatter(inner Formatter, maxTokens int, json bool) *BudgetFormatter {
	return &BudgetFormatter{
		inner:      inner,
		json:       json,
		limitBytes: maxTokens * budgetBytesPerToken,
	}
}

// matchLineCount counts real matching lines (excluding context lines and
// group separators).
func matchLineCount(r *Result) int {
	if r.MatchCount > 0 {
		return r.MatchCount
	}
	n := 0
	for i := range r.MatchSet.Matches {
		m := &r.MatchSet.Matches[i]
		if !m.IsContext && m.LineStart >= 0 {
			n++
		}
	}
	return n
}

func (f *BudgetFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	lines := matchLineCount(&result)
	if result.Err != nil || lines == 0 {
		return f.inner.Format(buf, result, multiFile)
	}

	if f.spent >= f.limitBytes {
		f.omittedLines += lines
		f.omittedFiles++
		return buf
	}

	before := len(buf)
	buf = f.inner.Format(buf, result, multiFile)
	f.spent += len(buf) - before
	f.shownLines += lines
	f.shownFiles++
	return buf
}

// Finish appends the budget summary. Call once after all results.
func (f *BudgetFormatter) Finish(buf []byte) []byte {
	if f.json {
		buf = append(buf, `{"type":"summary","shown_lines":`...)
		buf = strconv.AppendInt(buf, int64(f.shownLines), 10)
		buf = append(buf, `,"shown_files":`...)
		buf = strconv.AppendInt(buf, int64(f.shownFiles), 10)
		buf = append(buf, `,"omitted_lines":`...)
		buf = strconv.AppendInt(buf, int64(f.omittedLines), 10)
		buf = append(buf, `,"omitted_files":`...)
		buf = strconv.AppendInt(buf, int64(f.omittedFiles), 10)
		buf = append(buf, "}\n"...)
		return buf
	}
	if f.omittedLines == 0 && f.omittedFiles == 0 {
		return buf
	}
	buf = append(buf, "[agrep] output budget reached: showing "...)
	buf = strconv.AppendInt(buf, int64(f.shownLines), 10)
	buf = append(buf, " matching lines in "...)
	buf = strconv.AppendInt(buf, int64(f.shownFiles), 10)
	buf = append(buf, " files; omitted "...)
	buf = strconv.AppendInt(buf, int64(f.omittedLines), 10)
	buf = append(buf, " lines in "...)
	buf = strconv.AppendInt(buf, int64(f.omittedFiles), 10)
	buf = append(buf, " more files\n"...)
	return buf
}

// RegisterQueries forwards batch query registration to the wrapped
// formatter, so per-query zero totals survive budget wrapping.
func (f *BudgetFormatter) RegisterQueries(queries []string) {
	if qr, ok := f.inner.(QueryRegistrar); ok {
		qr.RegisterQueries(queries)
	}
}

// Finisher is implemented by formatters that emit a trailer after the
// last result (e.g. BudgetFormatter's omission summary).
type Finisher interface {
	Finish(buf []byte) []byte
}

// QueryRegistrar is implemented by formatters that report per-query
// totals and want the full query list up front (--batch).
type QueryRegistrar interface {
	RegisterQueries(queries []string)
}

var _ Formatter = (*BudgetFormatter)(nil)
var _ Finisher = (*BudgetFormatter)(nil)
