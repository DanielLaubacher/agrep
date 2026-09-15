package main

// skillText is printed by --skill: operating instructions for an AI
// agent using agrep as a sensing API. --help documents flags; this
// documents workflow. Keep it terse — it lands in an agent's context.
// cmd/agrep/skill_test.go guards it against drifting from the JSON
// types the code actually emits.
const skillText = `# agrep — agent skill

High-performance text search purpose-built for AI agents. Search is a
sensing API: iterate cheap queries, budget your context, cite what you
find, verify citations by re-fetching exact bytes.

## Exit codes
0 = match found, 1 = no match, 2 = error. A zero-hit search is a
normal outcome, not a failure — pair it with --suggest.

## Core workflow (corpus question-answering)

1. Survey — who talks about the concept:
     agrep --outline --top 10 -r 'backoff' CORPUS/
   One row per file: count, path, exemplar (the file's most informative
   matching line; definition lines win). --rank density orders by
   matches/KB and demotes vendored/generated files; --rank defs puts
   files with definition-shaped matches first and sinks tests — the
   right mode for "where is X defined". Both emit their signal
   (score / defs) in every row so the order is explicable.

2. Expand — probe a concept as several lexical variants in one pass:
     printf 'retry\nbackoff\nexponential delay\n' > /tmp/probes
     agrep --batch /tmp/probes --json -r CORPUS/
   N patterns cost one walk, not N. Matches carry "query"; the summary
   reports per-query totals, zero-hit queries listed explicitly.

3. Narrow — read matches with context, under a budget:
     agrep -rn --scope --max-tokens 2000 'jittered backoff' CORPUS/
   --scope names the enclosing function/class (Markdown: the heading;
   --sections is the Markdown-only variant; both add "page" from
   <!-- p.N --> markers in PDF-extracted text). --max-tokens is a hard
   cap enforced per record — it cuts inside a file, overshooting by at
   most one record — and the summary reports exactly what was omitted
   (totals are always true: the search itself never truncates). Output
   order is deterministic (sorted walk), so a re-run costs the same
   tokens. Add --collapse to stop generated/lock files repeating one
   line hundreds of times: repeats past 3 are tallied; the summary
   "lines" stays the true total with "shown_lines" alongside.

4. Zero hits — let the tool propose the next query:
     agrep -r --suggest 'ConnectTimeout' CORPUS/
   Probes the case-insensitive form and identifier fragments, reports
   which occur and how often (rarest first). Never silent: "no variant
   occurs" and "no derivable variants" are reported as findings.
   Works with multiple -e patterns; ignored (with a warning) on stdin.

5. Cite and verify — ground every claim:
   --json matches carry "span" [start,end) byte range and "region"
   ("path@start-end"). Quote using the region id, and re-fetch to
   verify before asserting:
     agrep --get-region 'CORPUS/book.md@3120-3245'
   Line form 'file@:120-160' (1-based, inclusive) speaks the dialect
   of compilers and stack traces; --expand N adds N whole lines of
   context around either form. Multiline (-U) spans cite the same way.
   Named forms return the whole unit you would otherwise read by
   guessed line range — usually the cheapest follow-up to a match:
     agrep --get-region 'src/config.go@func:parseConfig'
     agrep --get-region 'book.md@section:Gob'
   func: finds the definition (word-bounded, per language family) and
   prints its whole block; section: the Markdown section (name matched
   case-insensitively). Ambiguity is reported on stderr with the other
   candidates' line numbers — append #N to pick the Nth candidate
   ('@section:Discussion#3'), or qualify with a parent heading
   ('@section:Recipe 8/Discussion'). Add --json to get one
   {type:"region"} record (file, line_number, span, region, section,
   page, text) — verify and cite in a single parseable step.
   "span" and "region" always cover the FULL line regardless of any
   display truncation, so region bytes match the "text" field exactly.
   A region past EOF (a stale or fabricated citation) fails with exit
   2 — it never "verifies" as silently empty.

## Precision queries

- Whole words: -w wraps each pattern as \b(?:PATTERN)\b. A word search
  for a literal (-w 'error', or '\berror\b' written out) runs on the
  SIMD literal path and costs the same as the bare literal; -w composes
  with -F and -i.
- Identifiers: --ident treats the pattern as an identifier name —
  matches camelCase, snake_case, kebab-case, SCREAMING_SNAKE, flat,
  word-bounded (searching Match will not hit MatchSet):
     agrep -rn --ident 'connectTimeout' src/
- Structural templates: --structural makes the pattern a template
  with :[name] holes that match lazily within balanced delimiters,
  across lines, skipping strings/comments (--lang go|py|js|c|rs|sh|rb|md).
  Without --lang, a single recognizable file argument auto-detects its
  family; anything less certain (a directory, mixed files) warns and
  falls back to generic (delimiters only — a string/comment containing
  an unbalanced bracket can then misparse real code, so pass --lang
  for a directory of real source). Whitespace matches any run.
     agrep -rn --structural 'NewClient(:[args])' --lang go src/
  JSON matches carry "captures":{"args":"..."} — the hole bindings.
  Template text inside strings/comments never matches, and a template
  starting with an identifier is word-bounded ('Client(' does not
  match 'NewClient('). The definition itself matches too but carries
  "kind":"definition" — filter on it to count only call sites. One
  probe answers "call sites and what gets passed", multi-line calls
  included.
- Enumerate values: --histogram counts distinct matched texts
  (built-in sort|uniq -c), composing with -o pipelines:
     agrep -r --histogram -oe 'ERR_[A-Z_]+' src/
  With --structural, aggregate a hole instead of the whole match:
     agrep -r --structural 'logWarn(:[args])' --lang go \
           --histogram --capture args src/
- Whole blocks: --block emits each match as its enclosing definition
  (function/class body; Markdown: the whole section). Matches in one
  block dedupe to a single emission; span/region cover exactly the
  emitted bytes; oversize blocks are cut at 32KB with "truncated":true.
     agrep -rn --block 'retryPolicy' src/
  JSON block records carry "scope" (the definition line / heading)
  so a block is self-describing without a second call.
- Cross-line shapes: -U lets the pattern match across lines; output
  and span cover the whole block; ^ $ anchor per line. Regex only.

## Case and display

- Case: -i forces case-insensitive (full Unicode folding); -S
  smart-case (insensitive only when the pattern is all-lowercase);
  -s forces case-sensitive and overrides earlier -i/-S — including
  ones injected by a ~/.agrep config file. If counts differ from
  grep, check for -S in the config file.
- -M N truncates displayed lines to N bytes (0 = 75-byte default,
  -1 = never), snapping to word boundaries; a "..." marks each cut
  edge, so a truncated line is never mistaken for a genuinely short
  one. In JSON, an explicit -M N windows only "text" (with
  "truncated":true) — "span", "region", and all totals stay
  line-accurate, so survey with '-M 150 --compact' and cite from the
  region. Without -M, JSON text is always the full line.

## Scoping the corpus

- --changed-since REF — only files changed since the git ref (plus
  untracked). The right default while iterating on a branch.
- --files-from - — search files listed on stdin: one query's -l
  output feeds the next (agrep -rl A . | agrep --files-from - B).
- --with-file P / --without-file P — file-level conditions: report
  files matching the pattern that also/never contain P ("call sites
  not yet migrated"). Suppressed-file counts go to stderr, never lost.
- -g GLOB include/exclude ('!x' excludes), --hidden, --no-ignore. A
  glob without '/' matches the base name at any depth ('*.py' finds
  every Python file in the tree); with '/' it matches the path as
  printed, '**' spanning directories ('src/**/*_test.go'). Include
  globs never prune directories, only exclusions do ('!vendor').

## Repeated queries: --use-index

Skip it unless a cold scan takes seconds: on a warm cache agrep
scans ~10GB/s, and the index build costs far more than it saves on
small trees. It pays off only when the tree is BIG or on slow/cold
storage, mostly static, and the queries are rare literals (common
words prune nothing; regexes without a strong literal gain nothing).
     agrep --use-index -rn 'pattern' ROOT/
First use builds a trigram index (stored under
$XDG_CACHE_HOME/agrep/); later queries prune to candidate files.
Freshness is automatic — every query stat-sweeps and incrementally
reindexes (results always identical to a cold scan) — but any change
to the tree makes the next query pay a reindex, so never use it on a
tree you are editing. --clear-index PATH deletes index state.

## JSON contract (--json)

JSON-Lines, one object per line; "type" discriminates:
  match    {type,file,line_number,byte_offset,text,matches,
            span,region,section?,scope?,page?,kind?,captures?,
            truncated?,query?}  kind:"definition" on def-shaped lines
  region   {type,file,line_number,span,region,section?,page?,text}
           from --get-region --json
  context  {type,file,line_number,byte_offset,text,span,region}
           context lines around a match (-C/-A/-B)
  count    {type,file,count,query?}            with -c
  file     {type,file,query?}                  with -l
  outline  {type,file,count,exemplar,score?}  score with --rank density
  variant  {type,text,count,files}             with --histogram
  suggest  {type,variant,kind,lines,files}     zero counts included
  suggest_summary  {type,patterns,tried,found} always ends --suggest
  collapsed {type,lines,texts}                 what --collapse hid
  error    {type,file,error}                   unreadable files
  summary  exact totals; every --json run ends with one:
           {files,lines,errors} plain; {files,errors} after -l;
           {files,lines,shown} after --outline;
           {distinct,total,shown} after --histogram;
           adds queries:[{query,files,lines}] after --batch;
           adds shown_lines after --collapse (lines stays the total);
           {shown_lines,shown_files,omitted_lines,omitted_files,
            queries?} after --max-tokens (true totals, never just
            what was shown)
Line numbers are always real (no flag needed). In -U mode "text" is
the whole matched block and line_number is its first line.
A walk error (missing root, unreadable dir) emits an error record,
counts in "errors", and exits 2 — absence claims need errors == 0.

## Flag quick reference

-r recurse  -n line numbers  -i ignore case  -l files only  -c counts
-s case-sensitive  -S smart-case  -M N display truncation (-1 = off)
-F fixed string  -e PAT (repeatable, OR)  -C N context  -v invert
-U multiline  --ident  --scope  --histogram  --collapse
-g GLOB ('!x' excludes)  --hidden  --no-ignore  --changed-since REF

Multi-stage narrowing: -t pipes each stage's matched text into the
next pattern; -o emits only the matched text. Example over a log line
"ERROR conn reset timeout=350ms":
     agrep -e 'ERROR' -te 'timeout=\d+' -toe '\d+' app.log
   stage 1 keeps lines containing ERROR
   stage 2 narrows to their "timeout=350" fragments
   stage 3 emits just "350"

## Rules of thumb

- Prefer --outline before reading matches; prefer -l/-c when you only
  need locations or magnitudes; prefer --histogram when the question
  is "what values exist" rather than "where".
- Searching for an identifier? Reach for --ident before crafting
  case-variant regexes by hand. Asking about call sites or arguments?
  Reach for --structural before regex gymnastics.
- Need the surrounding function, not the line? --block beats
  iterating --get-region --expand.
- Iterating on a branch? --changed-since HEAD (or main) scopes every
  query to the diff surface.
- Always pass --json when a program (you) consumes the output; add
  --compact when reading many matches — it keeps file, line, region,
  text (everything needed to read and cite) and drops span/offsets/
  positions, about a third cheaper per record.
- Budget with --max-tokens instead of head/tail — the summary keeps
  totals exact so you never mistake truncation for absence. "errors"
  in the summary is corpus you could not see — treat nonzero as a
  caveat on any "absent" claim.
- A --suggest run always reports what it tried; zero variants
  occurring is itself a finding — stop probing that vocabulary.
- Patterns without a strong literal ('\bgo\s+func', bare -U shapes)
  cost seconds of CPU per query on a large tree — ~70x the literal
  path. Prefer a literal prefilter stage (-Fe 'func main' -te '...'),
  --structural, or --use-index only when the tree is huge and static.
- Cite only via region ids you have verified with --get-region.
- -P (PCRE) is excluded from the default build; the stock engine is
  RE2-class (no lookbehind). Use bin/agrep-pcre when you need it.
`
