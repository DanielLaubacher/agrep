package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/dl/gogrep/internal/walker"
)

// BuildStats reports how much work a build actually did.
type BuildStats struct {
	Files  int // files in the index
	Read   int // files read and digested this build
	Reused int // digests reused (previous build or child roots)
}

// Build (re)builds the index for root under its cache dir. The walk
// honors opts (the user's effective search options, so later queries
// match). Builds are incremental: digests from this root's previous
// build and from any registered child root are reused for every file
// whose (size, mtime) fingerprint is unchanged — only new/changed
// content is read.
func Build(root string, opts WalkOpts) (*Meta, *BuildStats, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	absRoot = filepath.Clean(absRoot)
	dir := Dir(absRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}

	// Digest reuse pool: own previous build, then registered child
	// roots (their paths re-keyed relative to this root).
	reuse := loadForward(dir)
	for _, child := range rootsUnder(absRoot) {
		rel, err := filepath.Rel(absRoot, child)
		if err != nil {
			continue
		}
		for p, d := range loadForward(Dir(child)) {
			key := rel + "/" + p
			if _, exists := reuse[key]; !exists {
				reuse[key] = d
			}
		}
	}

	fileCh, errCh := walker.Walk([]string{absRoot}, walker.WalkOptions{
		Recursive:      true,
		NoIgnore:       opts.NoIgnore,
		Hidden:         opts.Hidden,
		FollowSymlinks: opts.Follow,
		Globs:          opts.Globs,
	})
	go func() {
		for range errCh {
		}
	}()

	// Digest extraction is CPU-bound (trigram + token scan of every
	// byte read); fan out across cores. The reuse map is read-only here.
	var (
		manifest []FileEntry
		digests  []*digest
		stats    BuildStats
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	workers := runtime.GOMAXPROCS(0)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scratch := newTriScratch()
			for entry := range fileCh {
				rel, err := filepath.Rel(absRoot, entry.Path)
				if err != nil {
					continue
				}
				var st unix.Stat_t
				if err := unix.Stat(entry.Path, &st); err != nil {
					continue
				}
				mtimeNs := st.Mtim.Sec*1e9 + st.Mtim.Nsec

				var d *digest
				reused := false
				if cached := reuse[rel]; cached != nil && cached.size == st.Size && cached.mtimeNs == mtimeNs {
					d = cached
					reused = true
				} else {
					data, err := os.ReadFile(entry.Path)
					if err != nil {
						continue
					}
					if walker.IsBinary(data) {
						d = &digest{binary: true}
					} else {
						d = extractDigest(data, scratch)
					}
					d.size = st.Size
					d.mtimeNs = mtimeNs
				}
				mu.Lock()
				if reused {
					stats.Reused++
				} else {
					stats.Read++
				}
				manifest = append(manifest, FileEntry{
					Path:    rel,
					Size:    st.Size,
					MtimeNs: mtimeNs,
					Binary:  d.binary,
				})
				digests = append(digests, d)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	stats.Files = len(manifest)

	// Deterministic manifest order regardless of worker completion order.
	order := make([]int, len(manifest))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return manifest[order[i]].Path < manifest[order[j]].Path })
	sortedManifest := make([]FileEntry, len(manifest))
	sortedDigests := make([]*digest, len(digests))
	for i, o := range order {
		sortedManifest[i] = manifest[o]
		sortedDigests[i] = digests[o]
	}
	manifest, digests = sortedManifest, sortedDigests

	// Merge digests into the inverted structures (CPU-only: no I/O).
	postings := make(map[uint32][]uint32, 1<<16)
	vocab := make(map[string][2]uint32, 1<<16)
	for id, d := range digests {
		for _, t := range d.tris {
			postings[t] = append(postings[t], uint32(id))
		}
		for _, tc := range d.toks {
			c := vocab[tc.tok]
			c[0]++
			c[1] += tc.occ
			vocab[tc.tok] = c
		}
	}

	if err := writeForward(dir, manifest, digests); err != nil {
		return nil, nil, err
	}
	if err := writeManifest(dir, manifest); err != nil {
		return nil, nil, err
	}
	if err := writePostings(dir, postings); err != nil {
		return nil, nil, err
	}
	if err := writeVocab(dir, vocab); err != nil {
		return nil, nil, err
	}

	meta := &Meta{
		Version: FormatVersion,
		Root:    absRoot,
		BuiltAt: time.Now().UTC().Format(time.RFC3339),
		Files:   len(manifest),
		Opts:    opts,
	}
	if err := writeMeta(dir, meta); err != nil {
		return nil, nil, err
	}
	registerRoot(absRoot)
	return meta, &stats, nil
}

func writeManifest(dir string, entries []FileEntry) error {
	var buf []byte
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries)))
	for _, e := range entries {
		if len(e.Path) > 0xFFFF {
			return fmt.Errorf("path too long: %s", e.Path)
		}
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(e.Path)))
		buf = append(buf, e.Path...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.Size))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.MtimeNs))
		var flags byte
		if e.Binary {
			flags = 1
		}
		buf = append(buf, flags)
	}
	return writeAtomic(filepath.Join(dir, manifestFile), buf)
}

func writePostings(dir string, postings map[uint32][]uint32) error {
	tris := make([]uint32, 0, len(postings))
	for t := range postings {
		tris = append(tris, t)
	}
	sort.Slice(tris, func(i, j int) bool { return tris[i] < tris[j] })

	// Layout: magic, numTris u32, table[numTris]{tri u32, count u32,
	// offset u64}, then varint-delta posting data. Offsets are relative
	// to the start of the data region.
	var data []byte
	table := make([]byte, 0, len(tris)*postEntrySize)
	for _, t := range tris {
		ids := postings[t]
		table = binary.LittleEndian.AppendUint32(table, t)
		table = binary.LittleEndian.AppendUint32(table, uint32(len(ids)))
		table = binary.LittleEndian.AppendUint64(table, uint64(len(data)))
		prev := uint32(0)
		for i, id := range ids {
			if i == 0 {
				data = putUvarint(data, uint64(id))
			} else {
				data = putUvarint(data, uint64(id-prev))
			}
			prev = id
		}
	}

	var buf []byte
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(tris)))
	buf = append(buf, table...)
	buf = append(buf, data...)
	return writeAtomic(filepath.Join(dir, postingsFile), buf)
}

func writeVocab(dir string, vocab map[string][2]uint32) error {
	toks := make([]string, 0, len(vocab))
	for t := range vocab {
		toks = append(toks, t)
	}
	sort.Strings(toks)

	// Layout: magic, numToks u32, table[numToks]{blobOff u32, len u16,
	// pad u16, files u32, occs u32}, then the token blob.
	var blob strings.Builder
	table := make([]byte, 0, len(toks)*vocabEntrySize)
	for _, t := range toks {
		c := vocab[t]
		table = binary.LittleEndian.AppendUint32(table, uint32(blob.Len()))
		table = binary.LittleEndian.AppendUint16(table, uint16(len(t)))
		table = binary.LittleEndian.AppendUint16(table, 0)
		table = binary.LittleEndian.AppendUint32(table, c[0])
		table = binary.LittleEndian.AppendUint32(table, c[1])
		blob.WriteString(t)
	}

	var buf []byte
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(toks)))
	buf = append(buf, table...)
	buf = append(buf, blob.String()...)
	return writeAtomic(filepath.Join(dir, vocabFile), buf)
}
