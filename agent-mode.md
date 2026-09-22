# Agent-First agrep: Plan

agrep's next consumer is not a human at a terminal but an AI agent using
search as a sensing API inside a loop. Agents differ from humans in three
ways that drive every feature below: their context window is a scarce
budget, they iterate queries rather than page through results, and they
need machine-verifiable grounding (citations they can re-fetch), not
scannable prose.

This plan covers the per-invocation features. The trigram index
(`--use-index`, see index.md) shipped separately — daemonless, with
per-query freshness sweeps.

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

### 3. Batch multi-query — `--batch FILE`
One pattern per line (blank lines and `#` comments ignored); every file
is read once and searched by all queries; each result is attributed to
its query (`"query"` field in JSON, `[pattern]` prefix in text). This is
the execution half of agent-side semantic search: the agent expands a
concept into N lexical probes and pays one walk + one read per file for
all of them.

### 4. Zero-hit guidance — `--suggest`
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

### 5. Verifiable regions — `--get-region PATH@START-END` + JSON spans
JSON matches now carry `"span": [start, end)` (absolute byte range of the
line) and a `"region"` id (`path@start-end`). `--get-region` fetches the
exact bytes for a region id — an agent can cite a span in an answer and
later re-fetch or verify it without re-reading the file. Region ids are
self-contained: no index required.

## Phase 2 (implemented) — precision, scoping, honesty

### 6. Identifier matching — `--ident`
The pattern is an identifier name; matches every case convention
(camelCase, PascalCase, snake_case, kebab-case, SCREAMING_SNAKE, flat),
word-bounded — `Match` does not hit `MatchSet`. A rewrite to
`(?i)\bconnect[_-]?timeout\b` routed through the normal engine factory.
The proactive twin of `--suggest`.

### 7. Enclosing scope — `--scope`
Every match is annotated with its enclosing context: a bounded backward
sticky-scope scan returns the enclosing definition line (per-language
predicates chosen by extension; Markdown falls back to the nearest
preceding heading, bounded to 64KB backward — the section it lives in,
which is the context prose/book corpora actually need). JSON `"scope"`
field, text `§` group lines. (An earlier release shipped this as two
flags, `--sections` for Markdown headings and `--scope` for code; since
`--scope` already auto-detected Markdown and produced identical output,
`--sections` was folded in and removed rather than kept as a redundant,
less-safe alias — unlike `--scope`, it never checked file type, so
pointed at code it could misread a `#`-comment line as a heading.)

### 8. Value enumeration — `--histogram`
Distinct matched texts with occurrence/file counts, most frequent first
(`sort | uniq -c` built in). Composes with `-o` pipelines; `--top K`.

### 9. Corpus scoping — `--changed-since REF`, `--files-from`, file conditions
`--changed-since` restricts any mode to the git diff surface (plus
untracked); `--files-from -` feeds one query's `-l` output into the
next; `--with-file`/`--without-file` express "files matching A but
also/never B" as a Matcher wrapper (suppressed counts reported). All
flow through one fileSource helper.

### 10. Citation ergonomics — line regions + `--expand`
`--get-region` accepts `path@:120-160` line form; `--expand N` widens
any region by whole lines. The cite loop also serves "show me the
neighborhood".

### 11. Output honesty — error objects, `--collapse`, `--rank density`
Unreadable files emit `{"type":"error"}` in-stream and every summary
counts `"errors"` (the recursive path previously dropped them
silently). `--collapse` suppresses repeats of an identical line past 3,
reporting exactly what was hidden. `--outline --rank density` orders by
matches/KB and demotes vendored/generated files.

### 12. Multiline — `-U`
Patterns match across lines (RE2 whole-buffer; extraction spans the
block, `(?m)` anchors per line). Spans/regions cover the block, so
multiline citations verify like any other. Unsupported combos (`-P`,
`-v`, `-t`, `--watch`) rejected at validation.

## Phase 3 (implemented) — structural sensing (comby lineage, not AST lineage)

Analysis of semgrep/ast-grep/comby (TODO.md) settled the direction:
adopt comby's insight — structural matching needs only balanced
delimiters, strings, and comments, not parsers — and reject the
tree-sitter/AST path (per-language grammar treadmill, violates pure-Go,
duplicates agents' LSP tools). Three features, sharing one small
language-family table (`internal/lang`):

### 13. Structural holes — `-S 'foo(:[args])'`
The pattern is a template: literal text plus `:[name]` holes. A hole
matches lazily across lines within balanced delimiters, skipping string
and comment contents (per `--lang`, default `generic` = delimiters
only). Whitespace in the pattern matches any whitespace run. One probe
answers "call sites of foo and what gets passed" — multi-line calls
included — which previously took several regex round-trips. Matches are
block-spanning (like `-U`) with working span/region citations.

### 14. Capture bindings — `"captures"` + `--capture`
Every hole's text is captured and emitted in JSON
(`"captures":{"args":"ctx, retry"}`), and `--histogram --capture args`
aggregates a hole's values across the corpus: "histogram of the first
argument to NewClient(...)" is one command returning tens of tokens
instead of hundreds of match lines to tabulate by hand.

### 15. Whole-block output — `--block`
Each match is emitted as its whole enclosing definition block (backward
scope scan to the definition line, forward balance/indent/heading scan
to its end; Markdown blocks run heading→next heading). Replaces the
agent's `--get-region --expand` guess-loop with one exact fetch; the
span/region covers exactly the emitted block, and multiple matches in
one block dedupe to a single emission. Capped at 32KB with an explicit
truncation flag.

## Deferred (later, if ever)

Phase 2 absorbed most of the old deferred list (`--changed-since` is
git-based, near-duplicate collapsing is `--collapse`, basic ranking is
`--rank density`). Still open:

- **Vocabulary-backed `--suggest`**: variants ranked by true corpus
  rarity from a token table built during indexing, plus prefix/fragment
  lookups the rescan approach can't afford.
- **Deadline + coverage accounting** (`--deadline`): matters at network
  filesystem scale; local corpora finish in well under a second.
- **Statistical relevance ranking**: `--rank density` covers the common
  case; IDF/recency ranking needs corpus statistics the index could
  provide.

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
