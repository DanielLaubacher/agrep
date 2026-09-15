package output

// --block: emit each match as its whole enclosing definition block —
// the function/class body in code, the section in Markdown — instead
// of just the matching line. Replaces the agent's `--get-region
// --expand N` guess-loop with one exact fetch: the emitted span/region
// covers exactly the block, so citations round-trip unchanged.
// Implemented as a formatter wrapper that rewrites match extents before
// the base formatter runs; multiple matches in one block dedupe into a
// single emission.

import (
	"bytes"

	"github.com/DanielLaubacher/agrep/internal/lang"
	"github.com/DanielLaubacher/agrep/internal/matcher"
)

// blockMaxBytes caps an emitted block; larger blocks are cut at a line
// boundary and flagged truncated (the span still covers exactly the
// emitted bytes).
const blockMaxBytes = 32 * 1024

type BlockFormatter struct {
	inner Formatter
}

func NewBlockFormatter(inner Formatter) *BlockFormatter {
	return &BlockFormatter{inner: inner}
}

// blockBounds resolves the enclosing block of the line at lineStart:
// [start, end) exclusive of the trailing newline. ok is false when the
// family has no block notion (Generic) or no enclosing definition
// exists — the match then falls through unchanged.
func blockBounds(data []byte, lineStart int, path string) (int, int, bool, bool) {
	fam := lang.ByPath(path)
	if fam == lang.Generic {
		return 0, 0, false, false
	}

	if fam == lang.Markdown {
		h, hs := sectionHeadingAt(data, lineStart)
		if h == nil {
			return 0, 0, false, false
		}
		// Section runs to the next heading line or EOF.
		end := len(data)
		pos := lineEnd(data, hs)
		for pos < len(data) {
			ls := pos + 1
			if ls < len(data) && data[ls] == '#' {
				end = ls
				break
			}
			pos = lineEnd(data, ls)
		}
		return clampBlock(data, hs, end)
	}

	_, defStart, ok := enclosingScopeAt(data, lineStart, path)
	if !ok {
		return 0, 0, false, false
	}

	switch fam {
	case lang.Python, lang.Ruby:
		// Indentation rule: the block ends before the first non-blank
		// line at or below the definition's indent.
		defIndent := lineIndent(data, defStart)
		end := len(data)
		pos := lineEnd(data, defStart)
		for pos < len(data) {
			ls := pos + 1
			le := lineEnd(data, ls)
			trimmed := bytes.TrimSpace(data[ls:le])
			if len(trimmed) > 0 && lineIndent(data, ls) <= defIndent && ls > lineStart {
				end = ls
				break
			}
			pos = le
		}
		return clampBlock(data, defStart, end)
	default:
		// Brace rule: from the definition line, balance the first '{'
		// to its close (strings and comments are atoms), then run to
		// that line's end. A ';' first (a declaration) or no brace
		// within a sane distance means no body to extract.
		spec := fam.Spec()
		pos := defStart
		limit := min(len(data), defStart+4096)
		for pos < limit && data[pos] != '{' && data[pos] != ';' {
			if j, ok := lang.SkipAtom(spec, data, pos); ok && j > pos {
				pos = j
				continue
			}
			pos++
		}
		if pos >= limit || data[pos] == ';' {
			return 0, 0, false, false
		}
		depth := 0
		for pos < len(data) {
			if j, ok := lang.SkipAtom(spec, data, pos); ok && j > pos {
				pos = j
				continue
			}
			switch data[pos] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return clampBlock(data, defStart, lineEnd(data, pos)+1)
				}
			}
			pos++
		}
		return clampBlock(data, defStart, len(data))
	}
}

// clampBlock applies the size cap and strips trailing newlines; end is
// snapped back to a line boundary when cut.
func clampBlock(data []byte, start, end int) (int, int, bool, bool) {
	if end > len(data) {
		end = len(data)
	}
	truncated := false
	if end-start > blockMaxBytes {
		end = start + blockMaxBytes
		if i := bytes.LastIndexByte(data[start:end], '\n'); i > 0 {
			end = start + i
		}
		truncated = true
	}
	for end > start && data[end-1] == '\n' {
		end--
	}
	if end <= start {
		return 0, 0, false, false
	}
	return start, end, truncated, true
}

