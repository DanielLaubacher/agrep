package matcher

import (
	"bytes"
	"fmt"
	"testing"
)

// BenchmarkMultiPrefilter compares single-literal vs multi-literal prefiltering.
// The regex has 3 required literals: "ERROR", "code=", "request_id=".
// Single prefilter: SIMD scans for "request_id=" (longest), then regex on candidates.
// Multi prefilter: SIMD scans for "request_id=", then checks "ERROR" and "code="
// with SIMD on candidate lines before running regex.

func BenchmarkMultiPrefilter_Sparse(b *testing.B) {
	// 1% of lines match the full pattern
	data := makeMultiLiteralData(100000, 100)
	pattern := `ERROR.*code=[45]\d{2}.*request_id=[0-9a-f]+`

	b.Run("MultiPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		if len(m.extraFilters) == 0 {
			b.Fatalf("expected extra filters, got none (prefilter=%q)", m.prefilter)
		}
		b.Logf("prefilter=%q extras=%d", m.prefilter, len(m.extraFilters))
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("SinglePrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.extraFilters = nil // disable multi-prefilter
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("NoPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.prefilter = nil // disable all prefilters
		m.extraFilters = nil
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

func BenchmarkMultiPrefilter_Medium(b *testing.B) {
	// 10% of lines match the primary prefilter, but only 1% match all literals
	data := makeMultiLiteralData(100000, 10)
	pattern := `ERROR.*code=[45]\d{2}.*request_id=[0-9a-f]+`

	b.Run("MultiPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("SinglePrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.extraFilters = nil
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

func BenchmarkMultiPrefilter_ManyLiterals(b *testing.B) {
	// Pattern with 4 required literals
	// Lines have partial matches to stress the cascaded rejection
	data := makeFourLiteralData(100000, 20)
	pattern := `ALERT.*host=prod-.*service=auth.*request_id=[0-9a-f]+`

	b.Run("MultiPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		b.Logf("prefilter=%q extras=%d", m.prefilter, len(m.extraFilters))
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("SinglePrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.extraFilters = nil
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

func BenchmarkMultiPrefilter_MatchExists(b *testing.B) {
	data := makeMultiLiteralData(100000, 100)
	pattern := `ERROR.*code=[45]\d{2}.*request_id=[0-9a-f]+`

	b.Run("MultiPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.MatchExists(data)
		}
	})

	b.Run("SinglePrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.extraFilters = nil
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.MatchExists(data)
		}
	})
}

func BenchmarkMultiPrefilter_CountAll(b *testing.B) {
	data := makeMultiLiteralData(100000, 100)
	pattern := `ERROR.*code=[45]\d{2}.*request_id=[0-9a-f]+`

	b.Run("MultiPrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.CountAll(data)
		}
	})

	b.Run("SinglePrefilter", func(b *testing.B) {
		m, _ := NewRegexMatcher(pattern, false, false)
		m.extraFilters = nil
		m.extraCI = nil
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.CountAll(data)
		}
	})
}

// makeMultiLiteralData creates data where:
// - 1 in `sparsity` lines match ALL three literals (ERROR, code=4xx, request_id=hex)
// - Other lines contain "request_id=" (primary prefilter matches!) but NOT "ERROR"
// This stresses multi-prefilter: single prefilter lets many false positives through
// to the regex, multi-prefilter rejects them with cheap SIMD checks.
func makeMultiLiteralData(lines int, sparsity int) []byte {
	var buf bytes.Buffer
	for i := range lines {
		if i%sparsity == 0 {
			// Full match: all 3 literals present
			fmt.Fprintf(&buf, "ERROR: host=prod-auth-1 code=%d msg=failed request_id=%08x\n", 400+i%100, i)
		} else if i%3 == 0 {
			// Partial: has request_id= but not ERROR — triggers primary prefilter
			fmt.Fprintf(&buf, "INFO: host=dev-user-1 latency=%dms request_id=%08x\n", i%500, i)
		} else {
			// No match at all
			fmt.Fprintf(&buf, "DEBUG: host=dev-auth-%d pid=%d uptime=%ds\n", i%5, 1000+i%9000, i%86400)
		}
	}
	return buf.Bytes()
}

// makeFourLiteralData creates data with 4 required literals and partial matches.
func makeFourLiteralData(lines int, sparsity int) []byte {
	var buf bytes.Buffer
	for i := range lines {
		if i%sparsity == 0 {
			// Full match
			fmt.Fprintf(&buf, "ALERT: host=prod-web-%d service=auth pid=%d request_id=%08x\n", i%20, 1000+i, i)
		} else if i%4 == 0 {
			// Has "request_id=" and "host=prod-" but not "ALERT" or "service=auth"
			fmt.Fprintf(&buf, "INFO: host=prod-web-%d service=user pid=%d request_id=%08x\n", i%20, 1000+i, i)
		} else if i%3 == 0 {
			// Has "request_id=" only
			fmt.Fprintf(&buf, "DEBUG: host=dev-api-%d pid=%d request_id=%08x\n", i%5, 1000+i, i)
		} else {
			fmt.Fprintf(&buf, "DEBUG: host=dev-auth-%d pid=%d uptime=%ds\n", i%5, 1000+i, i%86400)
		}
	}
	return buf.Bytes()
}
