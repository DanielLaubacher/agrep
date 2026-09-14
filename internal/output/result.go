package output

import "github.com/dl/gogrep/internal/matcher"

// Result aggregates the matches found in a single file.
type Result struct {
	FilePath string
	SeqNum   int
	MatchSet matcher.MatchSet
	// Query names the batch query that produced this result (--batch);
	// empty for single-query searches.
	Query string
	// MatchCount holds the count for -c mode without building Match structs.
	// When set to 0 (default), len(MatchSet.Matches) is used instead.
	MatchCount int
	Err        error
	// Closer releases the underlying buffer that MatchSet.Data points into.
	// Must be called after the result has been fully formatted/consumed.
	Closer func()
}

// Count returns the number of matches in this result.
func (r *Result) Count() int {
	if r.MatchCount > 0 {
		return r.MatchCount
	}
	return len(r.MatchSet.Matches)
}

// HasMatch returns true if this result has at least one match.
func (r *Result) HasMatch() bool {
	return r.MatchCount > 0 || len(r.MatchSet.Matches) > 0
}
