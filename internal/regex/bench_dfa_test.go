package regex

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestBenchFindAllIndex(t *testing.T) {
	data, err := os.ReadFile("/tmp/agrep_bench_data.txt")
	if err != nil {
		t.Skip("bench data not available")
	}

	patterns := []struct {
		name    string
		pattern string
	}{
		{"DatePattern", `\d{4}-\d{2}-\d{2}`},
		{"UpperWords", `[A-Z]{2,}`},
		{"Email", `[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`},
	}

	for _, p := range patterns {
		re, err := Compile(p.pattern)
		if err != nil {
			t.Fatal(err)
		}

		// Warmup
		re.FindAllIndex(data, -1)

		best := time.Duration(1<<63 - 1)
		var count int
		for i := 0; i < 10; i++ {
			start := time.Now()
			matches := re.FindAllIndex(data, -1)
			elapsed := time.Since(start)
			count = len(matches)
			if elapsed < best {
				best = elapsed
			}
		}
		fmt.Printf("%s: %v (best of 10), %d matches, %.1f MB\n", p.name, best, count, float64(len(data))/1e6)
	}
}
