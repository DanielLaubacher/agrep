package output

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// JSONFormatter formats results as JSON Lines (one JSON object per match).
// It tallies what it emits and appends an exact {"type":"summary",...}
// trailer via Finish, so a JSON consumer always receives totals.
type JSONFormatter struct {
	// Sections annotates each match with its enclosing Markdown heading.
	Sections bool
	// Scope annotates each match with its enclosing definition line
	// (--scope); Markdown files fall back to the heading.
	Scope bool
	// CountOnly emits {"type":"count"} objects instead of matches (-c).
	CountOnly bool
	// FilesOnly emits {"type":"file"} objects instead of matches (-l).
	FilesOnly bool
	// Compact emits lean match records (no span/byte_offset/matches):
	// the region id still carries the exact byte range, so citations
	// lose nothing while drill-in reads cost ~1/3 less.
	Compact bool
	// MaxColumns > 0 (an explicit -M) windows the "text" field around
	// the first match with "truncated":true — a triage size opt-in.
	// span/region always cover the full line, so citations stay exact.
	MaxColumns int

	files int
	lines int
	errs  int
	// suppressedLines were found but not emitted (--collapse); they are
	// included in `lines` so its meaning (total found) never changes.
	suppressedLines int
	// Last tallied (file, query) — consecutive Format calls for the same
	// result (budget chunking) must count the file once.
	lastFile  string
	lastQuery string
	// Per-query totals for --batch, in first-seen order.
	queryOrder  []string
	queryTotals map[string]*[2]int // query -> {files, lines}
}

// NewJSONFormatter creates a JSONFormatter.
func NewJSONFormatter() *JSONFormatter {
	return &JSONFormatter{}
}

// jsonMatch is the JSON serialization format for a match line.
type jsonMatch struct {
	Type       string    `json:"type"`
	File       string    `json:"file,omitempty"`
	LineNum    int       `json:"line_number"`
	ByteOffset int64     `json:"byte_offset"`
	Text       string    `json:"text"`
	Matches    []jsonPos `json:"matches,omitempty"`
	// Span is the absolute [start, end) byte range of the line within the
	// file; Region is the same as a self-contained "path@start-end" id
	// resolvable with --get-region.
	Span    *[2]int64 `json:"span,omitempty"`
	Region  string    `json:"region,omitempty"`
	Section string    `json:"section,omitempty"`
	Scope   string    `json:"scope,omitempty"`
	// Page is the nearest <!-- p.N --> marker at or before the match
	// (PDF-extracted documents); emitted with --sections/--scope.
	Page int `json:"page,omitempty"`
	// Kind is "definition" when the matched line is definition-shaped
	// (func/def/class/type per language family) — lets an agent split
	// definitions from references and call sites without a second pass.
	Kind string `json:"kind,omitempty"`
	// Captures holds structural hole bindings (-S): hole name → matched
	// text. Anonymous :[_] holes are omitted.
	Captures map[string]string `json:"captures,omitempty"`
	// Truncated marks a --block snippet cut at the size cap; the span
	// still covers exactly the emitted bytes.
	Truncated bool   `json:"truncated,omitempty"`
	Query     string `json:"query,omitempty"`
}

// jsonMatchCompact is the --compact match record: everything an agent
// needs to read and cite, nothing more.
type jsonMatchCompact struct {
	Type      string            `json:"type"`
	File      string            `json:"file,omitempty"`
	LineNum   int               `json:"line_number"`
	Text      string            `json:"text"`
	Region    string            `json:"region,omitempty"`
	Section   string            `json:"section,omitempty"`
	Scope     string            `json:"scope,omitempty"`
	Page      int               `json:"page,omitempty"`
	Kind      string            `json:"kind,omitempty"`
	Captures  map[string]string `json:"captures,omitempty"`
	Truncated bool              `json:"truncated,omitempty"`
	Query     string            `json:"query,omitempty"`
}

type jsonPos struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// RegisterQueries pre-seeds the per-query totals (--batch), so queries
// with zero hits still appear in the summary — for an agent, an explicit
// zero is a finding, not an omission.
func (f *JSONFormatter) RegisterQueries(queries []string) {
	for _, q := range queries {
		f.queryEntry(q)
	}
}

