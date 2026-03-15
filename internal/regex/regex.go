package regex

// Public API: stdlib-compatible regexp replacement.
//
// Usage:
//   re, err := regex.Compile(`\d{4}-\d{2}-\d{2}`)
//   matches := re.FindAllIndex(data, -1)
//
// Engine selection (automatic):
//   1. Pure literal → LiteralEngine (SIMD IndexAll)
//   2. Small NFA + no ambiguity → attempts lazy DFA
//   3. Default → LazyDFA with PikeVM fallback
//   4. Cache overflow → PikeVM (guaranteed linear)

import (
	"bytes"
	"regexp/syntax"
	"strings"

	"github.com/dl/gogrep/internal/simd"
)

// Regexp is a compiled regular expression.
// It is safe for concurrent use by multiple goroutines.
type Regexp struct {
	pattern    string
	nfa        *NFA
	fwdDFA     *forwardDFA // single-pass match-only DFA
	searchDFA  *searchDFA  // start-position-loop DFA for findIndex
	vm         *pikeVM
	prefilter  *prefilter
	flags      syntax.Flags
	engineType engineType
	literal    []byte
	literalCI  bool
}

type engineType uint8

const (
	engineDFA     engineType = iota
	enginePikeVM
	engineLiteral
)

// Compile parses a regular expression and returns a Regexp object.
func Compile(pattern string) (*Regexp, error) {
	return compile_pattern(pattern, syntax.Perl)
}

// CompilePOSIX parses a POSIX regular expression.
func CompilePOSIX(pattern string) (*Regexp, error) {
	return compile_pattern(pattern, syntax.POSIX)
}

// MustCompile is like Compile but panics on error.
func MustCompile(pattern string) *Regexp {
	re, err := Compile(pattern)
	if err != nil {
		panic("regex: Compile(" + quote(pattern) + "): " + err.Error())
	}
	return re
}

func compile_pattern(pattern string, baseFlags syntax.Flags) (*Regexp, error) {
	flags := baseFlags
	re, err := syntax.Parse(pattern, flags)
	if err != nil {
		return nil, err
	}
	re = re.Simplify()

	nfa := compile(re)
	vm := &pikeVM{nfa: nfa}

	rx := &Regexp{
		pattern: pattern,
		nfa:     nfa,
		vm:      vm,
		flags:   flags,
	}

	// Check if pattern is a pure literal
	if nfa.Flags&FlagLiteral != 0 && re.Op == syntax.OpLiteral {
		rx.engineType = engineLiteral
		lit := string(re.Rune)
		if re.Flags&syntax.FoldCase != 0 {
			lit = strings.ToLower(lit)
		}
		rx.literal = []byte(lit)
		rx.literalCI = re.Flags&syntax.FoldCase != 0
		return rx, nil
	}

	// Patterns with assertions fall back to PikeVM (DFA can't handle
	// position-dependent assertions). No prefilter — the prefilter path
	// extracts lines which changes assertion semantics ($, \b, etc.).
	if nfa.Flags&FlagHasAssert != 0 {
		rx.engineType = enginePikeVM
		return rx, nil
	}

	// Build both DFAs: forward (match-only, single-pass) and search (findIndex)
	rx.fwdDFA = newForwardDFA(nfa)
	rx.searchDFA = newSearchDFA(nfa)
	rx.engineType = engineDFA

	// Extract prefilter literals
	rx.prefilter = extractPrefilter(pattern, flags)

	// Heuristic: if the search DFA's SIMD range scan is already selective
	// (few start bytes), a rare-byte prefilter adds overhead (line extraction
	// + per-line DFA calls) without benefit. Only keep rare-byte prefilter
	// when start ranges are wide (>40 live bytes = >15% of byte space).
	if rx.prefilter != nil && rx.prefilter.hasRareByte {
		rx.searchDFA.warmStart()
		liveBytes := 0
		for b := 0; b < 256; b++ {
			if rx.searchDFA.canStart[b] {
				liveBytes++
			}
		}
		if liveBytes <= 40 {
			rx.prefilter = nil // search DFA's SIMD scan is sufficient
		}
	}

	return rx, nil
}

