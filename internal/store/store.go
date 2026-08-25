// Package store is the local append-only event log.
//
// See docs/schema.md for the design. The two decisions that shape everything
// here: raw transcript bytes stay on disk and the database only indexes them,
// and replication ships log segments identified by a cursor rather than current
// state.
//
// The driver is modernc.org/sqlite — pure Go, so CGO_ENABLED=0 holds and the
// binary cross-compiles from one machine. FTS5 (porter tokenizer, bm25) is
// available under it; that was verified before any of this was built.
package store

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Kind values for events. Open set: a new kind is additive, never a schema change.
const (
	KindSessionCaptured = "session.captured"

	KindMemoryProposed   = "memory.proposed"
	KindMemoryAsserted   = "memory.asserted"
	KindMemoryAccepted   = "memory.accepted"
	KindMemorySuperseded = "memory.superseded"
	KindMemoryRejected   = "memory.rejected"
	KindMemoryRetracted  = "memory.retracted"
	KindMemoryPurged     = "memory.purged"

	KindCheckpointSaved = "checkpoint.saved"

	KindRunbookCreated    = "runbook.created"
	KindRunbookRegistered = "runbook.registered"
	KindRunbookRevised    = "runbook.revised"
	KindRunbookRefreshed  = "runbook.refreshed"

	KindExternalIndexed = "external.indexed"
	KindExternalRemoved = "external.removed"

	KindArtifactRetracted = "artifact.retracted"
	KindArtifactPurged    = "artifact.purged"
)

// DB wraps the local store.
type DB struct {
	sql  *sql.DB
	root string
}

// Open opens (creating if needed) the store under dir, which is normally
// ~/.shale. Blobs live beside the database in dir/blobs.
func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create blob dir: %w", err)
	}
	path := filepath.Join(dir, "shale.db")

	// WAL so a reader (the MCP server) never blocks the writer (the watcher).
	// busy_timeout so a concurrent sweep waits instead of failing outright.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=secure_delete(1)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s: %w", path, err)
	}
	db := &DB{sql: sqlDB, root: dir}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// OpenReadOnly opens an existing store without migrations or filesystem writes.
// It exists for `watch --dry-run`: even an idempotent migration or WAL setup
// would violate a preview's promise not to touch persistent state.
func OpenReadOnly(dir string) (*DB, error) {
	path := filepath.Join(dir, "shale.db")
	dsn := "file:" + path + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("cannot open %s read-only: %w", path, err)
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("cannot open %s read-only: %w", path, err)
	}
	return &DB{sql: sqlDB, root: dir}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// BlobPath is where a scrubbed transcript with this content hash lives.
// Content-addressing means a re-captured session that hasn't changed costs
// nothing, and a grown session shares every unchanged byte with its earlier form.
//
// The ".gz" is part of the path this function returns, not something the writer
// bolts on afterwards. It used to be: BlobPath named a .jsonl that the writer
// then renamed to .jsonl.gz, so every pointer recorded in events named a file
// that has never existed on disk. One function owns the name now, and both the
// writer and the reader ask it — which is the only arrangement in which they
// cannot drift apart again.
func (d *DB) BlobPath(contentHash string) string {
	if len(contentHash) < 2 {
		return filepath.Join(d.root, "blobs", contentHash+blobExt)
	}
	return filepath.Join(d.root, "blobs", contentHash[:2], contentHash+blobExt)
}

// blobExt is the on-disk suffix for a stored transcript: JSONL, gzipped.
const blobExt = ".jsonl.gz"

