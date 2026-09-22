package simd

import (
	"bytes"
	"math/bits"

	"simd/archsimd"
)

// Index returns the index of the first occurrence of pattern in data, or -1 if not present.
// Delegates to bytes.Index which uses optimized AVX2 assembly internally.
func Index(data, pattern []byte) int {
	return bytes.Index(data, pattern)
}

// IndexAll returns all byte offsets where pattern occurs in data.
// Non-overlapping matches only. Uses bytes.Index (AVX2 asm) for the scan loop.
func IndexAll(data, pattern []byte) []int {
	plen := len(pattern)
	switch {
	case plen == 0:
		return nil
	case plen == 1:
		return indexAllByte(data, pattern[0])
	case plen > len(data):
		return nil
	}

	// Collect into a non-escaping stack buffer first, then copy to heap
	// only if we found matches. This avoids a 128-byte heap alloc on no-match.
	var stackBuf [16]int
	n := 0
	var overflow []int
	i := 0

	for {
		idx := bytes.Index(data[i:], pattern)
		if idx < 0 {
			break
		}
		if n < len(stackBuf) {
			stackBuf[n] = i + idx
		} else {
			if overflow == nil {
				overflow = make([]int, 0, 64)
				overflow = append(overflow, stackBuf[:]...)
			}
			overflow = append(overflow, i+idx)
		}
		n++
		i += idx + plen
	}

	if n == 0 {
		return nil
	}
	if overflow != nil {
		return overflow
	}
	result := make([]int, n)
	copy(result, stackBuf[:n])
	return result
}

// indexAllByte returns all byte offsets where byte c occurs in data.
func indexAllByte(data []byte, c byte) []int {
	var stackBuf [16]int
	n := 0
	var overflow []int
	needle := archsimd.BroadcastUint8x32(c)
	i := 0

	for i+32 <= len(data) {
		chunk := archsimd.LoadUint8x32(data[i:])
		mask := chunk.Equal(needle)
		b := mask.ToBits()
		for b != 0 {
			j := bits.TrailingZeros32(b)
			if n < len(stackBuf) {
				stackBuf[n] = i + j
			} else {
				if overflow == nil {
					overflow = make([]int, 0, 64)
					overflow = append(overflow, stackBuf[:]...)
				}
				overflow = append(overflow, i+j)
			}
			n++
			b &= b - 1
		}
		i += 32
	}

	for ; i < len(data); i++ {
		if data[i] == c {
			if n < len(stackBuf) {
				stackBuf[n] = i
			} else {
				if overflow == nil {
					overflow = make([]int, 0, 64)
					overflow = append(overflow, stackBuf[:]...)
				}
				overflow = append(overflow, i)
			}
			n++
		}
	}

	archsimd.ClearAVXUpperBits()
	if n == 0 {
		return nil
	}
	if overflow != nil {
		return overflow
	}
	result := make([]int, n)
	copy(result, stackBuf[:n])
	return result
}

// byteFreq scores how common a (case-folded) byte is in typical text and
// source code. Higher = more common = worse prefilter byte.
var byteFreq = func() [256]uint8 {
	var f [256]uint8
	for i := range f {
		f[i] = 16 // uncommon by default (punctuation, control, high bytes)
	}
	// Approximate English/code letter frequencies (both cases share a score
	// since the case-insensitive scan tests both variants).
	freqs := map[byte]uint8{
		'e': 255, 't': 220, 'a': 210, 'o': 200, 'i': 195, 'n': 190,
		's': 180, 'r': 175, 'h': 150, 'l': 140, 'd': 120, 'c': 110,
		'u': 100, 'm': 90, 'f': 80, 'p': 75, 'g': 70, 'w': 60,
		'y': 55, 'b': 50, 'v': 40, 'k': 30, 'x': 12, 'j': 10, 'q': 8, 'z': 8,
		' ': 255, '\t': 200, '_': 90,
	}
	for b, v := range freqs {
		f[b] = v
	}
	for b := byte('0'); b <= '9'; b++ {
		f[b] = 100
	}
	return f
}()

// pickRarePair returns the two distinct pattern positions whose bytes are
// rarest in typical data (minimizing the joint false-positive rate). The
// SIMD prefilter tests these two positions; rarer bytes mean fewer
// false-positive candidates to verify. Identical byte values are fine —
// for "err" the pair (r,r) selects the rare "rr" bigram, far more selective
// than the common "er".
func pickRarePair(pattern []byte) (int, int) {
	plen := len(pattern)
	if plen == 1 {
		return 0, 0
	}
	o1, o2 := 0, 1
	if byteFreq[pattern[1]] < byteFreq[pattern[0]] {
		o1, o2 = 1, 0
	}
	for i := 2; i < plen; i++ {
		f := byteFreq[pattern[i]]
		if f < byteFreq[pattern[o1]] {
			o2 = o1
			o1 = i
		} else if f < byteFreq[pattern[o2]] {
			o2 = i
		}
	}
	if o1 > o2 {
		o1, o2 = o2, o1
	}
	return o1, o2
}

