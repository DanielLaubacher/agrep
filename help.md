# agrep Usage

## Synopsis

```
agrep [OPTIONS] PATTERN [FILE...]
agrep [OPTIONS] -e PATTERN [-e PATTERN...] [FILE...]
```

If no files are given and stdin is a terminal, searches the current directory recursively. If stdin is piped, reads from stdin. Use `--` to separate flags from patterns that start with `-`.

Short flags can be combined: `-rin` is equivalent to `-r -i -n`.

## Options

### Pattern Selection

| Flag | Short | Description |
|---|---|---|
| `--regexp PATTERN` | `-e` | Pattern to match (repeatable; multiple = OR, or AND with `-t`) |
| `--fixed-strings` | `-F` | Treat next `-e` pattern as a literal string, not a regex |
| `--perl-regexp` | `-P` | Use PCRE2 for next `-e` pattern. **Not available in the default build** (`make build`) — `-P` errors with `PCRE support not compiled in`. Build `bin/agrep-pcre` with `make build-pcre` (`-tags pcre`) to use it; the default build skips PCRE to avoid a ~5ms/invocation startup tax from its C-library init. |
| `--pipe` | `-t` | Pipe: next `-e` filters lines matched by the previous `-e` |
| `--only-matching` | `-o` | Print only the matched part of the line for next `-e` |
| `--word-regexp` | `-w` | Match only whole words: `\b(?:PATTERN)\b` |
| `--ignore-case` | `-i` | Case-insensitive matching |
| `--smart-case` | `-S` | Case-insensitive if pattern is all lowercase |
| `--case-sensitive` | `-s` | Force case-sensitive, overriding earlier `-i`/`-S` — including flags injected by `AGREP_CONFIG_PATH`/`~/.agrep` |
| `--invert-match` | `-v` | Select lines that do NOT match |

**Per-stage flags**: `-F`, `-P`, `-t`, `-o` are per-stage modifiers that apply to the next `-e` and reset after it. They can be combined with short flag syntax: `-Ftoe 'pattern'` = fixed + pipe + only-matching. `-e` must be last in any combined group since it takes a value.

### Output Control

| Flag | Short | Description |
|---|---|---|
| `--line-number` | `-n` | Print line numbers |
| `--count` | `-c` | Print only a count of matching lines per file |
| `--files-with-matches` | `-l` | Print only filenames containing matches |
| `--color MODE` | | Color output: `auto` (default), `always`, `never` |
| `--colour MODE` | | Alias for `--color` |
| `--max-columns NUM` | `-M` | Truncate lines longer than NUM bytes (0=auto, -1=no limit); a "..." marks each cut edge |
| `--json` | | Output results as JSON Lines |
| `--compact` | | With `--json`: lean match records (file, line, region, text — no span/byte_offset/matches), cheaper for reading many matches |

### Context

| Flag | Short | Description |
|---|---|---|
| `--before-context NUM` | `-B` | Print NUM lines before each match |
| `--after-context NUM` | `-A` | Print NUM lines after each match |
| `--context NUM` | `-C` | Print NUM lines before and after each match |

### Search Modes

| Flag | Short | Description |
|---|---|---|
| `--recursive` | `-r` | Recursively search directories |
| `--glob PATTERN` | `-g` | Include/exclude files by glob (prefix `!` to exclude, repeatable) |
| `--no-ignore` | | Don't respect .gitignore files |
| `--hidden` | | Search hidden files and directories |
| `--follow` | `-L` | Follow symbolic links |
| `--watch` | | Watch files for changes and search new content |

### Multiline & Structural Matching

| Flag | Short | Description |
|---|---|---|
| `--multiline` | `-U` | Patterns may match across lines; output spans the whole matched block (regex only; `^`/`$` still anchor per line) |
| `--structural PATTERN` | | PATTERN is a structural template with `:[name]` holes, matched lazily within balanced delimiters |
| `--lang NAME` | | Language family for `--structural` string/comment handling: `go py js c rs sh rb md` (auto-detected from a single file argument; `generic` otherwise, with a warning) |
| `--capture NAME` | | With `--structural --histogram`: aggregate one hole's captured text instead of full matches |
| `--block` | | Emit each match's whole enclosing definition block (function/class body; Markdown section) instead of just the matching line |

## Exit Codes

| Code | Meaning |
|---|---|
| 0 | Match found |
| 1 | No match |
| 2 | Error |

## Examples

### Basic Search

Search for a pattern in a file:

