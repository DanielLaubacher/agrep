package cli

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

// parallelTestData builds a >parallelThreshold buffer with matches scattered
// throughout, including near chunk boundaries.
func parallelTestData(t testing.TB) []byte {
	rng := rand.New(rand.NewSource(7))
	var buf bytes.Buffer
	for buf.Len() < parallelThreshold+chunkMinSize {
		r := rng.Float64()
		switch {
		case r < 0.01:
			fmt.Fprintf(&buf, "ERROR connection refused at port %d\n", 1000+rng.Intn(9000))
		case r < 0.02:
			fmt.Fprintf(&buf, "date 2024-%02d-%02d event\n", 1+rng.Intn(12), 1+rng.Intn(28))
		default:
			fmt.Fprintf(&buf, "filler line %d with ordinary text content here\n", rng.Int())
		}
	}
	return buf.Bytes()
}

func TestParallelFindAllMatchesSequential(t *testing.T) {
	data := parallelTestData(t)

	patterns := []struct {
		name    string
		pattern string
		fixed   bool
	}{
		{"prefix-regex", `ERROR.*port [0-9]+`, false},
		{"plain-regex", `\d{4}-\d{2}-\d{2}`, false},
		{"fixed", `connection refused`, true},
	}

	for _, tc := range patterns {
		for _, lineNums := range []bool{false, true} {
			m, err := matcher.NewMatcher([]string{tc.pattern}, tc.fixed, false, false, false,
				matcher.MatcherOpts{NeedLineNums: lineNums})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !lineBounded(m) {
				t.Fatalf("%s: expected LineBounded matcher", tc.name)
			}

			seq := m.FindAll(data)
			par := parallelFindAll(data, m, lineNums)

			if !reflect.DeepEqual(seq.Matches, par.Matches) {
				t.Errorf("%s (lineNums=%v): Matches differ: seq=%d par=%d",
					tc.name, lineNums, len(seq.Matches), len(par.Matches))
				continue
			}
			if !reflect.DeepEqual(seq.Positions, par.Positions) {
				t.Errorf("%s (lineNums=%v): Positions differ", tc.name, lineNums)
			}

			if gotC, wantC := parallelCountAll(data, m), m.CountAll(data); gotC != wantC {
				t.Errorf("%s: parallelCountAll=%d want %d", tc.name, gotC, wantC)
			}
			if gotE, wantE := parallelMatchExists(data, m), m.MatchExists(data); gotE != wantE {
				t.Errorf("%s: parallelMatchExists=%v want %v", tc.name, gotE, wantE)
			}
		}
	}
}

func TestChunkBoundsLineAligned(t *testing.T) {
	data := parallelTestData(t)
	chunks := chunkBounds(data, 8)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	prev := 0
	for i, c := range chunks {
		if c[0] != prev {
			t.Fatalf("chunk %d starts at %d, want %d (contiguous)", i, c[0], prev)
		}
		if i < len(chunks)-1 && data[c[1]-1] != '\n' {
			t.Fatalf("chunk %d does not end at a line boundary", i)
		}
		prev = c[1]
	}
	if prev != len(data) {
		t.Fatalf("chunks cover %d bytes, want %d", prev, len(data))
	}
}
