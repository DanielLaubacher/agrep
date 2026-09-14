//go:build !pcre

package matcher

import "fmt"

// PCREMatcher stub for builds without PCRE support.
//
// The real PCRE backend (go.elara.ws/pcre) transitively links
// modernc.org/libc, whose netdb package init parses /etc/services
// (~300KB) at every process start — a ~5ms tax on every agrep
// invocation, even ones that never use -P. The default build therefore
// excludes it; `make build-pcre` produces a binary with full -P support.
type PCREMatcher struct {
	maxCols      int
	needLineNums bool
}

// NewPCREMatcher always fails in non-PCRE builds.
func NewPCREMatcher(pattern string, ignoreCase bool, invert bool) (*PCREMatcher, error) {
	return nil, fmt.Errorf("PCRE support not compiled in (rebuild with: make build-pcre)")
}

// The methods below are never reachable (NewPCREMatcher always errors);
// they exist so *PCREMatcher satisfies the Matcher interface in both builds.

func (m *PCREMatcher) FindAll(data []byte) MatchSet    { panic("pcre not compiled in") }
func (m *PCREMatcher) MatchExists(data []byte) bool    { panic("pcre not compiled in") }
func (m *PCREMatcher) CountAll(data []byte) int        { panic("pcre not compiled in") }
func (m *PCREMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	panic("pcre not compiled in")
}

// Close matches the real PCREMatcher's release method.
func (m *PCREMatcher) Close() {}
