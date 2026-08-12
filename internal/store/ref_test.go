package store

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestBlob lays down a gzipped blob the way capture does. The store package
// cannot import watch — watch imports store — so the few lines are repeated here
// rather than shared.
func writeTestBlob(t *testing.T, path, body string) error {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := io.WriteString(zw, body); err != nil {
		return err
	}
	return zw.Close()
}

func TestParseRefAcceptsEveryFormSomeoneHasInHand(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
	}{
		// The packet citation form, pasted back verbatim.
		{"chunk:12:3", Ref{EventSeq: 12, ChunkIndex: 3, HasChunk: true}},
		{"chunk:1:0", Ref{EventSeq: 1, ChunkIndex: 0, HasChunk: true}},
		// A whole session, spelled out or bare.
		{"session:12", Ref{EventSeq: 12}},
		{"12", Ref{EventSeq: 12}},
		// Whitespace survives a copy-paste out of a terminal.
		{"  chunk:12:3  ", Ref{EventSeq: 12, ChunkIndex: 3, HasChunk: true}},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if err != nil {
			t.Errorf("ParseRef(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestParseRefRejectsWhatIsNotARef(t *testing.T) {
	for _, in := range []string{"", "   ", "nonsense", "chunk:12", "chunk:12:3:4", "chunk:a:b", "session:", "session:x", "12.5"} {
		if got, err := ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) = %+v, want an error", in, got)
		}
	}
}

// Refs round-trip: whatever a packet or a search result prints has to parse back
// into the thing that printed it. A citation format only works if both ends
// agree, and they are written in different packages.
func TestRefRoundTripsThroughItsPrintedForm(t *testing.T) {
	hit := ChunkHit{EventSeq: 42, ChunkIndex: 7}
	parsed, err := ParseRef(hit.Ref())
	if err != nil {
		t.Fatalf("ParseRef(%q): %v", hit.Ref(), err)
	}
	if parsed.EventSeq != hit.EventSeq || parsed.ChunkIndex != hit.ChunkIndex || !parsed.HasChunk {
		t.Fatalf("round trip lost information: %+v", parsed)
	}
	if parsed.String() != hit.Ref() {
		t.Errorf("Ref.String() = %q, want %q", parsed.String(), hit.Ref())
	}
}

// The reason `shale search` moved off the digest index.
//
// There is one digest per session and many chunks, so the digest index holds a
// few percent of what was captured. Text that is plainly in a transcript is
// simply not in it — and the CLI reported that as a confident "no matches" while
// the MCP server, reading the chunk index, found it. Two surfaces, two answers,
// no way to tell from the outside. This pins the asymmetry so nothing points the
// CLI back at the wrong index.
func TestDigestIndexDoesNotContainWhatTheChunkIndexDoes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	_, inserted, err := db.PutSession(ctx, "hash-x", SessionRecord{
		Source: "claude_code", SourceKey: "hash-x",
		Title:  "kairos: spinning out modules",
		Digest: "assessed whether the platform could be whitelabeled",
		Repo:   "acme/app", EndedAt: time.Now().UTC(),
	}, []ChunkRow{{
		Index: 0, LineStart: 1, LineEnd: 9, Kind: "transcript",
		Text: "ConnectSX publishes per-seat pricing of $75 to $250 per user per month",
	}})
	if err != nil || !inserted {
		t.Fatalf("put: inserted=%v err=%v", inserted, err)
	}

	digestHits, err := db.Search(ctx, "per-seat pricing", 10)
	if err != nil {
		t.Fatalf("digest search: %v", err)
	}
	if len(digestHits) != 0 {
		t.Fatalf("digest index unexpectedly matched; the premise of this test is stale")
	}

	chunkHits, err := db.SearchChunks(ctx, "per-seat pricing", "", "", 0, 10)
	if err != nil {
		t.Fatalf("chunk search: %v", err)
	}
	if len(chunkHits) != 1 {
		t.Fatalf("chunk index found %d hits, want 1 — the CLI would report 'no matches' for text it holds", len(chunkHits))
	}
	if chunkHits[0].Ref() != "chunk:1:0" {
		t.Errorf("ref = %q, want chunk:1:0", chunkHits[0].Ref())
	}
}

