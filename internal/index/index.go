// Package index implements gogrep's daemonless trigram index: a
// prefilter that shrinks the set of files the real matchers verify,
// never an answerer (see indexing-daemon.md).
//
// State lives under $XDG_CACHE_HOME/gogrep/<root-id>/ and is maintained
// entirely by `--use-index` queries: the first builds the index, every
// later one validates freshness with a parallel stat sweep. Files that
// changed since the build are appended to the candidate set — the index
// may cost wasted verification, never a missed match. When the dirty
// set grows large the index transparently rebuilds, and rebuilds are
// incremental: per-file digests (forward.bin) keyed by (path, size,
// mtime) are reused from the previous build and from any indexed child
// root, so re-indexing a parent of an already-indexed tree re-reads
// nothing that hasn't changed.
//
// The on-disk format is mmap-friendly: queries binary-search the
// posting table in place and touch only the posting lists they need.
package index

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FormatVersion is bumped on any on-disk layout change.
const FormatVersion = 1

// WalkOpts are the traversal options an index was built with. A query
// may use the index only when its effective options are equal —
// otherwise the candidate set could diverge from a cold scan's file set.
type WalkOpts struct {
	Hidden   bool     `json:"hidden"`
	NoIgnore bool     `json:"no_ignore"`
	Follow   bool     `json:"follow"`
	Globs    []string `json:"globs"`
}

// Equal reports option equivalence (glob order-insensitive).
func (w WalkOpts) Equal(o WalkOpts) bool {
	if w.Hidden != o.Hidden || w.NoIgnore != o.NoIgnore || w.Follow != o.Follow {
		return false
	}
	if len(w.Globs) != len(o.Globs) {
		return false
	}
	a := append([]string(nil), w.Globs...)
	b := append([]string(nil), o.Globs...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Meta is the index's metadata sidecar (meta.json).
type Meta struct {
	Version int      `json:"version"`
	Root    string   `json:"root"`
	BuiltAt string   `json:"built_at"`
	Files   int      `json:"files"`
	Opts    WalkOpts `json:"opts"`
}

// FileEntry is one manifest row: a file the index covers, with the
// size/mtime fingerprint the sweep validates against. Binary files are
// recorded (so the sweep doesn't flag them dirty forever) but carry no
// postings and are never candidates.
type FileEntry struct {
	Path    string // root-relative
	Size    int64
	MtimeNs int64
	Binary  bool
}

// cacheBase returns $XDG_CACHE_HOME/gogrep (or ~/.cache/gogrep).
func cacheBase() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "gogrep")
}

// Dir returns the cache directory for a root path.
func Dir(root string) string {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	abs = filepath.Clean(abs)
	h := sha256.Sum256([]byte(abs))
	return filepath.Join(cacheBase(), hex.EncodeToString(h[:16]))
}

// --- root registry (roots.json: abs root path -> cache dir name) ---
//
// The registry exists for two management features: digest reuse across
// roots (indexing a parent adopts child digests) and --clear-index.
// It is advisory — losing it only loses those conveniences.

func registryPath() string { return filepath.Join(cacheBase(), "roots.json") }

func readRegistry() map[string]string {
	data, err := os.ReadFile(registryPath())
	if err != nil {
		return map[string]string{}
	}
	m := map[string]string{}
	if json.Unmarshal(data, &m) != nil {
		return map[string]string{}
	}
	return m
}

func writeRegistry(m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cacheBase(), 0o755); err != nil {
		return err
	}
	return writeAtomic(registryPath(), data)
}

func registerRoot(absRoot string) {
	m := readRegistry()
	name := filepath.Base(Dir(absRoot))
	if m[absRoot] != name {
		m[absRoot] = name
		_ = writeRegistry(m) // advisory; best-effort
	}
}

// rootsUnder returns registered roots strictly inside path (not path itself).
func rootsUnder(path string) []string {
	prefix := filepath.Clean(path) + string(filepath.Separator)
	var out []string
	for root := range readRegistry() {
		if strings.HasPrefix(root, prefix) {
			out = append(out, root)
		}
	}
	sort.Strings(out)
	return out
}

