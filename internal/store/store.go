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
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Kind values for events. Open set: a new kind is additive, never a schema change.
const (
	KindSessionCaptured = "session.captured"
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
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
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

func (d *DB) Close() error { return d.sql.Close() }

// BlobPath is where a scrubbed transcript with this content hash lives.
// Content-addressing means a re-captured session that hasn't changed costs
// nothing, and a grown session shares every unchanged byte with its earlier form.
func (d *DB) BlobPath(contentHash string) string {
	if len(contentHash) < 2 {
		return filepath.Join(d.root, "blobs", contentHash+".jsonl")
	}
	return filepath.Join(d.root, "blobs", contentHash[:2], contentHash+".jsonl")
}

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

-- Per-source replication and sweep watermarks. Deliberately NOT in events: a
-- cursor is mutable state about the log, not a fact that happened.
CREATE TABLE IF NOT EXISTS cursors (
  source        TEXT PRIMARY KEY,
  last_mtime_ms INTEGER NOT NULL,
  updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);
`

func (d *DB) migrate() error {
	if _, err := d.sql.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
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

// PutSession appends a session.captured event and indexes it for search.
//
// Idempotent on content_hash: re-capturing an unchanged session is a no-op, which
// is what makes the sweep safe to run as often as you like. A GROWN session has a
// different hash and is appended as a new event — the log keeps both, because
// "what did this session look like at 3pm" is a real question once a hub is
// joining across machines.
func (d *DB) PutSession(ctx context.Context, contentHash string, rec SessionRecord) (inserted bool, err error) {
	var exists int
	err = d.sql.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE content_hash = ? AND kind = ?`,
		contentHash, KindSessionCaptured).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("dedupe check: %w", err)
	}
	if exists > 0 {
		return false, nil
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("encode payload: %w", err)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return false, err
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

	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, kind, source, actor, occurred_at, scope, pointer, content_hash, payload)
		VALUES (?, ?, ?, 'agent', ?, ?, ?, ?, ?)`,
		contentHash+":"+rec.SourceKey, KindSessionCaptured, rec.Source,
		occurred.UTC().Format(time.RFC3339), scope, d.BlobPath(contentHash), contentHash, string(payload),
	)
	if err != nil {
		return false, fmt.Errorf("insert event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO events_fts (title, body, seq, source, scope, occurred_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		rec.Title, rec.Digest, seq, rec.Source, scope, occurred.UTC().Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf("index: %w", err)
	}

	return true, tx.Commit()
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
	Events     int
	Sessions   int
	Repos      int
	OldestAt   string
	NewestAt   string
	TotalTurns int
}

func (d *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := d.sql.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM events),
		  (SELECT count(*) FROM events WHERE kind = ?),
		  (SELECT count(DISTINCT scope) FROM events WHERE scope IS NOT NULL AND scope != ''),
		  (SELECT coalesce(min(occurred_at), '') FROM events),
		  (SELECT coalesce(max(occurred_at), '') FROM events)`,
		KindSessionCaptured,
	).Scan(&s.Events, &s.Sessions, &s.Repos, &s.OldestAt, &s.NewestAt)
	return s, err
}