func lineEnd(data []byte, pos int) int {
	for pos < len(data) && data[pos] != '\n' {
		pos++
	}
	return pos
}

func lineIndent(data []byte, lineStart int) int {
	i := lineStart
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	return i - lineStart
}

func (f *BlockFormatter) Format(buf []byte, result Result, multiFile bool) []byte {
	ms := &result.MatchSet
	if result.Err != nil || len(ms.Matches) == 0 {
		return f.inner.Format(buf, result, multiFile)
	}

	out := matcher.MatchSet{Data: ms.Data, Captures: ms.Captures}
	lastStart := -1
	changed := false
	for i := range ms.Matches {
		m := ms.Matches[i]
		if m.IsContext || m.LineStart < 0 {
			out.Matches = append(out.Matches, m)
			continue
		}
		bs, be, trunc, ok := blockBounds(ms.Data, m.LineStart, result.FilePath)
		// Safety net: the block must contain the match's line, or the
		// bounds heuristic missed (unmodeled syntax) — pass through.
		if ok && (bs > m.LineStart || be < m.LineStart) {
			ok = false
		}
		if !ok {
			// No block notion here: pass the match through, rebasing
			// nothing but keeping its positions.
			m.PosIdx, m.PosCount = rebase(&out, ms, i, m.LineStart, m.LineLen)
			out.Matches = append(out.Matches, m)
			lastStart = -1
			continue
		}
		changed = true
		if bs == lastStart {
			// Same block as the previous match: merge highlights.
			last := &out.Matches[len(out.Matches)-1]
			pi, pc := rebase(&out, ms, i, bs, last.LineLen)
			if last.PosCount == 0 {
				last.PosIdx = pi
			}
			last.PosCount += pc
			last.CapCount += m.CapCount
			continue
		}
		lineNum := m.LineNum
		if lineNum > 0 {
			lineNum -= bytes.Count(ms.Data[bs:m.LineStart], []byte{'\n'})
		}
		pi, pc := rebase(&out, ms, i, bs, be-bs)
		out.Matches = append(out.Matches, matcher.Match{
			LineNum:    lineNum,
			LineStart:  bs,
			LineLen:    be - bs,
			ByteOffset: int64(bs),
			PosIdx:     pi,
			PosCount:   pc,
			CapIdx:     m.CapIdx,
			CapCount:   m.CapCount,
			Truncated:  trunc,
		})
		lastStart = bs
	}

	if !changed {
		return f.inner.Format(buf, result, multiFile)
	}
	rewritten := result
	rewritten.MatchSet = out
	return f.inner.Format(buf, rewritten, multiFile)
}

// rebase copies match i's highlight positions into out, re-anchored to
// the new snippet start, clipped to its length.
func rebase(out *matcher.MatchSet, ms *matcher.MatchSet, i int, newStart, newLen int) (int, int) {
	src := ms.MatchPositions(i)
	idx := len(out.Positions)
	old := ms.Matches[i].LineStart
	for _, p := range src {
		s := old + p[0] - newStart
		e := old + p[1] - newStart
		if e <= 0 || s >= newLen {
			continue
		}
		if s < 0 {
			s = 0
		}
		if e > newLen {
			e = newLen
		}
		out.Positions = append(out.Positions, [2]int{s, e})
	}
	return idx, len(out.Positions) - idx
}

// Finish and RegisterQueries forward through the wrapper chain.
func (f *BlockFormatter) Finish(buf []byte) []byte {
	if fin, ok := f.inner.(Finisher); ok {
		return fin.Finish(buf)
	}
	return buf
}

func (f *BlockFormatter) RegisterQueries(queries []string) {
	if qr, ok := f.inner.(QueryRegistrar); ok {
		qr.RegisterQueries(queries)
	}
}

var _ Formatter = (*BlockFormatter)(nil)
var _ Finisher = (*BlockFormatter)(nil)
