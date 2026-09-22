package output

// --scope: annotate matches with their enclosing definition. A bounded
// backward "sticky scope" scan: walk line starts upward from the match,
// tracking the minimum
// indentation of significant lines seen; the first definition-shaped
// line (per language family, chosen by file extension) at lower
// indentation than everything below it is the enclosing scope. Comments
// and blank lines never narrow the indent, so doc comments above a
// function don't break the chain. Markdown files use the heading scan.

import (
	"bytes"

	"github.com/DanielLaubacher/agrep/internal/lang"
)

// scopeScanLimit bounds how far back enclosingScope searches. Applied
// per printed match only, same as sectionScanLimit.
const scopeScanLimit = 64 * 1024

// enclosingScope returns the definition line enclosing the match at
// lineStart, or nil if none is found. path selects the language family;
// Markdown routes to sectionHeading.
func enclosingScope(data []byte, lineStart int, path string) []byte {
	if s, _, ok := enclosingScopeAt(data, lineStart, path); ok {
		return s
	}
	return nil
}

// enclosingScopeAt additionally returns the byte offset of the
// definition (or heading) line, for callers that need the block start
// (--block).
func enclosingScopeAt(data []byte, lineStart int, path string) ([]byte, int, bool) {
	fam := lang.ByPath(path)
	switch fam {
	case lang.Markdown:
		h, off := sectionHeadingAt(data, lineStart)
		if h == nil {
			return nil, 0, false
		}
		return h, off, true
	case lang.Generic:
		return nil, 0, false
	}

	if lineStart > len(data) {
		lineStart = len(data)
	}
	lo := 0
	if lineStart > scopeScanLimit {
		lo = lineStart - scopeScanLimit
	}

	cur := lineStart
	first := true
	minIndent := 1 << 30
	for {
		lineEnd := cur
		for lineEnd < len(data) && data[lineEnd] != '\n' {
			lineEnd++
		}
		line := data[cur:lineEnd]
		ind, trimmed := indentAndTrim(line)

		if len(trimmed) > 0 && !isCommentLine(trimmed) {
			if first {
				// The match's own line: a definition is its own scope,
				// and a top-level non-definition has no enclosing one.
				if isDefLine(trimmed, fam) {
					return bytes.TrimRight(trimmed, " \t\r{"), cur, true
				}
				if ind == 0 {
					return nil, 0, false
				}
				minIndent = ind
			} else if ind < minIndent {
				if isDefLine(trimmed, fam) {
					return bytes.TrimRight(trimmed, " \t\r{"), cur, true
				}
				minIndent = ind
				if minIndent == 0 {
					// A top-level non-definition line above the match
					// (e.g. a closing brace): the match is not inside
					// any definition.
					return nil, 0, false
				}
			}
			first = false
		} else if first && len(trimmed) > 0 {
			// Match on a comment line: scope by its indentation.
			minIndent = ind
			first = false
		}

		if cur <= lo {
			return nil, 0, false
		}
		if i := bytes.LastIndexByte(data[lo:cur-1], '\n'); i >= 0 {
			cur = lo + i + 1
		} else {
			cur = lo
		}
	}
}

// IsDefinitionLine reports whether line looks like a definition
// (func/class/def/type... per the file's language family; a heading in
// Markdown). Used by --rank defs: agents ask "where is X defined" far
// more often than "where is X mentioned".
func IsDefinitionLine(line []byte, path string) bool {
	fam := lang.ByPath(path)
	_, trimmed := indentAndTrim(line)
	if len(trimmed) == 0 {
		return false
	}
	if fam == lang.Markdown {
		return trimmed[0] == '#'
	}
	return !isCommentLine(trimmed) && isDefLine(trimmed, fam)
}

// indentAndTrim returns the count of leading whitespace bytes and the
// line with surrounding whitespace removed.
func indentAndTrim(line []byte) (int, []byte) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	return i, bytes.TrimRight(line[i:], " \t\r")
}

