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
	"strings"

	"github.com/DanielLaubacher/agrep/internal/lang"
	"github.com/DanielLaubacher/agrep/internal/matcher"
)

// blockMaxBytes caps an emitted block; larger blocks are cut at a line
// boundary and flagged truncated (the span still covers exactly the
// emitted bytes).
const blockMaxBytes = 32 * 1024

type BlockFormatter struct {
	inner Formatter
	// Cross-call dedupe state: the budget formatter may split one file's
	// matches across consecutive Format calls, so the last emitted block
	// per (file, query) must survive between calls or a block with
	// matches in two chunks would be emitted twice.
	lastChunk chunkKey
	lastBlock int
}

func NewBlockFormatter(inner Formatter) *BlockFormatter {
	return &BlockFormatter{inner: inner, lastBlock: -1}
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
		if isListingHeading(h) {
			// A Table of Contents (or similar pure-listing section) is
			// technically the "enclosing block", but dumping the whole
			// multi-hundred-line listing is a low-value citation — the
			// match is a title mentioned in passing, not content about
			// it (report bug 3). Fall through to the plain match line,
			// same as when a family has no block notion at all.
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

// isListingHeading reports whether a Markdown heading names a
// pure-listing section (a table of contents, index, or similar) whose
// body is a directory of titles rather than content about any one of
// them — a bad --block citation unit (report bug 3).
func isListingHeading(h []byte) bool {
	title := strings.ToLower(strings.TrimSpace(strings.TrimLeft(string(h), "# \t")))
	switch title {
	case "table of contents", "contents", "toc", "index":
		return true
	}
	return false
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
	lastFromPrevCall := false
	if f.lastChunk.sameAs(result.FilePath, result.Query) {
		lastStart = f.lastBlock
		lastFromPrevCall = lastStart >= 0
	}
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
			lastFromPrevCall = false
			continue
		}
		changed = true
		if bs == lastStart {
			if lastFromPrevCall {
				// Block already emitted by an earlier chunked call: the
				// match's line is on screen, nothing more to add.
				continue
			}
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
		lastFromPrevCall = false
	}

	f.lastChunk.set(result.FilePath, result.Query)
	f.lastBlock = lastStart

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

// FindNamedBlock resolves a named unit in data for scope-addressable
// --get-region: kind "func" finds a definition line (per the file's
// language family) containing name as a whole word and returns its
// whole block; kind "section" finds a Markdown heading containing name
// (case-insensitive) and returns the whole section. A "Parent/Child"
// section name additionally requires each ancestor heading (in order)
// to match the parent segments — only tried when the full name matches
// nothing, so headings that themselves contain '/' still resolve.
// ordinal selects the Nth candidate (1-based; 0 means first). Returns
// the block bounds, the 1-based line numbers of every candidate (the
// block is the ordinal-th), and whether anything matched.
func FindNamedBlock(data []byte, path, kind, name string, ordinal int) (start, end int, candidates []int, ok bool) {
	start, end, candidates, ok = findNamedBlock(data, path, kind, name, nil, ordinal)
	if !ok && kind == "section" && strings.Contains(name, "/") {
		parts := strings.Split(name, "/")
		parents := parts[:len(parts)-1]
		start, end, candidates, ok = findNamedBlock(data, path, kind, parts[len(parts)-1], parents, ordinal)
	}
	return start, end, candidates, ok
}

func findNamedBlock(data []byte, path, kind, name string, parents []string, ordinal int) (start, end int, candidates []int, ok bool) {
	fam := lang.ByPath(path)
	nameBytes := []byte(name)
	if ordinal < 1 {
		ordinal = 1
	}
	// Heading ancestry stack for Parent/Child section names: one entry
	// per heading level currently open, lowercased text.
	var stack []string
	var stackLevels []int

	lineNum := 0
	pos := 0
	for pos < len(data) {
		lineNum++
		le := lineEnd(data, pos)
		line := data[pos:le]

		var hit bool
		switch kind {
		case "section":
			if len(line) > 0 && line[0] == '#' {
				level := 0
				for level < len(line) && line[level] == '#' {
					level++
				}
				for len(stackLevels) > 0 && stackLevels[len(stackLevels)-1] >= level {
					stack = stack[:len(stack)-1]
					stackLevels = stackLevels[:len(stackLevels)-1]
				}
				lower := strings.ToLower(string(line))
				hit = strings.Contains(lower, strings.ToLower(name)) &&
					ancestorsMatch(stack, parents)
				stack = append(stack, lower)
				stackLevels = append(stackLevels, level)
			}
		default: // func
			_, trimmed := indentAndTrim(line)
			hit = len(trimmed) > 0 && !isCommentLine(trimmed) &&
				isDefLine(trimmed, fam) && containsWord(trimmed, nameBytes)
		}
		if hit {
			candidates = append(candidates, lineNum)
			if len(candidates) == ordinal {
				bs, be, _, bok := blockBounds(data, pos, path)
				if bok {
					start, end = bs, be
				} else {
					start, end = pos, le // no block notion: the line itself
				}
			}
		}
		pos = le + 1
	}
	return start, end, candidates, len(candidates) >= ordinal
}

// ancestorsMatch reports whether the parent segments appear, in order,
// among the open ancestor headings (case-insensitive substring each).
func ancestorsMatch(stack []string, parents []string) bool {
	if len(parents) == 0 {
		return true
	}
	i := 0
	for _, h := range stack {
		if i < len(parents) && strings.Contains(h, strings.ToLower(parents[i])) {
			i++
		}
	}
	return i == len(parents)
}

// containsWord reports whether name occurs in line bounded by
// non-identifier bytes.
func containsWord(line, name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for from := 0; ; {
		i := bytes.Index(line[from:], name)
		if i < 0 {
			return false
		}
		i += from
		before := i == 0 || !isIdentByte(line[i-1])
		afterIdx := i + len(name)
		after := afterIdx >= len(line) || !isIdentByte(line[afterIdx])
		if before && after {
			return true
		}
		from = i + 1
	}
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
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

// AddSuppressedLines forwards --collapse suppression accounting.
func (f *BlockFormatter) AddSuppressedLines(n int) {
	if sr, ok := f.inner.(suppressedReporter); ok {
		sr.AddSuppressedLines(n)
	}
}

var _ Formatter = (*BlockFormatter)(nil)
var _ Finisher = (*BlockFormatter)(nil)
