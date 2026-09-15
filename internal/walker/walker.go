// Package walker traverses directory trees with raw getdents64,
// classifying entries via d_type (no per-file stat), honoring .gitignore
// stacks and glob filters, and skipping binary files by extension before
// they are ever opened. Directories are listed by a parallel pool, but
// files are emitted by a single goroutine in sorted-DFS order
// (name-sorted within each directory), so repeated runs produce
// identical output while the listing itself keeps up with the search
// workers downstream.
package walker

import (
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
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
// Seq is a 1-based, contiguous, emission-order sequence number: the
// walker (and every other fileSource) is a single sequential producer,
// so stamping it here is free, unlike numbering it downstream where
// concurrent workers would need to serialize on a lock to preserve order.
type FileEntry struct {
	Path string
	Seq  int
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
			seq := 0
			for _, root := range roots {
				var stat unix.Stat_t
				if err := unix.Stat(root, &stat); err != nil {
					errCh <- &WalkError{Path: root, Err: err}
					continue
				}
				if stat.Mode&unix.S_IFMT == unix.S_IFREG {
					seq++
					fileCh <- FileEntry{Path: root, Seq: seq}
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
		}
		tw.cond = sync.NewCond(&tw.mu)

		// Roots are resolved up front so directory listing can start
		// on every root at once; emission below replays them in
		// argument order.
		type rootItem struct {
			path string
			node *dirNode
		}
		var items []rootItem
		for _, root := range roots {
			// A root that is a regular file is searched as-is —
			// `agrep -r PAT file.txt dir/` must not fail on the file.
			var stat unix.Stat_t
			if err := unix.Stat(root, &stat); err != nil {
				tw.errCh <- &WalkError{Path: root, Err: err}
				continue
			}
			if stat.Mode&unix.S_IFMT == unix.S_IFREG {
				items = append(items, rootItem{path: root})
				continue
			}
			var layers []ignoreLayer
			if !opts.NoIgnore {
				layers = []ignoreLayer{loadIgnoreLayer(root)}
			}
			node := newDirNode(root, layers)
			tw.enqueue(node)
			items = append(items, rootItem{node: node})
		}

		tw.startListers()
		for _, it := range items {
			if it.node == nil {
				tw.sendFile(it.path)
				continue
			}
			tw.emit(it.node)
		}
	}()

	return fileCh, errCh
}

// treeWalker holds traversal state for one Walk call: a pool of
// directory listers feeding a single in-order emitter.
type treeWalker struct {
	fileCh         chan<- FileEntry
	errCh          chan<- error
	hidden         bool
	noIgnore       bool
	followSymlinks bool
	includeBinary  bool
	globs          []string

	// seq numbers emitted files. Only the single emitter goroutine (the
	// one running emit/sendFile) ever touches it, so it needs no lock.
	seq int

	mu      sync.Mutex
	cond    *sync.Cond // signaled when a directory is queued or the walk is done
	queue   []*dirNode // directories awaiting listing (LIFO: stays near the emitter's DFS position)
	pending int        // directories queued or being listed
	done    bool       // no directory left to list
}

// dirNode is one directory's listing. Listers fill items/errs and set
// ready under treeWalker.mu (broadcasting treeWalker.cond); the emitter
// waits on ready and replays items in order. Items are already sorted,
// filtered, and classified, so the emitter does no syscalls — it only
// sends. ready shares the treeWalker's single cond/mutex instead of each
// node allocating its own channel — cheap on trees with many directories.
type dirNode struct {
	path    string
	ignores []ignoreLayer
	ready   bool
	items   []dirItem
	errs    []error
}

// dirItem is a sorted-position entry: a file path, or a subdirectory
// whose subtree is emitted at this position.
type dirItem struct {
	path  string
	child *dirNode
}

func newDirNode(path string, ignores []ignoreLayer) *dirNode {
	return &dirNode{path: path, ignores: ignores}
}

// startListers launches the directory-listing pool. It exits on its own
// once every queued directory has been listed.
func (tw *treeWalker) startListers() {
	for range runtime.NumCPU() {
		go tw.lister()
	}
}

// enqueue queues a directory for listing. Broadcast (not Signal): the
// same cond also wakes the emitter waiting on a specific node's ready
// flag (see dirNode), and a Signal can be absorbed by that unrelated
// waiter, leaving every lister asleep.
func (tw *treeWalker) enqueue(n *dirNode) {
	tw.mu.Lock()
	tw.queue = append(tw.queue, n)
	tw.pending++
	tw.mu.Unlock()
	tw.cond.Broadcast()
}

// dequeue returns the next directory to list, blocking while the queue
// is temporarily empty. Returns false once the walk is complete.
func (tw *treeWalker) dequeue() (*dirNode, bool) {
	tw.mu.Lock()
	for len(tw.queue) == 0 && !tw.done {
		tw.cond.Wait()
	}
	if tw.done && len(tw.queue) == 0 {
		tw.mu.Unlock()
		return nil, false
	}
	n := tw.queue[len(tw.queue)-1]
	tw.queue = tw.queue[:len(tw.queue)-1]
	tw.mu.Unlock()
	return n, true
}

// finish marks one directory as listed.
func (tw *treeWalker) finish() {
	tw.mu.Lock()
	tw.pending--
	if tw.pending == 0 && len(tw.queue) == 0 {
		tw.done = true
		tw.cond.Broadcast()
	}
	tw.mu.Unlock()
}

// lister lists directories from the queue until the walk is complete.
func (tw *treeWalker) lister() {
	buf := make([]byte, 32*1024) // per-lister getdents buffer
	var scratch []Dirent         // per-lister dirent parse buffer
	for {
		n, ok := tw.dequeue()
		if !ok {
			return
		}
		scratch = tw.list(n, buf, scratch)
		tw.finish()
	}
}

// emit replays a listed directory in order: files are sent at their
// sorted position and subdirectories are emitted recursively at theirs.
// The order is a correctness contract: output sequence numbers
// downstream derive from emission order, so budgeted or truncated
// output must not vary between runs.
func (tw *treeWalker) emit(n *dirNode) {
	tw.mu.Lock()
	for !n.ready {
		tw.cond.Wait()
	}
	tw.mu.Unlock()
	for _, err := range n.errs {
		tw.errCh <- err
	}
	for _, it := range n.items {
		if it.child != nil {
			tw.emit(it.child)
		} else {
			tw.sendFile(it.path)
		}
	}
}

// sendFile emits one file with the next sequence number.
func (tw *treeWalker) sendFile(path string) {
	tw.seq++
	tw.fileCh <- FileEntry{Path: path, Seq: tw.seq}
}

// list reads one directory, sorts its entries by name, and classifies
// them into n.items, then queues its subdirectories for listing in
// reverse order so the LIFO queue hands the emitter's next directory to
// a lister first. The directory fd is closed before returning. Returns
// the scratch slice for reuse.
func (tw *treeWalker) list(n *dirNode, buf []byte, scratch []Dirent) []Dirent {
	defer func() {
		tw.mu.Lock()
		n.ready = true
		tw.mu.Unlock()
		tw.cond.Broadcast()
	}()
	path, ignores := n.path, n.ignores

	fd, err := openDir(path)
	if err != nil {
		n.errs = append(n.errs, &WalkError{Path: path, Err: err})
		return scratch
	}

	// Reuse scratch's backing array across getdents64 batches (and across
	// directories via the returned slice) — ParseDirents appends, so every
	// batch for this directory lands directly in entries with no second
	// copy.
	entries := scratch[:0]
	for {
		cnt, err := unix.Getdents(fd, buf)
		if err != nil {
			n.errs = append(n.errs, &WalkError{Path: path, Err: err})
			break
		}
		if cnt == 0 {
			break
		}
		entries = ParseDirents(buf, cnt, entries)
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
				n.errs = append(n.errs, &WalkError{Path: fullPath, Err: err})
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
			if tw.globExcludes(fullPath, entry.Name, true) {
				continue
			}
			// Child ignore layers: parent stack + this dir's .gitignore.
			var childIgnores []ignoreLayer
			if !tw.noIgnore {
				childIgnores = make([]ignoreLayer, len(ignores)+1)
				copy(childIgnores, ignores)
				childIgnores[len(ignores)] = loadIgnoreLayer(fullPath)
			}
			n.items = append(n.items, dirItem{child: newDirNode(fullPath, childIgnores)})

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
			if tw.globExcludes(fullPath, entry.Name, false) {
				continue
			}
			n.items = append(n.items, dirItem{path: fullPath})
		}
	}
	for i := len(n.items) - 1; i >= 0; i-- {
		if c := n.items[i].child; c != nil {
			tw.enqueue(c)
		}
	}
	return entries
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

// globExcludes applies the include/exclude globs to one entry. Globs
// prefixed with ! exclude; the rest include. A file is excluded when it
// matches any exclusion, or when inclusions exist and it matches none.
// Directories are pruned only by exclusions — an include glob such as
// '*.py' names files, so a directory that fails it must still be
// descended to reach the files inside (report bug 4: '-g *.py' used to
// prune every subdirectory and match only the top level).
//
// A glob without '/' matches the base name at any depth; one with '/'
// matches the path as agrep prints it (the search root joined to the
// entry, so cwd-relative for a relative root — ripgrep's convention),
// where '**' spans any number of directories ('src/**/*.go'). A leading
// './' on either side is ignored.
func (tw *treeWalker) globExcludes(path, name string, isDir bool) bool {
	if len(tw.globs) == 0 {
		return false
	}

	hasIncludes := false
	included := false
	for _, g := range tw.globs {
		if strings.HasPrefix(g, "!") {
			if matchPathGlob(g[1:], path, name) {
				return true
			}
			continue
		}
		if isDir {
			continue
		}
		hasIncludes = true
		if !included && matchPathGlob(g, path, name) {
			included = true
		}
	}
	return hasIncludes && !included
}

// matchPathGlob matches one glob against an entry given its printed path
// and base name (see globExcludes for the semantics).
func matchPathGlob(pattern, path, name string) bool {
	pattern = strings.TrimPrefix(pattern, "./")
	if !strings.Contains(pattern, "/") {
		return matchGlob(pattern, name)
	}
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok && !strings.Contains(rest, "/") {
		return matchGlob(rest, name)
	}
	path = strings.TrimPrefix(path, "./")
	return matchGlobSegments(strings.Split(pattern, "/"), strings.Split(path, "/"))
}

// matchGlobSegments matches path segments against pattern segments, where
// a "**" segment matches zero or more path segments.
func matchGlobSegments(pat, path []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(path); i++ {
				if matchGlobSegments(pat[1:], path[i:]) {
					return true
				}
			}
			return false
		}
		if len(path) == 0 || !matchGlob(pat[0], path[0]) {
			return false
		}
		pat, path = pat[1:], path[1:]
	}
	return len(path) == 0
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
// file's printed path, with the same semantics the walker uses during
// traversal. Exported for file sources that bypass the walk
// (--changed-since).
func MatchesGlobs(globs []string, path string) bool {
	tw := &treeWalker{globs: globs}
	return !tw.globExcludes(path, filepath.Base(path), false)
}