```sh
agrep "error" app.log
```

Search stdin:

```sh
cat app.log | agrep "timeout"
```

### Case-Insensitive Search

```sh
agrep -i "warning" app.log
```

### Fixed String Search

Treat the pattern as a literal string (no regex metacharacters):

```sh
agrep -F "[ERROR]" app.log
```

### Line Numbers

```sh
agrep -n "TODO" src/*.go
```

### Recursive Search

Search all files in a directory tree:

```sh
agrep -rn "func main" ./src/
```

### Invert Match

Show lines that do NOT contain the pattern:

```sh
agrep -v "DEBUG" app.log
```

### Count Matches

```sh
agrep -c "error" *.log
```

### Files With Matches

List only filenames that contain a match:

```sh
agrep -rl "TODO" ./src/
```

### Context Lines

Show 2 lines before and after each match:

```sh
agrep -C2 "panic" app.log
```

Show 3 lines after each match:

```sh
agrep -A3 "FATAL" app.log
```

### Multiple Patterns

Search for any of several patterns:

```sh
agrep -e "error" -e "warning" -e "fatal" app.log
```

Multiple fixed strings (uses Aho-Corasick for single-pass matching):

```sh
agrep -F -e "connection refused" -e "timeout" -e "EOF" app.log
```

### PCRE2 Regex

Use Perl-compatible regex for lookahead, lookbehind, backreferences.
**Requires `make build-pcre`** (`bin/agrep-pcre`) — the default `bin/agrep`
build stubs `-P` out and errors with `PCRE support not compiled in`:

```sh
# Lookahead: words followed by "world"
agrep -P '\w+(?=\s+world)' file.txt

# Lookbehind: words preceded by "hello "
agrep -P '(?<=hello\s)\w+' file.txt

# Backreference: repeated words
agrep -Pn '(\w+)\s+\1' document.txt
```

### JSON Output

Output matches as JSON Lines (one JSON object per match):

```sh
agrep --json "error" app.log
```

```json
{"type":"match","file":"app.log","line_number":42,"byte_offset":1847,"text":"2024-01-15 ERROR: connection refused","matches":[{"start":11,"end":16}],"span":[1847,1883],"region":"app.log@1847-1883"}
{"type":"summary","files":1,"lines":1,"errors":0}
```

