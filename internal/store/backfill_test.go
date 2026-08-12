package store

import (
	"context"
	"testing"
	"time"
)

func rec(hash string) SessionRecord {
	return SessionRecord{
		Source: "claude_code", SourceKey: hash, Title: "t", Digest: "d",
		Repo: "acme/app", EndedAt: time.Now().UTC(),
	}
}

func chunkSet(n int) []ChunkRow {
	out := make([]ChunkRow, n)
	for i := range out {
		out[i] = ChunkRow{Index: i, LineStart: i*10 + 1, LineEnd: i*10 + 9,
			Kind: "transcript", Text: "passage about goreleaser signing step"}
	}
	return out
}

// An upgrade is the normal case, not the exotic one: a session captured by an
// older build has an event but no chunks, and nothing else will ever offer that
// file again because dedupe is keyed on content hash. Without this the entire
// existing corpus stays invisible to the tool that was just installed to search
// it.
func TestChunksBackfillIntoAnAlreadyCapturedSession(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Captured by an "older build": event written, no chunks.
	seq, inserted, err := db.PutSession(ctx, "hash-a", rec("hash-a"), nil)
	if err != nil || !inserted {
		t.Fatalf("first put: seq=%d inserted=%v err=%v", seq, inserted, err)
	}
	if n, _ := db.ChunkCount(ctx, seq); n != 0 {
		t.Fatalf("expected 0 chunks, got %d", n)
	}

	// Re-capture must report the EXISTING seq so the caller can repair it. A zero
	// seq is what made this unfixable: there was nothing to attach chunks to.
	seq2, inserted2, err := db.PutSession(ctx, "hash-a", rec("hash-a"), chunkSet(3))
	if err != nil {
		t.Fatalf("second put: %v", err)
	}
	if inserted2 {
		t.Error("duplicate content hash reported as a new insert")
	}
	if seq2 != seq {
		t.Fatalf("duplicate returned seq %d, want the existing %d", seq2, seq)
	}

	if err := db.PutChunks(ctx, seq2, "claude_code", "acme/app",
		time.Now().UTC().Format(time.RFC3339), chunkSet(3)); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n, _ := db.ChunkCount(ctx, seq); n != 3 {
		t.Errorf("after backfill: %d chunks, want 3", n)
	}

	hits, err := db.SearchChunks(ctx, "goreleaser", "", "", 0, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Error("backfilled chunks are not searchable")
	}
}

// Event and chunks must land together. Committing them separately means a crash
// between the two leaves a session that looks captured but can never be searched,
// and dedupe guarantees it is never offered again.
func TestEventAndChunksAreOneTransaction(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seq, inserted, err := db.PutSession(ctx, "hash-b", rec("hash-b"), chunkSet(4))
	if err != nil || !inserted {
		t.Fatalf("put: %v", err)
	}
	if n, _ := db.ChunkCount(ctx, seq); n != 4 {
		t.Errorf("chunks = %d, want 4 committed alongside the event", n)
	}
}

// Backfilling twice must not double the index. A repair path that duplicates on
// every run degrades ranking silently — the same passage outranks everything
// simply because it appears five times.
func TestBackfillIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seq, _, err := db.PutSession(ctx, "hash-c", rec("hash-c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < 3; i++ {
		if err := db.PutChunks(ctx, seq, "claude_code", "acme/app", stamp, chunkSet(2)); err != nil {
			t.Fatalf("put chunks (round %d): %v", i, err)
		}
	}
	if n, _ := db.ChunkCount(ctx, seq); n != 2 {
		t.Errorf("chunks = %d after 3 backfills, want 2", n)
	}
}

// sinceDays is part of the packet contract. Reporting a window that retrieval
// does not apply is worse than having no window at all: the agent is told the
// evidence is recent when it may be a year old.
func TestSearchChunksAppliesTheTimeWindow(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	old := SessionRecord{Source: "claude_code", SourceKey: "old", Title: "old", Digest: "d",
		Repo: "acme/app", EndedAt: time.Now().AddDate(0, 0, -200).UTC()}
	recent := SessionRecord{Source: "claude_code", SourceKey: "new", Title: "new", Digest: "d",
		Repo: "acme/app", EndedAt: time.Now().AddDate(0, 0, -2).UTC()}

	seqOld, _, _ := db.PutSession(ctx, "h-old", old, nil)
	seqNew, _, _ := db.PutSession(ctx, "h-new", recent, nil)
	db.PutChunks(ctx, seqOld, "claude_code", "acme/app", old.EndedAt.Format(time.RFC3339), chunkSet(1))
	db.PutChunks(ctx, seqNew, "claude_code", "acme/app", recent.EndedAt.Format(time.RFC3339), chunkSet(1))

	all, err := db.SearchChunks(ctx, "goreleaser", "", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unbounded search returned %d, want 2", len(all))
	}

	windowed, err := db.SearchChunks(ctx, "goreleaser", "", "", 30, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(windowed) != 1 {
		t.Fatalf("30-day window returned %d, want 1", len(windowed))
	}
}

// A recency fallback that returns nothing is a lie in the packet's own
// vocabulary: the field says recent evidence was substituted, so there must be
// evidence.
func TestRecentChunksReturnsEvidence(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Five sessions, newest last. The fallback returns ONE opening window per
	// session rather than several windows from one session — breadth across recent
	// work is more useful than depth into whichever session happened to be last.
	for i := 0; i < 5; i++ {
		hash := "h" + string(rune('a'+i))
		r := rec(hash)
		r.EndedAt = time.Now().AddDate(0, 0, -(5 - i)).UTC()
		seq, _, err := db.PutSession(ctx, hash, r, chunkSet(4))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		if n, _ := db.ChunkCount(ctx, seq); n != 4 {
			t.Fatalf("seed %d: %d chunks, want 4", i, n)
		}
	}

	hits, err := db.RecentChunks(ctx, "", 0, 3)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d recent chunks, want 3 (one per session)", len(hits))
	}
	for _, h := range hits {
		if h.ChunkIndex != 0 {
			t.Errorf("chunk index %d; the fallback serves session-opening windows", h.ChunkIndex)
		}
		if h.Body == "" {
			t.Error("recent chunk carries no body")
		}
	}
	// Newest first — a fallback that returned the oldest sessions would be worse
	// than returning nothing.
	if hits[0].OccurredAt < hits[len(hits)-1].OccurredAt {
		t.Errorf("not ordered newest-first: %s then %s", hits[0].OccurredAt, hits[len(hits)-1].OccurredAt)
	}
}