// isCommentLine reports whether a trimmed line is a comment across the
// supported families (// # /* * --).
func isCommentLine(trimmed []byte) bool {
	if len(trimmed) == 0 {
		return true
	}
	switch trimmed[0] {
	case '#':
		return true
	case '/':
		return len(trimmed) > 1 && (trimmed[1] == '/' || trimmed[1] == '*')
	case '*':
		return true // continuation of a block comment
	}
	return false
}

// hasPrefixWord reports whether trimmed starts with word followed by a
// non-identifier byte (so "func" doesn't match "function_table").
func hasPrefixWord(trimmed []byte, word string) bool {
	if len(trimmed) < len(word) || string(trimmed[:len(word)]) != word {
		return false
	}
	if len(trimmed) == len(word) {
		return true
	}
	c := trimmed[len(word)]
	return c == ' ' || c == '\t' || c == '('
}

var cControlWords = []string{"if", "for", "while", "switch", "return", "else", "do", "case", "break", "continue", "goto", "sizeof", "new", "delete", "throw", "catch"}

// isDefLine reports whether a trimmed significant line looks like a
// definition in the given language family.
func isDefLine(trimmed []byte, fam lang.Lang) bool {
	switch fam {
	case lang.Go:
		return hasPrefixWord(trimmed, "func") || hasPrefixWord(trimmed, "type") ||
			hasPrefixWord(trimmed, "const") || hasPrefixWord(trimmed, "var")
	case lang.Python:
		return hasPrefixWord(trimmed, "def") || hasPrefixWord(trimmed, "class") ||
			(hasPrefixWord(trimmed, "async") && hasPrefixWord(bytes.TrimLeft(trimmed[5:], " \t"), "def"))
	case lang.Rust:
		t := trimmed
		for _, kw := range []string{"pub(crate)", "pub", "unsafe", "async", "const", "extern"} {
			if hasPrefixWord(t, kw) {
				t = bytes.TrimLeft(t[len(kw):], " \t")
			}
		}
		for _, kw := range []string{"fn", "impl", "trait", "struct", "enum", "mod"} {
			if hasPrefixWord(t, kw) {
				return true
			}
		}
		return false
	case lang.JS:
		t := trimmed
		for _, kw := range []string{"export", "default", "public", "private", "protected", "static", "abstract", "async"} {
			if hasPrefixWord(t, kw) {
				t = bytes.TrimLeft(t[len(kw):], " \t")
			}
		}
		for _, kw := range []string{"function", "class", "interface", "enum"} {
			if hasPrefixWord(t, kw) {
				return true
			}
		}
		// Arrow/function assignment: const handler = async (req) => {
		if hasPrefixWord(t, "const") || hasPrefixWord(t, "let") || hasPrefixWord(t, "var") {
			return bytes.Contains(t, []byte("=>")) || bytes.Contains(t, []byte("function"))
		}
		return false
	case lang.C:
		c := trimmed[0]
		if !(c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
		if hasPrefixWord(trimmed, "class") || hasPrefixWord(trimmed, "struct") ||
			hasPrefixWord(trimmed, "enum") || hasPrefixWord(trimmed, "namespace") ||
			hasPrefixWord(trimmed, "interface") {
			return true
		}
		for _, kw := range cControlWords {
			if hasPrefixWord(trimmed, kw) {
				return false
			}
		}
		// Function-shaped: has a paren and is not a bare call statement
		// (heuristic: definitions don't end with ';').
		return bytes.IndexByte(trimmed, '(') > 0 && trimmed[len(trimmed)-1] != ';'
	case lang.Shell:
		return hasPrefixWord(trimmed, "function") || bytes.Contains(trimmed, []byte("() {"))
	case lang.Ruby:
		return hasPrefixWord(trimmed, "def") || hasPrefixWord(trimmed, "class") || hasPrefixWord(trimmed, "module")
	}
	return false
}
