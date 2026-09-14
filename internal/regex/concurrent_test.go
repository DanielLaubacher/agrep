package regex

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentUse verifies that one compiled Regexp is safe to share across
// goroutines. Before compile-time DFA precomputation, lazy transition
// computation raced when a Regexp was shared by scheduler workers
// (index-out-of-range panics in computeTransition).
func TestConcurrentUse(t *testing.T) {
	patterns := []string{
		`err(or|no|code)`,
		`\d{4}-\d{2}-\d{2}`,
		`[a-zA-Z]+@[a-zA-Z]+\.[a-zA-Z]+`,
		`ERROR.*port [0-9]+`,
		`[A-Z]{2,}`,
		`struct\s+\w+`,
	}

	// Varied inputs so different goroutines drive the DFA down different paths.
	var inputs [][]byte
	for i := 0; i < 64; i++ {
		var buf bytes.Buffer
		for j := 0; j < 200; j++ {
			fmt.Fprintf(&buf, "line %d-%d error errno errcode 2024-01-%02d struct Foo user@example.com ERROR at port %d\n",
				i, j, (j%28)+1, 8000+j)
		}
		inputs = append(inputs, buf.Bytes())
	}

	for _, pat := range patterns {
		re, err := Compile(pat)
		if err != nil {
			t.Fatalf("Compile(%q): %v", pat, err)
		}
		var wg sync.WaitGroup
		for w := 0; w < 16; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i, data := range inputs {
					if !re.Match(data) {
						t.Errorf("pattern %q: Match=false on input %d", pat, i)
						return
					}
					if locs := re.FindAllIndex(data, -1); len(locs) == 0 {
						t.Errorf("pattern %q: FindAllIndex empty on input %d", pat, i)
						return
					}
				}
			}(w)
		}
		wg.Wait()
	}
}
