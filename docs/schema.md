# The local event log and durable-state projection

This is the expensive-to-reverse decision. A hub replicates this log, MCP serves
from it, and a future distiller reads it. Everything else in shale is downstream.

## Shape

Append-only, one row per observed fact or state transition:

```
{ seq, id, source, actor, occurred_at, scope, pointer, content_hash, payload }
```

| Column | Meaning |
|---|---|
| `seq` | Monotonic local sequence. The replication cursor is a `seq`, nothing else. |
| `id` | Stable event identifier. A hub can dedupe without trusting local `seq`. |
| `source` | `claude_code`, `codex`, `git`, `manual`. Open set — new harnesses add values, never columns. |
| `actor` | Who produced it: `human`, `agent`, `system`. |
| `occurred_at` | When the fact happened, not when we noticed. |
| `scope` | Repo full name (`owner/name`) when known, else the absolute path prefix. Skill events always use a portable library key. |
| `pointer` | The blob path the writer recorded. Never the bytes themselves, and never the address a reader should use — see below. |
| `content_hash` | sha256 of the scrubbed raw bytes this event points at. **This is the address of record.** |
| `payload` | JSON for the small, query-driving extract. Bounded; the raw stays on disk. |

## Three decisions worth stating outright

**Raw transcripts stay on disk; the database indexes them.** SQLite holds the
index and the FTS table; scrubbed transcript bytes live beside it under
`~/.shale/blobs/`. This is what makes the distiller rewritable: a better
extraction pass next month re-derives from the raw log rather than needing a
corpus that no longer exists. `payload` is an extract, never the record.

**Replication ships log segments with a cursor, not current state.** A hub
receives `events since seq N` and is otherwise stateless glue. This is what makes
capture work offline and what makes a second machine cheap to add. Nothing in the
local schema may depend on having talked to a hub.

**Durable state is versioned, but its body does not live in the event row.** A
memory, checkpoint, runbook, or instruction snapshot appends a metadata event and
updates a rebuildable current-state projection. Its scrubbed JSON body lives in a
content-addressed blob. This keeps exact historical refs possible while making
an explicit hard purge meaningful: the immutable event retains provenance and a
tombstone, not the forgotten prose.

## Physical layout

```
~/.shale/
  config.json         url + apiKey (0600)
  machine.json        machine identity (0600)
  shale.db            SQLite: events, current projections, source registry, FTS5
  blobs/<hash[0:2]>/<hash>.jsonl.gz    scrubbed transcripts, content-addressed
  artifact-blobs/<hash[0:2]>/<hash>.json.gz   scrubbed typed artifact bodies
  skill-blobs/<hash[0:2]>/<hash>   exact skill-package files, content-addressed
  worktrees/skill-<id>/             reviewed Git edits awaiting normal Git workflow
```

Content-addressing the blobs means a re-captured session that hasn't changed
costs nothing, and a grown session shares every unchanged byte with its earlier
form.

**Resolve a transcript blob from `content_hash`, never from `pointer`.** One function —
`store.BlobPath` — builds the name, and both the writer and the reader ask it.
`pointer` is a record of what the writer believed at the time, and because
`events` rejects `UPDATE`, a pointer written by a build that named blobs wrongly
can never be corrected. A hash cannot go stale that way. This is not
hypothetical: `BlobPath` once returned a `.jsonl` that the writer renamed to
`.jsonl.gz`, so every pointer in the table named a file that had never existed —
capture succeeded, search worked, and only the provenance trail was wrong.

## Invariants enforced in DDL

Guards live in triggers rather than application code, because application code is
the thing most likely to be rewritten:

- `events` rejects `UPDATE` and `DELETE`. It is a log.
- `seq` is `INTEGER PRIMARY KEY AUTOINCREMENT` — never reused, even after the
  highest row is gone, so a replication cursor can never be silently rewound.
