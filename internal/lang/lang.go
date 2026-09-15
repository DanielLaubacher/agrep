// Package lang holds the lightweight per-language-family knowledge that
// structural features share: which delimiters nest, what a string
// literal looks like, and what a comment looks like. This is the comby
// insight — balanced-delimiter matching, hole capture, and block
// extraction need exactly these three facts, not a parser. Families are
// chosen by file extension; the Generic family (unknown extensions)
// knows only balanced delimiters, which degrades safely on prose.
package lang

import (
	"bytes"
	"strings"
)

type Lang int

const (
	Generic Lang = iota
	Go
	Python
	Rust
	JS
	C
	Shell
	Ruby
	Markdown
)

// ByPath maps a file extension to its language family.
func ByPath(path string) Lang {
	dot := strings.LastIndexByte(path, '.')
	if dot < 0 {
		return Generic
	}
	return ByName(path[dot+1:])
}

// ByName maps an extension or language name (as given to --lang) to a
// family. Unknown names map to Generic.
func ByName(name string) Lang {
	switch strings.ToLower(name) {
	case "go":
		return Go
	case "py", "pyi", "python":
		return Python
	case "rs", "rust":
		return Rust
	case "js", "jsx", "ts", "tsx", "mjs", "cjs", "javascript", "typescript":
		return JS
	case "c", "h", "cc", "cpp", "hpp", "cxx", "java", "kt", "cs", "scala":
		return C
	case "sh", "bash", "zsh", "shell":
		return Shell
	case "rb", "ruby":
		return Ruby
	case "md", "markdown":
		return Markdown
	}
	return Generic
}

// StringSpec describes one string-literal form.
type StringSpec struct {
	Open   string // opening delimiter
	Close  string // closing delimiter
	Escape byte   // escape byte inside the literal (0 = none, raw)
	// Interp lists interpolation openers recognized inside this string
	// form (JS backtick "${", Ruby/shell double-quote "#{"/"$("/"${").
	// Each must end in an open bracket ('(', '[', or '{'): on a match,
	// scanning switches to depth-balanced code (recursively skipping
	// nested atoms via SkipAtom, so a quote or comment inside the
	// interpolation can't prematurely end it) until that bracket's
	// matching closer, then resumes looking for Close. Without this, an
	// interpolation containing the string's own Close delimiter (or an
	// unbalanced bracket) would end the atom in the wrong place.
	Interp []string
}

// Spec is the structural knowledge for one family.
type Spec struct {
	LineComments []string // comment-to-end-of-line openers
	BlockOpen    string   // block comment opener ("" = none)
	BlockClose   string   // block comment closer
	// NestableBlockComment: block comments nest (Rust). SkipAtom then
	// tracks depth instead of stopping at the first BlockClose, so
	// `/* outer /* inner */ still comment */` is one atom, not two.
	NestableBlockComment bool
	Strings              []StringSpec
	// Heredoc enables shell/Ruby-style "<<TAG ... TAG" scanning: the atom
	// runs from the "<<" to the line holding the closing tag. See
	// scanHeredoc for the (deliberately conservative) tag heuristic.
	Heredoc bool
	// RegexLiteral enables JS/TS "/regex/flags" scanning, disambiguated
	// from division by regexLiteralStart's division-vs-regex heuristic.
	RegexLiteral bool
}

var specs = map[Lang]*Spec{
	Generic: {},
	Go: {
		LineComments: []string{"//"},
		BlockOpen:    "/*", BlockClose: "*/",
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: "`", Close: "`"},
			{Open: `'`, Close: `'`, Escape: '\\'},
		},
	},
	Python: {
		LineComments: []string{"#"},
		Strings: []StringSpec{
			{Open: `"""`, Close: `"""`, Escape: '\\'},
			{Open: `'''`, Close: `'''`, Escape: '\\'},
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: `'`, Close: `'`, Escape: '\\'},
		},
	},
	Rust: {
		LineComments: []string{"//"},
		BlockOpen:    "/*", BlockClose: "*/",
		NestableBlockComment: true,
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
		},
	},
	JS: {
		LineComments: []string{"//"},
		BlockOpen:    "/*", BlockClose: "*/",
		RegexLiteral: true,
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: `'`, Close: `'`, Escape: '\\'},
			{Open: "`", Close: "`", Escape: '\\', Interp: []string{"${"}},
		},
	},
	C: {
		LineComments: []string{"//"},
		BlockOpen:    "/*", BlockClose: "*/",
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: `'`, Close: `'`, Escape: '\\'},
		},
	},
	Shell: {
		LineComments: []string{"#"},
		Heredoc:      true,
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\', Interp: []string{"$(", "${"}},
			{Open: `'`, Close: `'`},
		},
	},
	Ruby: {
		LineComments: []string{"#"},
		Heredoc:      true,
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\', Interp: []string{"#{"}},
			{Open: `'`, Close: `'`, Escape: '\\'},
		},
	},
	Markdown: {
		Strings: []StringSpec{
			{Open: "```", Close: "```"}, // fenced code blocks are atoms
			{Open: "`", Close: "`"},
		},
	},
}

