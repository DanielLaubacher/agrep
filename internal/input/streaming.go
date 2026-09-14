package input

import (
	"bufio"
	"io"

	"github.com/DanielLaubacher/agrep/internal/matcher"
)

// StreamingReader processes an io.Reader line-by-line for streaming search.
// Unlike batch readers, it doesn't load the entire file into memory.
type StreamingReader struct {
	scanner *bufio.Scanner
	matcher matcher.Matcher
}

// NewStreamingReader creates a StreamingReader for the given io.Reader.
func NewStreamingReader(r io.Reader) *StreamingReader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return &StreamingReader{
		scanner: scanner,
	}
}

// StreamLine represents a single line read from the stream.
type StreamLine struct {
	Data    []byte
	LineNum int
	Offset  int64
	Err     error
}

// Lines returns a channel that yields lines from the stream.
func (r *StreamingReader) Lines() <-chan StreamLine {
	ch := make(chan StreamLine, 256)
	go func() {
		defer close(ch)
		lineNum := 0
		var offset int64
		for r.scanner.Scan() {
			lineNum++
			line := r.scanner.Bytes()
			// Copy the line since scanner reuses the buffer
			cp := make([]byte, len(line))
			copy(cp, line)
			ch <- StreamLine{
				Data:    cp,
				LineNum: lineNum,
				Offset:  offset,
			}
			offset += int64(len(line)) + 1
		}
		if err := r.scanner.Err(); err != nil {
			ch <- StreamLine{Err: err}
		}
	}()
	return ch
}

// SearchStream performs a streaming search, yielding matches as they are found.
// This is useful for piped input or tail-like watching where the entire content
// is not available upfront. Each emitted MatchSet contains a single match/context line.
func SearchStream(r io.Reader, m matcher.Matcher, before, after int) <-chan matcher.MatchSet {
	ch := make(chan matcher.MatchSet, 64)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

		lineNum := 0
		var offset int64

		// Ring buffer for context-before lines
		var ring []contextLine
		if before > 0 {
			ring = make([]contextLine, 0, before)
		}

		afterRemaining := 0

		for scanner.Scan() {
			lineNum++
			line := scanner.Bytes()
			lineCopy := make([]byte, len(line))
			copy(lineCopy, line)

			ms, ok := m.FindLine(lineCopy, lineNum, offset)
			offset += int64(len(line)) + 1

			if ok {
				// Emit buffered context-before lines
				for _, cl := range ring {
					ch <- matcher.MatchSet{
						Data: cl.data,
						Matches: []matcher.Match{{
							LineNum:    cl.lineNum,
							LineStart:  0,
							LineLen:    len(cl.data),
							ByteOffset: cl.offset,
							IsContext:  true,
						}},
					}
				}
				ring = ring[:0]

				// Emit the match
				ch <- ms
				afterRemaining = after
			} else if afterRemaining > 0 {
				// Context-after line
				ch <- matcher.MatchSet{
					Data: lineCopy,
					Matches: []matcher.Match{{
						LineNum:    lineNum,
						LineStart:  0,
						LineLen:    len(lineCopy),
						ByteOffset: offset - int64(len(line)) - 1,
						IsContext:  true,
					}},
				}
				afterRemaining--
			} else if before > 0 {
				// Store in ring buffer for potential context-before
				if len(ring) >= before {
					ring = ring[1:]
				}
				ring = append(ring, contextLine{
					data:    lineCopy,
					lineNum: lineNum,
					offset:  offset - int64(len(line)) - 1,
				})
			}
		}
	}()
	return ch
}

type contextLine struct {
	data    []byte
	lineNum int
	offset  int64
}
