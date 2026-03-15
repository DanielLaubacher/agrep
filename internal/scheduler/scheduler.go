package scheduler

import (
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/dl/gogrep/internal/input"
	"github.com/dl/gogrep/internal/matcher"
	"github.com/dl/gogrep/internal/output"
	"github.com/dl/gogrep/internal/walker"
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

// Run processes files from the file channel and returns results on the result channel.
// Results include sequence numbers for ordered output.
func (s *Scheduler) Run(files <-chan walker.FileEntry) <-chan output.Result {
	resultCh := make(chan output.Result, s.workers*2)
	var seq atomic.Int64

	var wg sync.WaitGroup
	for range s.workers {
		wg.Go(func() {
			for entry := range files {
				seqNum := int(seq.Add(1))
				result := s.processFile(entry)
				result.SeqNum = seqNum
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
