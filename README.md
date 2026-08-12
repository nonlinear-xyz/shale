# shale

Local memory for coding agents.

Your agents forget everything, and each new one starts from zero. shale captures
every agent session on your machine — whatever harness wrote it — keeps them in a
local append-only store, and serves them back to any agent over MCP.

```sh
shale repos              # what shale can see on this machine (no network, ever)
shale watch              # capture settled sessions into the local store
shale search <query>     # search your own corpus
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

## Search

Search is lexical (FTS5, porter stemming, bm25). Exact identifiers, file names
and error messages work best; `AND`/`OR`/`NOT` and `"quoted phrases"` work as
you'd expect, and everything else is treated as a literal term — so `kai-214`
and `C++` search for what you typed rather than erroring.

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
