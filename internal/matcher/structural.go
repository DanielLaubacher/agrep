package matcher

// StructuralMatcher (-S) implements comby-style structural templates:
// literal text plus :[name] holes. A hole matches lazily — as little as
// possible — within balanced delimiters, treating strings and comments
// as atoms (per the --lang family; the Generic family knows only
// delimiters). Whitespace in pattern literals matches any whitespace
// run, so `foo(:[a], :[b])` matches call sites however they're
// formatted, including across lines.
//
// No parser, no AST: this is a delimiter stack plus the lang.SkipAtom
// scanner — the comby insight that structural matching needs only
// "what nests, what's a string, what's a comment".
//
// Deliberately not LineBounded (matches cross lines); case-sensitive.

import (
	"bytes"
	"fmt"

	"github.com/DanielLaubacher/agrep/internal/lang"
	"github.com/DanielLaubacher/agrep/internal/simd"
)

// segment is one piece of a parsed template: a literal (hole == "") or
// a hole (lit == nil).
type segment struct {
	lit  []byte
	hole string
}

// parseStructural splits a template into alternating literal/hole
// segments. Rules: the template must begin with literal text (the
// SIMD anchor), holes may not be adjacent, and named holes must be
// unique (`_` is anonymous and may repeat).
func parseStructural(pattern string) ([]segment, error) {
	var segs []segment
	names := map[string]bool{}
	rest := []byte(bytes.TrimLeft([]byte(pattern), " \t\r\n"))
	for len(rest) > 0 {
		i := bytes.Index(rest, []byte(":["))
		if i < 0 {
			segs = append(segs, segment{lit: rest})
			break
		}
		if i > 0 {
			segs = append(segs, segment{lit: rest[:i]})
		}
		end := bytes.IndexByte(rest[i:], ']')
		if end < 0 {
			return nil, fmt.Errorf("unclosed hole %q", rest[i:])
		}
		name := string(rest[i+2 : i+end])
		if !validHoleName(name) {
			return nil, fmt.Errorf("invalid hole name %q (want :[identifier])", name)
		}
		if name != "_" {
			if names[name] {
				return nil, fmt.Errorf("duplicate hole name %q", name)
			}
			names[name] = true
		}
		if len(segs) == 0 {
			return nil, fmt.Errorf("template must begin with literal text before the first hole")
		}
		if segs[len(segs)-1].hole != "" {
			return nil, fmt.Errorf("holes :[%s] and :[%s] are adjacent; separate them with literal text", segs[len(segs)-1].hole, name)
		}
		segs = append(segs, segment{hole: name})
		rest = rest[i+end+1:]
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty template")
	}
	if segs[0].hole != "" || len(segs[0].lit) == 0 {
		return nil, fmt.Errorf("template must begin with literal text")
	}
	return segs, nil
}

func validHoleName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

type StructuralMatcher struct {
	segs         []segment
	spec         *lang.Spec
	anchor       []byte // first non-ws run of the first literal: the SIMD prefilter
	needLineNums bool
}

