package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dl/gogrep/internal/index"
	"github.com/dl/gogrep/internal/regex"
	"github.com/dl/gogrep/internal/walker"
)

// runClearIndex handles --clear-index PATH: delete index state for every
// root at or under PATH.
func runClearIndex(path string) int {
	cleared, err := index.ClearUnder(path)
	if err != nil {
		logWarn("clear-index: %v", err)
		return 2
	}
	if len(cleared) == 0 {
		fmt.Fprintf(os.Stderr, "gogrep: no index found under %s\n", path)
		return 1
	}
	for _, root := range cleared {
		fmt.Fprintf(os.Stderr, "gogrep: cleared index for %s\n", root)
	}
	return 0
}

// indexWalkOpts derives the walk options an index must match from the
// effective config.
func indexWalkOpts(cfg Config) index.WalkOpts {
	return index.WalkOpts{
		Hidden:   cfg.Hidden,
		NoIgnore: cfg.NoIgnore,
		Follow:   cfg.FollowSymlinks,
		Globs:    cfg.Globs,
	}
}

// indexPlan extracts, per OR-branch of the query, the literals every
// match of that branch must contain. ok=false means at least one branch
// is unconstrained, so candidates must fall back to all files.
func indexPlan(cfg Config) (branches [][][]byte, ok bool) {
	addBranch := func(pattern string, fixed, pcre bool) bool {
		if pcre {
			return false
		}
		if fixed {
			if len(pattern) < 3 {
				return false
			}
			branches = append(branches, [][]byte{[]byte(pattern)})
			return true
		}
		lits := regex.RequiredLiterals(pattern, cfg.IgnoreCase)
		if len(lits) == 0 {
			return false
		}
		branches = append(branches, lits)
		return true
	}

	if len(cfg.Patterns) > 1 {
		// Legacy multi-pattern OR (factory combines them itself).
		for _, p := range cfg.Patterns {
			if !addBranch(p, cfg.Fixed, cfg.PCRE) {
				return nil, false
			}
		}
		return branches, true
	}
	for _, pipeline := range cfg.Pipelines {
		if len(pipeline) == 0 {
			return nil, false
		}
		// Later -t stages only narrow stage 1's matches, so stage 1's
		// literals remain required for the whole pipeline.
		s := pipeline[0]
		if !addBranch(s.Pattern, s.Fixed, s.PCRE) {
			return nil, false
		}
	}
	return branches, true
}

// indexedFileChannel returns a file channel equivalent to walking root,
// but pruned by the trigram index: candidate files plus everything the
// freshness sweep flagged dirty. ok=false means the index doesn't apply
// (caller falls back to the cold walk).
func indexedFileChannel(cfg Config, root string) (<-chan walker.FileEntry, bool) {
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return nil, false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, false
	}
	absRoot = filepath.Clean(absRoot)
	opts := indexWalkOpts(cfg)
	dir := index.Dir(absRoot)

	meta, err := index.ReadMeta(dir)
	if err != nil {
		// No (valid) index yet: build one now.
		var stats *index.BuildStats
		meta, stats, err = index.Build(absRoot, opts)
		if err != nil {
			logWarn("index build failed, scanning cold: %v", err)
			return nil, false
		}
		fmt.Fprintf(os.Stderr, "gogrep: indexed %s (%d files, %d read, %d reused)\n",
			absRoot, stats.Files, stats.Read, stats.Reused)
	}
	if !meta.Opts.Equal(opts) {
		logWarn("index for %s was built with different options; scanning cold (--clear-index to re-key)", absRoot)
		return nil, false
	}

	ix, err := index.Load(dir)
	if err != nil {
		logWarn("index load failed, scanning cold: %v", err)
		return nil, false
	}
	sweep, err := ix.Sweep()
	if err != nil {
		ix.Close()
		logWarn("index sweep failed, scanning cold: %v", err)
		return nil, false
	}

	// Any drift: reindex transparently (cheap — digests are reused, so
	// only changed files are read) and the index is always current.
	// The post-rebuild sweep keeps the never-lie invariant even if
	// files change mid-rebuild.
	if len(sweep.Dirty)+len(sweep.Stale) > 0 {
		ix.Close()
		_, stats, err := index.Build(absRoot, opts)
		if err != nil {
			logWarn("index rebuild failed, scanning cold: %v", err)
			return nil, false
		}
		fmt.Fprintf(os.Stderr, "gogrep: reindexed %s (%d files, %d read, %d reused)\n",
			absRoot, stats.Files, stats.Read, stats.Reused)
		if ix, err = index.Load(dir); err != nil {
			logWarn("index load failed, scanning cold: %v", err)
			return nil, false
		}
		if sweep, err = ix.Sweep(); err != nil {
			ix.Close()
			logWarn("index sweep failed, scanning cold: %v", err)
			return nil, false
		}
	}

	// Candidate IDs: intersection per branch, union across branches.
	// Inverted matches need files WITHOUT hits, so no pruning applies.
	var ids []uint32
	pruned := false
	if !cfg.Invert {
		if branches, ok := indexPlan(cfg); ok {
			seen := make(map[uint32]bool)
			pruned = true
			for _, lits := range branches {
				branchIDs, constrained := ix.CandidateIDs(lits)
				if !constrained {
					pruned = false
					break
				}
				for _, id := range branchIDs {
					seen[id] = true
				}
			}
			if pruned {
				ids = make([]uint32, 0, len(seen))
				for id := range seen {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
			}
		}
	}
	if !pruned {
		// Unconstrained: all non-binary files are candidates — the
		// index still spares the query its own walk.
		ids = make([]uint32, 0, len(ix.Entries))
		for i := range ix.Entries {
			ids = append(ids, uint32(i))
		}
	}

	// Emit paths in the same form the user gave (the walker preserves
	// "./x"-style prefixes; so must we).
	prefix := strings.TrimSuffix(root, "/")
	join := func(rel string) string { return prefix + "/" + rel }

	ch := make(chan walker.FileEntry, 256)
	go func() {
		defer close(ch)
		defer ix.Close()
		for _, id := range ids {
			e := ix.Entries[id]
			if e.Binary || sweep.Stale[id] {
				continue
			}
			ch <- walker.FileEntry{Path: join(e.Path)}
		}
		for _, abs := range sweep.Dirty {
			if rel, err := filepath.Rel(absRoot, abs); err == nil {
				ch <- walker.FileEntry{Path: join(rel)}
			} else {
				ch <- walker.FileEntry{Path: abs}
			}
		}
	}()
	return ch, true
}
