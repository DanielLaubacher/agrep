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

// pageMarker is the marker PDF extraction leaves in Markdown: <!-- p.N -->.
var pageMarker = []byte("<!-- p.")

// nearestPage returns the page number of the nearest page marker at or
// before lineStart, or 0 if none is found within the scan window. It
// turns a citation in a PDF-extracted book into a page reference
// without a manual scan.
func nearestPage(data []byte, lineStart int) int {
	if lineStart > len(data) {
		lineStart = len(data)
	}
	lo := 0
	if lineStart > sectionScanLimit {
		lo = lineStart - sectionScanLimit
	}
	i := bytes.LastIndex(data[lo:lineStart], pageMarker)
	if i < 0 {
		return 0
	}
	pos := lo + i + len(pageMarker)
	page := 0
	for pos < len(data) && data[pos] >= '0' && data[pos] <= '9' {
		page = page*10 + int(data[pos]-'0')
		pos++
	}
	return page
}

// SectionHeadingFor exposes the Markdown heading lookup for callers
// outside the formatter chain (--get-region --json).
func SectionHeadingFor(data []byte, off int) []byte {
	return sectionHeading(data, off)
}

// PageFor exposes the <!-- p.N --> page lookup likewise.
func PageFor(data []byte, off int) int {
	return nearestPage(data, off)
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

	// Per-query TRUE totals for --batch (counted before the budget cut,
	// so the breakdown — including explicit zero-hit probes — survives
	// budget wrapping).
	queries queryTally
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
	if result.Err != nil {
		return f.inner.Format(buf, result, multiFile)
	}
	if result.Query != "" && (lines > 0 || result.HasMatch()) {
		qt := f.queries.entry(result.Query)
		qt[0]++
		qt[1] += lines
	}
	if lines == 0 {
		return f.inner.Format(buf, result, multiFile)
	}

	if f.spent >= f.limitBytes {
		f.omittedLines += lines
		f.omittedFiles++
		return buf
	}

	ms := &result.MatchSet
	// -c and -l results carry no per-line records — one small object each.
	if result.MatchCount > 0 || len(ms.Matches) == 0 {
		before := len(buf)
		buf = f.inner.Format(buf, result, multiFile)
		f.spent += len(buf) - before
		f.shownLines += lines
		f.shownFiles++
		return buf
	}

	// Per-record enforcement: emit one chunk at a time (a real match plus
	// its adjacent context/separator records) and stop the moment the
	// budget is spent — a single file with thousands of hits must not
	// blow through the cap. Wrapped formatters keep cross-call state
	// (section headers, block dedupe, JSON tallies) so chunked calls emit
	// exactly what one whole-file call would, minus the cut records.
	shown := 0
	for i := 0; i < len(ms.Matches); {
		if f.spent >= f.limitBytes {
			break
		}
		// Chunk [i, j): everything up to (not including) the real match
		// after the first real match at or beyond i.
		j := i
		seenReal := false
		for j < len(ms.Matches) {
			m := &ms.Matches[j]
			if !m.IsContext && m.LineStart >= 0 {
				if seenReal {
					break
				}
				seenReal = true
			}
			j++
		}
		sub := result
		sub.MatchSet = ms.WithMatches(ms.Matches[i:j])
		before := len(buf)
		buf = f.inner.Format(buf, sub, multiFile)
		f.spent += len(buf) - before
		if seenReal {
			shown++
		}
		i = j
	}

	f.shownLines += shown
	f.omittedLines += lines - shown
	if shown > 0 {
		f.shownFiles++
	} else {
		f.omittedFiles++
	}
	return buf
}

// budgetAware is implemented by formatters that fold a --max-tokens
// shown/omitted breakdown and true per-query totals (counted before any
// cut) into their own exact-totals summary record, per the documented
// JSON contract — not a second, undocumented record (report bug 1).
type budgetAware interface {
	SetBudgetTotals(shownLines, shownFiles, omittedLines, omittedFiles int, trueQueries *queryTally)
}

// Finish folds the budget breakdown into the wrapped formatter's own
// summary record (JSON mode), then finishes it so its totals still
// reach the stream. Call once after all results.
func (f *BudgetFormatter) Finish(buf []byte) []byte {
	if f.json {
		if ba, ok := f.inner.(budgetAware); ok {
			ba.SetBudgetTotals(f.shownLines, f.shownFiles, f.omittedLines, f.omittedFiles, &f.queries)
		}
	} else if f.omittedLines > 0 || f.omittedFiles > 0 {
		buf = append(buf, "[agrep] output budget reached: showing "...)
		buf = strconv.AppendInt(buf, int64(f.shownLines), 10)
		buf = append(buf, " matching lines in "...)
		buf = strconv.AppendInt(buf, int64(f.shownFiles), 10)
		buf = append(buf, " files; omitted "...)
		buf = strconv.AppendInt(buf, int64(f.omittedLines), 10)
		buf = append(buf, " lines in "...)
		buf = strconv.AppendInt(buf, int64(f.omittedFiles), 10)
		buf = append(buf, " more files\n"...)
	}
	if fin, ok := f.inner.(Finisher); ok {
		buf = fin.Finish(buf)
	}
	return buf
}

// AddSuppressedLines forwards --collapse suppression accounting through
// to the wrapped formatter, so CollapseFormatter's type assertion finds
// it even when BudgetFormatter sits between them in the chain.
func (f *BudgetFormatter) AddSuppressedLines(n int) {
	if sr, ok := f.inner.(suppressedReporter); ok {
		sr.AddSuppressedLines(n)
	}
}

// RegisterQueries seeds per-query totals (zero-hit probes must appear
// in the summary) and forwards to the wrapped formatter.
func (f *BudgetFormatter) RegisterQueries(queries []string) {
	for _, q := range queries {
		f.queries.entry(q)
	}
	if qr, ok := f.inner.(QueryRegistrar); ok {
		qr.RegisterQueries(queries)
	}
}

// Finisher is implemented by formatters that emit a trailer after the
// last result (e.g. BudgetFormatter's omission summary).
type Finisher interface {
	Finish(buf []byte) []byte
}

// DeferredFinish wraps a formatter so its Finish trailer is withheld
// until Final() — used by --suggest so probe records precede the
// closing summary instead of appearing after it.
type DeferredFinish struct {
	inner Formatter
}

func NewDeferredFinish(inner Formatter) *DeferredFinish {
	return &DeferredFinish{inner: inner}
}

func (d *DeferredFinish) Format(buf []byte, result Result, multiFile bool) []byte {
	return d.inner.Format(buf, result, multiFile)
}

// Finish is deferred: the summary is only produced by Final.
func (d *DeferredFinish) Finish(buf []byte) []byte { return buf }

// Final emits the wrapped formatter's real trailer.
func (d *DeferredFinish) Final(buf []byte) []byte {
	if fin, ok := d.inner.(Finisher); ok {
		return fin.Finish(buf)
	}
	return buf
}

// RegisterQueries forwards through the wrapper chain.
func (d *DeferredFinish) RegisterQueries(queries []string) {
	if qr, ok := d.inner.(QueryRegistrar); ok {
		qr.RegisterQueries(queries)
	}
}

var _ Formatter = (*DeferredFinish)(nil)
var _ Finisher = (*DeferredFinish)(nil)

// QueryRegistrar is implemented by formatters that report per-query
// totals and want the full query list up front (--batch).
type QueryRegistrar interface {
	RegisterQueries(queries []string)
}

var _ Formatter = (*BudgetFormatter)(nil)
var _ Finisher = (*BudgetFormatter)(nil)
