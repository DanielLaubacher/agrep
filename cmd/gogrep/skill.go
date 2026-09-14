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
   matching line — most occurrences, prefers lines with context around
   the match). Costs a few hundred tokens regardless of corpus size.

2. Expand — probe a concept as several lexical variants in one pass:
     printf 'retry\nbackoff\nexponential delay\n' > /tmp/probes
     gogrep --batch /tmp/probes --json -r CORPUS/
   N patterns cost one walk and one read per file, not N. Matches carry
   "query"; the summary reports per-query totals, and a query with zero
   hits still appears there — explicit absence, not omission.

3. Narrow — read matches with context, under a budget:
     gogrep -rn --sections --max-tokens 2000 'jittered backoff' CORPUS/BOOK/
   --sections adds the enclosing heading (Markdown corpora only; no-op
   elsewhere). --max-tokens caps output; the summary reports exactly
   what was omitted (totals are always true — the search itself never
   truncates).

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

## Repeated queries: --use-index

Add --use-index to any recursive search over a tree you will query
more than once:
     gogrep --use-index -rn 'pattern' ROOT/
First use builds a trigram index (roughly one cold scan, stored under
$XDG_CACHE_HOME/gogrep/); later queries prune to candidate files.
Freshness is automatic — every query stat-sweeps and incrementally
reindexes (only changed files are re-read), so results are always
identical to a cold scan; never stale, never a missed match. Pays off
from the second query on; not helpful for one-shot searches or single
files. Use --clear-index PATH to delete index state under PATH.

## JSON contract (--json)

JSON-Lines, one object per line; "type" discriminates:
  match    {type,file,line_number,byte_offset,text,matches,
            span,region,section?,query?}
  count    {type,file,count,query?}            with -c
  file     {type,file,query?}                  with -l
  outline  {type,file,count,exemplar}
  suggest  {type,variant,kind,lines,files}     zero counts included
  suggest_summary  {type,patterns,tried,found} always ends --suggest
  summary  exact totals; every --json run ends with one:
           {files,lines} plain; {files} after -l; {files,lines,shown}
           after --outline; adds queries:[{query,files,lines}] after
           --batch; {shown_lines,shown_files,omitted_lines,
           omitted_files} after --max-tokens
Line numbers are always real (no flag needed).

## Flag quick reference

-r recurse  -n line numbers  -i ignore case  -l files only  -c counts
-F fixed string  -e PAT (repeatable, OR)  -C N context  -v invert
-g GLOB include/exclude ('!x' excludes)  --hidden  --no-ignore

Multi-stage narrowing: -t pipes each stage's matched text into the
next pattern; -o emits only the matched text. Example over a log line
"ERROR conn reset timeout=350ms":
     gogrep -e 'ERROR' -te 'timeout=\d+' -toe '\d+' app.log
   stage 1 keeps lines containing ERROR
   stage 2 narrows to their "timeout=350" fragments
   stage 3 emits just "350"

## Rules of thumb

- Prefer --outline before reading matches; prefer -l/-c when you only
  need locations or magnitudes.
- Always pass --json when a program (you) consumes the output.
- Budget with --max-tokens instead of head/tail — the summary keeps
  totals exact so you never mistake truncation for absence.
- A --suggest run always reports what it tried; zero variants
  occurring is itself a finding — stop probing that vocabulary.
- Cite only via region ids you have verified with --get-region.
`