// IndexCaseInsensitive returns the index of the first case-insensitive occurrence of pattern in data.
// Pattern must be pre-lowered. Only handles ASCII case folding.
func IndexCaseInsensitive(data, patternLower []byte) int {
	plen := len(patternLower)
	switch {
	case plen == 0:
		return 0
	case plen > len(data):
		return -1
	}

	// Prefilter on the two RAREST pattern bytes (not first+last): for a
	// pattern like "define", first+last is 'd'+'e' and 'e' is the most
	// common letter in text — the rare pair ('f','d') cuts false-positive
	// verifications by ~6x. Bit j in the mask still marks match START i+j
	// because both loads are offset from the window base.
	o1, o2 := pickRarePair(patternLower)
	firstLo := patternLower[o1]
	firstHi := toUpperASCII(firstLo)
	lastLo := patternLower[o2]
	lastHi := toUpperASCII(lastLo)

	bFirstLo := archsimd.BroadcastUint8x32(firstLo)
	bFirstHi := archsimd.BroadcastUint8x32(firstHi)
	bLastLo := archsimd.BroadcastUint8x32(lastLo)
	bLastHi := archsimd.BroadcastUint8x32(lastHi)

	i := 0
	limit := len(data) - plen + 1

	for i+32 <= limit {
		blockFirst := archsimd.LoadUint8x32(data[i+o1:])
		blockLast := archsimd.LoadUint8x32(data[i+o2:])

		mFirstLo := blockFirst.Equal(bFirstLo)
		mFirstHi := blockFirst.Equal(bFirstHi)
		mFirst := mFirstLo.Or(mFirstHi)

		mLastLo := blockLast.Equal(bLastLo)
		mLastHi := blockLast.Equal(bLastHi)
		mLast := mLastLo.Or(mLastHi)

		b := mFirst.And(mLast).ToBits()

		for b != 0 {
			j := bits.TrailingZeros32(b)
			if matchCaseInsensitive(data[i+j:i+j+plen], patternLower) {
				archsimd.ClearAVXUpperBits()
				return i + j
			}
			b &= b - 1
		}

		i += 32
	}

	// Scalar tail
	for ; i < limit; i++ {
		if matchCaseInsensitive(data[i:i+plen], patternLower) {
			archsimd.ClearAVXUpperBits()
			return i
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// IndexAllCaseInsensitive returns all byte offsets of case-insensitive, non-overlapping matches.
func IndexAllCaseInsensitive(data, patternLower []byte) []int {
	plen := len(patternLower)
	if plen == 0 || plen > len(data) {
		return nil
	}

	// Rare-pair prefilter — see IndexCaseInsensitive for rationale.
	o1, o2 := pickRarePair(patternLower)
	firstLo := patternLower[o1]
	firstHi := toUpperASCII(firstLo)
	lastLo := patternLower[o2]
	lastHi := toUpperASCII(lastLo)

	bFirstLo := archsimd.BroadcastUint8x32(firstLo)
	bFirstHi := archsimd.BroadcastUint8x32(firstHi)
	bLastLo := archsimd.BroadcastUint8x32(lastLo)
	bLastHi := archsimd.BroadcastUint8x32(lastHi)

	var stackBuf [16]int
	n := 0
	var overflow []int
	i := 0
	limit := len(data) - plen + 1

	for i+32 <= limit {
		blockFirst := archsimd.LoadUint8x32(data[i+o1:])
		blockLast := archsimd.LoadUint8x32(data[i+o2:])

		mFirst := blockFirst.Equal(bFirstLo).Or(blockFirst.Equal(bFirstHi))
		mLast := blockLast.Equal(bLastLo).Or(blockLast.Equal(bLastHi))
		b := mFirst.And(mLast).ToBits()

		for b != 0 {
			j := bits.TrailingZeros32(b)
			pos := i + j
			if matchCaseInsensitive(data[pos:pos+plen], patternLower) {
				if n < len(stackBuf) {
					stackBuf[n] = pos
				} else {
					if overflow == nil {
						overflow = make([]int, 0, 64)
						overflow = append(overflow, stackBuf[:]...)
					}
					overflow = append(overflow, pos)
				}
				n++
				skipTo := j + plen
				if skipTo < 32 {
					b >>= skipTo
					b <<= skipTo
				} else {
					b = 0
				}
				continue
			}
			b &= b - 1
		}

		i += 32
	}

	for ; i < limit; i++ {
		if matchCaseInsensitive(data[i:i+plen], patternLower) {
			if n < len(stackBuf) {
				stackBuf[n] = i
			} else {
				if overflow == nil {
					overflow = make([]int, 0, 64)
					overflow = append(overflow, stackBuf[:]...)
				}
				overflow = append(overflow, i)
			}
			n++
			i += plen - 1
		}
	}

	archsimd.ClearAVXUpperBits()
	if n == 0 {
		return nil
	}
	if overflow != nil {
		return overflow
	}
	result := make([]int, n)
	copy(result, stackBuf[:n])
	return result
}

func matchCaseInsensitive(data, patternLower []byte) bool {
	for i, b := range data {
		if toLowerASCII(b) != patternLower[i] {
			return false
		}
	}
	return true
}

func toLowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

func toUpperASCII(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - ('a' - 'A')
	}
	return b
}
