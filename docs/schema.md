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
| `scope` | Repo full name (`owner/name`) when known, else the absolute path prefix. |
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
  artifacts and upgrades a version-zero database in one transaction.
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

### Three retrieval indexes

There are three FTS tables, and the difference matters more than it looks:

| | rows | holds |
|---|---|---|
| `events_fts` | one per session | the digest — title and summary |
| `chunks_fts` | dozens per session | the transcript bodies |
| `artifacts_fts` | one per active artifact | memory/checkpoint/runbook/instruction search text |

Transcript search uses `chunks_fts`. The digest index covers a few percent of
what was captured, so text plainly in a transcript may be absent from it;
`events_fts` is for whole-session lookup, not passage retrieval. Explicit
`--kind memory|checkpoint|runbook|instruction` searches `artifacts_fts`.

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
