package regex

// UTF-8 byte automaton helpers.
// Converts rune ranges into sequences of byte-level NFA transitions
// that correctly match UTF-8 encoded text without per-rune decoding.
//
// UTF-8 encoding structure:
//   U+0000..U+007F:    0xxxxxxx                          (1 byte)
//   U+0080..U+07FF:    110xxxxx 10xxxxxx                  (2 bytes)
//   U+0800..U+FFFF:    1110xxxx 10xxxxxx 10xxxxxx         (3 bytes)
//   U+10000..U+10FFFF: 11110xxx 10xxxxxx 10xxxxxx 10xxxxxx (4 bytes)

import "unicode/utf8"

// utf8Sequences returns the byte-level transitions needed to match
// a single rune range [lo, hi] in UTF-8. Returns a list of byte sequences,
// where each sequence is a list of byte ranges — one per UTF-8 byte position.
//
// For example, matching [U+0080, U+07FF] requires 2-byte sequences:
//   [[0xC2,0xDF], [0x80,0xBF]]
//
// This is the key insight from Rust's regex crate: rune classes become
// byte-level automaton fragments, so the DFA never decodes runes.
func utf8Sequences(lo, hi rune) [][]ByteRange {
	if lo > hi {
		return nil
	}

	// Clamp to valid Unicode
	if lo < 0 {
		lo = 0
	}
	if hi > utf8.MaxRune {
		hi = utf8.MaxRune
	}

	// Skip surrogates (U+D800..U+DFFF)
	if lo >= 0xD800 && lo <= 0xDFFF {
		lo = 0xE000
	}
	if hi >= 0xD800 && hi <= 0xDFFF {
		hi = 0xD7FF
	}
	if lo > hi {
		return nil
	}

	var result [][]ByteRange

	// Split the rune range by UTF-8 byte length boundaries
	// Each byte-length range is handled separately
	boundaries := []rune{0x80, 0x800, 0x10000}
	current := lo

	for _, boundary := range boundaries {
		if current >= boundary {
			continue
		}
		if hi < boundary {
			result = append(result, utf8SequencesForSize(current, hi)...)
			return result
		}
		result = append(result, utf8SequencesForSize(current, boundary-1)...)
		current = boundary
	}

	result = append(result, utf8SequencesForSize(current, hi)...)
	return result
}

// utf8SequencesForSize generates byte sequences for a rune range
// where all runes encode to the same number of UTF-8 bytes.
func utf8SequencesForSize(lo, hi rune) [][]ByteRange {
	if lo > hi {
		return nil
	}

	loBytes := runeToUTF8(lo)
	hiBytes := runeToUTF8(hi)
	loLen := utf8.RuneLen(lo)
	hiLen := utf8.RuneLen(hi)

	if loLen != hiLen {
		// Should not happen if caller splits by size boundaries
		// Handle by splitting at the midpoint
		mid := splitPoint(lo, hi, loLen)
		var result [][]ByteRange
		result = append(result, utf8SequencesForSize(lo, mid)...)
		result = append(result, utf8SequencesForSize(mid+1, hi)...)
		return result
	}

	n := loLen
	if n <= 0 {
		return nil
	}

	return splitUTF8Range(loBytes[:n], hiBytes[:n], 0)
}

