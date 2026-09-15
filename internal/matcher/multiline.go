package matcher

// MultilineMatcher (-U) lets a pattern match across line boundaries.
// The search side needs nothing new — every matcher already scans the
// whole buffer — the work is in extraction: each match's snippet spans
// from the start of its first line to the end of its last, so text
// output prints the whole block and JSON spans/regions cover it (a
// multiline citation re-fetches exactly like a single-line one).
//
// (?m) is enabled so ^ and $ anchor per line. Deliberately does NOT
// implement LineBounded: a chunked parallel scan could split a match.
//
// The engine is the internal DFA compiled in multiline mode — its literal
// prefilter only anchors at match starts or gates whole buffers, never
// confining verification to a line. The stdlib engine remains the
// fallback for constructs the internal one rejects and for non-ASCII
// case folding; it has no prefilter of its own under (?i), which made
// every lowercase -U query under smart-case a 3-4s whole-tree NFA walk
// (report bug 2). Falling back still gets a SIMD literal gate (below)
// when every pattern yields a required ASCII literal, which covers the
// non-ASCII case-insensitive patterns that can't use the fast DFA.

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/DanielLaubacher/agrep/internal/regex"
	"github.com/DanielLaubacher/agrep/internal/simd"
)

// mlEngine is what MultilineMatcher needs from a regex engine.
type mlEngine interface {
	Match(b []byte) bool
	FindAllIndex(b []byte, n int) [][2]int
}

// stdMLEngine adapts the stdlib engine to mlEngine.
type stdMLEngine struct{ re *regexp.Regexp }

func (e stdMLEngine) Match(b []byte) bool { return e.re.Match(b) }

func (e stdMLEngine) FindAllIndex(b []byte, n int) [][2]int {
	locs := e.re.FindAllIndex(b, n)
	if len(locs) == 0 {
		return nil
	}
	out := make([][2]int, len(locs))
	for i, l := range locs {
		out[i] = [2]int{l[0], l[1]}
	}
	return out
}

// literalGatedEngine wraps an mlEngine with a SIMD "may this buffer
// match at all" gate: a buffer containing none of the per-pattern
// required literals cannot match any OR'd pattern, so the (unprefiltered,
// possibly non-ASCII-case-folding) inner engine is skipped entirely. lits
// is empty only via mlLiteralPrefilter returning nil, in which case this
// wrapper is not used at all — see NewMultilineMatcher.
type literalGatedEngine struct {
	inner mlEngine
	lits  [][]byte // lowercased ASCII literals, one per OR'd pattern
}

func (e literalGatedEngine) mayMatch(b []byte) bool {
	for _, lit := range e.lits {
		if simd.IndexCaseInsensitive(b, lit) >= 0 {
			return true
		}
	}
	return false
}

func (e literalGatedEngine) Match(b []byte) bool {
	return e.mayMatch(b) && e.inner.Match(b)
}

func (e literalGatedEngine) FindAllIndex(b []byte, n int) [][2]int {
	if !e.mayMatch(b) {
		return nil
	}
	return e.inner.FindAllIndex(b, n)
}

// mlLiteralPrefilter returns one required ASCII literal per pattern
// (lowercased so simd.IndexCaseInsensitive can gate case-insensitively),
// or nil if any pattern has no extractable literal — a match of that
// pattern could occur without any literal present, so no sound "any
// literal present" gate exists and the caller must skip the fast path
// entirely rather than risk a missed match.
func mlLiteralPrefilter(patterns []string, fixed, ignoreCase bool) [][]byte {
	lits := make([][]byte, 0, len(patterns))
	for _, p := range patterns {
		var lit string
		if fixed {
			if !allASCII([]string{p}) {
				return nil
			}
			lit = p
			if ignoreCase {
				lit = strings.ToLower(lit)
			}
		} else {
			li, ok := extractLiteral(p, ignoreCase)
			if !ok {
				return nil
			}
			lit = li.literal
		}
		if len(lit) < minPrefilterLen {
			return nil
		}
		lits = append(lits, []byte(lit))
	}
	return lits
}

type MultilineMatcher struct {
	re           mlEngine
	needLineNums bool
}

