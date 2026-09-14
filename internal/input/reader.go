// Package input provides file-reading strategies behind a single Reader
// interface: pooled buffered pread for small files, mmap with sequential
// madvise hints for large ones, and an adaptive reader that picks per
// file. Buffers are returned via each ReadResult's Closer, which must be
// called exactly once after the data is no longer referenced.
package input

// ReadResult holds the data read from a file and a cleanup function.
type ReadResult struct {
	Data   []byte
	Closer func() error
}

// noopCloser is a package-level no-op closer to avoid allocating a func literal per file.
func noopCloser() error { return nil }

// Reader reads file content into a byte slice.
type Reader interface {
	Read(path string) (ReadResult, error)
}
