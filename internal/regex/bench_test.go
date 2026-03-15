package regex

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// generateText creates realistic English-like text for benchmarking.
func generateText(size int) []byte {
	words := []string{
		"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog",
		"error", "warning", "timeout", "connection", "failed", "retry",
		"processing", "request", "response", "server", "client", "data",
		"function", "return", "value", "struct", "interface", "package",
		"import", "const", "var", "type", "func", "defer", "go", "select",
		"channel", "goroutine", "mutex", "lock", "unlock", "sync", "atomic",
		"2024-01-15", "192.168.1.1", "user@host.com", "https://example.com",
		"INFO", "DEBUG", "ERROR", "WARN", "FATAL",
	}

	rng := rand.New(rand.NewSource(42))
	var buf bytes.Buffer
	lineLen := 0
	for buf.Len() < size {
		word := words[rng.Intn(len(words))]
		if lineLen > 0 {
			buf.WriteByte(' ')
			lineLen++
		}
		buf.WriteString(word)
		lineLen += len(word)
		if lineLen > 60+rng.Intn(40) {
			buf.WriteByte('\n')
			lineLen = 0
		}
	}
	return buf.Bytes()[:size]
}

// generateSparseText creates text where matches are rare.
func generateSparseText(size int) []byte {
	var buf bytes.Buffer
	line := strings.Repeat("abcdefghijklmnopqrstuvwxyz ", 3)
	for buf.Len() < size {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	// Insert a few matches
	data := buf.Bytes()[:size]
	// Add a needle every ~10000 bytes
	needle := []byte("NEEDLE_MATCH_HERE")
	for i := 5000; i < len(data)-len(needle); i += 10000 {
		copy(data[i:], needle)
	}
	return data
}

var benchPatterns = []struct {
	name    string
	pattern string
}{
	{"Literal", "timeout"},
	{"LiteralLong", "connection failed"},
	{"CharClass", `[a-zA-Z]+`},
	{"Digits", `\d+`},
	{"DatePattern", `\d{4}-\d{2}-\d{2}`},
	{"EmailLike", `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`},
	{"Alternation", `(?:error|warning|timeout)`},
	{"DotStar", `error.*timeout`},
	{"Repetition", `a{2,4}`},
	{"ComplexClass", `[^aeiou\s]+`},
}

// BenchmarkMatch_Stdlib benchmarks stdlib regexp.Match.
func BenchmarkMatch_Stdlib(b *testing.B) {
	data := generateText(1 << 20) // 1MB
	for _, bp := range benchPatterns {
		re := regexp.MustCompile(bp.pattern)
		b.Run(bp.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				re.Match(data)
			}
		})
	}
}

// BenchmarkMatch_Regex benchmarks our regex.Match.
func BenchmarkMatch_Regex(b *testing.B) {
	data := generateText(1 << 20) // 1MB
	for _, bp := range benchPatterns {
		re := MustCompile(bp.pattern)
		b.Run(bp.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				re.Match(data)
			}
		})
	}
}

// BenchmarkFindAllIndex_Stdlib benchmarks stdlib FindAllIndex.
func BenchmarkFindAllIndex_Stdlib(b *testing.B) {
	data := generateText(1 << 20)
	for _, bp := range benchPatterns {
		re := regexp.MustCompile(bp.pattern)
		b.Run(bp.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				re.FindAllIndex(data, -1)
			}
		})
	}
}

// BenchmarkFindAllIndex_Regex benchmarks our FindAllIndex.
func BenchmarkFindAllIndex_Regex(b *testing.B) {
	data := generateText(1 << 20)
	for _, bp := range benchPatterns {
		re := MustCompile(bp.pattern)
		b.Run(bp.name, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				re.FindAllIndex(data, -1)
			}
		})
	}
}

// BenchmarkSparseMatch compares on sparse data (few matches in large file).
func BenchmarkSparseMatch_Stdlib(b *testing.B) {
	data := generateSparseText(1 << 20)
	re := regexp.MustCompile("NEEDLE_MATCH_HERE")
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		re.FindAllIndex(data, -1)
	}
}

func BenchmarkSparseMatch_Regex(b *testing.B) {
	data := generateSparseText(1 << 20)
	re := MustCompile("NEEDLE_MATCH_HERE")
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		re.FindAllIndex(data, -1)
	}
}

// BenchmarkBySize benchmarks across different input sizes.
func BenchmarkBySize(b *testing.B) {
	sizes := []int{1 << 10, 1 << 14, 1 << 18, 1 << 20} // 1K, 16K, 256K, 1M
	pattern := `\d{4}-\d{2}-\d{2}`

	for _, size := range sizes {
		data := generateText(size)
		name := fmt.Sprintf("size=%d", size)

		b.Run("Stdlib/"+name, func(b *testing.B) {
			re := regexp.MustCompile(pattern)
			b.SetBytes(int64(size))
			for b.Loop() {
				re.FindAllIndex(data, -1)
			}
		})

		b.Run("Regex/"+name, func(b *testing.B) {
			re := MustCompile(pattern)
			b.SetBytes(int64(size))
			for b.Loop() {
				re.FindAllIndex(data, -1)
			}
		})
	}
}
