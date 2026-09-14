# The Trigram Index (`--use-index`)

The stateless agent features (agent-mode.md) made single searches cheap.
The index attacks the remaining cost structure: agents issue **dozens of
queries against a slowly-changing tree**, and without an index every one
pays a full walk + scan. `--use-index` converts that to O(changes) —
entirely daemonless: no server, no background process, no watchers. Every
invocation is self-contained.

## The one invariant everything hangs on

> **The index is a prefilter, never an answerer.** It may only shrink the
> set of files the real engines scan; every reported match comes from
> verifying actual disk bytes with the existing matchers.

Consequences, stated as rules:

1. Candidate sets are **supersets**: staleness may cost wasted
   verification, never a wrong match.
2. Files the index hasn't caught up on (the dirty set) are **always
   appended** to the candidate set — a new or modified file can never be
   silently invisible.
3. Any condition the index can't handle (option mismatch, `-v`, PCRE
   stages) falls back to a cold scan with a stderr note — never a
   rebuild-thrash, never a wrong answer.
4. Identical output with and without the index is a **tested property**,
   not an aspiration (differential tests: indexed vs cold scan,
   byte-equal).

## How it works

```
agrep --use-index -rn 'pattern' ROOT/
   │
   │  first use: build the index (parallel walk + trigram extraction)
   │  every use: parallel stat sweep → dirty set → incremental reindex
   ▼
$XDG_CACHE_HOME/agrep/<root-id>/   (nothing is ever written inside ROOT)
   ├── trigram postings segments
   ├── forward.bin   per-file digests keyed by (path, size, mtimeNs)
   └── roots.json    registry, shared digest reuse across roots
```

- **Trigram postings** (Google-Code-Search lineage): lowercased 3-byte
  shingle → delta-varint file-ID list. Lowercased postings serve both
  case modes — case-sensitive queries look up lowercased trigrams (a
  superset) and verification restores exactness.
- **Query planning reuses the existing literal machinery**: the prefilter
  extractor already produces required literals per pattern; literals ≥3
  bytes decompose into AND'd trigram lookups; multiple literals AND
  further; batch queries OR their plans and dedupe candidates. Patterns
  with no extractable ≥3-byte literal still use the index as a walk-skip
  (all indexed files + dirty set) — the directory walk is saved even when
  pruning isn't possible.
- **Freshness is a per-query stat sweep**, not watchers: before the index
  is used, a parallel sweep validates the known file list against the
  manifest (size+mtime; directory mtimes catch adds/removes). Divergent
  and new files join the dirty set that is appended to every candidate
  set — the never-lie invariant holds with zero background machinery.
  Cost ~1-2µs/file warm: ~2ms for an 836-file books corpus, ~25ms for
  /usr/include.
- **Reindexing is incremental at 0% drift threshold** — affordable
  because `forward.bin` stores each file's digest (trigram set + token
  counts) keyed by (path, size, mtimeNs), so only changed files are ever
  re-read. This is the git-blob-reuse property without content hashes (a
  content Merkle tree would cost the very reads it avoids; a stat "tree"
  can't roll up since directory mtimes don't propagate).
- **Digests are reusable across roots** via `roots.json`: building a
  parent of an already-indexed tree adopts the child's digests (measured:
  a parent build over an indexed child read only the uncovered files).
- `--clear-index PATH` deletes state for every root at/under PATH (the
  accidental-node_modules remedy).

## Measured (books corpus, 836 files / 531MB)

- Build: 7.8s parallel; index size 78MB (~15% of corpus).
- Selective query: 26ms indexed vs 49ms cold (1.85x, page-cache-warm —
  the win grows when cold).
- Single-file edit: reindex reads exactly 1 file.

Differential tests: indexed vs cold byte-equal on repo + corpus (literal,
regex, multi-`-e`); property tests for candidates ⊇ matches, sweep
add/modify/delete, digest reuse, and parent-adopts-child.

## Non-goals (settled)

- No FTS/SQLite/vector store as truth or matcher — files remain the
  truth; the index only accelerates reaching them (see agent-mode.md).
- No background process or watcher of any kind: freshness comes from the
  per-query sweep, and every correctness property holds with the index
  deleted. A resident indexing service was considered and deliberately
  not built — the sweep-based design already delivers correct,
  always-fresh results at single-invocation cost, without a lifecycle to
  manage.