const schema = `
CREATE TABLE IF NOT EXISTS events (
  seq          INTEGER PRIMARY KEY AUTOINCREMENT,
  id           TEXT NOT NULL UNIQUE,
  kind         TEXT NOT NULL,
  source       TEXT NOT NULL,
  actor        TEXT NOT NULL,
  occurred_at  TEXT NOT NULL,
  scope        TEXT,
  pointer      TEXT,
  content_hash TEXT,
  payload      TEXT NOT NULL,
  created_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS events_kind_occurred_idx ON events(kind, occurred_at DESC);
CREATE INDEX IF NOT EXISTS events_scope_idx         ON events(scope);
CREATE INDEX IF NOT EXISTS events_hash_idx          ON events(content_hash);

-- The log is a log. Guards live in DDL rather than application code because
-- application code is the thing most likely to be rewritten.
CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
  BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
  BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;

-- Porter stemming so a search for "hash" finds "hashing". Unindexed columns ride
-- along so a hit can be rendered without a second query.
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
  title, body,
  seq UNINDEXED, source UNINDEXED, scope UNINDEXED, occurred_at UNINDEXED,
  tokenize='porter unicode61'
);

-- Chunks: searchable windows over the transcript body, distinct from the digest.
-- The digest is the session's summary (what was asked, what broke, how it
-- ended); chunks are everything else, which is ~96% of what was captured and
-- would otherwise be stored but invisible.
--
-- Metadata rides as UNINDEXED columns so a hit renders without a second query.
-- kind separates errors, which drive a packet's corrections section.
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  body,
  event_seq UNINDEXED, chunk_index UNINDEXED,
  line_start UNINDEXED, line_end UNINDEXED, kind UNINDEXED,
  source UNINDEXED, scope UNINDEXED, occurred_at UNINDEXED,
  tokenize='porter unicode61'
);

-- Per-source replication and sweep watermarks. Deliberately NOT in events: a
-- cursor is mutable state about the log, not a fact that happened.
CREATE TABLE IF NOT EXISTS cursors (
  source        TEXT PRIMARY KEY,
  last_mtime_ms INTEGER NOT NULL,
  updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
`

func (d *DB) migrate() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer tx.Rollback()

	// `schema` is the version-zero baseline. It remains idempotent so a database
	// created by any older shale build can be upgraded without first knowing the
	// exact build that created it.
	if _, err := tx.Exec(schema); err != nil {
		return fmt.Errorf("migrate baseline: %w", err)
	}

	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version < 1 {
		if _, err := tx.Exec(artifactSchema); err != nil {
			return fmt.Errorf("migrate artifacts: %w", err)
		}
		if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
			return fmt.Errorf("record schema version: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration: %w", err)
	}
	return nil
}

