# shale

Local memory for coding agents.

Your agents forget everything, and each new one starts from zero. shale captures
every agent session on your machine — whatever harness wrote it — keeps them in a
local append-only store, and serves them back to any agent over MCP.

```sh
shale                    # open the browser
shale repos              # what shale can see on this machine (no network, ever)
shale watch              # capture settled sessions into the local store
shale search <query>     # search your own corpus
shale show <ref>         # read the passage or session a result cites
shale browse             # search, read and audit interactively
shale status             # what has been captured
shale mcp                # serve context to agents over stdio MCP   (in progress)
shale link               # replicate to a hub for cross-machine joins (in progress)
```

Sessions are swept once they have been idle for 30 minutes — a quiet-file
heuristic, not an end-of-session signal, so a long-running session may be
captured before it ends and re-captured when it grows. `shale watch --dry-run`
shows exactly what would be captured, and touches nothing.

## Giving it to your agent

**Claude Code:**

```sh
claude mcp add shale -s user -- shale mcp
```

**Codex:**

```sh
codex mcp add shale -- shale mcp
```

Both take an absolute path if `shale` is not on the PATH your editor inherits —
a GUI-launched editor often does not see a PATH set in `.zshrc`, which surfaces
as `Connection closed` rather than as `command not found`.

Or commit a project-level `.mcp.json` for Claude Code:

```json
{ "mcpServers": { "shale": { "type": "stdio", "command": "shale", "args": ["mcp"] } } }
```

Two read-only tools, no mutation surface:

- **`context_for_task`** — call at the start of work. Returns a bounded packet of
  relevant prior passages plus things that went wrong when similar work was
  attempted before. Every item carries provenance: source, repo, line range in
  the stored transcript, and freshness.
- **`search_evidence`** — a targeted keyword dig, optionally filtered to one repo
  or to `kind: "error"`.

A packet always reports **how** it retrieved (`match`, `match_or`, or
`recency_fallback`) and **what it dropped** (`budget.truncated`), so an agent can
weigh a fallback differently from an exact match and never mistakes a partial
packet for a complete one.

Add this to your `CLAUDE.md` to make the loop automatic:

```markdown
Before starting any nontrivial task, call `context_for_task` on the `shale`
MCP server with a one-sentence description of what you're about to do.
Treat `recency_fallback` packets with low confidence.
```

## Search, then read

Search is lexical (FTS5, porter stemming, bm25). Exact identifiers, file names
and error messages work best; `AND`/`OR`/`NOT` and `"quoted phrases"` work as
you'd expect, and everything else is treated as a literal term — so `kai-214`
and `C++` search for what you typed rather than erroring.

Every result carries a **ref**, and `shale show` takes it — so finding a passage
and reading around it are one motion:

```sh
$ shale search MACOS_SIGN_P12
  demo: why did the goreleaser signing step fail on the darwin build?
    claude_code · /tmp/demo · 2026-08-12 · lines 1–5 · error
    … tool_error: signing failed: MACOS_SIGN_P12 not set …
    shale show chunk:1:0

$ shale show chunk:1:0          # the passage, plus a chunk of context either side
$ shale show chunk:1:0 --full   # the whole session
$ shale show session:1 --lines 40,90
```

`--kind error`, `--repo owner/name` and `--since <days>` narrow a search the same
way the MCP tools do; the CLI and the MCP server read the same chunk index, so
they cannot give you different answers to the same question.

## Browsing

`shale search` and `shale show` are the same act performed twice — search, read
an excerpt, copy a ref, run `show`, find it was the wrong passage, start again.
`shale browse` collapses that loop into one screen: results narrow as you type,
and the transcript on the right opens on the matched line with its context above
it.

```
 Search   Sessions   Repos   Status                                       shale
────────────────────────────────────────────────────────────────────────────────
  search  worktree
╭──────────────────────────────────╮╭────────────────────────────────────────────╮
│ │ kairos: indexing the codebase  ││                   | grep -v node_modules   │
│ │ claude_code · kairos-app · 08-10││ ▸    36 output    === .claude/ ===         │
│   observatory: merged branches   ││                   .claude/settings.json    │
╰──────────────────────────────────╯╰────────────────────────────────────────────╯
 shale show chunk:45:5           tab pane  ↑↓ results  enter read  ctrl+c quit
```

Bare `shale` opens it too — but only when stdin and stdout are both terminals.
Piped or redirected, bare `shale` still prints usage and exits 2, so scripts that
relied on that keep working.

Four tabs: **Search**, **Sessions** (everything captured, newest first), **Repos**
(what discovery found and what it skipped — the same audit surface as `shale
repos`, with `a` to add a scan root and `r` to rescan), and **Status**.

The ref stays on the status bar throughout, because it is what this surface
exports: paste it into `shale show`, a commit message, or an agent prompt.

This is additive. `shale search` remains a print-and-exit command so it can be
piped, grepped and scripted — both read the same index.

## Colour

Colour follows the terminal. Piped, redirected, or with `NO_COLOR` set, every
command prints exactly the plain text it always did — there is nothing to strip,
because nothing is emitted. `shale search | grep` is unaffected.

The palette is sampled off a shale cobble beach — slate greys, sea-slate blues,
terracotta, ochre and iron red — and assigned by meaning rather than by hue: one
colour for a ref you can follow, one for the term that matched, one for something
skipped, one for something that failed. It degrades explicitly at 256 and 16
colours rather than by nearest-match, which collapsed *skipped* and *failed* onto
the same red.

`shale mcp` emits no styling at all: stdout there is the JSON-RPC transport.

## What leaves your machine

By default: nothing. The local tier is a SQLite index and a directory of scrubbed
transcripts under `~/.shale`. There is no account, no daemon phoning home, and no
LLM call — retrieval is FTS5 keyword search over your own corpus.

`shale link` is the only command that involves a server, and it is the upgrade
path, not the product. What it buys is the thing you cannot compute on one
machine: joins across machines, across repositories, and across people.

Before any of that, `shale repos` shows you the complete list of what this tool
can see, and nothing is uploaded until you say so. Discovery is local, sync is
opt-in.

## Secrets

Transcripts are scrubbed **before** they are hashed, stored, or uploaded — nine
named rules (cloud keys, tokens, JWTs, PEM blocks, `SECRET=`-shaped assignments)
plus a Shannon-entropy catch-all. Redactions are stable: the same secret produces
the same placeholder everywhere, so you can still see that line 40 and line 900
referenced one key without the value being recoverable.

Per-rule counts are recorded so you can audit what was caught. The values never
are.

## Design

- [`docs/schema.md`](docs/schema.md) — the local event log, and why raw bytes stay
  on disk while the database only indexes them.

## Building

```sh
CGO_ENABLED=0 go build ./cmd/shale
CGO_ENABLED=0 go test ./...
```

`CGO_ENABLED=0` is an invariant, not a preference. It is what keeps this a single
static binary that cross-compiles to every platform from one CI job. The SQLite
driver is `modernc.org/sqlite` (pure Go) for exactly this reason; anything that
would drag in cgo — tree-sitter, native embedding models — belongs on the hub or
in a sidecar, not here.

## Status

The local loop is closed: capture → store → search → serve. On the author's
machine that is 94 sessions, 7,854 indexed passages, 17 repositories and a month
of history, in a 27 MB database.

Hub replication (`shale link`) is next — cross-machine and cross-person joins are
the one thing that genuinely cannot be computed on a single laptop.

MIT.