// SpecFor returns the family's structural spec (never nil).
func (l Lang) Spec() *Spec {
	if s, ok := specs[l]; ok {
		return s
	}
	return specs[Generic]
}

// OpenDelim / CloseDelim classify balanced delimiters. CloseFor returns
// the matching closer for an opener.
func OpenDelim(c byte) bool  { return c == '(' || c == '[' || c == '{' }
func CloseDelim(c byte) bool { return c == ')' || c == ']' || c == '}' }

// SkipAtom: if data[i:] begins a string literal, comment, heredoc, or
// regex literal per spec, return the index just past it and true.
// Unterminated atoms run to end of data. Otherwise returns i, false.
func SkipAtom(spec *Spec, data []byte, i int) (int, bool) {
	rest := data[i:]
	if spec.Heredoc {
		if j, ok := scanHeredoc(data, i); ok {
			return j, true
		}
	}
	for _, lc := range spec.LineComments {
		if lc == "#" && hasPrefix(rest, "#!") {
			// A shebang is a directive, not prose commentary. PDF-extracted
			// text can flatten an entire script onto one physical "line"
			// (no real newline until the true end of that line); treating
			// "#!" there as an ordinary line comment swallows everything
			// after it as an unmatchable atom (report bug 2). Don't treat
			// it as a comment at all — its bytes fall through to plain
			// (non-delimiter) content and structural matching resumes
			// normally on whatever code follows.
			continue
		}
		if hasPrefix(rest, lc) {
			j := i + len(lc)
			for j < len(data) && data[j] != '\n' {
				j++
			}
			return j, true
		}
	}
	if spec.BlockOpen != "" && hasPrefix(rest, spec.BlockOpen) {
		depth := 1
		j := i + len(spec.BlockOpen)
		for j < len(data) {
			if spec.NestableBlockComment && hasPrefix(data[j:], spec.BlockOpen) {
				depth++
				j += len(spec.BlockOpen)
				continue
			}
			if hasPrefix(data[j:], spec.BlockClose) {
				depth--
				j += len(spec.BlockClose)
				if depth == 0 {
					return j, true
				}
				continue
			}
			j++
		}
		return len(data), true
	}
	for _, ss := range spec.Strings {
		if !hasPrefix(rest, ss.Open) {
			continue
		}
		j := i + len(ss.Open)
		for j < len(data) {
			if ss.Escape != 0 && data[j] == ss.Escape && j+1 < len(data) {
				j += 2
				continue
			}
			if op := matchInterp(ss.Interp, data, j); op != "" {
				if nj, ok := skipBalancedFrom(spec, data, j+len(op)); ok {
					j = nj
					continue
				}
				return len(data), true // unterminated interpolation
			}
			if hasPrefix(data[j:], ss.Close) {
				return j + len(ss.Close), true
			}
			j++
		}
		return len(data), true
	}
	if spec.RegexLiteral && i < len(data) && data[i] == '/' && regexLiteralStart(data, i) {
		if j, ok := scanRegexLiteral(data, i); ok {
			return j, true
		}
	}
	return i, false
}

// matchInterp returns whichever entry of opens is a prefix of data[j:],
// or "" if none match.
func matchInterp(opens []string, data []byte, j int) string {
	for _, op := range opens {
		if hasPrefix(data[j:], op) {
			return op
		}
	}
	return ""
}

// skipBalancedFrom scans code starting just past an interpolation
// opener's final open-bracket byte (already counted as depth 1),
// recursively skipping nested atoms (so a quote, comment, or nested
// interpolation inside can't prematurely close it), until the bracket
// rebalances. Returns the index just past the matching closer.
func skipBalancedFrom(spec *Spec, data []byte, pos int) (int, bool) {
	depth := 1
	for pos < len(data) {
		if j, ok := SkipAtom(spec, data, pos); ok && j > pos {
			pos = j
			continue
		}
		c := data[pos]
		if OpenDelim(c) {
			depth++
		} else if CloseDelim(c) {
			depth--
			if depth == 0 {
				return pos + 1, true
			}
		}
		pos++
	}
	return len(data), false
}