// NewMultilineMatcher compiles patterns (OR'd) for cross-line matching.
func NewMultilineMatcher(patterns []string, fixed bool, ignoreCase bool, opts MatcherOpts) (*MultilineMatcher, error) {
	parts := make([]string, len(patterns))
	for i, p := range patterns {
		if fixed {
			parts[i] = regexp.QuoteMeta(p)
		} else {
			parts[i] = "(?:" + p + ")"
		}
	}
	pattern := "(?m)" + strings.Join(parts, "|")
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	// Non-ASCII -i needs full Unicode folding, which only the stdlib
	// engine provides (the internal one folds ASCII).
	if !ignoreCase || allASCII(patterns) {
		if re, err := regex.CompileMode(pattern, regex.ModeMultiline); err == nil {
			return &MultilineMatcher{re: re, needLineNums: opts.NeedLineNums}, nil
		}
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	var engine mlEngine = stdMLEngine{re}
	if lits := mlLiteralPrefilter(patterns, fixed, ignoreCase); lits != nil {
		engine = literalGatedEngine{inner: engine, lits: lits}
	}
	return &MultilineMatcher{re: engine, needLineNums: opts.NeedLineNums}, nil
}

func (m *MultilineMatcher) MatchExists(data []byte) bool {
	return m.re.Match(data)
}

// CountAll counts matches (not lines): a block match is one result.
func (m *MultilineMatcher) CountAll(data []byte) int {
	return len(m.re.FindAllIndex(data, -1))
}

func (m *MultilineMatcher) FindAll(data []byte) MatchSet {
	locs := m.re.FindAllIndex(data, -1)
	if len(locs) == 0 {
		return MatchSet{}
	}

	ms := MatchSet{Data: data}
	lineNum := 1
	prevOff := 0
	lastBlockStart := -1

	for _, loc := range locs {
		start, end := loc[0], loc[1]

		// Block start: beginning of the line containing the match start.
		blockStart := 0
		if i := bytes.LastIndexByte(data[:start], '\n'); i >= 0 {
			blockStart = i + 1
		}
		// Block end: end of the line containing the last matched byte.
		// A match whose final byte is the newline itself belongs to the
		// line it terminates, not the next one.
		scan := end
		if end > start && data[end-1] == '\n' {
			scan = end - 1
		}
		blockEnd := len(data)
		if i := bytes.IndexByte(data[scan:], '\n'); i >= 0 {
			blockEnd = scan + i
		}
		blockLen := blockEnd - blockStart

		if m.needLineNums {
			lineNum += bytes.Count(data[prevOff:start], []byte{'\n'})
			prevOff = start
		}

		pos := [2]int{start - blockStart, min(end-blockStart, blockLen)}
		if blockStart == lastBlockStart {
			last := &ms.Matches[len(ms.Matches)-1]
			ms.Positions = append(ms.Positions, pos)
			last.PosCount++
			// Later match may extend the shared block.
			if blockLen > last.LineLen {
				last.LineLen = blockLen
			}
			continue
		}
		ms.Positions = append(ms.Positions, pos)
		ms.Matches = append(ms.Matches, Match{
			LineNum:    lineNum,
			LineStart:  blockStart,
			LineLen:    blockLen,
			ByteOffset: int64(blockStart),
			PosIdx:     len(ms.Positions) - 1,
			PosCount:   1,
		})
		lastBlockStart = blockStart
	}
	return ms
}

func (m *MultilineMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	locs := m.re.FindAllIndex(line, -1)
	if len(locs) == 0 {
		return MatchSet{}, false
	}
	ms := MatchSet{Data: line}
	ms.Positions = make([][2]int, len(locs))
	for i, loc := range locs {
		ms.Positions[i] = [2]int{loc[0], loc[1]}
	}
	ms.Matches = []Match{{
		LineNum:    lineNum,
		LineStart:  0,
		LineLen:    len(line),
		ByteOffset: byteOffset,
		PosIdx:     0,
		PosCount:   len(locs),
	}}
	return ms, true
}

var _ Matcher = (*MultilineMatcher)(nil)