// SessionRecord is the payload of a session.captured event.
type SessionRecord struct {
	Source     string            `json:"source"`
	SourceKey  string            `json:"sourceKey"`
	Title      string            `json:"title"`
	Digest     string            `json:"digest"`
	Repo       string            `json:"repo,omitempty"`
	Branch     string            `json:"branch,omitempty"`
	Project    string            `json:"project,omitempty"`
	CWD        string            `json:"cwd,omitempty"`
	Machine    string            `json:"machine,omitempty"`
	Turns      int               `json:"turns"`
	Usage      map[string]any    `json:"usageByModel,omitempty"`
	Redactions map[string]int    `json:"redactions,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	EndedAt    time.Time         `json:"endedAt"`
	LineCount  int               `json:"lineCount"`
	SizeBytes  int               `json:"sizeBytes"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// ChunkRow is one indexable window over a transcript, as the store sees it.
type ChunkRow struct {
	Index     int
	LineStart int
	LineEnd   int
	Kind      string
	Text      string
}

// PutSession writes a session.captured event, its digest index and its chunks in
// ONE transaction.
//
// Atomicity here is not fastidiousness. Committing the event separately from its
// chunks means a crash between the two leaves a session that looks captured but
// can never be searched — and because dedupe is keyed on content hash, the sweep
// never offers that file again. The corpus silently loses a session, with no
// error anywhere.
//
// On a duplicate content hash this returns the EXISTING seq with inserted=false,
// rather than zero. That is what makes repair possible: the caller can ask whether
// the already-stored session has chunks and backfill it if not. Returning zero was
// the bug that left everything captured by an older build permanently unindexed —
// an upgrade could not see the corpus it was installed to search.
//
// A GROWN session has a different hash and is appended as a new event. The log
// keeps both, because "what did this session look like at 3pm" is a real question
// once a hub is joining across machines.
func (d *DB) PutSession(ctx context.Context, contentHash string, rec SessionRecord, chunks []ChunkRow) (seq int64, inserted bool, err error) {
	err = d.sql.QueryRowContext(ctx,
		`SELECT seq FROM events WHERE content_hash = ? AND kind = ? LIMIT 1`,
		contentHash, KindSessionCaptured).Scan(&seq)
	switch {
	case err == nil:
		return seq, false, nil // already stored — the caller decides whether to repair
	case err != sql.ErrNoRows:
		return 0, false, fmt.Errorf("dedupe check: %w", err)
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return 0, false, fmt.Errorf("encode payload: %w", err)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	scope := rec.Repo
	if scope == "" {
		scope = rec.CWD
	}
	occurred := rec.EndedAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	stamp := occurred.UTC().Format(time.RFC3339)

	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, kind, source, actor, occurred_at, scope, pointer, content_hash, payload)
		VALUES (?, ?, ?, 'agent', ?, ?, ?, ?, ?)`,
		contentHash+":"+rec.SourceKey, KindSessionCaptured, rec.Source,
		stamp, scope, d.BlobPath(contentHash), contentHash, string(payload),
	)
	if err != nil {
		return 0, false, fmt.Errorf("insert event: %w", err)
	}
	seq, err = res.LastInsertId()
	if err != nil {
		return 0, false, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events_fts (title, body, seq, source, scope, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rec.Title, rec.Digest, seq, rec.Source, scope, stamp,
	); err != nil {
		return 0, false, fmt.Errorf("index digest: %w", err)
	}

	if err := insertChunks(ctx, tx, seq, rec.Source, scope, stamp, chunks); err != nil {
		return 0, false, err
	}

	return seq, true, tx.Commit()
}

// PutChunks indexes chunks against an existing event — the repair path, used for
// a session stored before chunking existed or one whose indexing was interrupted.
//
// Idempotent: existing chunks for the event are cleared first. A repair that
// appended would silently degrade ranking, since the same passage would outrank
// everything else merely by appearing several times.
func (d *DB) PutChunks(ctx context.Context, eventSeq int64, source, scope, occurredAt string, chunks []ChunkRow) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks_fts WHERE event_seq = ?`, eventSeq); err != nil {
		return fmt.Errorf("clear chunks: %w", err)
	}
	if err := insertChunks(ctx, tx, eventSeq, source, scope, occurredAt, chunks); err != nil {
		return err
	}
	return tx.Commit()
}

// execer is satisfied by both *sql.DB and *sql.Tx so chunk insertion can run
// inside the capture transaction or standalone during a repair.
type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertChunks(ctx context.Context, x execer, eventSeq int64, source, scope, occurredAt string, chunks []ChunkRow) error {
	for _, c := range chunks {
		if _, err := x.ExecContext(ctx, `
			INSERT INTO chunks_fts (body, event_seq, chunk_index, line_start, line_end, kind, source, scope, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Text, eventSeq, c.Index, c.LineStart, c.LineEnd, c.Kind, source, scope, occurredAt); err != nil {
			return fmt.Errorf("index chunk %d: %w", c.Index, err)
		}
	}
	return nil
}

// ChunkCount reports how many chunks are indexed for an event. Zero on an event
// that exists is exactly the upgrade case: captured by an older build, never
// indexed, invisible to search until backfilled.
func (d *DB) ChunkCount(ctx context.Context, eventSeq int64) (int, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM chunks_fts WHERE event_seq = ?`, eventSeq).Scan(&n)
	return n, err
}

// ChunkHit is one chunk-level search result, carrying enough provenance to point
// back at exact bytes in the blob on disk.
type ChunkHit struct {
	EventSeq   int64
	ChunkIndex int
	LineStart  int
	LineEnd    int
	Kind       string
	Source     string
	Scope      string
	OccurredAt string
	Body       string
	Excerpt    string
	Score      float64
	// Adjacent marks a chunk pulled in by window expansion rather than by matching
	// the query itself. An agent is told the difference so it can weight them.
	Adjacent bool
}

// Ref is the citation form used in packets: "chunk:<eventSeq>:<chunkIndex>".
func (h ChunkHit) Ref() string {
	return fmt.Sprintf("chunk:%d:%d", h.EventSeq, h.ChunkIndex)
}

// Ref addresses a passage, or a whole session when ChunkIndex is absent.
//
// A citation format with no parser is decoration. Every ref this binary hands
// out — in a packet, in a search result — has to come back in and resolve to the
// bytes it names, or the provenance it advertises is unverifiable in practice.
type Ref struct {
	EventSeq   int64
	ChunkIndex int
	HasChunk   bool // false for a whole-session ref
}

func (r Ref) String() string {
	if r.HasChunk {
		return fmt.Sprintf("chunk:%d:%d", r.EventSeq, r.ChunkIndex)
	}
	return fmt.Sprintf("session:%d", r.EventSeq)
}

// ParseRef accepts the forms a person or an agent actually has in hand:
//
//	chunk:12:3    a packet citation, pasted back verbatim
//	session:12    a whole session
//	12            the same, for someone typing rather than pasting
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Ref{}, errors.New("empty ref")
	}

	switch parts := strings.Split(s, ":"); {
	case len(parts) == 1:
		seq, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return Ref{}, fmt.Errorf("not a ref: %q (want chunk:<seq>:<index>, session:<seq> or <seq>)", s)
		}
		return Ref{EventSeq: seq}, nil

	case parts[0] == "session" && len(parts) == 2:
		seq, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return Ref{}, fmt.Errorf("bad session ref %q: %v", s, err)
		}
		return Ref{EventSeq: seq}, nil

	case parts[0] == "chunk" && len(parts) == 3:
		seq, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return Ref{}, fmt.Errorf("bad chunk ref %q: %v", s, err)
		}
		idx, err := strconv.Atoi(parts[2])
		if err != nil {
			return Ref{}, fmt.Errorf("bad chunk ref %q: %v", s, err)
		}
		return Ref{EventSeq: seq, ChunkIndex: idx, HasChunk: true}, nil

	default:
		return Ref{}, fmt.Errorf("not a ref: %q (want chunk:<seq>:<index>, session:<seq> or <seq>)", s)
	}
}