// String returns the pattern string.
func (re *Regexp) String() string {
	return re.pattern
}

// Match reports whether the byte slice b contains any match of the regexp.
func (re *Regexp) Match(b []byte) bool {
	if re.prefilter != nil && len(b) > 0 {
		if !re.prefilterCheck(b) {
			return false
		}
	}

	switch re.engineType {
	case engineLiteral:
		return re.literalMatch(b)
	case engineDFA:
		return re.fwdDFA.match(b)
	default:
		return re.vm.match(b)
	}
}

// MatchString reports whether the string s contains any match of the regexp.
func (re *Regexp) MatchString(s string) bool {
	return re.Match([]byte(s))
}

// Find returns the leftmost match in b, or nil if no match.
func (re *Regexp) Find(b []byte) []byte {
	loc := re.FindIndex(b)
	if loc[0] < 0 {
		return nil
	}
	return b[loc[0]:loc[1]]
}

// FindIndex returns the leftmost match location [start, end], or [-1, -1].
func (re *Regexp) FindIndex(b []byte) [2]int {
	if re.prefilter != nil && len(b) > 0 {
		return re.findIndexPrefiltered(b)
	}

	switch re.engineType {
	case engineLiteral:
		return re.literalFindIndex(b)
	case engineDFA:
		return re.searchDFA.findIndex(b)
	default:
		return re.vm.findIndex(b)
	}
}

// FindAll returns all non-overlapping matches in b.
func (re *Regexp) FindAll(b []byte, n int) [][]byte {
	locs := re.FindAllIndex(b, n)
	if locs == nil {
		return nil
	}
	result := make([][]byte, len(locs))
	for i, loc := range locs {
		result[i] = b[loc[0]:loc[1]]
	}
	return result
}

// FindAllIndex returns all non-overlapping match locations.
func (re *Regexp) FindAllIndex(b []byte, n int) [][2]int {
	if n == 0 {
		return nil
	}

	if re.prefilter != nil && len(b) > 0 {
		return re.findAllIndexPrefiltered(b, n)
	}

	switch re.engineType {
	case engineLiteral:
		return re.literalFindAllIndex(b, n)
	case engineDFA:
		return re.searchDFA.findAllIndex(b, n)
	default:
		return re.vm.findAllIndex(b, n)
	}
}

// FindString returns the leftmost match in s.
func (re *Regexp) FindString(s string) string {
	b := re.Find([]byte(s))
	if b == nil {
		return ""
	}
	return string(b)
}

// FindStringIndex returns the leftmost match location in s.
func (re *Regexp) FindStringIndex(s string) [2]int {
	return re.FindIndex([]byte(s))
}

// FindAllString returns all non-overlapping matches in s.
func (re *Regexp) FindAllString(s string, n int) []string {
	all := re.FindAll([]byte(s), n)
	if all == nil {
		return nil
	}
	result := make([]string, len(all))
	for i, b := range all {
		result[i] = string(b)
	}
	return result
}

// FindAllStringIndex returns all non-overlapping match locations in s.
func (re *Regexp) FindAllStringIndex(s string, n int) [][2]int {
	return re.FindAllIndex([]byte(s), n)
}

// FindAllStringSubmatch returns all matches with submatches.
func (re *Regexp) FindAllStringSubmatch(s string, n int) [][]string {
	// For now, delegate to FindAllString (no submatch extraction in DFA)
	all := re.FindAllString(s, n)
	if all == nil {
		return nil
	}
	result := make([][]string, len(all))
	for i, m := range all {
		result[i] = []string{m}
	}
	return result
}

// FindAllStringSubmatchIndex returns all match/submatch index pairs.
func (re *Regexp) FindAllStringSubmatchIndex(s string, n int) [][]int {
	locs := re.FindAllIndex([]byte(s), n)
	if locs == nil {
		return nil
	}
	result := make([][]int, len(locs))
	for i, loc := range locs {
		result[i] = []int{loc[0], loc[1]}
	}
	return result
}

