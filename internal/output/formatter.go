// Package output formats and writes search results: text and JSON Lines
// formatters, a writev-based writer, and an OrderedWriter that restores
// deterministic ordering across parallel workers while batching output
// into large flushes.
package output

// Formatter formats a Result into bytes for output.
// buf is a reusable buffer — implementations append to it and return the result.
// Callers can pass buf[:0] to reuse the underlying array without allocating.
type Formatter interface {
	Format(buf []byte, result Result, multiFile bool) []byte
}