// SessionInfo is what the log knows about one captured session, addressed by the
// event seq that every ref carries.
type SessionInfo struct {
	Seq         int64
	Source      string
	Scope       string
	OccurredAt  string
	ContentHash string
	Record      SessionRecord
}

// Session loads one session by event seq.
//
// The blob is located from ContentHash via BlobPath, never from the event's
// stored pointer. Events are append-only by DDL trigger, so pointers written by
// builds that named the blob wrongly cannot be corrected in place — but the
// content hash was always right, and content-addressing means the hash is the
// real address. The pointer column is a record of what the writer believed at
// the time; the hash is how you find the bytes.
func (d *DB) Session(ctx context.Context, seq int64) (SessionInfo, error) {
	var s SessionInfo
	var scope, hash sql.NullString
	var payload string
	err := d.sql.QueryRowContext(ctx, `
		SELECT seq, source, scope, occurred_at, content_hash, payload
		FROM events WHERE seq = ? AND kind = ?`, seq, KindSessionCaptured).
		Scan(&s.Seq, &s.Source, &scope, &s.OccurredAt, &hash, &payload)
	if err == sql.ErrNoRows {
		return s, fmt.Errorf("no session with seq %d", seq)
	}
	if err != nil {
		return s, fmt.Errorf("load session: %w", err)
	}
	s.Scope, s.ContentHash = scope.String, hash.String
	if err := json.Unmarshal([]byte(payload), &s.Record); err != nil {
		return s, fmt.Errorf("decode session %d: %w", seq, err)
	}
	return s, nil
}

// PointerFor returns the blob path the log recorded for an event.
//
// This is the writer's claim about where the bytes went, not the address the
// reader uses — that is BlobPath(ContentHash). The two must agree, and there is
// a test that says so. Exposed for that test and for diagnostics; resolving a
// ref should go through ContentHash, which cannot go stale.
func (d *DB) PointerFor(ctx context.Context, seq int64) (string, error) {
	var pointer sql.NullString
	err := d.sql.QueryRowContext(ctx, `SELECT pointer FROM events WHERE seq = ?`, seq).Scan(&pointer)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no event with seq %d", seq)
	}
	return pointer.String, err
}

