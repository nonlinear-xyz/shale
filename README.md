# shale

Local memory for coding agents.

Your agents forget everything, and each new one starts from zero. shale captures
agent sessions, stores explicit memories and task handoffs, indexes the memory
files your harnesses already maintain, and serves the useful slice back to any
agent over MCP. The entire loop is local and model-free.

```sh
shale                    # open the browser
shale repos              # what shale can see on this machine (no network, ever)
shale watch              # capture settled sessions into the local store
shale search <query>     # search your own corpus
shale show <ref>         # resolve a passage, session, or durable-state citation
shale remember <text>    # save an explicit memory (user, repo, or task scoped)
shale memories           # list active memories
shale proposals          # review inferred memories waiting for approval
shale checkpoints        # list resumable task handoffs
shale runbook list       # list personal and Git-backed runbooks
shale refresh            # snapshot Claude/Codex memory and instruction files
shale browse             # search, read and audit interactively
shale status             # what has been captured
shale mcp                # serve and record agent state over stdio MCP
shale link               # replicate to a hub for cross-machine joins (in progress)
```

Sessions are swept once they have been idle for 30 minutes — a quiet-file
heuristic, not an end-of-session signal, so a long-running session may be
captured before it ends and re-captured when it grows. `shale watch --dry-run`
shows exactly what would be captured, and touches nothing. A real watch also
refreshes registered runbooks and Claude/Codex memory files.

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

Six tools, with a deliberately narrow mutation boundary:

- **`context_for_task`** — call at the start of work. Returns a bounded packet of
  the latest task checkpoint, approved memories, relevant runbooks, prior
  failures, and transcript evidence. Every item carries provenance and a ref.
- **`search_evidence`** — a targeted keyword dig, optionally filtered to one repo
  or to `kind: "error"`.
- **`remember_explicit`** — writes an active memory only when the user explicitly
  asked the agent to remember something.
- **`propose_memory`** — records an inference as pending. Pending proposals never
  enter retrieval until a person runs `shale accept memory:<id>`.
- **`save_checkpoint`** — writes a structured, task-keyed handoff and chains it to
  the previous checkpoint for that task.
- **`read_ref`** — resolves exact artifact versions and transcript citations.

An agent cannot approve its own proposal through MCP. That asymmetry is the
point: explicit instructions can become state immediately; inferred state has a
quick human gate.

A packet always reports **how** it retrieved (`match`, `match_or`, or
`recency_fallback`) and **what it dropped** (`budget.truncated`), so an agent can
weigh a fallback differently from an exact match and never mistakes a partial
packet for a complete one.

Add this to your `CLAUDE.md` to make the loop automatic:

```markdown
Before starting any nontrivial task, call `context_for_task` on the `shale` MCP
server with a one-sentence description, the repository, and a stable task key
when one exists. Treat `recency_fallback` packets with low confidence. Only call
`remember_explicit` when I directly ask you to remember something; put inferred
durable knowledge in `propose_memory`. Save a checkpoint at meaningful stopping
points on work that will continue later.
```

## Memories and checkpoints

Memories have three recall scopes: `user` (available everywhere), `repo`, and
`task`. Scope is inferred from `--task` and `--repo`, or can be stated directly:

```sh
shale remember "Use pnpm in this repository" --repo nonlinear/shale
shale remember "Prefer terse status updates" --scope user

shale memories                         # active memories only
shale proposals                        # pending agent inferences
shale accept memory:<id>               # optionally: --file edited.md
shale reject memory:<id>               # rejects and destroys proposal content
shale supersede memory:<id> "Use bun" # keeps the old exact version addressable
```

`shale forget <ref>` retracts native state from future recall but retains its
auditable versions. `shale purge <ref> --yes` is the explicit destructive path:
it removes every Shale-managed body version, clears the search projection,
truncates the SQLite WAL, vacuums freed pages, and leaves only a metadata
tombstone. Non-interactive purge requires `--yes`.

