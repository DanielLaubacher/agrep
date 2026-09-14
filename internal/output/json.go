package output

import (
	"encoding/json"
	"strconv"
)

// JSONFormatter formats results as JSON Lines (one JSON object per match).
type JSONFormatter struct {
	// Sections annotates each match with its enclosing Markdown heading.
	Sections bool
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
	Query   string    `json:"query,omitempty"`
}

type jsonPos struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

func (f *JSONFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	ms := &result.MatchSet
	if len(ms.Matches) == 0 {
		return buf
	}

	for i := range ms.Matches {
		m := &ms.Matches[i]
		if m.IsContext {
			continue
		}

		var lineText string
		if m.LineStart >= 0 {
			lineText = string(ms.Data[m.LineStart : m.LineStart+m.LineLen])
		}

		jm := jsonMatch{
			Type:       "match",
			File:       result.FilePath,
			LineNum:    m.LineNum,
			ByteOffset: m.ByteOffset,
			Text:       lineText,
			Query:      result.Query,
		}

		if m.LineStart >= 0 {
			span := [2]int64{m.ByteOffset, m.ByteOffset + int64(m.LineLen)}
			jm.Span = &span
			jm.Region = result.FilePath + "@" +
				strconv.FormatInt(span[0], 10) + "-" + strconv.FormatInt(span[1], 10)
			if f.Sections {
				if h := sectionHeading(ms.Data, m.LineStart); h != nil {
					jm.Section = string(h)
				}
			}
		}

		positions := ms.MatchPositions(i)
		if len(positions) > 0 {
			jm.Matches = make([]jsonPos, len(positions))
			for j, pos := range positions {
				jm.Matches[j] = jsonPos{Start: pos[0], End: pos[1]}
			}
		}
		data, _ := json.Marshal(jm)
		buf = append(buf, data...)
		buf = append(buf, '\n')
	}
	return buf
}

// Ensure JSONFormatter implements Formatter.
var _ Formatter = (*JSONFormatter)(nil)