// Sessions loads several sessions at once, keyed by seq. Missing seqs are simply
// absent from the map — a search result whose event vanished should degrade to a
// missing title, not to a failed search.
func (d *DB) Sessions(ctx context.Context, seqs []int64) (map[int64]SessionInfo, error) {
	out := make(map[int64]SessionInfo, len(seqs))
	if len(seqs) == 0 {
		return out, nil
	}

	seen := make(map[int64]bool, len(seqs))
	placeholders := make([]string, 0, len(seqs))
	args := make([]any, 0, len(seqs)+1)
	args = append(args, KindSessionCaptured)
	for _, seq := range seqs {
		if seen[seq] {
			continue
		}
		seen[seq] = true
		placeholders = append(placeholders, "?")
		args = append(args, seq)
	}

	rows, err := d.sql.QueryContext(ctx, `
		SELECT seq, source, scope, occurred_at, content_hash, payload
		FROM events WHERE kind = ? AND seq IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var s SessionInfo
		var scope, hash sql.NullString
		var payload string
		if err := rows.Scan(&s.Seq, &s.Source, &scope, &s.OccurredAt, &hash, &payload); err != nil {
			return nil, err
		}
		s.Scope, s.ContentHash = scope.String, hash.String
		// A session whose payload will not decode still has usable identity, so
		// keep it rather than failing the whole lookup.
		_ = json.Unmarshal([]byte(payload), &s.Record)
		out[s.Seq] = s
	}
	return out, rows.Err()
}

// RecentSessions lists captured sessions newest first.
//
// Distinct from RecentChunks, which answers "what passages are recent" for a
// retrieval fallback. This answers "what did I work on", which is the question
// an interactive browser opens on — the corpus as a list of sessions, not as a
// bag of windows over them.
func (d *DB) RecentSessions(ctx context.Context, repo string, limit int) ([]SessionInfo, error) {
	if limit <= 0 {
		limit = 50
	}
	sqlText := `
		SELECT seq, source, scope, occurred_at, content_hash, payload
		FROM events WHERE kind = ?`
	args := []any{KindSessionCaptured}
	if repo != "" {
		sqlText += ` AND scope = ?`
		args = append(args, repo)
	}
	sqlText += ` ORDER BY occurred_at DESC, seq DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("recent sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionInfo
	for rows.Next() {
		var s SessionInfo
		var scope, hash sql.NullString
		var payload string
		if err := rows.Scan(&s.Seq, &s.Source, &scope, &s.OccurredAt, &hash, &payload); err != nil {
			return nil, err
		}
		s.Scope, s.ContentHash = scope.String, hash.String
		// As in Sessions: a session whose payload will not decode still has usable
		// identity, so keep it rather than failing the whole listing.
		_ = json.Unmarshal([]byte(payload), &s.Record)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Chunk loads one indexed passage by ref.
func (d *DB) Chunk(ctx context.Context, eventSeq int64, chunkIndex int) (ChunkHit, error) {
	var h ChunkHit
	var scope, occurred sql.NullString
	err := d.sql.QueryRowContext(ctx, `
		SELECT event_seq, chunk_index, line_start, line_end, kind, source, scope, occurred_at, body
		FROM chunks_fts WHERE event_seq = ? AND chunk_index = ?`, eventSeq, chunkIndex).
		Scan(&h.EventSeq, &h.ChunkIndex, &h.LineStart, &h.LineEnd, &h.Kind,
			&h.Source, &scope, &occurred, &h.Body)
	if err == sql.ErrNoRows {
		return h, fmt.Errorf("no chunk %d:%d", eventSeq, chunkIndex)
	}
	if err != nil {
		return h, fmt.Errorf("load chunk: %w", err)
	}
	h.Scope, h.OccurredAt = scope.String, occurred.String
	return h, nil
}

// ReadBlob returns the scrubbed transcript for a content hash, decompressed and
// split into lines. Line N of the result is line N+1 by the 1-based numbering
// that chunks and segments use, so a LineStart/LineEnd pair indexes it directly.
func (d *DB) ReadBlob(contentHash string) ([]string, error) {
	path := d.BlobPath(contentHash)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open blob: %w", err)
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read blob %s: %w", path, err)
	}
	defer zr.Close()

	// Transcript lines routinely exceed bufio.Scanner's default 64KB limit — a
	// single tool result carrying a file's contents is one line. Scanning with the
	// default silently truncates the session at the first big line.
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), maxBlobLine)

	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read blob %s: %w", path, err)
	}
	return lines, nil
}

