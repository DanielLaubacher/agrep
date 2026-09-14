package output

import (
	"strconv"

	"github.com/dl/gogrep/internal/matcher"
)

// separatorLine is the shared "--" separator text for context groups.
var separatorLine = []byte("--")

// TextFormatter formats results as human-readable text with optional color.
type TextFormatter struct {
	lineNumbers bool
	countOnly   bool
	filesOnly   bool
	useColor    bool
	maxColumns  int
	onlyMatch   bool // -o: output only matched text

	// Sections prints a "§ <heading>" group line whenever the enclosing
	// Markdown heading of the printed matches changes (--sections).
	Sections    bool
	lastSection string
	lastFile    string
}

// NewTextFormatter creates a TextFormatter.
func NewTextFormatter(lineNumbers bool, countOnly bool, filesOnly bool, useColor bool, maxColumns int, onlyMatch bool) *TextFormatter {
	return &TextFormatter{
		lineNumbers: lineNumbers,
		countOnly:   countOnly,
		filesOnly:   filesOnly,
		useColor:    useColor,
		maxColumns:  maxColumns,
		onlyMatch:   onlyMatch,
	}
}

func (f *TextFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	if f.filesOnly {
		if result.HasMatch() {
			if result.Query != "" {
				buf = append(buf, '[')
				buf = append(buf, result.Query...)
				buf = append(buf, "] "...)
			}
			buf = append(buf, result.FilePath...)
			buf = append(buf, '\n')
			return buf
		}
		return buf
	}

	if f.countOnly {
		count := result.Count()
		if count == 0 {
			return buf
		}
		if result.Query != "" {
			buf = append(buf, '[')
			buf = append(buf, result.Query...)
			buf = append(buf, "] "...)
		}
		if multiFile {
			buf = append(buf, result.FilePath...)
			buf = append(buf, ':')
		}
		buf = strconv.AppendInt(buf, int64(count), 10)
		buf = append(buf, '\n')
		return buf
	}

	ms := &result.MatchSet
	if f.onlyMatch {
		for i := range ms.Matches {
			buf = f.formatOnlyMatch(buf, result.FilePath, ms, i, multiFile)
		}
	} else {
		for i := range ms.Matches {
			if f.Sections {
				buf = f.formatSectionLine(buf, result.FilePath, ms, i)
			}
			buf = f.formatMatch(buf, result.FilePath, result.Query, ms, i, multiFile)
		}
	}
	return buf
}

// formatSectionLine emits a "§ heading" group line when the enclosing
// Markdown section of the match differs from the previously printed one.
func (f *TextFormatter) formatSectionLine(buf []byte, filePath string, ms *matcher.MatchSet, idx int) []byte {
	m := &ms.Matches[idx]
	if m.LineStart < 0 || m.IsContext {
		return buf
	}
	h := sectionHeading(ms.Data, m.LineStart)
	if h == nil {
		return buf
	}
	sec := string(h)
	if sec == f.lastSection && filePath == f.lastFile {
		return buf
	}
	f.lastSection = sec
	f.lastFile = filePath
	if f.useColor {
		buf = append(buf, ansiCyan...)
	}
	buf = append(buf, "§ "...)
	buf = append(buf, sec...)
	if f.useColor {
		buf = append(buf, ansiReset...)
	}
	buf = append(buf, '\n')
	return buf
}

