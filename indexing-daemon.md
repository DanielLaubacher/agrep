# Indexing Daemon: Plan

The stateless agent features (agent-mode.md) made single searches cheap.
The daemon attacks the remaining cost structure: agents issue **dozens of
queries against a slowly-changing tree**, and today every one pays a full
walk + scan. The daemon converts that to O(changes): millisecond repeated
queries, free `--suggest`, `--changed-since`, and session cursors.

## The one invariant everything hangs on

> **The index is a prefilter, never an answerer.** It may only shrink the
> set of files the real engines scan; every reported match comes from
> verifying actual disk bytes with the existing matchers.

Consequences, stated as rules:

1. Candidate sets are **supersets**: staleness may cost wasted verification,
   never a wrong match.
2. Files the index hasn't caught up on (dirty set) are **always appended**
   to the candidate set — a new or modified file can never be silently
   invisible.
3. If the watcher loses events (inotify queue overflow), the daemon marks
   itself stale and answers "scan everything" until re-indexed — degraded
   speed, never degraded truth.
4. Identical output with and without the daemon is a **tested property**,
   not an aspiration (differential tests: daemon vs cold scan, byte-equal).

## Architecture

```
gogrep (client, unchanged CLI)
   │  1. resolve search root against daemon registry
   │  2. send query plan over unix socket (50ms budget)
   │  3. on any error/timeout/absence → cold scan, identical output
   ▼
gogrep serve ROOT  (same binary, subcommand)
   ├── trigram index  (mmap'd segments in $XDG_CACHE_HOME/gogrep/<root-id>/)
   ├── token vocabulary  (word → file/line counts; powers suggest + IDF rank)
   ├── inotify journal  (recursive watch → dirty set → debounced re-index,
   │                     monotonically increasing corpus version)
   └── socket API  ($XDG_RUNTIME_DIR/gogrep/<root-id>.sock, 0700;
                    registry file maps root paths → sockets)
```

- **Same binary, separate identity**: `gogrep serve` keeps distribution
  simple; the daemon code must add no dependencies or init cost to the
  scanner path (the /etc/services lesson).
- **Nothing is written inside the corpus** — index and sockets live in
  XDG cache/runtime dirs keyed by root path hash. Corpora stay read-only
  and pollution-free (the books mirror rule, generalized).

### Trigram index

Google-Code-Search lineage, zoekt-style segments:

- Postings: lowercased 3-byte shingle → delta-varint file-ID list.
  Lowercased postings serve both case modes (case-sensitive queries look
  up lowercased trigrams — superset — and verification restores exactness).
- Large files additionally record coarse block bitmaps (~64KB granularity)
  so verification can seek, not rescan.
- **Query planning reuses the existing literal machinery**: the prefilter
  extractor already produces required literals per pattern; literals ≥3
  bytes decompose into AND'd trigram lookups; multiple literals AND
  further; batch queries OR their plans and dedupe candidates. Patterns
  with no extractable trigrams (pure classes like `[0-9][a-z]`) get plan
  "all files" — the daemon still saves the directory walk, and Cox-style
  regex→trigram AND/OR trees are a later refinement, not a blocker.
- Incremental updates are segment-based: changed files get tombstoned and
  re-indexed into fresh segments; background merge compacts. Readers work
  on an immutable epoch snapshot (swap pointer, no locks in the query path
  — the DFA precompute lesson applied to the index).
- Sizing (books corpus, 531MB/834 files): build ~1-2s parallel, index
  ~10-20% of corpus, query planning + intersection well under 1ms.

### Token vocabulary

Built during the same indexing pass: lowercased identifier/word →
(file count, approximate line count). Small, and it turns two features
from rescans into lookups:

- `--suggest` becomes instant and *better*: variants ranked by true corpus
  rarity, plus prefix/fragment lookups the rescan approach can't afford.
- IDF for `--rank`: order `--outline` and match output by how *informative*
  the matched term is, path priors (src > vendor, shallow > deep), and
  mtime recency — the deferred relevance ranking, now with real statistics.

### Change journal

- Recursive inotify (per-directory watches; `max_user_watches` documented,
  overflow → stale-mode per invariant 3).
- Every applied change increments the **corpus version**; the journal
  retains (version, path, kind) for a bounded window.
- `--changed-since VERSION` returns paths changed after VERSION and the
  current version — the agent primitive for "search only what moved since
  I last looked." Version tokens appear in every daemon response, so
  agents get them for free.

### Protocol

Newline-delimited JSON over the unix socket (matches the JSON-Lines
output ethos; trivially debuggable with `nc`):

- `candidates {plan} → {files, alsoScan, version, coverage}`
- `changed {since} → {paths, version}`
- `vocab {fragment, limit} → {terms: [{t, files, lines}]}`
- `session.open {plan} / session.next {id, cursor} / session.refine {id, plan}`
- `stat {} → {version, files, segments, dirty, mem}`

The client sends *plans* (extracted literals/trigrams + flags), not raw
patterns — planning stays in one place (the client's compile step, which
already does this work) and the daemon stays engine-agnostic.

## Client transparency

In `cli.Run`, before walking: if every search root falls under a live
registered daemon and a plan exists → fetch candidates, feed them to the
existing scheduler as the file channel (walker bypassed), verify as
always. Any failure → seamless cold scan. `GOGREP_NO_DAEMON=1` opts out;
`--daemon-status` reports what was used (agents can log it). The agent
runs *the same commands* either way — starting `gogrep serve ~/books-text`
once is the only new action, and `--suggest`/`--changed-since`/sessions
simply light up when the daemon is present.

## Phases (each independently shippable and testable)

**A — Index core, no daemon.** `gogrep index ROOT` builds the cache;
`--use-index` consumes it in-process. Proves format, planner, and the
superset property with differential tests (indexed vs cold byte-equal
output over randomized corpora; property test: candidates ⊇ files with
matches). This de-risks everything before any long-running process exists.

**B — Daemon + transparency.** `gogrep serve`, socket + registry,
auto-handoff with fallback, inotify journal, staleness contract,
idle-exit (`--idle-exit 2h` default). Differential tests re-run against
a live daemon under concurrent file mutation.

**C — Agent features on the index.** Vocab-backed `--suggest`,
`--changed-since`, IDF-informed `--rank`/outline ordering, session
cursors (`--session`, `--after`). Batch + outline composition lands here
too (both become daemon-native aggregations).

**D — MCP surface (optional).** The daemon speaks MCP over the socket so
agent harnesses connect directly: no process spawn, streaming results,
sessions as first-class tools. The CLI remains one client among two.

## Open questions (flagged, with leanings)

1. **Index granularity for huge files**: block bitmaps (leaning) vs
   line-range postings — decide with measurements in phase A.
2. **Daemon supervision**: none/manual for v1 (leaning); systemd user
   units documented, not required.
3. **Multi-root queries** (paths spanning daemons): v1 requires a single
   covering root, else cold scan (leaning — simplicity).
4. **Vocabulary tokenization**: identifier-aware splitting (camelCase,
   snake_case) on by default (leaning yes — it made --suggest useful).

## Non-goals (settled in prior discussions)

- No FTS/SQLite/vector store as truth or matcher — files remain the truth,
  the daemon only accelerates reaching them (see agent-mode.md and the
  grep-a-SQLite experiment).
- No daemon dependency for any correctness property: everything the
  scanner did yesterday, it does identically with the daemon gone.