// ClearUnder deletes index state for every root at or under path,
// returning the roots cleared. It also clears an unregistered index for
// the exact path (pre-registry builds).
func ClearUnder(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	reg := readRegistry()
	var cleared []string
	for root, name := range reg {
		if root == abs || strings.HasPrefix(root, abs+string(filepath.Separator)) {
			if err := os.RemoveAll(filepath.Join(cacheBase(), name)); err != nil {
				return cleared, err
			}
			delete(reg, root)
			cleared = append(cleared, root)
		}
	}
	if len(cleared) > 0 {
		if err := writeRegistry(reg); err != nil {
			return cleared, err
		}
	}
	// Exact-path index that predates the registry.
	d := Dir(abs)
	if _, err := os.Stat(filepath.Join(d, metaFile)); err == nil {
		found := false
		for _, r := range cleared {
			if r == abs {
				found = true
			}
		}
		if !found {
			if err := os.RemoveAll(d); err != nil {
				return cleared, err
			}
			cleared = append(cleared, abs)
		}
	}
	sort.Strings(cleared)
	return cleared, nil
}

// --- trigram helpers ---

// lowerTable folds ASCII A-Z to a-z; all other bytes map to themselves.
var lowerTable = func() (t [256]byte) {
	for i := range t {
		c := byte(i)
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		t[i] = c
	}
	return
}()

func packTri(b0, b1, b2 byte) uint32 {
	return uint32(lowerTable[b0])<<16 | uint32(lowerTable[b1])<<8 | uint32(lowerTable[b2])
}

// LiteralTrigrams decomposes a literal into its (lowercased) trigrams.
// Returns nil when the literal is too short to constrain the index.
func LiteralTrigrams(lit []byte) []uint32 {
	if len(lit) < 3 {
		return nil
	}
	tris := make([]uint32, 0, len(lit)-2)
	for i := 0; i+3 <= len(lit); i++ {
		tris = append(tris, packTri(lit[i], lit[i+1], lit[i+2]))
	}
	sort.Slice(tris, func(i, j int) bool { return tris[i] < tris[j] })
	out := tris[:0]
	var last uint32 = 1 << 30
	for _, t := range tris {
		if t != last {
			out = append(out, t)
			last = t
		}
	}
	return out
}

// --- binary encoding helpers ---

const (
	manifestFile = "manifest.bin"
	postingsFile = "postings.bin"
	vocabFile    = "vocab.bin"
	forwardFile  = "forward.bin"
	metaFile     = "meta.json"

	postEntrySize  = 16 // tri u32, count u32, offset u64
	vocabEntrySize = 16 // blobOff u32, len u16, pad u16, files u32, occs u32
)

var magic = []byte("GGIX")

func writeMeta(dir string, m *Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, metaFile), data)
}

// ReadMeta loads and validates meta.json from a cache dir.
func ReadMeta(dir string) (*Meta, error) {
	data, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Version != FormatVersion {
		return nil, fmt.Errorf("index format v%d (want v%d) — will rebuild", m.Version, FormatVersion)
	}
	return &m, nil
}

// writeAtomic writes via temp file + rename so concurrent readers see
// either the old or the new file, never a torn one.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func putUvarint(buf []byte, v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return append(buf, tmp[:n]...)
}

// tokenize yields lowercased [a-z0-9_]+ runs of length 3..32 — the
// vocabulary powering instant --suggest and future IDF ranking.
func tokenize(data []byte, yield func(tok string)) {
	start := -1
	flush := func(end int) {
		if start >= 0 {
			n := end - start
			if n >= 3 && n <= 32 {
				yield(strings.ToLower(string(data[start:end])))
			}
			start = -1
		}
	}
	for i, c := range data {
		isWord := c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if isWord {
			if start < 0 {
				start = i
			}
		} else {
			flush(i)
		}
	}
	flush(len(data))
}
