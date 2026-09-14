package output

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Writer writes formatted output to stdout, using writev for batching.
type Writer struct {
	fd int
}

// NewWriter creates a Writer that writes to stdout.
func NewWriter() *Writer {
	return &Writer{fd: int(os.Stdout.Fd())}
}

// Write writes the given bytes to stdout using writev for scatter-gather I/O.
func (w *Writer) Write(data []byte) error {
	if len(data) == 0 {
		return nil
	}

	for len(data) > 0 {
		iovs := [][]byte{data}
		n, err := unix.Writev(w.fd, iovs)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

// OrderedWriter receives results from a channel and writes them in sequence order.
// This ensures output is deterministic even with parallel workers.
type OrderedWriter struct {
	writer    *Writer
	formatter Formatter
	multiFile bool
}

// NewOrderedWriter creates an OrderedWriter.
func NewOrderedWriter(w *Writer, f Formatter, multiFile bool) *OrderedWriter {
	return &OrderedWriter{
		writer:    w,
		formatter: f,
		multiFile: multiFile,
	}
}

// flushThreshold is the accumulated-output size that triggers a writev.
// Batching many small per-file results into one syscall matters on
// output-heavy recursive searches (tens of thousands of matching files).
const flushThreshold = 256 * 1024

// WriteOrdered consumes results from the channel, buffering out-of-order results
// and writing them in sequence-number order. Formatted output accumulates in a
// single reused buffer and is flushed in large batches to minimize syscalls.
func (ow *OrderedWriter) WriteOrdered(results <-chan Result, onMatch func()) {
	nextSeq := 1
	pending := make(map[int]Result)
	var out []byte // accumulated formatted output, flushed in batches

	for r := range results {
		if r.Err == nil && r.HasMatch() {
			if onMatch != nil {
				onMatch()
			}
		}

		if r.SeqNum == nextSeq {
			out = ow.writeResult(out, r)
			nextSeq++
			// Flush any consecutive pending results
			for {
				if p, ok := pending[nextSeq]; ok {
					out = ow.writeResult(out, p)
					delete(pending, nextSeq)
					nextSeq++
				} else {
					break
				}
			}
		} else {
			pending[r.SeqNum] = r
		}
	}

	if fin, ok := ow.formatter.(Finisher); ok {
		out = fin.Finish(out)
	}
	if len(out) > 0 {
		ow.writer.Write(out)
	}
}

func (ow *OrderedWriter) writeResult(out []byte, r Result) []byte {
	if r.Err != nil {
		// Never silent: stderr for humans; the formatter additionally
		// puts an error object in the stream for JSON consumers.
		fmt.Fprintf(os.Stderr, "agrep: %s: %v\n", r.FilePath, r.Err)
	}
	out = ow.formatter.Format(out, r, ow.multiFile)
	if r.Closer != nil {
		r.Closer()
	}
	if len(out) >= flushThreshold {
		ow.writer.Write(out)
		out = out[:0]
	}
	return out
}
