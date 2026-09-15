# agrep — agent grep

> **v0.0.1** | **Experimental** — APIs and flags may change without notice.

Search built for AI agents. agrep is a high-performance grep alternative in pure Go (Linux AMD64, AVX2-accelerated, [competitive with ripgrep](architecture.md#performance-vs-ripgrep) — faster on most benchmarked workloads, slower on a few) — but speed is the table stakes, not the point. The point is that agents use search differently than humans, and no existing tool is built for how they use it.

<p align="center">
  <img src="https://static.wikia.nocookie.net/arrow/images/2/24/Vibe_with_his_powers_restored.png" alt="Vibe" width="300">
</p>

Built vibe coding with [Claude Code](https://claude.com/claude-code).

## Why a grep for agents?

An agent doing question-answering over a codebase or document corpus treats search as a **sensing API inside a loop**: fire a cheap query, read a little, refine, repeat. Four things go wrong with ordinary grep in that loop, and agrep is designed around each:

1. **Context is a budget.** A grep that dumps 40,000 matching lines into an agent's context has failed, and `| head` fails differently — the agent can no longer tell truncation from absence. `--max-tokens N` caps output while the search still runs to completion, and the summary reports *exactly* what was omitted. `--collapse` stops generated files from repeating one line 400 times. Totals are always true.

2. **Agents iterate; make each probe cheap.** `--outline` answers "who talks about X" across a whole corpus in a few hundred tokens. `--batch` runs N conceptual probes in one pass over the files. A zero-hit query isn't a dead end: `--suggest` probes case variants and identifier fragments and reports which ones actually occur — the tool proposes the next query.

3. **Claims need grounding.** Every `--json` match carries a byte-exact span and a self-contained region id (`path@3120-3245`). `--get-region` re-fetches exactly those bytes, so an agent can quote, then *verify the quote against disk* before asserting it — and a reviewer can do the same later. `--expand N` fetches the surrounding lines when the neighborhood matters.

4. **Absence is data.** Every `--json` run ends with an exact-totals summary, unreadable files appear in-stream as error objects and are counted, zero-hit batch queries are listed explicitly, and `--suggest` reports "none of the variants occur" as a finding rather than exiting silently. An agent should never mistake "couldn't see it" for "isn't there."

Everything is exact, never heuristic: budgets bound *output*, never *search*; the index is a prefilter, never an answerer.

## Install

Requires **Go 1.26+** with `GOEXPERIMENT=simd`, on **Linux AMD64** (AVX2).

```sh
GOEXPERIMENT=simd go install github.com/DanielLaubacher/agrep/cmd/agrep@latest
# or, from a checkout:
make build        # → bin/agrep
```

Build and development details (tests, benchmarks, the PCRE build) live in [architecture.md](architecture.md#building--development).

## Quick start — works like grep

```sh
agrep "error" app.log                     # basic search
agrep -rin "fixme" ./src/                 # recursive, case-insensitive, line numbers
agrep -F -e timeout -e refused app.log    # multiple fixed strings, one SIMD pass
agrep -C3 "panic" app.log                 # context lines
agrep -Fe 'ERROR' -toe '\d+' app.log      # pipeline: filter, then extract digits
```

Full flag reference: [help.md](help.md).

## Driving it from an agent

Run `agrep --skill` to print the agent operating manual (~3K tokens) — made to be loaded straight into an agent's context. It teaches the loop below; everything emits JSON-Lines with `--json`.

```sh
# 1. Survey: who talks about the concept? (a few hundred tokens, any corpus size)
agrep --outline --top 10 --rank density -r 'backpressure' ./src/

# 2. Expand: the agent turns one concept into several lexical probes — one pass
printf 'backpressure\nflow control\nbounded queue\n' > /tmp/probes
agrep --batch /tmp/probes --json -rc ./src/     # per-query totals, zeros listed

# 3. Narrow: read the best matches, annotated and budgeted
agrep -rn --scope --max-tokens 2000 'bounded queue' ./src/
#        └ --scope names the enclosing function/class on every match

# 4. Zero hits? The tool proposes the next query
agrep -r --suggest 'ConnectTimeout' ./src/
#   → "variants that do occur (rarest first): connect (26 lines), timeout (51)"

# 5. Cite, then verify the citation against disk bytes
agrep --json -rn 'jittered backoff' ./src/      # → "region":"src/retry.go@3120-3245"
agrep --get-region 'src/retry.go@3120-3245'     # re-fetch the exact bytes
agrep --get-region 'src/retry.go@:118-131' --expand 3   # or by lines, with context
```

Precision tools for code:

```sh
agrep -rn --ident 'connectTimeout' .   # matches connect_timeout, ConnectTimeout,
                                       # CONNECT_TIMEOUT... word-bounded: searching
                                       # Match will not hit MatchSet
agrep -r --histogram -oe 'ERR_[A-Z_]+' .        # what error codes exist? (uniq -c built in)
agrep -U 'type \w+ struct \{[^}]*Timeout' .     # patterns across line boundaries
```

Scoping the corpus:

```sh
agrep --changed-since main -rn 'TODO' .          # only the branch's diff surface
agrep -rl 'oldAPI' . --without-file 'newAPI'     # call sites not yet migrated
agrep -rl 'handler' . | agrep --files-from - -c 'metrics'   # compose queries
agrep --use-index -rn 'pattern' ROOT/            # trigram index for repeated queries
                                                 # (auto-fresh; results identical to a cold scan)
```

Design and rationale for all of this: [agent-mode.md](agent-mode.md). Index internals: [index.md](index.md).

## Querying a book library (EPUB/PDF → Markdown knowledge base)

`agrep-extract` mirrors a library of PDF/EPUB/MOBI files into a tree of plain Markdown — one `.md` per book, front-matter provenance, page markers preserved for citations, sources never written to. Extraction is incremental (re-runs only touch changed books), and a `manifest.tsv` records per-book status *including* books that yielded no text (scanned PDFs needing OCR) — the corpus's blind spots are themselves greppable.

```sh
agrep-extract -src ~/books -dst ~/books.extract
```

Then hand an agent `agrep --skill` and the mirror path, and lexical search plus an agent's own vocabulary becomes semantic retrieval — no embeddings, no vector store, and every answer grounded in bytes you can re-fetch. A real session over a ~800-book mirror answering *"what do my books say about backoff strategies?"* looks like:

```sh
# Which books discuss it at all? (one query, whole library, ~300 tokens)
agrep --outline --top 8 -ri 'backoff' ~/books.extract/

# The agent expands the concept and probes all variants in one pass
printf 'exponential backoff\njitter\nretry storm\nthundering herd\n' > /tmp/probes
agrep --batch /tmp/probes --json -rc ~/books.extract/

# Read the strongest sections, with chapter headings, under budget
agrep -rn --sections --max-tokens 3000 -i 'jitter' ~/books.extract/distributed/

# Quote with a page-anchored, verifiable citation
agrep --json -rn 'full jitter' ~/books.extract/aws-papers.md
#   → "section":"## Exponential Backoff And Jitter", "region":"...@41230-41395"
agrep --get-region '~/books.extract/aws-papers.md@41230-41395'
```

The division of labor is the whole trick: **the agent brings the semantics** (it knows "backoff" implies "jitter" and "thundering herd"), **agrep brings exact, honest, cited lexical retrieval** (real counts, real spans, real page context). Neither needs the other to be something it isn't.

## Performance

agrep beats ripgrep on most benchmarked workloads (up to 13x faster on single-file regex counts, ~2x on recursive `-l` driven by a rare literal), ties on plain recursive text output, and currently loses on recursive searches for a common multi-word literal phrase, where ripgrep's tuned `memchr` scan wins. The measured table — numbers, exact commands, and that losing case — is in [architecture.md](architecture.md#performance-vs-ripgrep); the full optimization story — SIMD Horspool, rare-pair Teddy, the lazy-DFA regex engine, closing and passing the ripgrep gap — is written up in [`education/`](education/).

## Documentation

- [help.md](help.md) — full CLI reference
- [agent-mode.md](agent-mode.md) — agent feature design and rationale
- [architecture.md](architecture.md) — pipeline, syscalls, SIMD, concurrency, benchmarks
- [education/](education/) — deep dives on every subsystem
- [index.md](index.md) — trigram index design (`--use-index`)

## License

MIT
