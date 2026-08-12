# The local event log

This is the expensive-to-reverse decision. A hub replicates this log, MCP serves
from it, and a future distiller reads it. Everything else in shale is downstream.

## Shape

Append-only, one row per observed fact:

```
{ seq, id, source, actor, occurred_at, scope, pointer, content_hash, payload }
```

| Column | Meaning |
|---|---|
| `seq` | Monotonic local sequence. The replication cursor is a `seq`, nothing else. |
| `id` | ULID. Stable across machines so a hub can dedupe without trusting `seq`. |
| `source` | `claude_code`, `codex`, `git`, `manual`. Open set — new harnesses add values, never columns. |
| `actor` | Who produced it: `human`, `agent`, `system`. |
| `occurred_at` | When the fact happened, not when we noticed. |
| `scope` | Repo full name (`owner/name`) when known, else the absolute path prefix. |
| `pointer` | The blob path the writer recorded. Never the bytes themselves, and never the address a reader should use — see below. |
| `content_hash` | sha256 of the scrubbed raw bytes this event points at. **This is the address of record.** |
| `payload` | JSON for the small, query-driving extract. Bounded; the raw stays on disk. |

## Two decisions worth stating outright

**Raw transcripts stay on disk; the database indexes them.** SQLite holds the
index and the FTS table; scrubbed transcript bytes live beside it under
`~/.shale/blobs/`. This is what makes the distiller rewritable: a better
extraction pass next month re-derives from the raw log rather than needing a
corpus that no longer exists. `payload` is an extract, never the record.

**Replication ships log segments with a cursor, not current state.** A hub
receives `events since seq N` and is otherwise stateless glue. This is what makes
capture work offline and what makes a second machine cheap to add. Nothing in the
local schema may depend on having talked to a hub.

## Physical layout

```
~/.shale/
  config.json         url + apiKey (0600)
  machine.json        machine identity (0600)
  shale.db            SQLite: events, blobs index, FTS5
  blobs/<hash[0:2]>/<hash>.jsonl.gz    scrubbed transcripts, content-addressed
```

Content-addressing the blobs means a re-captured session that hasn't changed
costs nothing, and a grown session shares every unchanged byte with its earlier
form.

**Resolve a blob from `content_hash`, never from `pointer`.** One function —
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
- A blob row cannot be deleted while an event points at it.

## What is deliberately absent

No embeddings, no derived claims, no decisions table, no notes. Those are
interpretation, and interpretation lives on the hub where it can change weekly
without shipping a binary. The local tier answers "what happened, and where are
the bytes" — that question has one right answer and it does not need a model.

## Search

FTS5 with the `porter unicode61` tokenizer over the payload extract, ranked by
`bm25()` with a recency decay applied in the query. Verified working under
`modernc.org/sqlite` with `CGO_ENABLED=0`, which is the constraint that made pure
Go possible at all.

Keyword search is the whole local retrieval story. It is not a placeholder for
semantic search — it is the tier that costs nothing, leaks nothing, and cannot
hallucinate. Ranking that requires a model belongs to the hub.

### Two indexes, one of which is not for searching

There are two FTS tables, and the difference matters more than it looks:

| | rows | holds |
|---|---|---|
| `events_fts` | one per session | the digest — title and summary |
| `chunks_fts` | dozens per session | the transcript bodies |

**Everything user-facing searches `chunks_fts`.** The digest index covers a few
percent of what was captured, so text that is plainly in a transcript is simply
absent from it. `shale search` used to read `events_fts` while the MCP server
read `chunks_fts`, which meant the CLI answered "no matches" for text shale was
holding — confidently, silently, and differently from the MCP server given the
identical query. `events_fts` is for whole-session lookup, not for retrieval.

### Refs

A ref addresses a passage: `chunk:<eventSeq>:<chunkIndex>`, or `session:<seq>`
for a whole session. Packets and search results both mint them, `store.ParseRef`
parses them, and `shale show` resolves them. All three ends are tested together —
a citation format nothing can resolve is decoration, and it is not obvious from
either end alone that the loop is broken.