func (f *TextFormatter) formatMatch(buf []byte, filePath string, query string, ms *matcher.MatchSet, idx int, multiFile bool) []byte {
	m := &ms.Matches[idx]

	// Resolve line bytes: separator sentinel or normal line
	var lineBytes []byte
	if m.LineStart < 0 {
		lineBytes = separatorLine
	} else {
		lineBytes = ms.Data[m.LineStart : m.LineStart+m.LineLen]
	}
	positions := ms.MatchPositions(idx)

	sep := ":"
	if m.IsContext {
		sep = "-"
	}

	// Batch query attribution
	if query != "" {
		buf = append(buf, '[')
		buf = append(buf, query...)
		buf = append(buf, "] "...)
	}

	// Filename prefix
	if multiFile {
		if f.useColor {
			buf = append(buf, ansiMagenta...)
			buf = append(buf, filePath...)
			buf = append(buf, ansiReset...)
			buf = append(buf, ansiCyan...)
			buf = append(buf, sep...)
			buf = append(buf, ansiReset...)
		} else {
			buf = append(buf, filePath...)
			buf = append(buf, sep...)
		}
	}

	// Line number
	if f.lineNumbers {
		if f.useColor {
			buf = append(buf, ansiGreen...)
			buf = strconv.AppendInt(buf, int64(m.LineNum), 10)
			buf = append(buf, ansiReset...)
			buf = append(buf, ansiCyan...)
			buf = append(buf, sep...)
			buf = append(buf, ansiReset...)
		} else {
			buf = strconv.AppendInt(buf, int64(m.LineNum), 10)
			buf = append(buf, sep...)
		}
	}

	// Truncate line content if needed, centering around the first match
	if f.maxColumns > 0 && len(lineBytes) > f.maxColumns {
		winStart, winEnd := truncateWindow(lineBytes, positions, f.maxColumns)
		lineBytes = lineBytes[winStart:winEnd]
		// Shift positions into the window and clip
		var clipped [][2]int
		for _, pos := range positions {
			s := pos[0] - winStart
			e := pos[1] - winStart
			if e <= 0 {
				continue
			}
			if s >= len(lineBytes) {
				break
			}
			if s < 0 {
				s = 0
			}
			if e > len(lineBytes) {
				e = len(lineBytes)
			}
			clipped = append(clipped, [2]int{s, e})
		}
		positions = clipped
	}

	// Line content with match highlighting
	if f.useColor && len(positions) > 0 {
		buf = f.highlightMatches(buf, lineBytes, positions)
	} else {
		buf = append(buf, lineBytes...)
	}
	buf = append(buf, '\n')
	return buf
}

// formatOnlyMatch outputs only the matched text portions, one per line.
// This implements grep -o behavior: each match position becomes its own output line.
func (f *TextFormatter) formatOnlyMatch(buf []byte, filePath string, ms *matcher.MatchSet, idx int, multiFile bool) []byte {
	m := &ms.Matches[idx]

	// Skip separators and context lines
	if m.LineStart < 0 || m.IsContext {
		return buf
	}

	lineBytes := ms.Data[m.LineStart : m.LineStart+m.LineLen]
	positions := ms.MatchPositions(idx)

	sep := ":"

	for _, pos := range positions {
		start, end := pos[0], pos[1]
		if start >= len(lineBytes) {
			break
		}
		if end > len(lineBytes) {
			end = len(lineBytes)
		}
		if start >= end {
			continue
		}
		matchText := lineBytes[start:end]

		// Filename prefix
		if multiFile {
			if f.useColor {
				buf = append(buf, ansiMagenta...)
				buf = append(buf, filePath...)
				buf = append(buf, ansiReset...)
				buf = append(buf, ansiCyan...)
				buf = append(buf, sep...)
				buf = append(buf, ansiReset...)
			} else {
				buf = append(buf, filePath...)
				buf = append(buf, sep...)
			}
		}

		// Line number
		if f.lineNumbers {
			if f.useColor {
				buf = append(buf, ansiGreen...)
				buf = strconv.AppendInt(buf, int64(m.LineNum), 10)
				buf = append(buf, ansiReset...)
				buf = append(buf, ansiCyan...)
				buf = append(buf, sep...)
				buf = append(buf, ansiReset...)
			} else {
				buf = strconv.AppendInt(buf, int64(m.LineNum), 10)
				buf = append(buf, sep...)
			}
		}

		// Output only the matched text
		if f.useColor {
			buf = append(buf, ansiBoldRed...)
			buf = append(buf, matchText...)
			buf = append(buf, ansiReset...)
		} else {
			buf = append(buf, matchText...)
		}
		buf = append(buf, '\n')
	}
	return buf
}

// truncateWindow computes a [start, end) byte window of maxCols bytes
// centered on the first match position.
func truncateWindow(line []byte, positions [][2]int, maxCols int) (int, int) {
	center := 0
	if len(positions) > 0 {
		center = (positions[0][0] + positions[0][1]) / 2
	}

	start := max(center-maxCols/2, 0)
	end := start + maxCols
	if end > len(line) {
		end = len(line)
		start = max(end-maxCols, 0)
	}
	return start, end
}

func (f *TextFormatter) highlightMatches(buf []byte, line []byte, positions [][2]int) []byte {
	prev := 0
	for _, pos := range positions {
		start, end := pos[0], pos[1]
		if start > len(line) {
			break
		}
		if end > len(line) {
			end = len(line)
		}
		if start > prev {
			buf = append(buf, line[prev:start]...)
		}
		buf = append(buf, ansiBoldRed...)
		buf = append(buf, line[start:end]...)
		buf = append(buf, ansiReset...)
		prev = end
	}
	if prev < len(line) {
		buf = append(buf, line[prev:]...)
	}
	return buf
}