// A ref is only worth printing if it resolves. This walks the whole path a
// citation takes: search mints a ref, the ref parses, and both the passage and
// the session it names come back.
func TestARefFromSearchResolvesToItsPassageAndSession(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seq, _, err := db.PutSession(ctx, "hash-y", SessionRecord{
		Source: "claude_code", SourceKey: "hash-y",
		Title:  "shale: capture policy",
		Digest: "decided where capture policy lives",
		Repo:   "acme/shale", Turns: 15, EndedAt: time.Now().UTC(),
	}, []ChunkRow{
		{Index: 0, LineStart: 1, LineEnd: 9, Kind: "transcript", Text: "opening the session"},
		{Index: 1, LineStart: 10, LineEnd: 19, Kind: "transcript", Text: "the goreleaser signing step failed"},
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	hits, err := db.SearchChunks(ctx, "goreleaser", "", "", 0, 10)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %d hits, err=%v", len(hits), err)
	}

	ref, err := ParseRef(hits[0].Ref())
	if err != nil {
		t.Fatalf("parse minted ref: %v", err)
	}

	chunk, err := db.Chunk(ctx, ref.EventSeq, ref.ChunkIndex)
	if err != nil {
		t.Fatalf("resolve chunk: %v", err)
	}
	if chunk.LineStart != 10 || chunk.LineEnd != 19 {
		t.Errorf("lines %d–%d, want 10–19", chunk.LineStart, chunk.LineEnd)
	}

	info, err := db.Session(ctx, ref.EventSeq)
	if err != nil {
		t.Fatalf("resolve session: %v", err)
	}
	if info.Seq != seq || info.Record.Title != "shale: capture policy" || info.Record.Turns != 15 {
		t.Errorf("wrong session: %+v", info)
	}
	if info.ContentHash != "hash-y" {
		t.Errorf("content hash = %q, want hash-y", info.ContentHash)
	}
}

func TestSessionsBatchLookupSkipsWhatIsMissing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	seed(t, db, "aaa", "one", "first session", "acme/a")
	seed(t, db, "bbb", "two", "second session", "acme/b")

	// Duplicate and unknown seqs are both things a caller hands over routinely:
	// several hits land in one session, and an event can be gone.
	got, err := db.Sessions(ctx, []int64{1, 2, 2, 9999})
	if err != nil {
		t.Fatalf("sessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2", len(got))
	}
	if got[1].Record.Title != "one" || got[2].Record.Title != "two" {
		t.Errorf("wrong titles: %q, %q", got[1].Record.Title, got[2].Record.Title)
	}
	if _, ok := got[9999]; ok {
		t.Error("a missing seq should be absent, not zero-valued")
	}
}

func TestReadBlobRoundTripsALongLine(t *testing.T) {
	db := newTestDB(t)

	// Tool results carrying a whole file are one line and routinely exceed
	// bufio.Scanner's 64KB default, which truncates the session silently.
	long := make([]byte, 300_000)
	for i := range long {
		long[i] = byte('a' + i%26)
	}
	body := "first line\n" + string(long) + "\nlast line"

	if err := writeTestBlob(t, db.BlobPath("hash-z"), body); err != nil {
		t.Fatalf("write: %v", err)
	}

	lines, err := db.ReadBlob("hash-z")
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(lines))
	}
	if len(lines[1]) != len(long) {
		t.Errorf("long line truncated: %d bytes, want %d", len(lines[1]), len(long))
	}
	if lines[2] != "last line" {
		t.Errorf("last line = %q", lines[2])
	}
}

func TestReadBlobOnAMissingBlobSaysSo(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.ReadBlob("nope"); err == nil {
		t.Fatal("expected an error for a blob that is not on disk")
	}
}
