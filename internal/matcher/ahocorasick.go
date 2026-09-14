package matcher

import "bytes"

// acNode is a node in the Aho-Corasick automaton.
type acNode struct {
	children [256]*acNode
	fail     *acNode
	output   []int // indices of patterns that match at this node
	depth    int
}

// AhoCorasickMatcher matches multiple fixed patterns simultaneously
// using the Aho-Corasick algorithm.
type AhoCorasickMatcher struct {
	root         *acNode
	patterns     [][]byte // original patterns
	ignoreCase   bool
	invert       bool
	maxCols      int
	needLineNums bool
}

// NewAhoCorasickMatcher creates an AhoCorasickMatcher for multiple fixed patterns.
func NewAhoCorasickMatcher(patterns []string, ignoreCase bool, invert bool) *AhoCorasickMatcher {
	m := &AhoCorasickMatcher{
		root:       &acNode{},
		ignoreCase: ignoreCase,
		invert:     invert,
	}

	// Build the trie
	for i, p := range patterns {
		pat := []byte(p)
		if ignoreCase {
			pat = bytes.ToLower(pat)
		}
		m.patterns = append(m.patterns, pat)
		m.addPattern(pat, i)
	}

	// Build failure links via BFS
	m.buildFailureLinks()

	return m
}

func (m *AhoCorasickMatcher) addPattern(pattern []byte, index int) {
	node := m.root
	for _, b := range pattern {
		if node.children[b] == nil {
			node.children[b] = &acNode{depth: node.depth + 1}
		}
		node = node.children[b]
	}
	node.output = append(node.output, index)
}

func (m *AhoCorasickMatcher) buildFailureLinks() {
	// BFS from root
	queue := make([]*acNode, 0, 256)

	// Initialize depth-1 nodes: fail links point to root
	for i := range 256 {
		child := m.root.children[i]
		if child != nil {
			child.fail = m.root
			queue = append(queue, child)
		}
	}

	// BFS to build failure links
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for i := range 256 {
			child := current.children[i]
			if child == nil {
				continue
			}

			queue = append(queue, child)

			// Follow failure links to find the longest proper suffix
			fail := current.fail
			for fail != nil && fail.children[i] == nil {
				fail = fail.fail
			}
			if fail == nil {
				child.fail = m.root
			} else {
				child.fail = fail.children[i]
			}

			// Merge output from failure node
			if child.fail != nil && len(child.fail.output) > 0 {
				child.output = append(child.output, child.fail.output...)
			}
		}
	}
}

// searchLocs scans text for all pattern matches, returning [2]int{start, end} pairs.
// Uses a stack buffer for ≤16 matches to avoid heap allocation on sparse matches.
func (m *AhoCorasickMatcher) searchLocs(text []byte) [][2]int {
	var stackBuf [16][2]int
	n := 0
	var overflow [][2]int
	node := m.root

	for i, b := range text {
		if m.ignoreCase {
			b = toLower(b)
		}

		for node != m.root && node.children[b] == nil {
			node = node.fail
		}
		if node.children[b] != nil {
			node = node.children[b]
		}

		if len(node.output) > 0 {
			for _, pidx := range node.output {
				plen := len(m.patterns[pidx])
				loc := [2]int{i - plen + 1, i + 1}
				if n < len(stackBuf) {
					stackBuf[n] = loc
				} else {
					if overflow == nil {
						overflow = make([][2]int, 0, 64)
						overflow = append(overflow, stackBuf[:]...)
					}
					overflow = append(overflow, loc)
				}
				n++
			}
		}
	}

	if n == 0 {
		return nil
	}
	if overflow != nil {
		return overflow
	}
	result := make([][2]int, n)
	copy(result, stackBuf[:n])
	return result
}

// matchExists walks the automaton until the first match, zero allocations.
func (m *AhoCorasickMatcher) matchExists(data []byte) bool {
	node := m.root
	for _, b := range data {
		if m.ignoreCase {
			b = toLower(b)
		}
		for node != m.root && node.children[b] == nil {
			node = node.fail
		}
		if node.children[b] != nil {
			node = node.children[b]
		}
		if len(node.output) > 0 {
			return true
		}
	}
	return false
}

// LineBounded reports that no match spans a newline (no pattern contains
// one), enabling parallel line-aligned chunked search.
func (m *AhoCorasickMatcher) LineBounded() bool {
	for _, p := range m.patterns {
		if bytes.ContainsRune(p, '\n') {
			return false
		}
	}
	return true
}

func (m *AhoCorasickMatcher) MatchExists(data []byte) bool {
	if m.invert {
		return len(data) > 0
	}
	return m.matchExists(data)
}

func (m *AhoCorasickMatcher) CountAll(data []byte) int {
	if m.invert {
		return countInvert(data, func(line []byte) bool {
			return !m.matchExists(line)
		})
	}

	// Walk automaton and count unique lines directly — zero allocation.
	node := m.root
	count := 0
	lineEnd := -1

	for i, b := range data {
		if m.ignoreCase {
			b = toLower(b)
		}
		for node != m.root && node.children[b] == nil {
			node = node.fail
		}
		if node.children[b] != nil {
			node = node.children[b]
		}
		if len(node.output) > 0 && i > lineEnd {
			count++
			j := bytes.IndexByte(data[i:], '\n')
			if j >= 0 {
				lineEnd = i + j
			} else {
				lineEnd = len(data)
			}
		}
	}

	return count
}

func (m *AhoCorasickMatcher) FindAll(data []byte) MatchSet {
	if m.invert {
		return m.findAllInvert(data)
	}

	locs := m.searchLocs(data)
	if len(locs) == 0 {
		return MatchSet{}
	}
	return matchSetFromLocs(data, locs, m.maxCols, m.needLineNums)
}

func (m *AhoCorasickMatcher) findAllInvert(data []byte) MatchSet {
	ms := MatchSet{Data: data}
	var offset int64
	lineNum := 1
	remaining := data

	for len(remaining) > 0 {
		idx := bytes.IndexByte(remaining, '\n')
		var lineLen int
		if idx >= 0 {
			lineLen = idx
		} else {
			lineLen = len(remaining)
		}
		lineStart := int(offset)
		line := remaining[:lineLen]

		if !m.matchExists(line) {
			ms.Matches = append(ms.Matches, Match{
				LineNum:    lineNum,
				LineStart:  lineStart,
				LineLen:    lineLen,
				ByteOffset: offset,
			})
		}

		if idx >= 0 {
			remaining = remaining[idx+1:]
		} else {
			remaining = nil
		}
		offset += int64(lineLen) + 1
		lineNum++
	}

	return ms
}

func (m *AhoCorasickMatcher) FindLine(line []byte, lineNum int, byteOffset int64) (MatchSet, bool) {
	locs := m.searchLocs(line)
	hasMatch := len(locs) > 0

	if m.invert {
		hasMatch = !hasMatch
	}

	if !hasMatch {
		return MatchSet{}, false
	}

	ms := MatchSet{Data: line}
	match := Match{
		LineNum:    lineNum,
		LineStart:  0,
		LineLen:    len(line),
		ByteOffset: byteOffset,
	}
	if !m.invert {
		match.PosIdx = 0
		match.PosCount = len(locs)
		ms.Positions = make([][2]int, len(locs))
		copy(ms.Positions, locs)
	}
	ms.Matches = []Match{match}

	return ms, true
}
