// Package matcher implements agrep's pattern-matching engines behind the
// Matcher interface: SIMD Boyer-Moore for single fixed strings, rare-pair
// Teddy and Aho-Corasick for fixed-string sets, a custom lazy-DFA regex
// engine (internal/regex), optional PCRE2, and pipeline composition. The
// factory in factory.go selects an engine from the pattern shape.
package matcher

// Match is a pointer-free struct representing a single matched (or context) line.
// Line content and positions are resolved via the owning MatchSet's shared backing arrays.
// Because Match contains no pointer types, a []Match does not cause GC scanning.
type Match struct {
	LineNum    int   // 1-based line number (0 = group separator)
	LineStart  int   // byte offset of line snippet start in MatchSet.Data
	LineLen    int   // length of line snippet in bytes
	ByteOffset int64 // byte offset of line start within the original file
	PosIdx     int   // start index into MatchSet.Positions
	PosCount   int   // number of highlight positions for this match
	CapIdx     int   // start index into MatchSet.Captures (-S holes)
	CapCount   int   // number of captures for this match
	IsContext  bool
}

// Capture is one structural hole binding: the text Data[Start:End)
// matched by :[Name] (offsets are absolute within MatchSet.Data).
type Capture struct {
	Name       string
	Start, End int
}

// MatchSet holds matches and the shared backing data they reference.
// Only MatchSet contains pointer types — individual Match structs are pointer-free,
// so the GC scans O(1) pointers regardless of match count.
type MatchSet struct {
	Data      []byte    // the file data buffer (matches reference offsets into this)
	Matches   []Match   // pointer-free match structs
	Positions [][2]int  // shared positions array; each match indexes a sub-range
	Captures  []Capture // shared hole-capture array (-S); indexed like Positions
}

// Len returns the number of matches.
func (ms *MatchSet) Len() int {
	return len(ms.Matches)
}

// LineBytes returns the line content for match at index i.
func (ms *MatchSet) LineBytes(i int) []byte {
	m := &ms.Matches[i]
	return ms.Data[m.LineStart : m.LineStart+m.LineLen]
}

// MatchPositions returns the highlight positions for match at index i.
func (ms *MatchSet) MatchPositions(i int) [][2]int {
	m := &ms.Matches[i]
	if m.PosCount == 0 {
		return nil
	}
	return ms.Positions[m.PosIdx : m.PosIdx+m.PosCount]
}

// MatchCaptures returns the hole captures for match at index i.
func (ms *MatchSet) MatchCaptures(i int) []Capture {
	m := &ms.Matches[i]
	if m.CapCount == 0 {
		return nil
	}
	return ms.Captures[m.CapIdx : m.CapIdx+m.CapCount]
}

// HasMatch returns true if the set contains at least one match.
func (ms *MatchSet) HasMatch() bool {
	return len(ms.Matches) > 0
}

// Matcher finds pattern matches in data.
type Matcher interface {
	// FindAll scans data (full file content) and returns all matches.
	FindAll(data []byte) MatchSet

	// MatchExists returns true if there is at least one match in data.
	MatchExists(data []byte) bool

	// CountAll returns the number of matching lines in data.
	CountAll(data []byte) int

	// FindLine checks a single line for matches.
	// lineNum is 1-based, byteOffset is the offset of the line start in the file.
	FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool)
}

// WithMatches returns a copy of the set sharing Data and Positions but
// holding only the given matches (used by output-side filtering).
func (ms *MatchSet) WithMatches(matches []Match) MatchSet {
	return MatchSet{Data: ms.Data, Matches: matches, Positions: ms.Positions, Captures: ms.Captures}
}
