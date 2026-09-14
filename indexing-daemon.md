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
3. Freshness degradation is a ladder that never reaches "stale": watch
   overflow drops a root to per-query sweeps; a failed sweep drops the
   query to a cold scan. Only `frozen` roots trade freshness away, and
   they do it explicitly and visibly.
4. Identical output with and without the daemon is a **tested property**,
   not an aspiration (differential tests: daemon vs cold scan, byte-equal).

## Architecture

```
gogrep (client, unchanged CLI)
   │  1. send search path + query plan to the well-known socket
   │     ($XDG_RUNTIME_DIR/gogrep/daemon.sock, 50ms budget)
   │  2. daemon longest-prefix-matches the path against its root table
   │  3. on any error/timeout/absence/uncovered path → cold scan,
   │     identical output
   ▼
gogrep serve  (ONE daemon per user — watchman model — hosting many roots)
   ├── root table: path → independent index unit
   │     each root has its own:
   │       • trigram segments ($XDG_CACHE_HOME/gogrep/<root-id>/)
   │       • token vocabulary (word → file/line counts; suggest + IDF)
   │       • inotify subtree + journal + monotonic corpus version
   │       • idle timer (cold roots drop mmaps/watches, keep registration)
   └── one socket API for all roots (0700 runtime dir)
```

- **One daemon per user, many roots**: agents in any repo talk to one
  well-known socket — no per-root discovery or startup. Roots share the
  process but no state: independent segments, versions, watches, and
  failure modes (one root's inotify overflow or rebuild never affects
  another). Registration via `gogrep index ROOT`; optional opt-in lazy
  adoption indexes a new root in the background after its first (cold)
  search. In phase D this also yields a single MCP endpoint spanning
  every corpus.
- **The root table is a partition — never overlapping coverage.** Every
  file is indexed by exactly one root, preserved by three registration
  rules: re-adding an existing root is an idempotent no-op (at most a
  freshness sweep); adding a path already inside a root's coverage
  creates an **alias** (no new index — resolution already routes there);
  adding a **parent** of existing roots absorbs them — the children's
  segments are adopted as-is via a path-prefix stamp (segments are
  relocatable: each carries its own file table of root-relative paths
  plus a prefix field), their inotify watches transfer, only the
  genuinely uncovered remainder is indexed, and the retired child roots
  become aliases. Tokens issued against an absorbed root are answered
  with an explicit redirect ("absorbed into <root>; current version
  <root>:<v>"), never an error or a wrong answer.
- **Root-scoped tokens**: corpus versions, `--changed-since` cursors, and
  session ids are all namespaced `<root-id>:<value>`; the daemon rejects
  tokens presented against the wrong root rather than answering nonsense.
  Concurrent agents across repos hold one cursor/session per root and
  cannot interfere with each other; concurrent readers of the same root
  get lock-free immutable epoch snapshots.
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
- **Segments are relocatable**: each segment stores a segment-local file
  table (root-relative paths) and a mount-prefix field, so a segment can
  be adopted under a new root by stamping a prefix — no rebuild. This is
  what makes parent-root absorption free, and it simplifies background
  merges and quarantine as a side effect. Required from the first format
  version (cheap now, painful to retrofit).
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

### Freshness and the change journal

**Freshness is pull-based by default; watchers are an optimization, not
architecture.** Every root runs one of three policies (auto-selected,
overridable at registration):

- **`sweep` (default)**: before the index is used, a parallel stat sweep
  validates the known file list against the index manifest (size+mtime;
  directory mtimes catch adds/removes). Divergent and new files join the
  dirty set that is already appended to every candidate response — the
  never-lie invariant holds with zero watchers. Cost ~1-2µs/file warm:
  ~2ms for the books corpus, ~25ms for /usr/include; only monorepo scale
  (100K+ files) makes sweeps expensive. The daemon caches sweep results
  briefly (~2s) so query bursts pay one sweep.
- **`watch`**: inotify replaces the sweep — enabled only when the root's
  directory count fits the remaining `max_user_watches` budget (watches
  are per-directory, ~1KB kernel memory each; counted at index time).
  Overflow or budget pressure degrades the root to `sweep`, never to
  staleness.
- **`frozen`**: declared-static corpora (e.g. the books mirror) skip
  both; the index is trusted until `gogrep index --refresh`. Explicit at
  registration and surfaced in `stat` output — staleness as a visible
  choice, never a surprise.

**The journal is source-agnostic**: sweep diffs and watch events feed the
same (version, path, kind) log; every applied change increments the
root's **corpus version**. `--changed-since VERSION` therefore works
identically under all three policies (frozen roots only move on manual
refresh — the honest semantic). Version tokens ride along on every
response, so agents hold them for free.

A consequence worth stating: **Phase A plus the sweep policy is a
complete, correct, daemonless product** (`--use-index` per invocation,
manual `gogrep index` runs, no watchers anywhere). The daemon adds
cross-query sweep caching, sessions, the vocabulary service, the watch
upgrade, and MCP — value, not correctness.

### Protocol

Newline-delimited JSON over the unix socket (matches the JSON-Lines
output ethos; trivially debuggable with `nc`):

- `candidates {plan} → {files, alsoScan, version, coverage}`
- `changed {since} → {paths, version}`
- `vocab {fragment, limit} → {terms: [{t, files, lines}]}`
- `session.open {plan} / session.next {id, cursor} / session.refine {id, plan}`
- `resolve {path} → {root, version} | {uncovered}`
- `roots.add {path} → {root} | {alias-of} | {absorbed: [children]}`
- `roots.list {} → [{root, version, aliases, state}]`
- `stat {root?} → {version, files, segments, dirty, mem}`

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
`--use-index` consumes it in-process with sweep-validated freshness
(and `--frozen` to skip sweeps for static corpora). This is already a
complete correct product. Differential tests (indexed vs cold byte-equal
output over randomized corpora, including mid-test mutations caught by
the sweep; property test: candidates ⊇ files with matches) de-risk
everything before any long-running process exists.

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
3. **Multi-root queries** (paths spanning roots, or partially covered):
   v1 requires a single covering root, else cold scan (settled — see the
   one-daemon/many-roots section; correctness by retreat).
4. **Vocabulary tokenization**: identifier-aware splitting (camelCase,
   snake_case) on by default (leaning yes — it made --suggest useful).

## Non-goals (settled in prior discussions)

- No FTS/SQLite/vector store as truth or matcher — files remain the truth,
  the daemon only accelerates reaching them (see agent-mode.md and the
  grep-a-SQLite experiment).
- No daemon dependency for any correctness property: everything the
  scanner did yesterday, it does identically with the daemon gone.