func (f *JSONFormatter) queryEntry(query string) *[2]int {
	if f.queryTotals == nil {
		f.queryTotals = make(map[string]*[2]int)
	}
	qt := f.queryTotals[query]
	if qt == nil {
		qt = &[2]int{}
		f.queryTotals[query] = qt
		f.queryOrder = append(f.queryOrder, query)
	}
	return qt
}

// AddSuppressedLines records lines that were found but suppressed
// before formatting (--collapse), keeping the summary's `lines` field a
// true total. The summary reports `shown_lines` separately when they
// differ.
func (f *JSONFormatter) AddSuppressedLines(n int) {
	f.lines += n
	f.suppressedLines += n
}

// tally records emitted totals (overall and per batch query). A file
// split across consecutive calls (budget chunking) counts once.
func (f *JSONFormatter) tally(file, query string, lines int) {
	newFile := file != f.lastFile || query != f.lastQuery
	f.lastFile, f.lastQuery = file, query
	if newFile {
		f.files++
	}
	f.lines += lines
	if query == "" {
		return
	}
	qt := f.queryEntry(query)
	if newFile {
		qt[0]++
	}
	qt[1] += lines
}

func (f *JSONFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	// Unreadable files shrink the corpus; that belongs in the stream,
	// not just on stderr — an agent must never mistake "couldn't read"
	// for "no matches".
	if result.Err != nil {
		f.errs++
		buf = append(buf, `{"type":"error","file":`...)
		buf = appendJSONString(buf, result.FilePath)
		buf = append(buf, `,"error":`...)
		buf = appendJSONString(buf, result.Err.Error())
		buf = append(buf, "}\n"...)
		return buf
	}
	if !result.HasMatch() {
		return buf
	}

	// -l: one object per matching file; the dummy MatchSet carries no
	// line data, so a match object would be a broken citation.
	if f.FilesOnly {
		buf = append(buf, `{"type":"file","file":`...)
		buf = appendJSONString(buf, result.FilePath)
		if result.Query != "" {
			buf = append(buf, `,"query":`...)
			buf = appendJSONString(buf, result.Query)
		}
		buf = append(buf, "}\n"...)
		f.tally(result.FilePath, result.Query, 0)
		return buf
	}

	// -c: one count object per matching file.
	if f.CountOnly {
		count := result.Count()
		buf = append(buf, `{"type":"count","file":`...)
		buf = appendJSONString(buf, result.FilePath)
		buf = append(buf, `,"count":`...)
		buf = strconv.AppendInt(buf, int64(count), 10)
		if result.Query != "" {
			buf = append(buf, `,"query":`...)
			buf = appendJSONString(buf, result.Query)
		}
		buf = append(buf, "}\n"...)
		f.tally(result.FilePath, result.Query, count)
		return buf
	}

	ms := &result.MatchSet
	emitted := 0
	for i := range ms.Matches {
		m := &ms.Matches[i]
		// Skip group separators (LineStart < 0); context lines (-C) are
		// emitted as their own record type so JSON mode has parity with
		// plain mode (report bug 10).
		if m.LineStart < 0 {
			continue
		}

		lineBytes := ms.Data[m.LineStart : m.LineStart+m.LineLen]
		positions := ms.MatchPositions(i)

		jm := jsonMatch{
			Type:       "match",
			File:       result.FilePath,
			LineNum:    m.LineNum,
			ByteOffset: m.ByteOffset,
			Text:       string(lineBytes),
			Truncated:  m.Truncated,
			Query:      result.Query,
		}
		// Explicit -M: window the displayed text only; span/region below
		// still cover the full line.
		if f.MaxColumns > 0 && len(lineBytes) > f.MaxColumns {
			winStart, winEnd := truncateWindow(lineBytes, positions, f.MaxColumns)
			jm.Text = string(lineBytes[winStart:winEnd])
			jm.Truncated = true
			positions = clipPositions(positions, winStart, winEnd)
		}
		if m.IsContext {
			jm.Type = "context"
		} else if IsDefinitionLine(firstLine(ms.Data, m.LineStart, m.LineLen), result.FilePath) {
			jm.Kind = "definition"
		}

		span := [2]int64{m.ByteOffset, m.ByteOffset + int64(m.LineLen)}
		jm.Span = &span
		jm.Region = result.FilePath + "@" +
			strconv.FormatInt(span[0], 10) + "-" + strconv.FormatInt(span[1], 10)
		if !m.IsContext {
			if f.Sections {
				if h := sectionHeading(ms.Data, m.LineStart); h != nil {
					jm.Section = string(h)
				}
			}
			if f.Scope {
				if s := enclosingScope(ms.Data, m.LineStart, result.FilePath); s != nil {
					jm.Scope = string(s)
				}
			}
			if f.Sections || f.Scope {
				jm.Page = nearestPage(ms.Data, m.LineStart)
			}
			if caps := ms.MatchCaptures(i); len(caps) > 0 {
				jm.Captures = make(map[string]string, len(caps))
				for _, c := range caps {
					if c.Name != "_" {
						jm.Captures[c.Name] = string(ms.Data[c.Start:c.End])
					}
				}
			}
			emitted++

			if len(positions) > 0 {
				jm.Matches = make([]jsonPos, len(positions))
				for j, pos := range positions {
					jm.Matches[j] = jsonPos{Start: pos[0], End: pos[1]}
				}
			}
		}
		var data []byte
		if f.Compact {
			data, _ = json.Marshal(jsonMatchCompact{
				Type: jm.Type, File: jm.File, LineNum: jm.LineNum,
				Text: jm.Text, Region: jm.Region, Section: jm.Section,
				Scope: jm.Scope, Page: jm.Page, Kind: jm.Kind,
				Captures: jm.Captures, Truncated: jm.Truncated,
				Query: jm.Query,
			})
		} else {
			data, _ = json.Marshal(jm)
		}
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	if emitted > 0 {
		f.tally(result.FilePath, result.Query, emitted)
	}
	return buf
}

// Finish appends the exact-totals summary trailer. In -l mode line counts
// are unknown, so "lines" is omitted. Batch runs additionally carry
// per-query totals in first-seen order.
func (f *JSONFormatter) Finish(buf []byte) []byte {
	buf = append(buf, `{"type":"summary","files":`...)
	buf = strconv.AppendInt(buf, int64(f.files), 10)
	if !f.FilesOnly {
		buf = append(buf, `,"lines":`...)
		buf = strconv.AppendInt(buf, int64(f.lines), 10)
		if f.suppressedLines > 0 {
			buf = append(buf, `,"shown_lines":`...)
			buf = strconv.AppendInt(buf, int64(f.lines-f.suppressedLines), 10)
		}
	}
	buf = append(buf, `,"errors":`...)
	buf = strconv.AppendInt(buf, int64(f.errs), 10)
	if len(f.queryOrder) > 0 {
		buf = append(buf, `,"queries":[`...)
		for i, q := range f.queryOrder {
			if i > 0 {
				buf = append(buf, ',')
			}
			qt := f.queryTotals[q]
			buf = append(buf, `{"query":`...)
			buf = appendJSONString(buf, q)
			buf = append(buf, `,"files":`...)
			buf = strconv.AppendInt(buf, int64(qt[0]), 10)
			if !f.FilesOnly {
				buf = append(buf, `,"lines":`...)
				buf = strconv.AppendInt(buf, int64(qt[1]), 10)
			}
			buf = append(buf, '}')
		}
		buf = append(buf, ']')
	}
	buf = append(buf, "}\n"...)
	return buf
}

// firstLine returns the first line of the snippet at [start, start+n)
// — for multi-line blocks (-U, --structural, --block) the definition
// classification looks at the block's opening line.
func firstLine(data []byte, start, n int) []byte {
	line := data[start : start+n]
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		return line[:i]
	}
	return line
}

// appendJSONString appends s as a JSON string literal.
func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			buf = append(buf, '\\', c)
		case c == '\n':
			buf = append(buf, '\\', 'n')
		case c == '\t':
			buf = append(buf, '\\', 't')
		case c == '\r':
			buf = append(buf, '\\', 'r')
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

// Ensure JSONFormatter implements Formatter and Finisher.
var _ Formatter = (*JSONFormatter)(nil)
var _ Finisher = (*JSONFormatter)(nil)
