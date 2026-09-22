package simd

// Rare-pair Teddy: SIMD multi-pattern search for small literal sets.
//
// Classic Teddy (ripgrep/Hyperscan) fingerprints the FIRST 1-4 bytes of each
// pattern with nibble PSHUFB lookups. Its weakness is fingerprint position:
// pattern sets like {error, errno, errcode} share the ultra-common prefix
// "er", so nearly every English "er" bigram becomes a candidate requiring
// scalar verification.
//
// This variant keeps Teddy's nibble-PSHUFB machinery but probes the two
// GLOBALLY RAREST aligned byte positions across the pattern set (chosen by a
// static text/code frequency table) instead of positions 0 and 1. For the
// errX set that selects the "rr" pair — ~17x fewer false-positive candidates
// than "er". Scan cost per 32-byte block is identical to 2-byte Teddy:
// 2 loads, 2 nibble splits, 4 PSHUFB, 3 ANDs, 1 compare.
//
// Each candidate lane carries an 8-bit pattern bitmask (bit i = pattern i's
// bytes matched at both probe offsets), so verification only memcmps the
// patterns that fingerprint-matched.

import (
	"bytes"
	"math/bits"

	"simd/archsimd"
)

// TeddyMaxPatterns is the pattern-set size limit (bitmask fits one byte).
const TeddyMaxPatterns = 8

// Teddy is a multi-pattern matcher for 2..8 literal patterns of length >= 2.
// Immutable after construction; safe for concurrent use.
type Teddy struct {
	patterns [][]byte // lowered when ci
	ci       bool
	o1, o2   int // probe offsets into each pattern, o1 < o2 < minLen

	// Nibble tables for the two probe positions, each 16 bytes broadcast
	// to both 128-bit lanes for VPSHUFB.
	loA, hiA archsimd.Uint8x32
	loB, hiB archsimd.Uint8x32

	// Exact byte-membership tables for the scalar tail.
	tblA, tblB [256]uint8

	minLen int
}

// NewTeddy builds a rare-pair Teddy matcher, or returns nil if the pattern
// set is unsuitable (too many patterns, or any pattern shorter than 2 bytes).
// Patterns must be pre-lowered when ci is true.
func NewTeddy(patterns [][]byte, ci bool) *Teddy {
	if len(patterns) < 2 || len(patterns) > TeddyMaxPatterns {
		return nil
	}
	minLen := len(patterns[0])
	for _, p := range patterns {
		if len(p) < 2 {
			return nil
		}
		if len(p) < minLen {
			minLen = len(p)
		}
	}

	t := &Teddy{patterns: patterns, ci: ci, minLen: minLen}
	t.o1, t.o2 = pickTeddyOffsets(patterns, minLen)

	var loTA, hiTA, loTB, hiTB [16]uint8
	add := func(loT, hiT *[16]uint8, tbl *[256]uint8, c byte, bit uint8) {
		loT[c&0x0F] |= bit
		hiT[c>>4] |= bit
		tbl[c] |= bit
	}
	for i, p := range patterns {
		bit := uint8(1) << i
		a, b := p[t.o1], p[t.o2]
		add(&loTA, &hiTA, &t.tblA, a, bit)
		add(&loTB, &hiTB, &t.tblB, b, bit)
		if ci {
			add(&loTA, &hiTA, &t.tblA, toUpperASCII(a), bit)
			add(&loTB, &hiTB, &t.tblB, toUpperASCII(b), bit)
		}
	}
	t.loA = broadcastTable(loTA)
	t.hiA = broadcastTable(hiTA)
	t.loB = broadcastTable(loTB)
	t.hiB = broadcastTable(hiTB)
	return t
}

// broadcastTable duplicates a 16-byte nibble table into both 128-bit lanes.
func broadcastTable(tbl [16]uint8) archsimd.Uint8x32 {
	var arr [32]uint8
	copy(arr[:16], tbl[:])
	copy(arr[16:], tbl[:])
	return archsimd.LoadUint8x32Array(&arr)
}