Every run ends with a `{"type":"summary",...}` trailer carrying exact
totals (`files`, `lines`, `errors`) — see [Agent options](#agent-options)
below. `span` is the line's absolute byte range in the file; `region` is
the self-contained `path@start-end` id `--get-region` re-fetches. Add
`--compact` to drop `span`/`byte_offset`/`matches` when you only need
`region` for later verification.

### Watch Mode

Watch files for changes and search new content as it's appended:

```sh
agrep --watch "ERROR" /var/log/syslog
```

Watch multiple files:

```sh
agrep --watch "panic" app.log worker.log
```

### Color Control

Force color output (useful when piping to `less -R`):

```sh
agrep --color=always "pattern" file.txt | less -R
```

Disable color:

```sh
agrep --color=never "pattern" file.txt
```

### Combined Flags

Recursive, case-insensitive, with line numbers and context:

```sh
agrep -rinC3 "fixme" ./src/
```

Count fixed-string matches per file recursively:

```sh
agrep -rFc "TODO" ./src/
```

### Only Matching (-o)

Print only the matched portion of each line (like `grep -o`):

```sh
agrep -oe '\d+' app.log
# 503
# 200
# 42
```

### Regex Pipeline (-t)

Chain patterns with `-t` to filter lines through multiple stages. Each stage must match for the line to appear. The final stage's matches define the output.

```sh
# SIMD fixed-string prefilter, then extract digits
agrep -Fe 'ERROR' -toe '\d+' app.log
# 503
# 200
```

This is equivalent to `grep 'ERROR' app.log | grep -o '\d+'` but runs in a single process, preserving file context (filename, line numbers).

Three-stage narrowing — each stage can use a different engine:

```sh
agrep -Fe 'HTTP' -te 'status=\d+' -toe 'status=\d+' access.log
# status=200
# status=503
```

Mix fixed-string SIMD stages with regex or PCRE:

```sh
agrep -Fe 'ERROR' -Fte 'prod-' -Fte 'timeout' -toe '\d+' app.log
```

Multiple OR branches, each with their own pipeline:

```sh
# Branch 1: ERROR lines → extract digits
# Branch 2: WARN lines (no pipeline)
agrep -Fe 'ERROR' -toe '\d+' -Fe 'WARN' app.log
```

`-e` without a preceding `-t` starts a new OR branch. `-e` with `-t` continues the current pipeline.

### Searching Binary Files

agrep automatically detects binary files (by checking for NUL bytes in the first 8 KB) and skips them entirely during a search — no summary line, no match reported, same as ripgrep's default. A binary file is never opened for anything other than that 8 KB probe:

```sh
agrep -r "magic" ./data/
# archive.bin is silently excluded even if it contains "magic";
# exit code is 1 (no match) if nothing else in the tree matches
```

There is currently no flag to force-search a file agrep has classified as binary; extract or convert its content first if you need to search it.

## Agent options

Designed for AI agents using agrep as a sensing API (see agent-mode.md):

| Flag | Description |
|---|---|
| `--ident` | Match the pattern as an identifier: every case convention (camelCase, snake_case, kebab-case, SCREAMING_CASE), word-bounded. |
| `--max-tokens N` | Budget output to ~N tokens (4 bytes/token heuristic). Search always completes; a trailer reports exactly what was omitted, and totals stay exact. |
| `--outline` | Per-file survey (count + one exemplar matching line), busiest files first. |
| `--top K` | Limit `--outline`/`--histogram` to the K busiest entries. |
| `--histogram` | Count distinct matched texts (`uniq -c` built in); composes with `-o` pipelines. |
| `--rank MODE` | `--outline` ordering: `count` (default), `density` (matches/KB, demotes vendored/generated files), or `defs` (definition lines first, demotes tests). |
| `--collapse` | Suppress repeats of an identical match line after the 3rd occurrence (stops a generated file from repeating one line hundreds of times); the summary reports exactly what was collapsed and totals stay true. |
| `--scope` | Annotate matches with their enclosing definition (func/class/def by language; falls back to the Markdown heading) — `§` group lines in text, `"scope"` field in JSON. |
| `--batch FILE` | Run every pattern in FILE (one per line, blank lines and `#`-prefixed comments skipped) in a single pass; each file is read once. Results carry their query (`[pattern]` prefix / `"query"` field), and zero-hit queries are still listed explicitly. |
| `--files-from FILE` | Search the files listed in FILE (`-` = stdin), one path per line, instead of walking — lets you compose `agrep -rl ... \| agrep --files-from - ...`. |
| `--changed-since REF` | Search only files changed since the git REF, plus untracked files (requires git). |
| `--with-file PAT` | Only report files that also contain PAT (repeatable). |
| `--without-file PAT` | Only report files that do not contain PAT (repeatable). |
| `--suggest` | On zero hits, probe derived variants of the pattern (case, identifier fragments) and report which actually occur — always reports something, even "none of the variants occur." |
| `--get-region SPAN` | Print exact bytes for `path@start-end`, whole lines for `path@:120-160` (1-based), a whole definition for `path@func:Name`, or a whole Markdown section for `path@section:Name` (append `#N` to pick the Nth candidate; `Parent/Child` matches nested headings). With `--json`, emits one `{"type":"region"}` record. |
| `--expand N` | Widen `--get-region` by N whole lines on each side. |
| `--use-index` | Build (first use) and use a trigram index for a recursive search over exactly one root path (not combined with `--structural` or `--files-from`, and not when multiple paths are given). Every query re-validates freshness with a parallel stat sweep and reindexes incrementally on any drift, so results stay current. When the pattern can't be literal-pruned (`-v`, a PCRE stage, or no literal ≥3 bytes), the index still saves the directory walk — every indexed file becomes a candidate and the real matcher verifies each one. A true fallback to a cold walk happens only if the index was built with different walk options (`--hidden`/`--no-ignore`/`--follow` changed since) or fails to load. |
| `--clear-index PATH` | Delete index state for every indexed root at or under PATH (e.g. an accidentally indexed `node_modules`). |
| `--skill` | Print operating instructions for an AI agent (~3K tokens): the survey→expand→narrow→cite→verify workflow, JSON contract, and rules of thumb. Load it into an agent's context instead of `--help`, which is a flag reference for humans. |

Example agent workflow over a book corpus:

    agrep --outline --top 10 -r 'backoff' ./books_text/     # who covers it
    agrep --scope -rn --max-tokens 2000 'backoff' ./books_text/Manning/
    agrep --batch probes.txt --json -r ./books_text/        # expanded concept
    agrep --get-region 'books_text/Manning/x.md@3120-3245'  # verify a citation