For an external snapshot, remove its canonical file and run `shale refresh`
first; once retracted, purge can destroy Shale's retained copy and source
registration. An existing canonical file cannot be purged through Shale because
the next refresh would truthfully restore it.

Checkpoints are structured handoffs rather than free-form memories: goal,
current summary, decisions, changed artifacts, open loops, next actions, and
evidence refs. `context_for_task` returns only the latest checkpoint for an exact
task key; `shale checkpoints --task <key>` audits the history.

## Runbooks and existing harness memory

Runbooks use a hybrid ownership model:

```sh
shale runbook create --file personal-release.md   # Shale-native, user scoped
shale runbook revise runbook:<id> --file new.md
shale runbook register docs/release.md             # file stays Git-canonical
shale runbook list
```

A registered runbook must be inside a Git worktree. Shale snapshots it for local
retrieval, records revisions when its bytes change, and retracts the snapshot if
the source disappears; it never edits the file. There is intentionally no
runbook executor or execution-history model in this release.

`shale refresh` also snapshots existing Codex memory, Claude auto-memory, and
global/repository instruction files. The original files remain canonical and
untouched. Indexed instruction files are available through
`shale search --kind instruction` and `shale show`, but are not duplicated into
context packets because the harness already loads them. External memories carry
their harness and `external_generated` authority in provenance rather than being
presented as user assertions.

Discovery follows the harnesses' documented locations: `$CODEX_HOME/memories`
(or `~/.codex/memories`), global and repository `AGENTS.override.md` / `AGENTS.md`,
Claude's configured `autoMemoryDirectory` or
`~/.claude/projects/<project>/memory/`, and global/repository `CLAUDE.md`,
`.claude/CLAUDE.md`, `CLAUDE.local.md`, and `.claude/rules/**/*.md`. See the
[Codex memory documentation](https://learn.chatgpt.com/docs/customization/memories)
and [Claude Code memory documentation](https://code.claude.com/docs/en/memory)
for the canonical path semantics.

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

`--kind error`, `--repo owner/name` and `--since <days>` narrow transcript
search. `--kind memory|checkpoint|runbook|instruction` searches durable state;
artifact results cite an exact version such as `memory:<id>@<eventSeq>`, while
the versionless ref follows the current state.

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

`shale mcp` emits no styling at all: stdout there is the JSON-RPC transport, and
a test runs the real binary with colour forced on to prove it stays pure.

**If an agent shells out to shale**, nothing changes when the harness pipes the
output — which is the normal case, and what Claude Code's Bash tool does. If your
harness allocates a PTY instead, pass `--no-color` (or set `NO_COLOR=1`) so the
agent reads prose rather than escape sequences. One caveat there: Bubble Tea v1
queries the terminal's background colour from a package `init()`, before `main`
runs, so on a PTY three escape bytes precede the output of every command no
matter what flags are passed. It is upstream, documented as removed in Bubble
Tea v2, and does not affect MCP or piped output.

## What leaves your machine

By default: nothing. The local tier is a SQLite index plus content-addressed,
scrubbed transcript and artifact blobs under `~/.shale`. There is no account, no
daemon phoning home, and no LLM call — retrieval is FTS5 keyword search over your
own corpus.

`shale link` is the only command that involves a server, and it is the upgrade
path, not the product. What it buys is the thing you cannot compute on one
machine: joins across machines, across repositories, and across people.

Before any of that, `shale repos` shows you the complete list of what this tool
can see, and nothing is uploaded until you say so. Discovery is local, sync is
opt-in.

## Secrets

Transcripts, native memories, checkpoints, runbooks, and external snapshots are
scrubbed **before** they are hashed or stored — nine named rules (cloud keys,
tokens, JWTs, PEM blocks, `SECRET=`-shaped assignments) plus a Shannon-entropy
catch-all. Redactions are stable: the same secret produces the same placeholder
everywhere, so you can still see that two records referenced one key without the
value being recoverable.

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
