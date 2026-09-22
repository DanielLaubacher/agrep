// Package simd provides SIMD-accelerated byte search functions using Go 1.26's
// simd/archsimd intrinsics. Requires GOEXPERIMENT=simd at build time.
package simd

import (
	"math/bits"

	"simd/archsimd"
)

// IndexByte returns the index of the first occurrence of c in data, or -1 if not present.
// Uses AVX2 VPCMPEQB to compare 32 bytes per iteration.
func IndexByte(data []byte, c byte) int {
	n := len(data)
	if n == 0 {
		return -1
	}

	needle := archsimd.BroadcastUint8x32(c)
	i := 0

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32(data[i:])
		mask := chunk.Equal(needle)
		b := mask.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(b)
		}
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		if data[i] == c {
			archsimd.ClearAVXUpperBits()
			return i
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// LastIndexByte returns the index of the last occurrence of c in data, or -1 if not present.
// Uses AVX2 scanning from the end.
func LastIndexByte(data []byte, c byte) int {
	n := len(data)
	if n == 0 {
		return -1
	}

	needle := archsimd.BroadcastUint8x32(c)

	// Scalar tail for bytes that don't fill a full 32-byte chunk
	tail := n % 32
	for i := n - 1; i >= n-tail; i-- {
		if data[i] == c {
			archsimd.ClearAVXUpperBits()
			return i
		}
	}

	// SIMD scan from end, 32 bytes at a time
	for i := n - tail - 32; i >= 0; i -= 32 {
		chunk := archsimd.LoadUint8x32(data[i:])
		mask := chunk.Equal(needle)
		b := mask.ToBits()
		if b != 0 {
			archsimd.ClearAVXUpperBits()
			// Highest set bit = last match in this chunk
			return i + 31 - bits.LeadingZeros32(b)
		}
	}

	archsimd.ClearAVXUpperBits()
	return -1
}

// Count returns the number of occurrences of c in data.
// Uses AVX2 to compare 32 bytes per iteration and popcount to sum matches.
func Count(data []byte, c byte) int {
	n := len(data)
	if n == 0 {
		return 0
	}

	needle := archsimd.BroadcastUint8x32(c)
	count := 0
	i := 0

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32(data[i:])
		mask := chunk.Equal(needle)
		b := mask.ToBits()
		count += bits.OnesCount32(b)
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		if data[i] == c {
			count++
		}
	}

	archsimd.ClearAVXUpperBits()
	return count
}

// ToLowerASCII lowercases ASCII bytes from src into dst using AVX2.
// dst must be at least len(src) bytes. Non-ASCII bytes are copied unchanged.
func ToLowerASCII(dst, src []byte) {
	n := len(src)
	if n == 0 {
		return
	}

	vecA := archsimd.BroadcastUint8x32('A')
	vecZ := archsimd.BroadcastUint8x32('Z')
	vec32 := archsimd.BroadcastUint8x32(0x20)
	i := 0

	for i+32 <= n {
		chunk := archsimd.LoadUint8x32(src[i:])
		isUpper := chunk.GreaterEqual(vecA).And(chunk.LessEqual(vecZ))
		lowered := chunk.Add(vec32.Masked(isUpper))
		lowered.Store(dst[i:])
		i += 32
	}

	// Scalar tail
	for ; i < n; i++ {
		b := src[i]
		if b >= 'A' && b <= 'Z' {
			b += 0x20
		}
		dst[i] = b
	}

	archsimd.ClearAVXUpperBits()
}
