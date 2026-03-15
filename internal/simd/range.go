package simd

import (
	"math/bits"

	"simd/archsimd"
)

// IndexByteRange returns the index of the first byte in data that is
// in the inclusive range [lo, hi], or -1 if none found.
// Uses AVX2 to check 32 bytes per iteration.
func IndexByteRange(data []byte, lo, hi byte) int {
	n := len(data)
	if n == 0 || lo > hi {
		return -1
	}

	// Single byte: delegate to IndexByte for optimal codegen
	if lo == hi {
		return IndexByte(data, lo)
	}

	vecLo := archsimd.BroadcastUint8x32(lo)
	vecHi := archsimd.BroadcastUint8x32(hi)
	i := 0

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])
		inRange := chunk.GreaterEqual(vecLo).And(chunk.LessEqual(vecHi))
		b := inRange.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		if data[i] >= lo && data[i] <= hi {
			archsimd.ClearAVXUpperBits()
			return i
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// ByteRangeScanner pre-computes AVX2 broadcast vectors for repeated
// byte-range scans. Eliminates per-call broadcast overhead (30ns saved
// per call — significant when called thousands of times in findAllIndex).
type ByteRangeScanner struct {
	vecLo archsimd.Uint8x32
	vecHi archsimd.Uint8x32
	lo    byte
	hi    byte
}

// NewByteRangeScanner creates a scanner for the inclusive byte range [lo, hi].
func NewByteRangeScanner(lo, hi byte) ByteRangeScanner {
	return ByteRangeScanner{
		vecLo: archsimd.BroadcastUint8x32(lo),
		vecHi: archsimd.BroadcastUint8x32(hi),
		lo:    lo,
		hi:    hi,
	}
}

// Next returns the offset of the first byte in data[from:] that is in [lo, hi].
// Returns -1 if none found. Uses pre-computed SIMD vectors — no broadcast overhead.
func (s ByteRangeScanner) Next(data []byte, from int) int {
	n := len(data)
	i := from

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])
		inRange := chunk.GreaterEqual(s.vecLo).And(chunk.LessEqual(s.vecHi))
		b := inRange.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}

	for ; i < n; i++ {
		if data[i] >= s.lo && data[i] <= s.hi {
			archsimd.ClearAVXUpperBits()
			return i
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// MultiByteRangeScanner pre-computes vectors for multiple byte ranges.
type MultiByteRangeScanner struct {
	vecs   []vecPair
	ranges [][2]byte
}

type vecPair struct {
	lo, hi archsimd.Uint8x32
}

// NewMultiByteRangeScanner creates a scanner for multiple byte ranges.
func NewMultiByteRangeScanner(ranges [][2]byte) MultiByteRangeScanner {
	vecs := make([]vecPair, len(ranges))
	for i, r := range ranges {
		vecs[i] = vecPair{
			lo: archsimd.BroadcastUint8x32(r[0]),
			hi: archsimd.BroadcastUint8x32(r[1]),
		}
	}
	return MultiByteRangeScanner{vecs: vecs, ranges: ranges}
}

// Next returns the offset of the first byte in data[from:] matching any range.
func (s MultiByteRangeScanner) Next(data []byte, from int) int {
	n := len(data)
	i := from

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])
		combined := chunk.GreaterEqual(s.vecs[0].lo).And(chunk.LessEqual(s.vecs[0].hi))
		for j := 1; j < len(s.vecs); j++ {
			r := chunk.GreaterEqual(s.vecs[j].lo).And(chunk.LessEqual(s.vecs[j].hi))
			combined = combined.Or(r)
		}
		b := combined.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}

	for ; i < n; i++ {
		for _, r := range s.ranges {
			if data[i] >= r[0] && data[i] <= r[1] {
				archsimd.ClearAVXUpperBits()
				return i
			}
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// BatchNext collects all positions of bytes in [lo, hi] within data[from:end]
// into the provided buf slice. Returns the count of positions written.
// Positions are absolute (not relative to from). buf must be large enough.
func (s ByteRangeScanner) BatchNext(data []byte, from, end int, buf []int) int {
	n := end
	if n > len(data) {
		n = len(data)
	}
	i := from
	count := 0
	bufLen := len(buf)

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])
		inRange := chunk.GreaterEqual(s.vecLo).And(chunk.LessEqual(s.vecHi))
		b := inRange.ToBits()
		for b != 0 {
			bit := bits.TrailingZeros32(b)
			if count >= bufLen {
				archsimd.ClearAVXUpperBits()
				return count
			}
			buf[count] = i + bit
			count++
			b &= b - 1 // clear lowest set bit
		}
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		if data[i] >= s.lo && data[i] <= s.hi {
			if count >= bufLen {
				break
			}
			buf[count] = i
			count++
		}
	}

	archsimd.ClearAVXUpperBits()
	return count
}

// BatchNextMulti collects all positions of bytes matching any range within
// data[from:end] into the provided buf slice. Returns the count of positions written.
func (s MultiByteRangeScanner) BatchNext(data []byte, from, end int, buf []int) int {
	n := end
	if n > len(data) {
		n = len(data)
	}
	i := from
	count := 0
	bufLen := len(buf)

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])
		combined := chunk.GreaterEqual(s.vecs[0].lo).And(chunk.LessEqual(s.vecs[0].hi))
		for j := 1; j < len(s.vecs); j++ {
			r := chunk.GreaterEqual(s.vecs[j].lo).And(chunk.LessEqual(s.vecs[j].hi))
			combined = combined.Or(r)
		}
		b := combined.ToBits()
		for b != 0 {
			bit := bits.TrailingZeros32(b)
			if count >= bufLen {
				archsimd.ClearAVXUpperBits()
				return count
			}
			buf[count] = i + bit
			count++
			b &= b - 1
		}
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		for _, r := range s.ranges {
			if data[i] >= r[0] && data[i] <= r[1] {
				if count >= bufLen {
					archsimd.ClearAVXUpperBits()
					return count
				}
				buf[count] = i
				count++
				break
			}
		}
	}

	archsimd.ClearAVXUpperBits()
	return count
}

// IndexByteRanges returns the index of the first byte in data that falls
// within any of the given [lo, hi] ranges. Each range is a [2]byte{lo, hi}.
// Uses AVX2 with OR'd range masks — up to 4 ranges are fully unrolled.
func IndexByteRanges(data []byte, ranges [][2]byte) int {
	n := len(data)
	if n == 0 || len(ranges) == 0 {
		return -1
	}

	if len(ranges) == 1 {
		return IndexByteRange(data, ranges[0][0], ranges[0][1])
	}

	// Prepare broadcast vectors for each range
	type vecPair struct {
		lo, hi archsimd.Uint8x32
	}
	vecs := make([]vecPair, len(ranges))
	for i, r := range ranges {
		vecs[i] = vecPair{
			lo: archsimd.BroadcastUint8x32(r[0]),
			hi: archsimd.BroadcastUint8x32(r[1]),
		}
	}

	i := 0
	for i+32 <= n {
		chunk := archsimd.LoadUint8x32Slice(data[i:])

		// OR all range masks together
		combined := chunk.GreaterEqual(vecs[0].lo).And(chunk.LessEqual(vecs[0].hi))
		for j := 1; j < len(vecs); j++ {
			r := chunk.GreaterEqual(vecs[j].lo).And(chunk.LessEqual(vecs[j].hi))
			combined = combined.Or(r)
		}

		b := combined.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		for _, r := range ranges {
			if data[i] >= r[0] && data[i] <= r[1] {
				archsimd.ClearAVXUpperBits()
				return i
			}
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}