// isTagByte reports whether c can appear in a heredoc tag or JS keyword.
func isTagByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// scanHeredoc recognizes a shell/Ruby heredoc opener: "<<", an optional
// "-"/"~" indent-strip modifier, then immediately (no space — the
// no-space requirement is deliberate, see below) either a quoted tag or
// a bareword tag. It returns the index just past the line holding the
// closing tag (unterminated heredocs run to end of data, like every
// other atom).
//
// Barewords must start with an uppercase letter or '_' (EOF, SQL, END_...
// — the near-universal heredoc tag convention in both languages). This
// is deliberately conservative: without it, Ruby's "<<" shovel operator
// used with a lowercase right-hand side (`arr << item`, extremely
// common) would misfire as a heredoc start and swallow the rest of the
// file looking for a closing "item" line. A quoted tag (`<<'EOF'`,
// `<<"EOF"`) is unambiguous and always accepted regardless of case.
func scanHeredoc(data []byte, i int) (int, bool) {
	if !hasPrefix(data[i:], "<<") {
		return i, false
	}
	j := i + 2
	strip := false
	if j < len(data) && (data[j] == '-' || data[j] == '~') {
		strip = true
		j++
	}
	var tag []byte
	if j < len(data) && (data[j] == '\'' || data[j] == '"') {
		q := data[j]
		j++
		start := j
		for j < len(data) && data[j] != q {
			j++
		}
		if j >= len(data) {
			return i, false
		}
		tag = data[start:j]
		j++
	} else {
		if j >= len(data) || !(data[j] == '_' || (data[j] >= 'A' && data[j] <= 'Z')) {
			return i, false
		}
		start := j
		for j < len(data) && isTagByte(data[j]) {
			j++
		}
		tag = data[start:j]
	}
	if len(tag) == 0 {
		return i, false
	}
	nl := bytes.IndexByte(data[j:], '\n')
	if nl < 0 {
		return len(data), true // opener is the last line: no body to scan
	}
	pos := j + nl + 1
	for pos <= len(data) {
		end := len(data)
		lineEnd := bytes.IndexByte(data[pos:], '\n')
		if lineEnd >= 0 {
			end = pos + lineEnd
		}
		line := data[pos:end]
		if strip {
			line = bytes.TrimLeft(line, " \t")
		}
		line = bytes.TrimRight(line, "\r")
		if bytes.Equal(line, tag) {
			if lineEnd < 0 {
				return len(data), true
			}
			return end + 1, true
		}
		if lineEnd < 0 {
			return len(data), true
		}
		pos = end + 1
	}
	return len(data), true
}

// jsRegexKeywords: identifiers after which a following '/' can only
// start a new expression (a regex literal), never continue one via
// division — a JS token can't immediately follow these keywords except
// by starting a fresh operand.
var jsRegexKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true,
	"of": true, "new": true, "delete": true, "void": true,
	"yield": true, "throw": true, "do": true, "else": true,
	"case": true, "await": true,
}

// regexLiteralStart reports whether data[i] == '/' begins a JS regex
// literal rather than division, using the standard division-vs-regex
// heuristic every JS tokenizer needs: look at the last significant byte
// before it. This is inherently a heuristic, not a full parse — e.g. a
// postfix `i++ / 2` is misread as regex-context because '+' is in the
// operator set below, an extremely rare construct in practice — but it
// covers assignment, argument, array/object position, and the
// keyword-prefixed forms above, which is the overwhelming majority of
// real code.
func regexLiteralStart(data []byte, i int) bool {
	p := i - 1
	for p >= 0 && (data[p] == ' ' || data[p] == '\t' || data[p] == '\n' || data[p] == '\r') {
		p--
	}
	if p < 0 {
		return true // start of file: must be an operand
	}
	c := data[p]
	switch {
	case isTagByte(c):
		if c >= '0' && c <= '9' {
			return false // end of a number literal: division
		}
		start := p
		for start > 0 && isTagByte(data[start-1]) {
			start--
		}
		return jsRegexKeywords[string(data[start:p+1])]
	case c == ')' || c == ']' || c == '}':
		return false // end of a grouped expression: division
	case c == '"' || c == '\'' || c == '`':
		return false // end of a string literal: division
	default:
		return true // operator/punctuation: must be a fresh operand
	}
}

// scanRegexLiteral scans a JS regex literal body (honoring character
// classes, where an unescaped '/' doesn't close the literal) plus any
// trailing flag letters. A newline before the closing '/' means it
// isn't a regex literal after all (JS regex literals can't span lines).
func scanRegexLiteral(data []byte, i int) (int, bool) {
	j := i + 1
	inClass := false
	for j < len(data) {
		c := data[j]
		if c == '\\' && j+1 < len(data) {
			j += 2
			continue
		}
		if c == '\n' {
			return i, false
		}
		switch {
		case c == '[':
			inClass = true
		case c == ']':
			inClass = false
		case c == '/' && !inClass:
			j++
			for j < len(data) && ((data[j] >= 'a' && data[j] <= 'z') || (data[j] >= 'A' && data[j] <= 'Z')) {
				j++
			}
			return j, true
		}
		j++
	}
	return i, false
}

func hasPrefix(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[:len(s)]) == s
}
