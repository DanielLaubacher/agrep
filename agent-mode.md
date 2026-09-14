# Agent-First gogrep: Plan

gogrep's next consumer is not a human at a terminal but an AI agent using
search as a sensing API inside a loop. Agents differ from humans in three
ways that drive every feature below: their context window is a scarce
budget, they iterate queries rather than page through results, and they
need machine-verifiable grounding (citations they can re-fetch), not
scannable prose.

This plan covers the **stateless** features — everything that improves a
single invocation with no index or daemon. The stateful layer (trigram
index daemon, sessions, `--changed-since`) is a separate later phase; see
"Deferred" below.

## Phase 1 (implemented)

### 1. Token budget — `--max-tokens N`
Cap emitted output at ~N tokens (bytes/4 heuristic). Matches beyond the
budget are counted, not printed, and a final summary reports what was
omitted (`omitted: 7802 matching lines in 275 files`). JSON mode emits a
`{"type":"summary", ...}` trailer with full totals. An agent always
learns the true size of the result set, but never floods its context.

### 2. Survey mode — `--outline [--top K]`
Per-file aggregation instead of match lines: count of matching lines plus
the first matching line as an exemplar, sorted by count descending,
optionally limited to the top K files. One line per file answers "who
talks about X" across a corpus for a few hundred output tokens. JSON mode
emits one object per file plus a summary trailer.

### 3. Section context — `--sections`
Every match is annotated with the nearest preceding Markdown heading —
the section it lives in. Text mode prints a `§ heading` group line when
the section changes; JSON mode adds a `"section"` field. Designed for
prose/book corpora where "which section is this from" is the context an
agent actually needs (a `#`-heading scan bounded to 64KB backward; cheap,
and only for printed matches).

### 4. Batch multi-query — `--batch FILE`
One pattern per line (blank lines and `#` comments ignored); every file
is read once and searched by all queries; each result is attributed to
its query (`"query"` field in JSON, `[pattern]` prefix in text). This is
the execution half of agent-side semantic search: the agent expands a
concept into N lexical probes and pays one walk + one read per file for
all of them.

### 5. Zero-hit guidance — `--suggest`
When a search finds nothing, derive variants automatically — the
case-insensitive form, and the word fragments of a split identifier
(`ConnectTimeout` → `connect`, `timeout`) — probe each, and report which
occur and how often. Turns the least informative outcome (empty output)
into the agent's next query.

### 6. Verifiable regions — `--get-region PATH@START-END` + JSON spans
JSON matches now carry `"span": [start, end)` (absolute byte range of the
line) and a `"region"` id (`path@start-end`). `--get-region` fetches the
exact bytes for a region id — an agent can cite a span in an answer and
later re-fetch or verify it without re-reading the file. Region ids are
self-contained: no index or daemon required.

## Deferred (daemon phase / later)

See **indexing-daemon.md** for the full design of this phase.

- **Trigram index daemon** (`gogrep serve`) with inotify change journal:
  millisecond repeated queries, `--changed-since`, sessions/cursors, and
  a vocabulary that makes `--suggest` free instead of a rescan.
- **Near-duplicate collapsing**: valuable for vendored/generated code,
  not for the book corpus driving this phase.
- **Deadline + coverage accounting** (`--deadline`): matters at network
  filesystem scale; local corpora finish in well under a second.
- **Full relevance ranking**: `--outline` count-ranking covers the survey
  case; rarity/path/recency ranking needs corpus statistics the daemon
  will own.

## Design rules

- Every feature is stateless and works on a cold tree — no setup step.
- Human output remains grep-compatible unless an agent flag is passed.
- JSON stays JSON-Lines: one object per line, `type` field discriminates
  (`match`, `outline`, `summary`, `suggest`).
- Budgets bound *output*, never *search*: totals reported are exact.