// ReplaceAll replaces all matches with repl.
func (re *Regexp) ReplaceAll(src, repl []byte) []byte {
	locs := re.FindAllIndex(src, -1)
	if len(locs) == 0 {
		return src
	}

	var buf []byte
	last := 0
	for _, loc := range locs {
		buf = append(buf, src[last:loc[0]]...)
		buf = append(buf, repl...)
		last = loc[1]
	}
	buf = append(buf, src[last:]...)
	return buf
}

// ReplaceAllString replaces all matches in s with repl.
func (re *Regexp) ReplaceAllString(src, repl string) string {
	return string(re.ReplaceAll([]byte(src), []byte(repl)))
}

// Split splits s by matches of the regexp.
func (re *Regexp) Split(s string, n int) []string {
	if n == 0 {
		return nil
	}

	b := []byte(s)
	locs := re.FindAllIndex(b, n-1)
	if len(locs) == 0 {
		return []string{s}
	}

	result := make([]string, 0, len(locs)+1)
	last := 0
	for _, loc := range locs {
		result = append(result, s[last:loc[0]])
		last = loc[1]
	}
	result = append(result, s[last:])
	return result
}

// FindAllRuneIndex returns match positions as rune offsets.
func (re *Regexp) FindAllRuneIndex(s string, n int) [][2]int {
	byteLocs := re.FindAllIndex([]byte(s), n)
	if byteLocs == nil {
		return nil
	}

	result := make([][2]int, len(byteLocs))
	b := []byte(s)
	runePos := 0
	bytePos := 0

	locIdx := 0
	for locIdx < len(byteLocs) && bytePos <= len(b) {
		if bytePos == byteLocs[locIdx][0] {
			result[locIdx][0] = runePos
		}
		if bytePos == byteLocs[locIdx][1] {
			result[locIdx][1] = runePos
			locIdx++
			continue
		}
		if bytePos < len(b) {
			_, size := decodeRune(b[bytePos:])
			bytePos += size
			runePos++
		} else {
			break
		}
	}

	return result[:locIdx]
}

// --- Literal engine ---

func (re *Regexp) literalMatch(b []byte) bool {
	if re.literalCI {
		return simd.IndexCaseInsensitive(b, re.literal) >= 0
	}
	return bytes.Contains(b, re.literal)
}

func (re *Regexp) literalFindIndex(b []byte) [2]int {
	var idx int
	if re.literalCI {
		idx = simd.IndexCaseInsensitive(b, re.literal)
	} else {
		idx = bytes.Index(b, re.literal)
	}
	if idx < 0 {
		return [2]int{-1, -1}
	}
	return [2]int{idx, idx + len(re.literal)}
}

func (re *Regexp) literalFindAllIndex(b []byte, n int) [][2]int {
	var offsets []int
	if re.literalCI {
		offsets = simd.IndexAllCaseInsensitive(b, re.literal)
	} else {
		offsets = simd.IndexAll(b, re.literal)
	}
	if len(offsets) == 0 {
		return nil
	}

	limit := len(offsets)
	if n >= 0 && n < limit {
		limit = n
	}

	result := make([][2]int, limit)
	plen := len(re.literal)
	for i := 0; i < limit; i++ {
		result[i] = [2]int{offsets[i], offsets[i] + plen}
	}
	return result
}

// --- Prefilter-accelerated paths ---

func (re *Regexp) prefilterCheck(data []byte) bool {
	pf := re.prefilter
	if pf.hasRareByte {
		return simd.IndexByte(data, pf.rareByte) >= 0
	}
	var idx int
	if pf.primaryCI {
		idx = simd.IndexCaseInsensitive(data, pf.primary)
	} else {
		idx = simd.Index(data, pf.primary)
	}
	return idx >= 0
}

func (re *Regexp) findIndexPrefiltered(data []byte) [2]int {
	pf := re.prefilter

	if pf.hasRareByte {
		return re.findIndexRareByte(data)
	}

	// SIMD scan for primary literal
	var offsets []int
	if pf.primaryCI {
		offsets = simd.IndexAllCaseInsensitive(data, pf.primary)
	} else {
		offsets = simd.IndexAll(data, pf.primary)
	}

	if len(offsets) == 0 {
		return [2]int{-1, -1}
	}

	// For each candidate, find containing line and verify with engine
	for _, off := range offsets {
		// Find line boundaries
		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}

		line := data[lineStart:lineEnd]

		// Check extras
		posInLine := off - lineStart + len(pf.primary)
		if !re.checkExtras(line, posInLine) {
			continue
		}

		// Verify with engine on the line
		var match [2]int
		switch re.engineType {
		case engineDFA:
			match = re.searchDFA.findIndex(line)
		default:
			match = re.vm.findIndex(line)
		}
		if match[0] >= 0 {
			return [2]int{lineStart + match[0], lineStart + match[1]}
		}
	}

	return [2]int{-1, -1}
}

