package index

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// digest is one file's contribution to the index, keyed by its stat
// fingerprint. Digests are the unit of reuse: a rebuild (or a parent
// root's build) re-reads only files whose (size, mtime) fingerprint has
// no cached digest — the git-blob-reuse property without content hashes.
type digest struct {
	size    int64
	mtimeNs int64
	binary  bool
	tris    []uint32   // sorted distinct lowercased trigrams
	toks    []tokCount // sorted by token
}

type tokCount struct {
	tok string
	occ uint32
}

// writeForward serializes per-file digests (paths from the parallel
// manifest slice). Layout: magic, version u32, count u32, then per file:
// u16 pathLen, path, u64 size, u64 mtimeNs, u8 flags, u32 nTris,
// delta-uvarint tris, u32 nToks, {u8 len, bytes, uvarint occ}*.
func writeForward(dir string, entries []FileEntry, digests []*digest) error {
	var buf []byte
	buf = append(buf, magic...)
	buf = binary.LittleEndian.AppendUint32(buf, FormatVersion)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries)))
	for i, e := range entries {
		d := digests[i]
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(e.Path)))
		buf = append(buf, e.Path...)
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.Size))
		buf = binary.LittleEndian.AppendUint64(buf, uint64(e.MtimeNs))
		var flags byte
		if d.binary {
			flags = 1
		}
		buf = append(buf, flags)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(d.tris)))
		prev := uint32(0)
		for _, t := range d.tris {
			buf = putUvarint(buf, uint64(t-prev))
			prev = t
		}
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(d.toks)))
		for _, tc := range d.toks {
			buf = append(buf, byte(len(tc.tok)))
			buf = append(buf, tc.tok...)
			buf = putUvarint(buf, uint64(tc.occ))
		}
	}
	return writeAtomic(filepath.Join(dir, forwardFile), buf)
}

// loadForward reads a forward file into a path-keyed digest map.
// Any parse problem returns an empty map — reuse is an optimization.
func loadForward(dir string) map[string]*digest {
	out := map[string]*digest{}
	buf, err := os.ReadFile(filepath.Join(dir, forwardFile))
	if err != nil {
		return out
	}
	defer func() {
		if recover() != nil {
			out = map[string]*digest{}
		}
	}()
	p := 0
	need := func(n int) error {
		if p+n > len(buf) {
			return fmt.Errorf("truncated")
		}
		return nil
	}
	if need(12) != nil || string(buf[:4]) != string(magic) {
		return map[string]*digest{}
	}
	if binary.LittleEndian.Uint32(buf[4:]) != FormatVersion {
		return map[string]*digest{}
	}
	count := int(binary.LittleEndian.Uint32(buf[8:]))
	p = 12
	for i := 0; i < count; i++ {
		if need(2) != nil {
			return map[string]*digest{}
		}
		plen := int(binary.LittleEndian.Uint16(buf[p:]))
		p += 2
		if need(plen+17) != nil {
			return map[string]*digest{}
		}
		path := string(buf[p : p+plen])
		p += plen
		d := &digest{
			size:    int64(binary.LittleEndian.Uint64(buf[p:])),
			mtimeNs: int64(binary.LittleEndian.Uint64(buf[p+8:])),
			binary:  buf[p+16]&1 != 0,
		}
		p += 17
		if need(4) != nil {
			return map[string]*digest{}
		}
		nTris := int(binary.LittleEndian.Uint32(buf[p:]))
		p += 4
		d.tris = make([]uint32, nTris)
		prev := uint32(0)
		for j := 0; j < nTris; j++ {
			v, n := binary.Uvarint(buf[p:])
			if n <= 0 {
				return map[string]*digest{}
			}
			p += n
			prev += uint32(v)
			d.tris[j] = prev
		}
		if need(4) != nil {
			return map[string]*digest{}
		}
		nToks := int(binary.LittleEndian.Uint32(buf[p:]))
		p += 4
		d.toks = make([]tokCount, nToks)
		for j := 0; j < nToks; j++ {
			if need(1) != nil {
				return map[string]*digest{}
			}
			tlen := int(buf[p])
			p++
			if need(tlen) != nil {
				return map[string]*digest{}
			}
			tok := string(buf[p : p+tlen])
			p += tlen
			v, n := binary.Uvarint(buf[p:])
			if n <= 0 {
				return map[string]*digest{}
			}
			p += n
			d.toks[j] = tokCount{tok: tok, occ: uint32(v)}
		}
		out[path] = d
	}
	return out
}

// triScratch is a per-worker trigram dedup array. Epoch marking makes
// per-file reset O(1): a slot is "seen this file" iff it equals the
// current epoch, and the array is only cleared when the epoch wraps.
type triScratch struct {
	seen  []uint8
	epoch uint8
}

func newTriScratch() *triScratch { return &triScratch{seen: make([]uint8, 1<<24)} }

func (s *triScratch) next() {
	s.epoch++
	if s.epoch == 0 {
		clear(s.seen)
		s.epoch = 1
	}
}

// extractDigest computes a file's digest from its content.
func extractDigest(data []byte, s *triScratch) *digest {
	s.next()
	d := &digest{}
	for i := 0; i+3 <= len(data); i++ {
		tri := packTri(data[i], data[i+1], data[i+2])
		if s.seen[tri] != s.epoch {
			s.seen[tri] = s.epoch
			d.tris = append(d.tris, tri)
		}
	}
	sort.Slice(d.tris, func(i, j int) bool { return d.tris[i] < d.tris[j] })

	occ := map[string]uint32{}
	tokenize(data, func(tok string) { occ[tok]++ })
	d.toks = make([]tokCount, 0, len(occ))
	for t, c := range occ {
		d.toks = append(d.toks, tokCount{tok: t, occ: c})
	}
	sort.Slice(d.toks, func(i, j int) bool { return d.toks[i].tok < d.toks[j].tok })
	return d
}
