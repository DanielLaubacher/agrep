package output

// chunkKey tracks the (file, query) of the previous Format call so a
// formatter can tell whether the current call continues the same result
// split across budget chunks (BudgetFormatter emits one file's matches
// across several consecutive calls) rather than starting a new file.
// Shared by JSONFormatter (file tally dedup) and BlockFormatter (block
// dedup), whose only difference is what they do once they know.
type chunkKey struct {
	file  string
	query string
}

// sameAs reports whether (file, query) continues the same chunk as the
// last call.
func (k *chunkKey) sameAs(file, query string) bool {
	return file == k.file && query == k.query
}

// set records (file, query) as the current chunk key.
func (k *chunkKey) set(file, query string) {
	k.file, k.query = file, query
}