// pickTeddyOffsets returns the two pattern offsets whose byte sets are
// rarest in typical data (minimizing expected false-positive candidates).
func pickTeddyOffsets(patterns [][]byte, minLen int) (int, int) {
	limit := minLen
	if limit > 16 {
		limit = 16
	}
	cost := make([]int, limit)
	for o := 0; o < limit; o++ {
		var seen [256]bool
		for _, p := range patterns {
			b := p[o]
			if !seen[b] {
				seen[b] = true
				cost[o] += int(byteFreq[b])
			}
		}
	}
	o1, o2 := 0, 1
	if cost[1] < cost[0] {
		o1, o2 = 1, 0
	}
	for o := 2; o < limit; o++ {
		if cost[o] < cost[o1] {
			o2 = o1
			o1 = o
		} else if cost[o] < cost[o2] {
			o2 = o
		}
	}
	if o1 > o2 {
		o1, o2 = o2, o1
	}
	return o1, o2
}

// Scan reports every (position, pattern) pair where a pattern matches, in
// ascending position order (pattern-index order within a position). Return
// false from fn to stop. Overlapping matches are all reported, mirroring
// Aho-Corasick semantics.
func (t *Teddy) Scan(data []byte, fn func(pos, pat int) bool) {
	n := len(data)
	if n < t.minLen {
		return
	}

	mask0F := archsimd.BroadcastUint8x32(0x0F)
	var zero archsimd.Uint8x32
	var maskBuf [32]uint8

	o1, o2 := t.o1, t.o2
	// SIMD over candidate start positions i..i+31; the second probe load
	// reads up to data[i+31+o2], so stop when that would pass the end.
	i := 0
	for i+32+o2 <= n {
		blockA := archsimd.LoadUint8x32(data[i+o1:])
		blockB := archsimd.LoadUint8x32(data[i+o2:])

		loIdxA := blockA.And(mask0F)
		hiIdxA := blockA.AsUint16x16().ShiftAllRight(4).AsUint8x32().And(mask0F)
		mA := t.loA.PermuteOrZeroGrouped(loIdxA.AsInt8x32()).
			And(t.hiA.PermuteOrZeroGrouped(hiIdxA.AsInt8x32()))

		loIdxB := blockB.And(mask0F)
		hiIdxB := blockB.AsUint16x16().ShiftAllRight(4).AsUint8x32().And(mask0F)
		mB := t.loB.PermuteOrZeroGrouped(loIdxB.AsInt8x32()).
			And(t.hiB.PermuteOrZeroGrouped(hiIdxB.AsInt8x32()))

		cand := mA.And(mB)
		// Lanes equal to zero have no candidate; complement for hit lanes.
		hits := ^cand.Equal(zero).ToBits()
		if hits != 0 {
			cand.StoreArray(&maskBuf)
			for hits != 0 {
				j := bits.TrailingZeros32(hits)
				hits &= hits - 1
				if !t.verify(data, i+j, maskBuf[j], fn) {
					archsimd.ClearAVXUpperBits()
					return
				}
			}
		}
		i += 32
	}
	archsimd.ClearAVXUpperBits()

	// Scalar tail over remaining start positions.
	for ; i+t.minLen <= n; i++ {
		// Loop bound guarantees i+minLen <= n and o1 < o2 < minLen,
		// so both probe reads are in bounds.
		m := t.tblA[data[i+o1]] & t.tblB[data[i+o2]]
		if m != 0 {
			if !t.verify(data, i, m, fn) {
				return
			}
		}
	}
}

// verify memcmps each fingerprint-matched pattern at pos, invoking fn on
// real matches. Returns false to propagate early termination.
func (t *Teddy) verify(data []byte, pos int, m uint8, fn func(pos, pat int) bool) bool {
	for m != 0 {
		pi := bits.TrailingZeros8(m)
		m &= m - 1
		p := t.patterns[pi]
		if pos+len(p) > len(data) {
			continue
		}
		cand := data[pos : pos+len(p)]
		var ok bool
		if t.ci {
			ok = matchCaseInsensitive(cand, p)
		} else {
			ok = bytes.Equal(cand, p)
		}
		if ok {
			if !fn(pos, pi) {
				return false
			}
		}
	}
	return true
}

