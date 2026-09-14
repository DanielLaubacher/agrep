package main

// skillText is printed by --skill: operating instructions for an AI
// agent using gogrep as a sensing API. --help documents flags; this
// documents workflow. Keep it terse — it lands in an agent's context.
// cmd/gogrep/skill_test.go guards it against drifting from the JSON
// types the code actually emits.
const skillText = `# gogrep — agent skill

High-performance text search purpose-built for AI agents. Search is a
sensing API: iterate cheap queries, budget your context, cite what you
find, verify citations by re-fetching exact bytes.

## Exit codes
0 = match found, 1 = no match, 2 = error. A zero-hit search is a
normal outcome, not a failure — pair it with --suggest.

## Core workflow (corpus question-answering)

1. Survey — who talks about the concept:
     gogrep --outline --top 10 -r 'backoff' CORPUS/
   One row per file: count, path, exemplar (the file's most informative
   matching line). --rank density orders by matches/KB and demotes
   vendored/generated files — better than raw counts in big trees.

2. Expand — probe a concept as several lexical variants in one pass:
     printf 'retry\nbackoff\nexponential delay\n' > /tmp/probes
     gogrep --batch /tmp/probes --json -r CORPUS/
   N patterns cost one walk, not N. Matches carry "query"; the summary
   reports per-query totals, zero-hit queries listed explicitly.

3. Narrow — read matches with context, under a budget:
     gogrep -rn --scope --max-tokens 2000 'jittered backoff' CORPUS/
   --scope names the enclosing function/class (Markdown: the heading;
   --sections is the Markdown-only variant). --max-tokens caps output;
   the summary reports exactly what was omitted (totals are always
   true — the search itself never truncates). Add --collapse to stop
   generated/lock files repeating one line hundreds of times: repeats
   past 3 are tallied, and shown + collapsed = true total.

4. Zero hits — let the tool propose the next query:
     gogrep -r --suggest 'ConnectTimeout' CORPUS/
   Probes the case-insensitive form and identifier fragments, reports
   which occur and how often (rarest first). Never silent: "no variant
   occurs" and "no derivable variants" are reported as findings.
   Works with multiple -e patterns; ignored (with a warning) on stdin.

5. Cite and verify — ground every claim:
   --json matches carry "span" [start,end) byte range and "region"
   ("path@start-end"). Quote using the region id, and re-fetch to
   verify before asserting:
     gogrep --get-region 'CORPUS/book.md@3120-3245'
   Line form 'file@:120-160' (1-based, inclusive) speaks the dialect
   of compilers and stack traces; --expand N adds N whole lines of
   context around either form. Multiline (-U) spans cite the same way.

## Precision queries

- Identifiers: --ident treats the pattern as an identifier name —
  matches camelCase, snake_case, kebab-case, SCREAMING_SNAKE, flat,
  word-bounded (searching Match will not hit MatchSet):
     gogrep -rn --ident 'connectTimeout' src/
- Enumerate values: --histogram counts distinct matched texts
  (built-in sort|uniq -c), composing with -o pipelines:
     gogrep -r --histogram -oe 'ERR_[A-Z_]+' src/
- Cross-line shapes: -U lets the pattern match across lines; output
  and span cover the whole block; ^ $ anchor per line. Regex only.

## Scoping the corpus

- --changed-since REF — only files changed since the git ref (plus
  untracked). The right default while iterating on a branch.
- --files-from - — search files listed on stdin: one query's -l
  output feeds the next (gogrep -rl A . | gogrep --files-from - B).
- --with-file P / --without-file P — file-level conditions: report
  files matching the pattern that also/never contain P ("call sites
  not yet migrated"). Suppressed-file counts go to stderr, never lost.
- -g GLOB include/exclude ('!x' excludes), --hidden, --no-ignore.

## Repeated queries: --use-index

Add --use-index to any recursive search over a tree you will query
more than once:
     gogrep --use-index -rn 'pattern' ROOT/
First use builds a trigram index (roughly one cold scan, stored under
$XDG_CACHE_HOME/gogrep/); later queries prune to candidate files.
Freshness is automatic — every query stat-sweeps and incrementally
reindexes, so results are always identical to a cold scan; never
stale. Pays off from the second query; not for one-shot searches.
--clear-index PATH deletes index state under PATH.

## JSON contract (--json)

JSON-Lines, one object per line; "type" discriminates:
  match    {type,file,line_number,byte_offset,text,matches,
            span,region,section?,scope?,query?}
  count    {type,file,count,query?}            with -c
  file     {type,file,query?}                  with -l
  outline  {type,file,count,exemplar}
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
           {shown_lines,shown_files,omitted_lines,omitted_files}
           after --max-tokens
Line numbers are always real (no flag needed). In -U mode "text" is
the whole matched block and line_number is its first line.

## Flag quick reference

-r recurse  -n line numbers  -i ignore case  -l files only  -c counts
-F fixed string  -e PAT (repeatable, OR)  -C N context  -v invert
-U multiline  --ident  --scope  --histogram  --collapse
-g GLOB ('!x' excludes)  --hidden  --no-ignore  --changed-since REF

Multi-stage narrowing: -t pipes each stage's matched text into the
next pattern; -o emits only the matched text. Example over a log line
"ERROR conn reset timeout=350ms":
     gogrep -e 'ERROR' -te 'timeout=\d+' -toe '\d+' app.log
   stage 1 keeps lines containing ERROR
   stage 2 narrows to their "timeout=350" fragments
   stage 3 emits just "350"

## Rules of thumb

- Prefer --outline before reading matches; prefer -l/-c when you only
  need locations or magnitudes; prefer --histogram when the question
  is "what values exist" rather than "where".
- Searching for an identifier? Reach for --ident before crafting
  case-variant regexes by hand.
- Iterating on a branch? --changed-since HEAD (or main) scopes every
  query to the diff surface.
- Always pass --json when a program (you) consumes the output.
- Budget with --max-tokens instead of head/tail — the summary keeps
  totals exact so you never mistake truncation for absence. "errors"
  in the summary is corpus you could not see — treat nonzero as a
  caveat on any "absent" claim.
- A --suggest run always reports what it tried; zero variants
  occurring is itself a finding — stop probing that vocabulary.
- Cite only via region ids you have verified with --get-region.
`
