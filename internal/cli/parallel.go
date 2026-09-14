package cli

// Parallel single-buffer search. ripgrep searches one file with one thread;
// agrep splits large buffers into line-aligned chunks and searches them
// concurrently, then rebases and merges the per-chunk results. Valid only
// when the matcher guarantees matches never span a newline (LineBounded),
// which also implies chunk results are position-rebasable.

import (
	"bytes"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

const (
	// parallelThreshold is the minimum buffer size worth chunking; below
	// this the goroutine + merge overhead exceeds the win.
	parallelThreshold = 4 << 20
	// chunkMinSize keeps chunks large enough to amortize per-chunk setup.
	chunkMinSize = 1 << 20
)

// lineBounded reports whether the matcher guarantees no match contains '\n'.
// Matchers advertising this are also safe for concurrent use.
func lineBounded(m matcher.Matcher) bool {
	type lb interface{ LineBounded() bool }
	if v, ok := m.(lb); ok {
		return v.LineBounded()
	}
	return false
}

func useParallelSearch(data []byte, m matcher.Matcher) bool {
	return len(data) >= parallelThreshold && runtime.GOMAXPROCS(0) > 1 && lineBounded(m)
}

// chunkBounds splits data into up to want line-aligned [start,end) chunks.
// Every chunk except the last ends just past a '\n'.
func chunkBounds(data []byte, want int) [][2]int {
	n := len(data)
	if want > n/chunkMinSize {
		want = n / chunkMinSize
	}
	if want < 2 {
		return [][2]int{{0, n}}
	}
	chunks := make([][2]int, 0, want)
	target := n / want
	start := 0
	for start < n {
		end := start + target
		if end >= n {
			chunks = append(chunks, [2]int{start, n})
			break
		}
		if i := bytes.IndexByte(data[end:], '\n'); i >= 0 {
			end += i + 1
		} else {
			end = n
		}
		chunks = append(chunks, [2]int{start, end})
		start = end
	}
	return chunks
}

// parallelMatchExists runs MatchExists over chunks concurrently with early
// cancellation once any chunk matches.
func parallelMatchExists(data []byte, m matcher.Matcher) bool {
	chunks := chunkBounds(data, runtime.GOMAXPROCS(0))
	if len(chunks) == 1 {
		return m.MatchExists(data)
	}
	var found atomic.Bool
	var wg sync.WaitGroup
	for _, c := range chunks {
		wg.Add(1)
		go func(c [2]int) {
			defer wg.Done()
			if found.Load() {
				return
			}
			if m.MatchExists(data[c[0]:c[1]]) {
				found.Store(true)
			}
		}(c)
	}
	wg.Wait()
	return found.Load()
}

// parallelCountAll sums per-chunk match-line counts. Line-aligned chunks
// mean no line is split, so per-chunk counts add exactly.
func parallelCountAll(data []byte, m matcher.Matcher) int {
	chunks := chunkBounds(data, runtime.GOMAXPROCS(0))
	if len(chunks) == 1 {
		return m.CountAll(data)
	}
	counts := make([]int, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func(i int, c [2]int) {
			defer wg.Done()
			counts[i] = m.CountAll(data[c[0]:c[1]])
		}(i, c)
	}
	wg.Wait()
	total := 0
	for _, c := range counts {
		total += c
	}
	return total
}

// parallelFindAll searches chunks concurrently and merges the MatchSets,
// rebasing chunk-relative offsets (and line numbers, when needed) onto the
// full buffer. The merged MatchSet references the original data, so the
// formatter and everything downstream are unchanged.
func parallelFindAll(data []byte, m matcher.Matcher, needLineNums bool) matcher.MatchSet {
	chunks := chunkBounds(data, runtime.GOMAXPROCS(0))
	if len(chunks) == 1 {
		return m.FindAll(data)
	}

	sets := make([]matcher.MatchSet, len(chunks))
	nlCounts := make([]int, len(chunks))
	var wg sync.WaitGroup
	for i, c := range chunks {
		wg.Add(1)
		go func(i int, c [2]int) {
			defer wg.Done()
			chunk := data[c[0]:c[1]]
			sets[i] = m.FindAll(chunk)
			if needLineNums {
				// Counted while the chunk is cache-hot in this worker.
				nlCounts[i] = bytes.Count(chunk, []byte{'\n'})
			}
		}(i, c)
	}
	wg.Wait()

	totalM, totalP := 0, 0
	for _, s := range sets {
		totalM += len(s.Matches)
		totalP += len(s.Positions)
	}
	if totalM == 0 {
		return matcher.MatchSet{}
	}

	matches := make([]matcher.Match, 0, totalM)
	positions := make([][2]int, 0, totalP)
	lineBase := 0
	for i, s := range sets {
		chunkStart := chunks[i][0]
		posBase := len(positions)
		positions = append(positions, s.Positions...)
		for _, mt := range s.Matches {
			mt.LineStart += chunkStart
			mt.ByteOffset += int64(chunkStart)
			mt.LineNum += lineBase
			mt.PosIdx += posBase
			matches = append(matches, mt)
		}
		lineBase += nlCounts[i]
	}
	return matcher.MatchSet{Data: data, Matches: matches, Positions: positions}
}