func (re *Regexp) findIndexRareByte(data []byte) [2]int {
	rb := re.prefilter.rareByte
	offsets := simd.IndexAll(data, []byte{rb})
	for _, off := range offsets {
		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}

		line := data[lineStart:lineEnd]
		var match [2]int
		switch re.engineType {
		case engineDFA:
			match = re.searchDFA.findIndex(line)
		default:
			match = re.vm.findIndex(line)
		}
		if match[0] >= 0 {
			return [2]int{lineStart + match[0], lineStart + match[1]}
		}
	}
	return [2]int{-1, -1}
}

func (re *Regexp) findAllIndexRareByte(data []byte, n int) [][2]int {
	rb := re.prefilter.rareByte
	offsets := simd.IndexAll(data, []byte{rb})
	if len(offsets) == 0 {
		return nil
	}

	var results [][2]int
	lastLineEnd := -1

	for _, off := range offsets {
		if n >= 0 && len(results) >= n {
			break
		}
		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}
		if lineStart <= lastLineEnd {
			continue
		}
		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}
		lastLineEnd = lineEnd

		line := data[lineStart:lineEnd]
		var lineLocs [][2]int
		switch re.engineType {
		case engineDFA:
			lineLocs = re.searchDFA.findAllIndex(line, -1)
		default:
			lineLocs = re.vm.findAllIndex(line, -1)
		}
		for _, loc := range lineLocs {
			if n >= 0 && len(results) >= n {
				break
			}
			results = append(results, [2]int{lineStart + loc[0], lineStart + loc[1]})
		}
	}
	return results
}

func (re *Regexp) findAllIndexPrefiltered(data []byte, n int) [][2]int {
	pf := re.prefilter

	if pf.hasRareByte {
		return re.findAllIndexRareByte(data, n)
	}

	var offsets []int
	if pf.primaryCI {
		offsets = simd.IndexAllCaseInsensitive(data, pf.primary)
	} else {
		offsets = simd.IndexAll(data, pf.primary)
	}

	if len(offsets) == 0 {
		return nil
	}

	var results [][2]int
	lastLineEnd := -1

	for _, off := range offsets {
		if n >= 0 && len(results) >= n {
			break
		}

		lineStart := 0
		if off > 0 {
			if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
				lineStart = i + 1
			}
		}

		// Dedup by line
		if lineStart <= lastLineEnd {
			continue
		}

		lineEnd := len(data)
		if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
			lineEnd = off + i
		}
		lastLineEnd = lineEnd

		line := data[lineStart:lineEnd]
		posInLine := off - lineStart + len(pf.primary)
		if !re.checkExtras(line, posInLine) {
			continue
		}

		// Find all matches on this line
		var lineLocs [][2]int
		switch re.engineType {
		case engineDFA:
			lineLocs = re.searchDFA.findAllIndex(line, -1)
		default:
			lineLocs = re.vm.findAllIndex(line, -1)
		}

		for _, loc := range lineLocs {
			if n >= 0 && len(results) >= n {
				break
			}
			results = append(results, [2]int{lineStart + loc[0], lineStart + loc[1]})
		}
	}

	return results
}

func (re *Regexp) checkExtras(line []byte, after int) bool {
	pf := re.prefilter
	pos := after
	for i, pat := range pf.extras {
		remaining := line[pos:]
		var idx int
		if pf.extrasCI[i] {
			idx = simd.IndexCaseInsensitive(remaining, pat)
		} else {
			idx = simd.Index(remaining, pat)
		}
		if idx < 0 {
			return false
		}
		pos += idx + len(pat)
	}
	return true
}

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