// NewStructuralMatcher compiles a template for the given language
// family.
func NewStructuralMatcher(pattern string, family lang.Lang, opts MatcherOpts) (*StructuralMatcher, error) {
	segs, err := parseStructural(pattern)
	if err != nil {
		return nil, err
	}
	first := segs[0].lit
	ws := bytes.IndexAny(first, " \t\r\n")
	anchor := first
	if ws > 0 {
		anchor = first[:ws]
	}
	return &StructuralMatcher{
		segs:         segs,
		spec:         family.Spec(),
		anchor:       anchor,
		needLineNums: opts.NeedLineNums,
	}, nil
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// matchLit matches a pattern literal at data[pos:]. A whitespace run in
// the literal matches one-or-more whitespace bytes in the data.
func matchLit(data []byte, pos int, lit []byte) (int, bool) {
	i := 0
	for i < len(lit) {
		if isWS(lit[i]) {
			for i < len(lit) && isWS(lit[i]) {
				i++
			}
			start := pos
			for pos < len(data) && isWS(data[pos]) {
				pos++
			}
			if pos == start {
				return 0, false
			}
			continue
		}
		if pos >= len(data) || data[pos] != lit[i] {
			return 0, false
		}
		pos++
		i++
	}
	return pos, true
}

// matchAt attempts the full template at data[start:]. Returns the match
// end and hole captures on success.
func (m *StructuralMatcher) matchAt(data []byte, start int) (int, []Capture, bool) {
	pos := start
	var caps []Capture

	for si := 0; si < len(m.segs); si++ {
		seg := &m.segs[si]
		if seg.hole == "" {
			np, ok := matchLit(data, pos, seg.lit)
			if !ok {
				return 0, nil, false
			}
			pos = np
			continue
		}

		// Hole: lazily consume balanced text until the next literal
		// matches at depth 0 (or EOF / an unbalanced closer for a
		// trailing hole).
		var next []byte
		if si+1 < len(m.segs) {
			next = m.segs[si+1].lit
		}
		holeStart := pos
		depth := 0
		for {
			if pos >= len(data) {
				if next == nil {
					caps = append(caps, Capture{Name: seg.hole, Start: holeStart, End: pos})
					return pos, caps, true
				}
				return 0, nil, false
			}
			if depth == 0 && next != nil {
				if np, ok := matchLit(data, pos, next); ok {
					caps = append(caps, Capture{Name: seg.hole, Start: holeStart, End: pos})
					pos = np
					si++ // the next literal segment is consumed
					goto nextSegment
				}
			}
			if j, ok := lang.SkipAtom(m.spec, data, pos); ok && j > pos {
				pos = j
				continue
			}
			c := data[pos]
			if lang.OpenDelim(c) {
				depth++
			} else if lang.CloseDelim(c) {
				if depth == 0 {
					// The hole may not escape its balanced region.
					if next == nil {
						caps = append(caps, Capture{Name: seg.hole, Start: holeStart, End: pos})
						return pos, caps, true
					}
					return 0, nil, false
				}
				depth--
			}
			pos++
		}
	nextSegment:
	}
	return pos, caps, true
}

// atomScanner tracks string/comment spans left-to-right so ascending
// candidate offsets can be classified in one linear pass total.
type atomScanner struct {
	spec    *lang.Spec
	data    []byte
	pos     int
	atomEnd int // end of the most recent atom seen (candidates before it are inside)
}

// inAtom reports whether target lies inside a string or comment.
// Targets must be queried in ascending order.
func (s *atomScanner) inAtom(target int) bool {
	if target < s.atomEnd {
		return true
	}
	for s.pos < target {
		if j, ok := lang.SkipAtom(s.spec, s.data, s.pos); ok {
			s.pos = j
			if j > target {
				s.atomEnd = j
				return true
			}
			continue
		}
		s.pos++
	}
	return false
}

// findAll streams every non-overlapping match to emit. Template text
// found inside a string or comment is not a match — the anchor
// prefilter can't know that, so candidates are atom-checked first.
func (m *StructuralMatcher) findAll(data []byte, emit func(start, end int, caps []Capture) bool) {
	offsets := simd.IndexAll(data, m.anchor)
	if len(offsets) == 0 {
		return
	}
	var atoms *atomScanner
	if len(m.spec.LineComments) > 0 || len(m.spec.Strings) > 0 || m.spec.BlockOpen != "" {
		atoms = &atomScanner{spec: m.spec, data: data}
	}
	lastEnd := 0
	for _, off := range offsets {
		if off < lastEnd {
			continue
		}
		if atoms != nil && atoms.inAtom(off) {
			continue
		}
		end, caps, ok := m.matchAt(data, off)
		if !ok {
			continue
		}
		if end <= off {
			end = off + 1
		}
		lastEnd = end
		if !emit(off, end, caps) {
			return
		}
	}
}

func (m *StructuralMatcher) MatchExists(data []byte) bool {
	found := false
	m.findAll(data, func(int, int, []Capture) bool {
		found = true
		return false
	})
	return found
}

// CountAll counts matches (not lines), like -U.
func (m *StructuralMatcher) CountAll(data []byte) int {
	n := 0
	m.findAll(data, func(int, int, []Capture) bool {
		n++
		return true
	})
	return n
}

func (m *StructuralMatcher) FindAll(data []byte) MatchSet {
	ms := MatchSet{Data: data}
	lineNum := 1
	prevOff := 0

	m.findAll(data, func(start, end int, caps []Capture) bool {
		// Block spans from the start of the match's first line to the
		// end of its last (same extraction as -U).
		blockStart := 0
		if i := bytes.LastIndexByte(data[:start], '\n'); i >= 0 {
			blockStart = i + 1
		}
		scan := end
		if end > start && data[end-1] == '\n' {
			scan = end - 1
		}
		blockEnd := len(data)
		if i := bytes.IndexByte(data[scan:], '\n'); i >= 0 {
			blockEnd = scan + i
		}

		if m.needLineNums {
			lineNum += bytes.Count(data[prevOff:start], []byte{'\n'})
			prevOff = start
		}

		ms.Positions = append(ms.Positions, [2]int{start - blockStart, min(end-blockStart, blockEnd-blockStart)})
		capIdx := len(ms.Captures)
		ms.Captures = append(ms.Captures, caps...)
		ms.Matches = append(ms.Matches, Match{
			LineNum:    lineNum,
			LineStart:  blockStart,
			LineLen:    blockEnd - blockStart,
			ByteOffset: int64(blockStart),
			PosIdx:     len(ms.Positions) - 1,
			PosCount:   1,
			CapIdx:     capIdx,
			CapCount:   len(caps),
		})
		return true
	})
	return ms
}

func (m *StructuralMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	ms := MatchSet{Data: line}
	m.findAll(line, func(start, end int, caps []Capture) bool {
		ms.Positions = append(ms.Positions, [2]int{start, end})
		capIdx := len(ms.Captures)
		ms.Captures = append(ms.Captures, caps...)
		ms.Matches = append(ms.Matches, Match{
			LineNum:    lineNum,
			LineStart:  0,
			LineLen:    len(line),
			ByteOffset: byteOffset,
			PosIdx:     len(ms.Positions) - 1,
			PosCount:   1,
			CapIdx:     capIdx,
			CapCount:   len(caps),
		})
		return true
	})
	return ms, ms.HasMatch()
}

var _ Matcher = (*StructuralMatcher)(nil)