// maxBlobLine caps a single transcript record. Tool outputs are the long ones;
// 16MB is far past anything observed and still bounded.
const maxBlobLine = 16 << 20

// SearchChunks runs FTS over transcript bodies.
//
// kind filters to errors only when set to "error", which is how a packet builds
// its corrections section — the question "what went wrong when we tried this
// before" is worth asking separately from "what do we know about this".
func (d *DB) SearchChunks(ctx context.Context, query, repo, kind string, sinceDays, limit int) ([]ChunkHit, error) {
	if limit <= 0 {
		limit = 12
	}
	match := BuildMatchQuery(query)
	if match == "" {
		return nil, nil
	}

	sqlText := `
		SELECT event_seq, chunk_index, line_start, line_end, kind, source, scope, occurred_at,
		       body, snippet(chunks_fts, 0, '', '', ' … ', 28) AS excerpt, bm25(chunks_fts) AS score
		FROM chunks_fts
		WHERE chunks_fts MATCH ?`
	args := []any{match}
	if repo != "" {
		sqlText += ` AND scope = ?`
		args = append(args, repo)
	}
	if kind != "" {
		sqlText += ` AND kind = ?`
		args = append(args, kind)
	}
	// The packet reports sinceDays, so retrieval has to honor it. Reporting a
	// window that is not applied is worse than having no window: the agent is told
	// the evidence is recent when it may be a year old, and provenance it cannot
	// trust is provenance it will start ignoring.
	if sinceDays > 0 {
		sqlText += ` AND occurred_at >= ?`
		args = append(args, cutoff(sinceDays))
	}
	sqlText += ` ORDER BY score LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("search chunks: %w", err)
	}
	defer rows.Close()

	var out []ChunkHit
	for rows.Next() {
		var h ChunkHit
		var scope, occurred sql.NullString
		if err := rows.Scan(&h.EventSeq, &h.ChunkIndex, &h.LineStart, &h.LineEnd, &h.Kind,
			&h.Source, &scope, &occurred, &h.Body, &h.Excerpt, &h.Score); err != nil {
			return nil, err
		}
		h.Scope, h.OccurredAt = scope.String, occurred.String
		h.Score = -h.Score
		out = append(out, h)
	}
	return out, rows.Err()
}

// RecentChunks returns the most recent passages regardless of query.
//
// This is what a recency fallback actually serves. A packet labelled
// recency_fallback that carries nothing is a lie in the packet's own vocabulary:
// the field states that recent evidence was substituted for a failed match, so
// there had better be evidence.
func (d *DB) RecentChunks(ctx context.Context, repo string, sinceDays, limit int) ([]ChunkHit, error) {
	if limit <= 0 {
		limit = 8
	}
	sqlText := `
		SELECT event_seq, chunk_index, line_start, line_end, kind, source, scope, occurred_at, body
		FROM chunks_fts WHERE 1=1`
	var args []any
	if repo != "" {
		sqlText += ` AND scope = ?`
		args = append(args, repo)
	}
	if sinceDays > 0 {
		sqlText += ` AND occurred_at >= ?`
		args = append(args, cutoff(sinceDays))
	}
	// chunk_index 0 opens a session — the user's ask and the first move — which is
	// the most useful single window when there is nothing better to go on.
	sqlText += ` AND chunk_index = 0 ORDER BY occurred_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("recent chunks: %w", err)
	}
	defer rows.Close()

	var out []ChunkHit
	for rows.Next() {
		var h ChunkHit
		var scope, occurred sql.NullString
		if err := rows.Scan(&h.EventSeq, &h.ChunkIndex, &h.LineStart, &h.LineEnd, &h.Kind,
			&h.Source, &scope, &occurred, &h.Body); err != nil {
			return nil, err
		}
		h.Scope, h.OccurredAt = scope.String, occurred.String
		h.Excerpt = firstChars(h.Body, 280)
		out = append(out, h)
	}
	return out, rows.Err()
}

