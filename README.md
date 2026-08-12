# shale

Local memory for coding agents.

Your agents forget everything, and each new one starts from zero. shale captures
every agent session on your machine — whatever harness wrote it — keeps them in a
local append-only store, and serves them back to any agent over MCP.

```sh
shale repos      # what shale can see on this machine (no network, ever)
shale init       # install the watcher, register the MCP server
shale watch      # capture continuously
shale mcp        # serve context over stdio MCP
shale link       # optional: replicate to a hub for cross-machine joins
```

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

Early. Repository discovery and the secret scrubber are ported and tested against
the implementation they replace. Local capture, the event store, and the MCP
server are in progress.

MIT.
