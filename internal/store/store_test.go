package store

import (
	"context"
	"testing"
	"time"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seed(t *testing.T, db *DB, hash, title, digest, repo string) {
	t.Helper()
	_, ok, err := db.PutSession(context.Background(), hash, SessionRecord{
		Source: "claude_code", SourceKey: hash, Title: title, Digest: digest,
		Repo: repo, EndedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("put %s: %v", hash, err)
	}
	if !ok {
		t.Fatalf("put %s reported duplicate on first insert", hash)
	}
}

func TestSearchRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seed(t, db, "aaa", "personal-platform-rails: planning", "why is this being done on main and not a worktree?", "acme/rails")
	seed(t, db, "bbb", "shale: capture", "the capture policy is owned by the app, not the collector", "acme/shale")

	hits, err := db.Search(ctx, "worktree", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1", len(hits))
	}
	if hits[0].Title != "personal-platform-rails: planning" {
		t.Errorf("wrong hit: %q", hits[0].Title)
	}
	if hits[0].Excerpt == "" {
		t.Error("expected a snippet excerpt")
	}
	if hits[0].Scope != "acme/rails" {
		t.Errorf("scope = %q, want acme/rails", hits[0].Scope)
	}
}

// Titles and repo names are full of hyphens. FTS5 treats a bare hyphen as a
// syntax error in a query, so anything that lets stored text reach the MATCH
// expression blows up on ordinary data.
func TestSearchHandlesHyphenatedAndPunctuatedQueries(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seed(t, db, "aaa", "personal-platform-rails: planning", "context about kai-214 and the pack-size units", "acme/rails")

	for _, q := range []string{
		"kai-214",
		"personal-platform-rails",
		"pack-size",
		`"pack-size units"`,
		"units OR nothing",
		"kai-214 AND units",
		"C++",
		"what's",
		"trailing-",
	} {
		t.Run(q, func(t *testing.T) {
			if _, err := db.Search(ctx, q, 5); err != nil {
				t.Errorf("Search(%q) errored: %v", q, err)
			}
		})
	}
}

func TestSearchEmptyResultIsNotAnError(t *testing.T) {
	db := newTestDB(t)
	hits, err := db.Search(context.Background(), "nonexistentterm", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("got %d hits, want 0", len(hits))
	}
}

// Re-capturing an unchanged session must be a no-op, which is what makes the
// sweep safe to run as often as you like.
func TestPutSessionIsIdempotentOnContentHash(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	seed(t, db, "aaa", "one", "body", "acme/x")

	_, ok, err := db.PutSession(ctx, "aaa", SessionRecord{
		Source: "claude_code", SourceKey: "aaa", Title: "one", Digest: "body", EndedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if ok {
		t.Error("second put with the same content hash reported as inserted")
	}

	seen, err := db.HasContent(ctx, "aaa")
	if err != nil || !seen {
		t.Errorf("HasContent = %v, %v; want true, nil", seen, err)
	}
}

// The log is a log. These guards live in DDL because application code is the
// thing most likely to be rewritten.
func TestEventsAreAppendOnly(t *testing.T) {
	db := newTestDB(t)
	seed(t, db, "aaa", "one", "body", "acme/x")

	if _, err := db.sql.Exec(`UPDATE events SET kind = 'tampered'`); err == nil {
		t.Error("UPDATE on events was allowed")
	}
	if _, err := db.sql.Exec(`DELETE FROM events`); err == nil {
		t.Error("DELETE on events was allowed")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	got, err := db.Cursor(ctx, "claude_code")
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if got != 0 {
		t.Errorf("unset cursor = %d, want 0", got)
	}

	if err := db.SetCursor(ctx, "claude_code", 1700); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, _ = db.Cursor(ctx, "claude_code"); got != 1700 {
		t.Errorf("cursor = %d, want 1700", got)
	}

	// Per-source: advancing one must never move another, or a narrowed run would
	// skip the untouched source's backlog forever.
	if got, _ = db.Cursor(ctx, "codex"); got != 0 {
		t.Errorf("codex cursor = %d, want 0 — sources must be independent", got)
	}
}
