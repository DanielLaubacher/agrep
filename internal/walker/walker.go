// Package walker traverses directory trees with raw getdents64,
// classifying entries via d_type (no per-file stat), honoring .gitignore
// stacks and glob filters, and skipping binary files by extension before
// they are ever opened. Traversal is a single-producer sorted DFS:
// entries are emitted in a deterministic order (name-sorted within each
// directory) so repeated runs produce identical output — search workers
// downstream provide the parallelism.
package walker

import (
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// noatimeWorks tracks whether O_NOATIME is usable for directory opens.
// Starts as 1 (try it); set to 0 after the first EPERM.
var noatimeWorks atomic.Int32

func init() { noatimeWorks.Store(1) }

// openDir opens a directory with O_NOATIME, falling back without it.
func openDir(path string) (int, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY
	if noatimeWorks.Load() != 0 {
		fd, err := unix.Open(path, flags|unix.O_NOATIME, 0)
		if err == nil {
			return fd, nil
		}
		if err == unix.EPERM {
			noatimeWorks.Store(0)
		}
	}
	return unix.Open(path, flags, 0)
}

// FileEntry represents a file discovered during directory traversal.
type FileEntry struct {
	Path string
}

// WalkOptions configures directory traversal behavior.
type WalkOptions struct {
	Recursive      bool
	NoIgnore       bool     // skip .gitignore processing
	Hidden         bool     // include hidden files and directories
	FollowSymlinks bool     // follow symbolic links
	IncludeBinary  bool     // include files with known binary extensions (.so, .o, .png, etc.)
	Globs          []string // include/exclude globs (prefix ! to exclude)
}

// Walk traverses directories and sends discovered files on the returned channel.
// It uses raw getdents64 for maximum Linux performance.
// Respects .gitignore files and skips hidden files/directories by default.
// Files are emitted in deterministic sorted-DFS order.
// If recursive is false, only the given paths are used as literal file paths.
func Walk(roots []string, opts WalkOptions) (<-chan FileEntry, <-chan error) {
	fileCh := make(chan FileEntry, 256)
	errCh := make(chan error, 16)

	go func() {
		defer close(fileCh)
		defer close(errCh)

		if !opts.Recursive {
			for _, root := range roots {
				var stat unix.Stat_t
				if err := unix.Stat(root, &stat); err != nil {
					errCh <- &WalkError{Path: root, Err: err}
					continue
				}
				if stat.Mode&unix.S_IFMT == unix.S_IFREG {
					fileCh <- FileEntry{Path: root}
				}
			}
			return
		}

		tw := &treeWalker{
			fileCh:         fileCh,
			errCh:          errCh,
			hidden:         opts.Hidden,
			noIgnore:       opts.NoIgnore,
			followSymlinks: opts.FollowSymlinks,
			includeBinary:  opts.IncludeBinary,
			globs:          opts.Globs,
			buf:            make([]byte, 32*1024),
		}

		for _, root := range roots {
			// A root that is a regular file is searched as-is —
			// `agrep -r PAT file.txt dir/` must not fail on the file.
			var stat unix.Stat_t
			if err := unix.Stat(root, &stat); err != nil {
				tw.errCh <- &WalkError{Path: root, Err: err}
				continue
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFREG {
				fileCh <- FileEntry{Path: root}
				continue
			}
			var layers []ignoreLayer
			if !opts.NoIgnore {
				layers = []ignoreLayer{loadIgnoreLayer(root)}
			}
			tw.walkDir(root, layers)
		}
	}()

	return fileCh, errCh
}

// treeWalker holds traversal state for one Walk call.
type treeWalker struct {
	fileCh         chan<- FileEntry
	errCh          chan<- error
	hidden         bool
	noIgnore       bool
	followSymlinks bool
	includeBinary  bool
	globs          []string

	buf     []byte   // getdents buffer, reused across directories
	scratch []Dirent // per-batch parse buffer, reused across directories
}

// walkDir reads one directory, sorts its entries by name, and processes
// them in order — emitting files and recursing into subdirectories at
// their sorted position. The deterministic order is a correctness
// contract: output sequence numbers downstream derive from emission
// order, so budgeted or truncated output must not vary between runs.
// The directory fd is closed before recursing.
func (tw *treeWalker) walkDir(path string, ignores []ignoreLayer) {
	fd, err := openDir(path)
	if err != nil {
		tw.errCh <- &WalkError{Path: path, Err: err}
		return
	}

	var entries []Dirent
	for {
		n, err := unix.Getdents(fd, tw.buf)
		if err != nil {
			tw.errCh <- &WalkError{Path: path, Err: err}
			break
		}
		if n == 0 {
			break
		}
		tw.scratch = ParseDirents(tw.buf, n, tw.scratch)
		entries = append(entries, tw.scratch...)
	}
	unix.Close(fd)

	slices.SortFunc(entries, func(a, b Dirent) int {
		return strings.Compare(a.Name, b.Name)
	})

	for _, entry := range entries {
		fullPath := joinPath(path, entry.Name)

		// Resolve DT_LNK / DT_UNKNOWN to a concrete kind via stat.
		kind := entry.Type
		switch kind {
		case DT_LNK:
			if !tw.followSymlinks {
				continue
			}
			var stat unix.Stat_t
			if err := unix.Stat(fullPath, &stat); err != nil {
				continue // silently skip broken symlinks
			}
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				kind = DT_REG
			case unix.S_IFDIR:
				kind = DT_DIR
			default:
				continue
			}
		case DT_UNKNOWN:
			var stat unix.Stat_t
			if err := unix.Stat(fullPath, &stat); err != nil {
				tw.errCh <- &WalkError{Path: fullPath, Err: err}
				continue
			}
			switch stat.Mode & unix.S_IFMT {
			case unix.S_IFREG:
				kind = DT_REG
			case unix.S_IFDIR:
				kind = DT_DIR
			default:
				continue
			}
		}

		switch kind {
		case DT_DIR:
			if skipDir(entry.Name, tw.hidden) {
				continue
			}
			if ignores != nil && isIgnoredByLayers(ignores, fullPath, true) {
				continue
			}
			if tw.isGlobExcluded(entry.Name) {
				continue
			}
			// Child ignore layers: parent stack + this dir's .gitignore.
			var childIgnores []ignoreLayer
			if !tw.noIgnore {
				childIgnores = make([]ignoreLayer, len(ignores)+1)
				copy(childIgnores, ignores)
				childIgnores[len(ignores)] = loadIgnoreLayer(fullPath)
			}
			tw.walkDir(fullPath, childIgnores)

		case DT_REG:
			if !tw.hidden && len(entry.Name) > 0 && entry.Name[0] == '.' {
				continue
			}
			if !tw.includeBinary && IsBinaryExtension(entry.Name) {
				continue
			}
			if ignores != nil && isIgnoredByLayers(ignores, fullPath, false) {
				continue
			}
			if tw.isGlobExcluded(entry.Name) {
				continue
			}
			tw.fileCh <- FileEntry{Path: fullPath}
		}
	}
}

// joinPath concatenates a directory and entry name with a single separator.
// Avoids filepath.Join overhead (no Clean, no validation) since we control
// the inputs: dirPath is always a valid directory path, name is a plain filename.
// Uses a single allocation via make+copy instead of string concatenation.
func joinPath(dirPath, name string) string {
	needsSep := len(dirPath) == 0 || dirPath[len(dirPath)-1] != '/'
	n := len(dirPath) + len(name)
	if needsSep {
		n++
	}
	buf := make([]byte, n)
	copy(buf, dirPath)
	i := len(dirPath)
	if needsSep {
		buf[i] = '/'
		i++
	}
	copy(buf[i:], name)
	return unsafe.String(&buf[0], len(buf))
}

// skipDir returns true for directories that should be skipped.
// VCS directories (.git, .svn, .hg) and node_modules are always skipped.
// Other hidden directories are skipped unless hidden is true.
func skipDir(name string, hidden bool) bool {
	switch name {
	case ".git", ".svn", ".hg", "node_modules":
		return true
	}
	if !hidden && len(name) > 0 && name[0] == '.' {
		return true
	}
	return false
}

// isGlobExcluded checks if a filename matches any glob exclusion patterns.
// Globs prefixed with ! are exclusion patterns; others are inclusion patterns.
// If only exclusion patterns exist, a file is excluded if it matches any exclusion.
// If any inclusion patterns exist, a file must match at least one inclusion AND not
// match any exclusion.
func (tw *treeWalker) isGlobExcluded(name string) bool {
	if len(tw.globs) == 0 {
		return false
	}

	hasIncludes := false
	included := false
	for _, g := range tw.globs {
		if strings.HasPrefix(g, "!") {
			// Exclusion glob
			pattern := g[1:]
			if matchGlob(pattern, name) {
				return true
			}
		} else {
			// Inclusion glob
			hasIncludes = true
			if matchGlob(g, name) {
				included = true
			}
		}
	}

	if hasIncludes && !included {
		return true
	}
	return false
}

// matchGlob matches a name against a glob pattern.
// Supports brace expansion for {a,b,c} patterns.
func matchGlob(pattern, name string) bool {
	// Fast path: patterns with no metacharacters (e.g. "!.git" exclusions
	// from config files) are exact-name comparisons — skip filepath.Match.
	if !strings.ContainsAny(pattern, `*?[{\`) {
		return pattern == name
	}
	// Handle brace expansion: {a,b,c} → try each alternative
	if i := strings.IndexByte(pattern, '{'); i >= 0 {
		if j := strings.IndexByte(pattern[i:], '}'); j >= 0 {
			prefix := pattern[:i]
			suffix := pattern[i+j+1:]
			alts := strings.SplitSeq(pattern[i+1:i+j], ",")
			for alt := range alts {
				if matchGlob(prefix+alt+suffix, name) {
					return true
				}
			}
			return false
		}
	}
	matched, _ := filepath.Match(pattern, name)
	return matched
}

// WalkError represents an error during directory traversal.
type WalkError struct {
	Path string
	Err  error
}

func (e *WalkError) Error() string {
	return "walk " + e.Path + ": " + e.Err.Error()
}

func (e *WalkError) Unwrap() error {
	return e.Err
}

// MatchesGlobs applies include/exclude globs (prefix ! to exclude) to a
// base name, with the same semantics the walker uses during traversal.
// Exported for file sources that bypass the walk (--changed-since).
func MatchesGlobs(globs []string, name string) bool {
	tw := &treeWalker{globs: globs}
	return !tw.isGlobExcluded(name)
}
