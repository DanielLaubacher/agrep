package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/dl/gogrep/internal/walker"
)

// Index is a loaded, queryable index. The posting file is mmap'd and
// binary-searched in place: load cost is independent of index size.
type Index struct {
	Meta    *Meta
	Entries []FileEntry

	postings []byte // mmap
	table    []byte // posting table region (postEntrySize rows)
	data     []byte // varint-delta posting lists
	numTris  int
}

// Load opens the index in dir. Close releases the mapping.
func Load(dir string) (*Index, error) {
	meta, err := ReadMeta(dir)
	if err != nil {
		return nil, err
	}
	entries, err := readManifest(filepath.Join(dir, manifestFile))
	if err != nil {
		return nil, err
	}
	if len(entries) != meta.Files {
		return nil, fmt.Errorf("manifest/meta mismatch")
	}

	f, err := os.Open(filepath.Join(dir, postingsFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ix := &Index{Meta: meta, Entries: entries}
	if st.Size() > 0 {
		m, err := unix.Mmap(int(f.Fd()), 0, int(st.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
		if err != nil {
			return nil, err
		}
		ix.postings = m
	}
	if len(ix.postings) < 8 || string(ix.postings[:4]) != string(magic) {
		ix.Close()
		return nil, fmt.Errorf("bad postings file")
	}
	ix.numTris = int(binary.LittleEndian.Uint32(ix.postings[4:]))
	tableEnd := 8 + ix.numTris*postEntrySize
	if tableEnd > len(ix.postings) {
		ix.Close()
		return nil, fmt.Errorf("truncated postings file")
	}
	ix.table = ix.postings[8:tableEnd]
	ix.data = ix.postings[tableEnd:]
	return ix, nil
}

// Close releases the posting mapping.
func (ix *Index) Close() {
	if ix.postings != nil {
		_ = unix.Munmap(ix.postings)
		ix.postings = nil
	}
}

func readManifest(path string) ([]FileEntry, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(buf) < 8 || string(buf[:4]) != string(magic) {
		return nil, fmt.Errorf("bad manifest")
	}
	count := int(binary.LittleEndian.Uint32(buf[4:]))
	entries := make([]FileEntry, 0, count)
	p := 8
	for i := 0; i < count; i++ {
		if p+2 > len(buf) {
			return nil, fmt.Errorf("truncated manifest")
		}
		plen := int(binary.LittleEndian.Uint16(buf[p:]))
		p += 2
		if p+plen+17 > len(buf) {
			return nil, fmt.Errorf("truncated manifest")
		}
		e := FileEntry{
			Path:    string(buf[p : p+plen]),
			Size:    int64(binary.LittleEndian.Uint64(buf[p+plen:])),
			MtimeNs: int64(binary.LittleEndian.Uint64(buf[p+plen+8:])),
			Binary:  buf[p+plen+16]&1 != 0,
		}
		p += plen + 17
		entries = append(entries, e)
	}
	return entries, nil
}

// postingCount returns the number of files containing tri (0 if absent),
// via binary search over the mmap'd table.
func (ix *Index) postingRow(tri uint32) (row int, ok bool) {
	lo, hi := 0, ix.numTris
	for lo < hi {
		mid := (lo + hi) / 2
		t := binary.LittleEndian.Uint32(ix.table[mid*postEntrySize:])
		switch {
		case t < tri:
			lo = mid + 1
		case t > tri:
			hi = mid
		default:
			return mid, true
		}
	}
	return 0, false
}

// postingList decodes the file-ID list for a table row.
func (ix *Index) postingList(row int) []uint32 {
	base := row * postEntrySize
	count := int(binary.LittleEndian.Uint32(ix.table[base+4:]))
	off := binary.LittleEndian.Uint64(ix.table[base+8:])
	ids := make([]uint32, 0, count)
	p := int(off)
	prev := uint32(0)
	for i := 0; i < count; i++ {
		v, n := binary.Uvarint(ix.data[p:])
		if n <= 0 {
			return nil
		}
		p += n
		prev += uint32(v)
		ids = append(ids, prev)
	}
	return ids
}

// CandidateIDs intersects the posting lists for every trigram of every
// literal in one AND-branch and returns the matching file IDs (sorted).
// A nil/empty literal list means the branch is unconstrained: (nil,
// false) — the caller must fall back to all files for that branch.
func (ix *Index) CandidateIDs(literals [][]byte) ([]uint32, bool) {
	var tris []uint32
	for _, lit := range literals {
		tris = append(tris, LiteralTrigrams(lit)...)
	}
	if len(tris) == 0 {
		return nil, false
	}
	// Dedup, then resolve rows; a required trigram absent from the
	// corpus means no file can match this branch.
	sort.Slice(tris, func(i, j int) bool { return tris[i] < tris[j] })
	type rowRef struct{ row, count int }
	var rows []rowRef
	var last uint32 = 1 << 30
	for _, t := range tris {
		if t == last {
			continue
		}
		last = t
		row, ok := ix.postingRow(t)
		if !ok {
			return []uint32{}, true
		}
		count := int(binary.LittleEndian.Uint32(ix.table[row*postEntrySize+4:]))
		rows = append(rows, rowRef{row, count})
	}
	// Intersect rarest-first for early exit.
	sort.Slice(rows, func(i, j int) bool { return rows[i].count < rows[j].count })
	result := ix.postingList(rows[0].row)
	for _, r := range rows[1:] {
		if len(result) == 0 {
			break
		}
		result = intersectSorted(result, ix.postingList(r.row))
	}
	return result, true
}

func intersectSorted(a, b []uint32) []uint32 {
	out := a[:0]
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			i++
		case a[i] > b[j]:
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	return out
}

// SweepResult is the freshness diff between the index and the live tree.
type SweepResult struct {
	// Dirty are absolute paths that must be scanned regardless of the
	// candidate computation: new files and changed files.
	Dirty []string
	// Stale marks manifest IDs that must NOT be emitted from the
	// candidate set: changed files (they re-enter via Dirty) and
	// deleted files.
	Stale map[uint32]bool
}

// Sweep walks the root with the index's build options and stat-compares
// every file against the manifest fingerprint. It never misses: any
// divergence lands in Dirty/Stale, and verification does the rest.
func (ix *Index) Sweep() (*SweepResult, error) {
	root := ix.Meta.Root
	byPath := make(map[string]uint32, len(ix.Entries))
	for i, e := range ix.Entries {
		byPath[e.Path] = uint32(i)
	}

	fileCh, errCh := walker.Walk([]string{root}, walker.WalkOptions{
		Recursive:      true,
		NoIgnore:       ix.Meta.Opts.NoIgnore,
		Hidden:         ix.Meta.Opts.Hidden,
		FollowSymlinks: ix.Meta.Opts.Follow,
		Globs:          ix.Meta.Opts.Globs,
	})
	go func() {
		for range errCh {
		}
	}()

	res := &SweepResult{Stale: map[uint32]bool{}}
	seen := make([]bool, len(ix.Entries))
	var mu sync.Mutex
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	if workers > 16 {
		workers = 16
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for entry := range fileCh {
				rel, err := filepath.Rel(root, entry.Path)
				if err != nil {
					continue
				}
				var st unix.Stat_t
				if err := unix.Stat(entry.Path, &st); err != nil {
					continue
				}
				mtimeNs := st.Mtim.Sec*1e9 + st.Mtim.Nsec
				mu.Lock()
				if id, ok := byPath[rel]; ok {
					seen[id] = true
					e := ix.Entries[id]
					if e.Size != st.Size || e.MtimeNs != mtimeNs {
						res.Stale[id] = true
						res.Dirty = append(res.Dirty, entry.Path)
					}
				} else {
					res.Dirty = append(res.Dirty, entry.Path)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for i, s := range seen {
		if !s {
			res.Stale[uint32(i)] = true
		}
	}
	return res, nil
}

// VocabLookup returns corpus-wide (fileCount, occurrenceCount) for a
// lowercased token, for vocabulary-backed suggestions.
func VocabLookup(dir, token string) (files, occs uint32, ok bool) {
	buf, err := os.ReadFile(filepath.Join(dir, vocabFile))
	if err != nil || len(buf) < 8 || string(buf[:4]) != string(magic) {
		return 0, 0, false
	}
	n := int(binary.LittleEndian.Uint32(buf[4:]))
	table := buf[8:]
	blobOff := 8 + n*vocabEntrySize
	if blobOff > len(buf) {
		return 0, 0, false
	}
	blob := buf[blobOff:]
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		base := mid * vocabEntrySize
		off := int(binary.LittleEndian.Uint32(table[base:]))
		tlen := int(binary.LittleEndian.Uint16(table[base+4:]))
		tok := string(blob[off : off+tlen])
		switch {
		case tok < token:
			lo = mid + 1
		case tok > token:
			hi = mid
		default:
			return binary.LittleEndian.Uint32(table[base+8:]),
				binary.LittleEndian.Uint32(table[base+12:]), true
		}
	}
	return 0, 0, false
}