// splitUTF8Range recursively splits a UTF-8 byte range into sequences
// where each byte position can be represented as a single contiguous range.
func splitUTF8Range(lo, hi []byte, pos int) [][]ByteRange {
	if pos >= len(lo) {
		return nil
	}

	if pos == len(lo)-1 {
		// Last byte: single range
		return [][]ByteRange{{ByteRange{lo[pos], hi[pos]}}}
	}

	if bytesEqual(lo[pos:], hi[pos:]) {
		// All remaining bytes are equal: single sequence
		seq := make([]ByteRange, len(lo)-pos)
		for i := pos; i < len(lo); i++ {
			seq[i-pos] = ByteRange{lo[i], hi[i]}
		}
		return [][]ByteRange{seq}
	}

	// If leading bytes are equal, descend
	if lo[pos] == hi[pos] {
		sub := splitUTF8Range(lo, hi, pos+1)
		result := make([][]ByteRange, len(sub))
		for i, s := range sub {
			r := make([]ByteRange, len(s)+1)
			r[0] = ByteRange{lo[pos], hi[pos]}
			copy(r[1:], s)
			result[i] = r
		}
		return result
	}

	var result [][]ByteRange

	// Split into: [lo..loMax] [middle] [hiMin..hi]

	// Lower part: lo[pos] with remaining bytes [lo[pos+1:]..0xBF,0xBF,...]
	if lo[pos+1] > 0x80 || !allTrailMax(lo, pos+1) {
		loMax := make([]byte, len(lo))
		copy(loMax, lo)
		for i := pos + 1; i < len(lo); i++ {
			loMax[i] = 0xBF
		}
		sub := splitUTF8Range(lo, loMax, pos+1)
		for _, s := range sub {
			r := make([]ByteRange, len(s)+1)
			r[0] = ByteRange{lo[pos], lo[pos]}
			copy(r[1:], s)
			result = append(result, r)
		}
	} else {
		// lo already starts at trailing min, include as full range
		seq := make([]ByteRange, len(lo)-pos)
		seq[0] = ByteRange{lo[pos], lo[pos]}
		for i := pos + 1; i < len(lo); i++ {
			seq[i-pos] = ByteRange{0x80, 0xBF}
		}
		result = append(result, seq)
	}

	// Middle part: (lo[pos]+1)..(hi[pos]-1) with full trailing range
	if lo[pos]+1 <= hi[pos]-1 {
		seq := make([]ByteRange, len(lo)-pos)
		seq[0] = ByteRange{lo[pos] + 1, hi[pos] - 1}
		for i := 1; i < len(seq); i++ {
			seq[i] = ByteRange{0x80, 0xBF}
		}
		result = append(result, seq)
	}

	// Upper part: hi[pos] with remaining bytes [0x80,0x80,...hi[pos+1:]]
	if hi[pos+1] < 0xBF || !allTrailMin(hi, pos+1) {
		hiMin := make([]byte, len(hi))
		copy(hiMin, hi)
		for i := pos + 1; i < len(hi); i++ {
			hiMin[i] = 0x80
		}
		sub := splitUTF8Range(hiMin, hi, pos+1)
		for _, s := range sub {
			r := make([]ByteRange, len(s)+1)
			r[0] = ByteRange{hi[pos], hi[pos]}
			copy(r[1:], s)
			result = append(result, r)
		}
	} else {
		seq := make([]ByteRange, len(hi)-pos)
		seq[0] = ByteRange{hi[pos], hi[pos]}
		for i := pos + 1; i < len(hi); i++ {
			seq[i-pos] = ByteRange{0x80, 0xBF}
		}
		result = append(result, seq)
	}

	return result
}

// runeToUTF8 encodes a rune to its UTF-8 byte sequence.
func runeToUTF8(r rune) [4]byte {
	var buf [4]byte
	utf8.EncodeRune(buf[:], r)
	return buf
}

// splitPoint finds the largest rune <= hi that has a different UTF-8 length than lo.
func splitPoint(lo, hi rune, loLen int) rune {
	boundaries := [...]rune{0x7F, 0x7FF, 0xFFFF}
	for _, b := range boundaries {
		if lo <= b && b < hi {
			return b
		}
	}
	return lo // shouldn't happen
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func allTrailMax(b []byte, from int) bool {
	for i := from; i < len(b); i++ {
		if b[i] != 0xBF {
			return false
		}
	}
	return true
}

func allTrailMin(b []byte, from int) bool {
	for i := from; i < len(b); i++ {
		if b[i] != 0x80 {
			return false
		}
	}
	return true
}

// isASCIIByte returns true if b < 128.
func isASCIIByte(b byte) bool {
	return b&0x80 == 0
}

// utf8ByteLen returns the expected UTF-8 sequence length from the leading byte.
// Returns 0 for invalid lead bytes (continuation bytes, overlong encodings, etc.).
func utf8ByteLen(b byte) int {
	if b < 0x80 {
		return 1
	}
	if b < 0xC2 {
		return 0 // 0x80-0xC1: continuation bytes or overlong
	}
	if b < 0xE0 {
		return 2
	}
	if b < 0xF0 {
		return 3
	}
	if b < 0xF5 {
		return 4
	}
	return 0 // 0xF5-0xFF: invalid
}
