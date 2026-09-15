// Package scheduler distributes files from the walker across a worker
// pool (NumCPU*2 goroutines), each reading and matching independently and
// emitting sequence-numbered results for ordered output.
package scheduler

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/DanielLaubacher/agrep/internal/input"
	"github.com/DanielLaubacher/agrep/internal/matcher"
	"github.com/DanielLaubacher/agrep/internal/output"
	"github.com/DanielLaubacher/agrep/internal/walker"
)

// Scheduler manages a pool of workers that search files concurrently.
type Scheduler struct {
	workers   int
	matcher   matcher.Matcher
	reader    input.Reader
	filesOnly bool // when true, use MatchExists for faster -l mode
	countOnly bool // when true, use CountAll for faster -c mode
}

// New creates a Scheduler with the given number of workers.
// If workers is 0, defaults to NumCPU * 2.
func New(workers int, m matcher.Matcher, r input.Reader, filesOnly bool, countOnly bool) *Scheduler {
	if workers <= 0 {
		workers = runtime.NumCPU() * 2
	}
	return &Scheduler{
		workers:   workers,
		matcher:   m,
		reader:    r,
		filesOnly: filesOnly,
		countOnly: countOnly,
	}
}

// seqEntry pairs a file with its walk-order sequence number. Sequence
// numbers are assigned by a single tagger goroutine before workers
// consume entries — claiming them inside the workers would race, making
// output order (and therefore budgeted output) vary between runs.
type seqEntry struct {
	entry walker.FileEntry
	seq   int
}

// tagEntries assigns walk-order sequence numbers to incoming files.
func tagEntries(files <-chan walker.FileEntry, buffered int) <-chan seqEntry {
	tagged := make(chan seqEntry, buffered)
	go func() {
		defer close(tagged)
		seq := 0
		for entry := range files {
			seq++
			tagged <- seqEntry{entry: entry, seq: seq}
		}
	}()
	return tagged
}

// Run processes files from the file channel and returns results on the result channel.
// Results include sequence numbers for ordered output.
func (s *Scheduler) Run(files <-chan walker.FileEntry) <-chan output.Result {
	resultCh := make(chan output.Result, s.workers*2)
	tagged := tagEntries(files, s.workers*2)

	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() {
			for te := range tagged {
				result := s.processFile(te.entry)
				result.SeqNum = te.seq
				resultCh <- result
			}
		})
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	return resultCh
}

// RunBatch searches every file with all matchers (reading each file
// once), emitting one sequence-numbered result per (file, query) so the
// OrderedWriter can attribute matches to queries deterministically.
// Empty results are emitted too — sequence numbers must stay contiguous.
func (s *Scheduler) RunBatch(files <-chan walker.FileEntry, matchers []matcher.Matcher, queries []string) <-chan output.Result {
	q := len(matchers)
	resultCh := make(chan output.Result, s.workers*2)
	tagged := tagEntries(files, s.workers*2)

	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() {
			for te := range tagged {
				base := (te.seq - 1) * q
				results := s.processFileBatch(te.entry, matchers, queries)
				for i := range results {
					results[i].SeqNum = base + i + 1
					resultCh <- results[i]
				}
			}
		})
	}

	go func() {
		wg.Wait()
		close(resultCh)
	}()

	return resultCh
}

// processFileBatch reads entry once and runs every matcher over it. In
// full mode, results with matches share the file buffer via a
// reference-counted closer; the buffer is released once the last of them
// has been formatted.
func (s *Scheduler) processFileBatch(entry walker.FileEntry, matchers []matcher.Matcher, queries []string) []output.Result {
	results := make([]output.Result, len(matchers))
	for i := range results {
		results[i] = output.Result{FilePath: entry.Path, Query: queries[i]}
	}

	readResult, err := s.reader.Read(entry.Path)
	if err != nil {
		results[0].Err = err
		return results
	}
	closeReader := func() {
		if readResult.Closer != nil {
			readResult.Closer()
		}
	}

	if readResult.Data == nil || walker.IsBinary(readResult.Data) {
		closeReader()
		return results
	}

	holders := 0
	for i, m := range matchers {
		switch {
		case s.filesOnly:
			if m.MatchExists(readResult.Data) {
				results[i].MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
			}
		case s.countOnly:
			results[i].MatchCount = m.CountAll(readResult.Data)
		default:
			results[i].MatchSet = m.FindAll(readResult.Data)
			if results[i].MatchSet.HasMatch() {
				holders++
			}
		}
	}

	if holders == 0 {
		closeReader()
		return results
	}
	var remaining atomic.Int32
	remaining.Store(int32(holders))
	shared := func() {
		if remaining.Add(-1) == 0 {
			closeReader()
		}
	}
	for i := range results {
		if results[i].MatchSet.HasMatch() && !s.filesOnly && !s.countOnly {
			results[i].Closer = shared
		}
	}
	return results
}

func (s *Scheduler) processFile(entry walker.FileEntry) output.Result {
	result := output.Result{FilePath: entry.Path}

	readResult, err := s.reader.Read(entry.Path)
	if err != nil {
		result.Err = err
		return result
	}

	closeReader := func() {
		if readResult.Closer != nil {
			readResult.Closer()
		}
	}

	if readResult.Data == nil {
		closeReader()
		return result
	}

	// Binary detection: skip binary files entirely (like ripgrep)
	if walker.IsBinary(readResult.Data) {
		closeReader()
		return result
	}

	if s.filesOnly {
		if s.matcher.MatchExists(readResult.Data) {
			result.MatchSet = matcher.MatchSet{Matches: []matcher.Match{{}}}
		}
		closeReader()
	} else if s.countOnly {
		count := s.matcher.CountAll(readResult.Data)
		result.MatchCount = count
		closeReader()
	} else {
		result.MatchSet = s.matcher.FindAll(readResult.Data)
		if result.MatchSet.HasMatch() {
			result.Closer = closeReader
		} else {
			closeReader()
		}
	}
	return result
}