// cutoff renders the RFC3339 lower bound for a day window. Timestamps are stored
// as RFC3339 UTC strings, which sort lexicographically in the same order they sort
// chronologically — so a string comparison is a correct time comparison.
func cutoff(sinceDays int) string {
	return time.Now().UTC().AddDate(0, 0, -sinceDays).Format(time.RFC3339)
}

// ExpandWindow pulls the chunks immediately before and after each hit.
//
// Retrieval finds the chunk containing the match, but the sentence that explains
// it is often in the neighbour — a decision is stated in one window and its
// reason in the next. Without expansion, evidence reads as disconnected
// fragments; with it, as passages.
//
// Adjacent chunks inherit a fraction of their anchor's score so they rank near it
// without displacing genuine matches, and are flagged Adjacent so the agent knows
// which chunks actually matched.
func (d *DB) ExpandWindow(ctx context.Context, hits []ChunkHit, factor float64) ([]ChunkHit, error) {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Ref()] = true
	}

	out := append([]ChunkHit(nil), hits...)
	for _, h := range hits {
		for _, idx := range []int{h.ChunkIndex - 1, h.ChunkIndex + 1} {
			if idx < 0 {
				continue
			}
			ref := fmt.Sprintf("chunk:%d:%d", h.EventSeq, idx)
			if seen[ref] {
				continue
			}
			var n ChunkHit
			var scope, occurred sql.NullString
			err := d.sql.QueryRowContext(ctx, `
				SELECT event_seq, chunk_index, line_start, line_end, kind, source, scope, occurred_at, body
				FROM chunks_fts WHERE event_seq = ? AND chunk_index = ?`, h.EventSeq, idx).
				Scan(&n.EventSeq, &n.ChunkIndex, &n.LineStart, &n.LineEnd, &n.Kind,
					&n.Source, &scope, &occurred, &n.Body)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return out, err
			}
			n.Scope, n.OccurredAt = scope.String, occurred.String
			n.Score = h.Score * factor
			n.Adjacent = true
			n.Excerpt = firstChars(n.Body, 280)
			seen[ref] = true
			out = append(out, n)
		}
	}
	return out, nil
}

func firstChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + " …"
}

// HasContent reports whether this exact content hash is already in the log. Used
// by a dry run, which must answer "would this be captured?" honestly without
// writing anything.
func (d *DB) HasContent(ctx context.Context, contentHash string) (bool, error) {
	var n int
	err := d.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE content_hash = ? AND kind = ?`,
		contentHash, KindSessionCaptured).Scan(&n)
	return n > 0, err
}

// Hit is one search result.
type Hit struct {
	Seq        int64
	Source     string
	Scope      string
	OccurredAt string
	Title      string
	Excerpt    string
	Score      float64
}

// BuildMatchQuery turns user input into a safe FTS5 MATCH expression.
//
// This is not optional sanitization. FTS5 treats its input as a query LANGUAGE,
// so ordinary developer vocabulary is a syntax error or, worse, silently means
// something else:
//
//	kai-214    → "kai" NOT column "214"  → error: no such column: 214
//	C++        → error: syntax error near "+"
//	what's     → error: syntax error near "'"
//	trailing-  → error: syntax error near ""
//
// Ticket ids, package names and error strings are precisely what anyone searches
// a transcript corpus for, so passing raw input through makes search fail on the
// queries that matter most.
//
// Bare terms are wrapped in double quotes, which makes FTS5 read them as phrase
// literals and tokenize them internally — so "kai-214" matches the same text the
// indexer stored. The documented operators (AND, OR, NOT) and explicit "quoted
// phrases" are preserved, so the power users the help text promises still work.
func BuildMatchQuery(input string) string {
	var out []string
	for _, tok := range tokenizeQuery(input) {
		switch {
		case tok == "":
			continue
		case tok == "AND", tok == "OR", tok == "NOT":
			out = append(out, tok)
		case strings.HasPrefix(tok, `"`):
			out = append(out, tok) // already a phrase literal
		default:
			out = append(out, `"`+strings.ReplaceAll(tok, `"`, `""`)+`"`)
		}
	}
	return strings.Join(out, " ")
}

