# gogrep Usage

## Synopsis

```
gogrep [OPTIONS] PATTERN [FILE...]
gogrep [OPTIONS] -e PATTERN [-e PATTERN...] [FILE...]
```

If no files are given and stdin is a terminal, searches the current directory recursively. If stdin is piped, reads from stdin. Use `--` to separate flags from patterns that start with `-`.

Short flags can be combined: `-rin` is equivalent to `-r -i -n`.

## Options

### Pattern Selection

| Flag | Short | Description |
|---|---|---|
| `--regexp PATTERN` | `-e` | Pattern to match (repeatable; multiple = OR, or AND with `-t`) |
| `--fixed-strings` | `-F` | Treat next `-e` pattern as a literal string, not a regex |
| `--perl-regexp` | `-P` | Use PCRE2 for next `-e` pattern |
| `--pipe` | `-t` | Pipe: next `-e` filters lines matched by the previous `-e` |
| `--only-matching` | `-o` | Print only the matched part of the line for next `-e` |
| `--ignore-case` | `-i` | Case-insensitive matching |
| `--smart-case` | `-S` | Case-insensitive if pattern is all lowercase |
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
| `--max-columns NUM` | `-M` | Truncate lines longer than NUM bytes (0=auto, -1=no limit) |
| `--json` | | Output results as JSON Lines |

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
gogrep "error" app.log
```

Search stdin:

```sh
cat app.log | gogrep "timeout"
```

### Case-Insensitive Search

```sh
gogrep -i "warning" app.log
```

### Fixed String Search

Treat the pattern as a literal string (no regex metacharacters):

```sh
gogrep -F "[ERROR]" app.log
```

### Line Numbers

```sh
gogrep -n "TODO" src/*.go
```

### Recursive Search

Search all files in a directory tree:

```sh
gogrep -rn "func main" ./src/
```

### Invert Match

Show lines that do NOT contain the pattern:

```sh
gogrep -v "DEBUG" app.log
```

### Count Matches

```sh
gogrep -c "error" *.log
```

### Files With Matches

List only filenames that contain a match:

```sh
gogrep -rl "TODO" ./src/
```

### Context Lines

Show 2 lines before and after each match:

```sh
gogrep -C2 "panic" app.log
```

Show 3 lines after each match:

```sh
gogrep -A3 "FATAL" app.log
```

### Multiple Patterns

Search for any of several patterns:

```sh
gogrep -e "error" -e "warning" -e "fatal" app.log
```

Multiple fixed strings (uses Aho-Corasick for single-pass matching):

```sh
gogrep -F -e "connection refused" -e "timeout" -e "EOF" app.log
```

### PCRE2 Regex

Use Perl-compatible regex for lookahead, lookbehind, backreferences:

```sh
# Lookahead: words followed by "world"
gogrep -P '\w+(?=\s+world)' file.txt

# Lookbehind: words preceded by "hello "
gogrep -P '(?<=hello\s)\w+' file.txt

# Backreference: repeated words
gogrep -Pn '(\w+)\s+\1' document.txt
```

### JSON Output

Output matches as JSON Lines (one JSON object per match):

```sh
gogrep --json "error" app.log
```

```json
{"type":"match","file":"app.log","line_number":42,"byte_offset":1847,"text":"2024-01-15 ERROR: connection refused","matches":[[15,20]]}
```

### Watch Mode

Watch files for changes and search new content as it's appended:

```sh
gogrep --watch "ERROR" /var/log/syslog
```

Watch multiple files:

```sh
gogrep --watch "panic" app.log worker.log
```

### Color Control

Force color output (useful when piping to `less -R`):

```sh
gogrep --color=always "pattern" file.txt | less -R
```

Disable color:

```sh
gogrep --color=never "pattern" file.txt
```

### Combined Flags

Recursive, case-insensitive, with line numbers and context:

```sh
gogrep -rinC3 "fixme" ./src/
```

Count fixed-string matches per file recursively:

```sh
gogrep -rFc "TODO" ./src/
```

### Only Matching (-o)

Print only the matched portion of each line (like `grep -o`):

```sh
gogrep -oe '\d+' app.log
# 503
# 200
# 42
```

### Regex Pipeline (-t)

Chain patterns with `-t` to filter lines through multiple stages. Each stage must match for the line to appear. The final stage's matches define the output.

```sh
# SIMD fixed-string prefilter, then extract digits
gogrep -Fe 'ERROR' -toe '\d+' app.log
# 503
# 200
```

This is equivalent to `grep 'ERROR' app.log | grep -o '\d+'` but runs in a single process, preserving file context (filename, line numbers).

Three-stage narrowing — each stage can use a different engine:

```sh
gogrep -Fe 'HTTP' -te 'status=\d+' -toe 'status=\d+' access.log
# status=200
# status=503
```

Mix fixed-string SIMD stages with regex or PCRE:

```sh
gogrep -Fe 'ERROR' -Fte 'prod-' -Fte 'timeout' -toe '\d+' app.log
```

Multiple OR branches, each with their own pipeline:

```sh
# Branch 1: ERROR lines → extract digits
# Branch 2: WARN lines (no pipeline)
gogrep -Fe 'ERROR' -toe '\d+' -Fe 'WARN' app.log
```

`-e` without a preceding `-t` starts a new OR branch. `-e` with `-t` continues the current pipeline.

### Searching Binary Files

gogrep automatically detects binary files (by checking for NUL bytes in the first 8 KB). Binary files with matches print a summary instead of the matched content:

```sh
gogrep -r "magic" ./data/
# Binary file ./data/archive.bin matches
```

## Agent options

Designed for AI agents using gogrep as a sensing API (see agent-mode.md):

| Flag | Description |
|---|---|
| `--max-tokens N` | Budget output to ~N tokens (4 bytes/token heuristic). Search always completes; a trailer reports exactly what was omitted. |
| `--outline` | Per-file survey: `count TAB path TAB first-matching-line`, busiest files first. |
| `--top K` | Limit `--outline` to the K busiest files. |
| `--sections` | Annotate matches with the enclosing Markdown heading (`§` group lines in text, `"section"` field in JSON). |
| `--batch FILE` | Run every pattern in FILE (one per line, `#` comments) in a single pass; each file is read once. Results carry their query (`[pattern]` prefix / `"query"` field). |
| `--suggest` | On zero hits, probe the case-insensitive form and identifier fragments of the pattern; report which occur and where. |
| `--get-region PATH@START-END` | Print the exact bytes of a span id (as emitted in JSON `"region"`). Lets an agent re-fetch or verify a citation without re-reading the file. |
| `--use-index` | Build (first use) and use a trigram index for recursive search. Every query re-validates freshness with a parallel stat sweep and transparently reindexes on any drift — reindexes are incremental (only changed files are read), so the index is always current and can never miss a match. Falls back to a cold scan whenever it doesn't apply (different walk options, `-v`, PCRE, patterns with no ≥3-byte literal). |
| `--clear-index PATH` | Delete index state for every indexed root at or under PATH (e.g. an accidentally indexed `node_modules`). |

JSON output (`--json`) always includes real line numbers, plus `"span"`
(absolute byte range of the line) and `"region"` (a self-contained id for
`--get-region`).

Example agent workflow over a book corpus:

    gogrep --outline --top 10 -r 'backoff' ./books_text/     # who covers it
    gogrep --sections -rn --max-tokens 2000 'backoff' ./books_text/Manning/
    gogrep --batch probes.txt --json -r ./books_text/        # expanded concept
    gogrep --get-region 'books_text/Manning/x.md@3120-3245'  # verify a citation
