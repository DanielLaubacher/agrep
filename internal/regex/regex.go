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

	// allowedBytes is the set of bytes any match can contain (union of all
	// NFA consuming transitions). Used to bound rare-byte verify windows.
	allowedBytes [256]bool
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
	rx.allowedBytes = computeAllowedBytes(nfa)

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

	// Precompute every reachable transition at compile time. After this the
	// DFAs are immutable, which makes Regexp safe for concurrent use (lazy
	// computation would race when one Regexp is shared across goroutines).
	// On cache overflow, fall back to the PikeVM (allocates per call, safe).
	rx.searchDFA.warmStart()
	if !rx.searchDFA.precomputeAll() || !rx.fwdDFA.precomputeAll() {
		rx.fwdDFA = nil
		rx.searchDFA = nil
		rx.engineType = enginePikeVM
		rx.prefilter = extractPrefilter(pattern, flags)
		return rx, nil
	}
	rx.searchDFA.precomputed = true

	// Extract prefilter literals
	rx.prefilter = extractPrefilter(pattern, flags)

	// Heuristic: if the search DFA's SIMD range scan is already selective
	// (few start bytes), a rare-byte prefilter adds overhead (line extraction
	// + per-line DFA calls) without benefit. Only keep rare-byte prefilter
	// when start ranges are wide (>40 live bytes = >15% of byte space).
	if rx.prefilter != nil && rx.prefilter.hasRareByte {
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

// CanMatchNewline reports whether any match can contain a '\n' byte.
// When false, matches never span lines, so a buffer may be searched in
// line-aligned chunks (in parallel) without missing or splitting matches.
func (re *Regexp) CanMatchNewline() bool {
	return re.allowedBytes['\n']
}

// Match reports whether the byte slice b contains any match of the regexp.
func (re *Regexp) Match(b []byte) bool {
	if re.prefilter != nil && len(b) > 0 {
		// Candidate-driven: SIMD scan for the required literal/byte and
		// verify only around hits. Far cheaper than walking the forward
		// DFA over the whole buffer when the prefilter is selective.
		return re.findIndexPrefiltered(b)[0] >= 0
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

// FindAllIndexFunc streams all non-overlapping match locations to yield, in
// order; yield returning false stops the search. Streaming lets the caller
// process each match (line extraction, newline counting, formatting) while
// the surrounding bytes are still cache-hot from the scan — on buffers larger
// than L3 this avoids a second cold pass over the data.
func (re *Regexp) FindAllIndexFunc(b []byte, yield func(start, end int) bool) {
	if re.prefilter != nil && len(b) > 0 {
		pf := re.prefilter
		switch {
		case pf.hasRareByte:
			re.findAllIndexRareByteFunc(b, yield)
		case pf.primaryIsPrefix && re.engineType == engineDFA:
			re.findAllIndexPrefixLitFunc(b, yield)
		default:
			re.findAllIndexPrefilteredFunc(b, yield)
		}
		return
	}

	if re.engineType == engineDFA {
		re.searchDFA.findAllIndexFunc(b, yield)
		return
	}

	// Cold engines (literal, PikeVM): collect then replay. These either
	// stream internally already (literal) or are rare fallbacks.
	for _, loc := range re.FindAllIndex(b, -1) {
		if !yield(loc[0], loc[1]) {
			return
		}
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

// nextPrimary returns the position of the next primary-literal hit at or
// after pos, or -1.
func (re *Regexp) nextPrimary(data []byte, pos int) int {
	pf := re.prefilter
	if pos >= len(data) {
		return -1
	}
	var idx int
	if pf.primaryCI {
		idx = simd.IndexCaseInsensitive(data[pos:], pf.primary)
	} else {
		idx = bytes.Index(data[pos:], pf.primary)
	}
	if idx < 0 {
		return -1
	}
	return pos + idx
}

// lineBoundsAround returns [start, end) of the line containing off.
func lineBoundsAround(data []byte, off int) (int, int) {
	start := 0
	if off > 0 {
		if i := bytes.LastIndexByte(data[:off], '\n'); i >= 0 {
			start = i + 1
		}
	}
	end := len(data)
	if i := bytes.IndexByte(data[off:], '\n'); i >= 0 {
		end = off + i
	}
	return start, end
}

// rareByteWindow returns the verify window [left, right) for a rare-byte hit
// at off. Every byte inside a match is consumable by the pattern, so a match
// containing off lies entirely within the maximal run of allowed bytes
// around it — walking outward over allowed bytes bounds the window on both
// sides. This keeps the DFA from re-scanning from every plausible start
// across the rest of the line.
func (re *Regexp) rareByteWindow(data []byte, off int) (int, int) {
	allowed := &re.allowedBytes
	left := off
	for left > 0 {
		b := data[left-1]
		if b == '\n' || !allowed[b] {
			break
		}
		left--
	}
	right := off + 1
	for right < len(data) {
		b := data[right]
		if b == '\n' || !allowed[b] {
			break
		}
		right++
	}
	return left, right
}

func (re *Regexp) findIndexPrefiltered(data []byte) [2]int {
	pf := re.prefilter

	if pf.hasRareByte {
		return re.findIndexRareByte(data)
	}

	anchored := pf.primaryIsPrefix && re.engineType == engineDFA
	pos := 0
	for {
		hit := re.nextPrimary(data, pos)
		if hit < 0 {
			return [2]int{-1, -1}
		}

		if anchored {
			// Every match starts with the literal: verify directly at the hit.
			if m := re.searchDFA.tryMatchAt(data, hit); m[0] >= 0 {
				return m
			}
			pos = hit + 1
			continue
		}

		// Mid-pattern literal: verify the containing line. A failed verify
		// clears the whole line (extras are position-monotone and the DFA
		// scanned the full line), so skip straight past it.
		lineStart, lineEnd := lineBoundsAround(data, hit)
		line := data[lineStart:lineEnd]
		if re.checkExtras(line, hit-lineStart+len(pf.primary)) {
			var m [2]int
			switch re.engineType {
			case engineDFA:
				m = re.searchDFA.findIndex(line)
			default:
				m = re.vm.findIndex(line)
			}
			if m[0] >= 0 {
				return [2]int{lineStart + m[0], lineStart + m[1]}
			}
		}
		pos = lineEnd + 1
	}
}

func (re *Regexp) findIndexRareByte(data []byte) [2]int {
	rb := re.prefilter.rareByte
	pos := 0
	for pos < len(data) {
		idx := bytes.IndexByte(data[pos:], rb)
		if idx < 0 {
			break
		}
		off := pos + idx
		left, right := re.rareByteWindow(data, off)

		window := data[left:right]
		var m [2]int
		switch re.engineType {
		case engineDFA:
			m = re.searchDFA.findIndex(window)
		default:
			m = re.vm.findIndex(window)
		}
		if m[0] >= 0 {
			return [2]int{left + m[0], left + m[1]}
		}
		pos = right
	}
	return [2]int{-1, -1}
}

func (re *Regexp) findAllIndexRareByte(data []byte, n int) [][2]int {
	var results [][2]int
	re.findAllIndexRareByteFunc(data, func(s, e int) bool {
		results = append(results, [2]int{s, e})
		return n < 0 || len(results) < n
	})
	return results
}

func (re *Regexp) findAllIndexRareByteFunc(data []byte, yield func(s, e int) bool) {
	rb := re.prefilter.rareByte
	pos := 0

	for pos < len(data) {
		idx := bytes.IndexByte(data[pos:], rb)
		if idx < 0 {
			break
		}
		off := pos + idx
		left, right := re.rareByteWindow(data, off)

		window := data[left:right]
		var lineLocs [][2]int
		switch re.engineType {
		case engineDFA:
			lineLocs = re.searchDFA.findAllIndex(window, -1)
		default:
			lineLocs = re.vm.findAllIndex(window, -1)
		}
		for _, loc := range lineLocs {
			if !yield(left+loc[0], left+loc[1]) {
				return
			}
		}
		// Every match overlapping this allowed-byte run has been found;
		// resume the rare-byte scan just past it (later hits on the same
		// line get their own runs).
		pos = right
	}
}

func (re *Regexp) findAllIndexPrefiltered(data []byte, n int) [][2]int {
	pf := re.prefilter

	if pf.hasRareByte {
		return re.findAllIndexRareByte(data, n)
	}

	if pf.primaryIsPrefix && re.engineType == engineDFA {
		var results [][2]int
		re.findAllIndexPrefixLitFunc(data, func(s, e int) bool {
			results = append(results, [2]int{s, e})
			return n < 0 || len(results) < n
		})
		return results
	}

	var results [][2]int
	re.findAllIndexPrefilteredFunc(data, func(s, e int) bool {
		results = append(results, [2]int{s, e})
		return n < 0 || len(results) < n
	})
	return results
}

// findAllIndexPrefilteredFunc is the mid-pattern-literal streaming core:
// SIMD-scan for the literal, verify the containing line, yield its matches.
func (re *Regexp) findAllIndexPrefilteredFunc(data []byte, yield func(s, e int) bool) {
	pf := re.prefilter
	pos := 0
	for {
		hit := re.nextPrimary(data, pos)
		if hit < 0 {
			return
		}

		lineStart, lineEnd := lineBoundsAround(data, hit)
		line := data[lineStart:lineEnd]
		if re.checkExtras(line, hit-lineStart+len(pf.primary)) {
			var lineLocs [][2]int
			switch re.engineType {
			case engineDFA:
				lineLocs = re.searchDFA.findAllIndex(line, -1)
			default:
				lineLocs = re.vm.findAllIndex(line, -1)
			}
			for _, loc := range lineLocs {
				if !yield(lineStart+loc[0], lineStart+loc[1]) {
					return
				}
			}
		}
		pos = lineEnd + 1
	}
}

// findAllIndexPrefixLitFunc handles patterns whose every match begins with
// the primary literal: SIMD-scan for the literal and run the DFA anchored at
// each hit. No line extraction, no per-start-byte rescanning.
func (re *Regexp) findAllIndexPrefixLitFunc(data []byte, yield func(s, e int) bool) {
	pos := 0
	for {
		hit := re.nextPrimary(data, pos)
		if hit < 0 {
			return
		}
		if m := re.searchDFA.tryMatchAt(data, hit); m[0] >= 0 {
			if !yield(m[0], m[1]) {
				return
			}
			pos = m[1]
			if pos == hit { // zero-width safety; cannot happen with a literal
				pos++
			}
		} else {
			pos = hit + 1
		}
	}
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