- `PRAGMA user_version` gates additive migrations; version 1 introduces durable
  artifacts and version 2 introduces skill libraries, revisions, proposals,
  targets, and installations. Each older database upgrades in one transaction.
- `secure_delete=ON` is enabled. Hard purge also checkpoints/truncates WAL and
  vacuums database pages after removing FTS content.

## Durable artifacts

Four artifact kinds share one lifecycle and reference model:

| kind | purpose | normal scope |
|---|---|---|
| `memory` | a durable fact, preference, or decision | user, repo, or task |
| `checkpoint` | a structured handoff keyed to ongoing work | task |
| `runbook` | reusable Markdown procedure | user (native) or repo (Git-backed) |
| `instruction` | a snapshot of harness/repository instructions | user or repo |

`artifacts` is the current projection. `artifact_versions` maps every content
event to a content hash. `artifacts_fts` contains active artifacts only. Those
three are downstream of the event log and may be rebuilt. `artifact_sources` is
different: it is mutable operational configuration that records which canonical
files refresh should watch.

Every artifact records:

- `status`: `pending`, `active`, `retracted`, `rejected`, or `purged`;
- `scope_kind` + `scope_key`, with an optional repository association;
- `origin`: native, Git file, Claude memory, Codex memory, or instruction file;
- `authority`: `asserted`, `proposed`, `external_generated`, or
  `external_asserted`;
- `source` and optional canonical source path;
- the current event sequence and content hash.

The body is typed JSON. Memories and runbooks primarily use `text` and an
optional recall `trigger`. Checkpoints carry `taskKey`, `goal`, `summary`,
`decisions`, `artifacts`, `openLoops`, `nextActions`, `evidenceRefs`, and the
previous checkpoint ref.

### Lifecycle

An explicit memory appends `memory.asserted` and is active immediately. An
inference appends `memory.proposed`; pending rows are deliberately absent from
FTS and context packets. Human acceptance appends `memory.accepted`; rejection
appends `memory.rejected` and destroys proposal content. Replacement appends
`memory.superseded` under the stable artifact ID, so both the current and exact
historical refs remain useful.

`forget` appends a retraction and removes the artifact from FTS without deleting
its versions. `purge` is a separate, explicit operation: it appends a metadata
tombstone, deletes every version mapping and unshared body blob, clears FTS,
truncates WAL, and vacuums SQLite. Event payloads never contain body text, which
is the property that lets an append-only log and a real hard-delete path coexist.

External sources do not pretend Shale is canonical. Refresh snapshots current
bytes, appends a new version only when content changes, and retracts a snapshot
when a registered source disappears. Permission failures retain the last good
snapshot. Registered runbook symlinks must resolve inside their Git worktree;
auto-discovered sources must resolve inside their trusted memory/instruction
root.

## Skill libraries and recursive learning

Skills use dedicated projections because an exact multi-file package is not a
free-form durable artifact. `skill_libraries` records a portable key, future-safe
`owner_kind` / `owner_key` (`user:local` today, `team:<id>` later), canonical
mode (`native` or `git`), and local operational source fields. Absolute source,
install, and worktree paths exist only in projections and never enter events.

`skills` is current state (`draft`, `active`, or `retracted`).
`skill_revisions` is the immutable revision graph. A revision's SHA-256 tree
identity is calculated over sorted relative paths, normalized executable bits,
and exact per-file blob hashes. `skill_revision_files` maps that identity back to
the exact blobs. This makes the same skill revision portable across checkout
paths and machines without pretending SQLite chunks are the procedure.

`skill_changes` is the review queue:

1. An agent or human proposes a required scrubbed lesson and optional exact
   replacement `SKILL.md` against one base tree hash (`pending`).
2. Only a human transition can accept or reject it. Acceptance does not alter
   behavior; rejection clears lesson, rationale, evidence, and unshared
   replacement bytes while retaining a metadata tombstone.
