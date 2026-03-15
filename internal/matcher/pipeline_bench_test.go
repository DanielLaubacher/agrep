package matcher

import (
	"bytes"
	"fmt"
	"testing"
)

// BenchmarkPipeline_SparseMatch benchmarks a two-stage pipeline vs a single
// complex regex on data with 1% match density.
// Pipeline: -Fe "ERROR" -toe "\d+"
// Single:   "ERROR.*\d+"
func BenchmarkPipeline_SparseMatch(b *testing.B) {
	data := makeSparseData(10000, 100)

	b.Run("SingleRegex", func(b *testing.B) {
		m, _ := NewMatcher([]string{`ERROR.*\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("Pipeline_Fixed+Regex", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

// BenchmarkPipeline_DenseMatch benchmarks pipeline vs single regex when
// all lines match (worst case for pipeline overhead).
func BenchmarkPipeline_DenseMatch(b *testing.B) {
	data := makeDenseData(10000)

	b.Run("SingleRegex", func(b *testing.B) {
		m, _ := NewMatcher([]string{`ERROR.*\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("Pipeline_Fixed+Regex", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

// BenchmarkPipeline_ThreeStage benchmarks a three-stage pipeline.
// Pipeline: -Fe "HTTP" -te "status=\d+" -toe "status=\d+"
// Single:   "HTTP.*status=\d+"
func BenchmarkPipeline_ThreeStage(b *testing.B) {
	data := makeHTTPData(10000, 10)

	b.Run("SingleRegex", func(b *testing.B) {
		m, _ := NewMatcher([]string{`HTTP.*status=\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("Pipeline_3Stage", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: "HTTP", Fixed: true},
			{Pattern: `status=\d+`},
			{Pattern: `status=\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

// BenchmarkPipeline_MatchExists benchmarks the -l fast path.
func BenchmarkPipeline_MatchExists(b *testing.B) {
	data := makeSparseData(10000, 100)

	b.Run("SingleRegex", func(b *testing.B) {
		m, _ := NewMatcher([]string{`ERROR.*\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.MatchExists(data)
		}
	})

	b.Run("Pipeline_Fixed+Regex", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.MatchExists(data)
		}
	})
}

// BenchmarkPipeline_OnlyMatch benchmarks -o output mode vs regular.
func BenchmarkPipeline_OnlyMatch(b *testing.B) {
	data := makeDenseData(10000)

	b.Run("FindAll_NoOnlyMatch", func(b *testing.B) {
		m, _ := NewMatcher([]string{`\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})

	b.Run("FindAll_OnlyMatch", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: `\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.FindAll(data)
		}
	})
}

// BenchmarkPipeline_CountAll benchmarks -c mode with pipeline.
func BenchmarkPipeline_CountAll(b *testing.B) {
	data := makeSparseData(10000, 100)

	b.Run("SingleRegex", func(b *testing.B) {
		m, _ := NewMatcher([]string{`ERROR.*\d+`}, false, false, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.CountAll(data)
		}
	})

	b.Run("Pipeline_Fixed+Regex", func(b *testing.B) {
		stages := [][]StageConfig{{
			{Pattern: "ERROR", Fixed: true},
			{Pattern: `\d+`, OnlyMatch: true},
		}}
		m, _ := NewMatcherFromPipelines(stages, false, false, MatcherOpts{})
		b.SetBytes(int64(len(data)))
		b.ResetTimer()
		for b.Loop() {
			m.CountAll(data)
		}
	})
}

// makeSparseData creates test data where 1 in every `sparsity` lines contain "ERROR: code NNN".
func makeSparseData(lines int, sparsity int) []byte {
	var buf bytes.Buffer
	for i := range lines {
		if i%sparsity == 0 {
			fmt.Fprintf(&buf, "ERROR: code %d timeout\n", 500+i%10)
		} else {
			buf.WriteString("INFO: the quick brown fox jumps over the lazy dog\n")
		}
	}
	return buf.Bytes()
}

// makeDenseData creates test data where every line contains "ERROR" and digits.
func makeDenseData(lines int) []byte {
	var buf bytes.Buffer
	for i := range lines {
		fmt.Fprintf(&buf, "ERROR: operation %d failed with code %d\n", i, 500+i%10)
	}
	return buf.Bytes()
}

// makeHTTPData creates test data with HTTP status lines at given density.
func makeHTTPData(lines int, sparsity int) []byte {
	var buf bytes.Buffer
	for i := range lines {
		if i%sparsity == 0 {
			fmt.Fprintf(&buf, "HTTP/1.1 status=%d OK request_id=abc%d\n", 200+i%5, i)
		} else {
			buf.WriteString("FTP: transfer complete, bytes=1024\n")
		}
	}
	return buf.Bytes()
}