// tokenizeQuery splits on whitespace while keeping "quoted phrases" intact.
func tokenizeQuery(s string) []string {
	var toks []string
	var cur strings.Builder
	inQuotes := false

	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			cur.WriteRune(r)
			if inQuotes {
				flush()
			}
			inQuotes = !inQuotes
		case (r == ' ' || r == '\t' || r == '\n') && !inQuotes:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if inQuotes {
		cur.WriteRune('"') // unbalanced quote — close it rather than erroring
	}
	flush()
	return toks
}

// Search runs an FTS5 query. bm25 returns lower-is-better, so it is negated to
// make Score read the way callers expect.
//
// This is lexical search, deliberately: it costs nothing, leaks nothing, needs no
// model, and cannot hallucinate. Exact identifiers, file names and error messages
// are what agents actually look for.
func (d *DB) Search(ctx context.Context, query string, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	match := BuildMatchQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT seq, source, scope, occurred_at, title,
		       snippet(events_fts, 1, '', '', ' … ', 24) AS excerpt,
		       bm25(events_fts) AS score
		FROM events_fts
		WHERE events_fts MATCH ?
		ORDER BY score
		LIMIT ?`, match, limit)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var out []Hit
	for rows.Next() {
		var h Hit
		var scope, occurred sql.NullString
		if err := rows.Scan(&h.Seq, &h.Source, &scope, &occurred, &h.Title, &h.Excerpt, &h.Score); err != nil {
			return nil, err
		}
		h.Scope, h.OccurredAt = scope.String, occurred.String
		h.Score = -h.Score
		out = append(out, h)
	}
	return out, rows.Err()
}

// Cursor returns the sweep watermark for a source, 0 when never set.
func (d *DB) Cursor(ctx context.Context, source string) (int64, error) {
	var ms int64
	err := d.sql.QueryRowContext(ctx, `SELECT last_mtime_ms FROM cursors WHERE source = ?`, source).Scan(&ms)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return ms, err
}

// SetCursor advances the watermark for a source.
//
// Callers must clamp before calling: readdir returns directory order, not mtime
// order, so a file that fails is routinely followed by a NEWER file that succeeds
// and would drag the watermark past it — leaving the failure below the cursor,
// skipped on every future sweep, silently lost. See watch.Clamp.
func (d *DB) SetCursor(ctx context.Context, source string, mtimeMS int64) error {
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO cursors (source, last_mtime_ms, updated_at) VALUES (?, ?, datetime('now'))
		ON CONFLICT(source) DO UPDATE SET last_mtime_ms = excluded.last_mtime_ms, updated_at = excluded.updated_at`,
		source, mtimeMS)
	return err
}

// Stats summarizes the corpus for `shale status`.
type Stats struct {
	Events       int
	Sessions     int
	Repos        int
	Chunks       int
	Memories     int
	Proposals    int
	Checkpoints  int
	Runbooks     int
	Instructions int
	Sources      int
	OldestAt     string
	NewestAt     string
	TotalTurns   int
}

func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := d.sql.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM events),
		  (SELECT count(*) FROM events WHERE kind = ?),
		  (SELECT count(DISTINCT scope) FROM events WHERE kind = ? AND scope IS NOT NULL AND scope != ''),
		  (SELECT count(*) FROM chunks_fts),
		  (SELECT count(*) FROM artifacts WHERE kind = 'memory' AND status = 'active'),
		  (SELECT count(*) FROM artifacts WHERE kind = 'memory' AND status = 'pending'),
		  (SELECT count(*) FROM artifacts WHERE kind = 'checkpoint' AND status = 'active'),
		  (SELECT count(*) FROM artifacts WHERE kind = 'runbook' AND status = 'active'),
		  (SELECT count(*) FROM artifacts WHERE kind = 'instruction' AND status = 'active'),
		  (SELECT count(*) FROM artifact_sources),
		  (SELECT coalesce(min(occurred_at), '') FROM events),
		  (SELECT coalesce(max(occurred_at), '') FROM events)`,
		KindSessionCaptured, KindSessionCaptured,
	).Scan(&s.Events, &s.Sessions, &s.Repos, &s.Chunks, &s.Memories,
		&s.Proposals, &s.Checkpoints, &s.Runbooks, &s.Instructions, &s.Sources,
		&s.OldestAt, &s.NewestAt)
	return s, err
}
