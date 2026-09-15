// Package lang holds the lightweight per-language-family knowledge that
// structural features share: which delimiters nest, what a string
// literal looks like, and what a comment looks like. This is the comby
// insight — balanced-delimiter matching, hole capture, and block
// extraction need exactly these three facts, not a parser. Families are
// chosen by file extension; the Generic family (unknown extensions)
// knows only balanced delimiters, which degrades safely on prose.
package lang

import "strings"

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
}

// Spec is the structural knowledge for one family.
type Spec struct {
	LineComments []string // comment-to-end-of-line openers
	BlockOpen    string   // block comment opener ("" = none)
	BlockClose   string   // block comment closer
	Strings      []StringSpec
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
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
		},
	},
	JS: {
		LineComments: []string{"//"},
		BlockOpen:    "/*", BlockClose: "*/",
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: `'`, Close: `'`, Escape: '\\'},
			{Open: "`", Close: "`", Escape: '\\'},
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
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
			{Open: `'`, Close: `'`},
		},
	},
	Ruby: {
		LineComments: []string{"#"},
		Strings: []StringSpec{
			{Open: `"`, Close: `"`, Escape: '\\'},
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

// SkipAtom: if data[i:] begins a string literal or comment per spec,
// return the index just past it and true. Unterminated atoms run to end
// of data. Otherwise returns i, false.
func SkipAtom(spec *Spec, data []byte, i int) (int, bool) {
	rest := data[i:]
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
		j := i + len(spec.BlockOpen)
		for j < len(data) {
			if hasPrefix(data[j:], spec.BlockClose) {
				return j + len(spec.BlockClose), true
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
			if hasPrefix(data[j:], ss.Close) {
				return j + len(ss.Close), true
			}
			j++
		}
		return len(data), true
	}
	return i, false
}

func hasPrefix(b []byte, s string) bool {
	return len(b) >= len(s) && string(b[:len(s)]) == s
}
