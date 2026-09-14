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
an exemplar line, sorted by count descending, optionally limited to the
top K files. The exemplar is the file's most informative matching line,
not merely its first — the line with the most occurrences wins, then a
line carrying text beyond the match itself, then the earliest — because
the literal first match in code is usually boilerplate (`package foo`).
One line per file answers "who talks about X" across a corpus for a few
hundred output tokens. JSON mode emits one object per file (`"exemplar"`
field) plus a summary trailer.

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
into the agent's next query. The report is never silent: JSON lists every
probe (zero counts included) and ends with a `suggest_summary` object;
text always states an outcome, including "no derivable variants" and
"none of the derived variants occur". Works with multiple `-e` patterns
(variants are merged and deduplicated); stdin has no corpus to probe, so
`--suggest` warns and is ignored there.

### 6. Verifiable regions — `--get-region PATH@START-END` + JSON spans
JSON matches now carry `"span": [start, end)` (absolute byte range of the
line) and a `"region"` id (`path@start-end`). `--get-region` fetches the
exact bytes for a region id — an agent can cite a span in an answer and
later re-fetch or verify it without re-reading the file. Region ids are
self-contained: no index or daemon required.

## Phase 2 (implemented) — precision, scoping, honesty

### 7. Identifier matching — `--ident`
The pattern is an identifier name; matches every case convention
(camelCase, PascalCase, snake_case, kebab-case, SCREAMING_SNAKE, flat),
word-bounded — `Match` does not hit `MatchSet`. A rewrite to
`(?i)\bconnect[_-]?timeout\b` routed through the normal engine factory.
The proactive twin of `--suggest`.

### 8. Enclosing scope — `--scope`
`--sections` generalized to code: a bounded backward sticky-scope scan
returns the enclosing definition line (per-language predicates chosen by
extension; Markdown falls back to headings). JSON `"scope"` field, text
`§` group lines.

### 9. Value enumeration — `--histogram`
Distinct matched texts with occurrence/file counts, most frequent first
(`sort | uniq -c` built in). Composes with `-o` pipelines; `--top K`.

### 10. Corpus scoping — `--changed-since REF`, `--files-from`, file conditions
`--changed-since` restricts any mode to the git diff surface (plus
untracked); `--files-from -` feeds one query's `-l` output into the
next; `--with-file`/`--without-file` express "files matching A but
also/never B" as a Matcher wrapper (suppressed counts reported). All
flow through one fileSource helper.

### 11. Citation ergonomics — line regions + `--expand`
`--get-region` accepts `path@:120-160` line form; `--expand N` widens
any region by whole lines. The cite loop also serves "show me the
neighborhood".

### 12. Output honesty — error objects, `--collapse`, `--rank density`
Unreadable files emit `{"type":"error"}` in-stream and every summary
counts `"errors"` (the recursive path previously dropped them
silently). `--collapse` suppresses repeats of an identical line past 3,
reporting exactly what was hidden. `--outline --rank density` orders by
matches/KB and demotes vendored/generated files.

### 13. Multiline — `-U`
Patterns match across lines (RE2 whole-buffer; extraction spans the
block, `(?m)` anchors per line). Spans/regions cover the block, so
multiline citations verify like any other. Unsupported combos (`-P`,
`-v`, `-t`, `--watch`) rejected at validation.

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
  (`match`, `count`, `file`, `outline`, `variant`, `summary`, `suggest`,
  `suggest_summary`, `collapsed`, `error`).
- Every `--json` run ends with a `summary` trailer carrying exact totals
  (`-c` emits `count` objects, `-l` emits `file` objects — never
  degenerate match objects). `--batch` summaries carry per-query totals,
  with zero-hit queries listed explicitly.
- Budgets bound *output*, never *search*: totals reported are exact.