3. A native apply creates a child revision and becomes `applied`. A Git apply
   requires the same clean HEAD and exact base tree, creates an isolated
   uncommitted worktree, and becomes `materialized`. It never rebases, commits,
   pushes, opens a PR, or executes project-defined commands.
4. A later refresh observes exact replacement bytes in canonical Git and marks
   the proposal `applied`. A changed base becomes `stale` and requires a
   superseding proposal.

`skill_targets` and `skill_installations` are local operational state. Install
requires an exact `@tree-hash`, stages a complete sibling directory, and refuses
unmanaged or divergent overwrites. Reinstalling an older exact ref is rollback.
Plugin cache directories are never write targets.

Skill source bytes do not pass through the memory scrubber: doing so could
silently corrupt code and ordered procedural constraints. They remain exact and
local. Proposal prose is scrubbed. A future team transport must perform an
explicit secret scan and consent check without rewriting the package.

## What is deliberately absent

No embeddings, local LLM, semantic distiller, automatic proposal approval,
runbook runner, or runbook execution history. The local tier stores facts the
user or harness actually supplied and agent inferences that are visibly pending;
it does not manufacture authoritative claims. Keyword retrieval remains local,
deterministic, and inspectable.

## Search

FTS5 with the `porter unicode61` tokenizer over transcript chunks and artifact
search text, ranked by `bm25()` and bounded by explicit time windows where
applicable. Verified working under
`modernc.org/sqlite` with `CGO_ENABLED=0`, which is the constraint that made pure
Go possible at all.

Keyword search is the whole local retrieval story. It is not a placeholder for
semantic search — it is the tier that costs nothing, leaks nothing, and cannot
hallucinate. Ranking that requires a model belongs to the hub.

### Four retrieval indexes

There are four FTS tables, and the difference matters more than it looks:

| | rows | holds |
|---|---|---|
| `events_fts` | one per session | the digest — title and summary |
| `chunks_fts` | dozens per session | the transcript bodies |
| `artifacts_fts` | one per active artifact | memory/checkpoint/runbook/instruction search text |
| `skills_fts` | one per searchable file in the current skill revision | routing metadata, relevant path, and a discovery excerpt |

Transcript search uses `chunks_fts`. The digest index covers a few percent of
what was captured, so text plainly in a transcript may be absent from it;
`events_fts` is for whole-session lookup, not passage retrieval. Explicit
`--kind memory|checkpoint|runbook|instruction` searches `artifacts_fts`.
`--kind skill` searches `skills_fts`, but returns compact metadata and an exact
file ref rather than treating an FTS excerpt as authoritative. Skills remain out
of `context_for_task`; the normal harness loads explicitly installed skills.

Context assembly uses independent retrieval ladders for checkpoints, memories,
runbooks, corrections, and raw evidence. An exact task key takes the latest
matching checkpoint. Other durable sections try an all-term match, then an
any-term match. Transcript evidence retains its strict/loose/recency fallback.
Within the default 8,000-token envelope, a 90% content fill region is divided
15%/20%/15%/20%/30% respectively; unused capacity carries forward, every
omission or excerpt is reported, and the total is a hard ceiling.

### Refs

A transcript ref addresses a passage (`chunk:<eventSeq>:<chunkIndex>`) or a whole
session (`session:<seq>`). Durable refs are `memory:<id>`, `checkpoint:<id>`,
`runbook:<id>`, and `instruction:<id>`. Adding `@<eventSeq>` pins an exact
artifact version; omitting it follows the current projection. Packets, CLI
search, `shale show`, and MCP `read_ref` share these parsers and resolvers. A
citation format nothing can resolve is decoration, so each direction is tested.

Skill refs are `skill:<library-key>/<skill-name>`; appending `@<tree-hash>` pins
an exact full-tree revision. A fragment addresses one exact progressively loaded
file, for example
`skill:nonlinear-xyz/factory-kit/factory-testing@<tree-hash>#references/details.md`.
Skill change tombstones use `skill-change:<id>`.
