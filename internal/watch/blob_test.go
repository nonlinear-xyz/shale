package watch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nonlinear-xyz/shale/internal/store"
)

// The blob name is built in one place and used in two, and for a while those two
// disagreed: store.BlobPath named a ".jsonl" that writeBlob then renamed to
// ".jsonl.gz". Every pointer recorded in the events table therefore named a file
// that had never existed, and because events are append-only by DDL trigger,
// none of them could be corrected afterwards. Nothing failed loudly — capture
// succeeded, search worked, and the provenance trail was simply wrong.
//
// This test exists at the seam where the two sides drifted: it writes a blob the
// way capture does, records an event the way capture does, and insists the path
// on disk and the path in the log are the same string.
func TestRecordedPointerNamesTheFileOnDisk(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const hash = "d3adb33fd3adb33fd3adb33fd3adb33fd3adb33fd3adb33fd3adb33fd3adb33f"
	const body = `{"type":"user","message":{"role":"user","content":"why is the pointer wrong?"}}`

	if err := writeBlob(db.BlobPath(hash), body); err != nil {
		t.Fatalf("write blob: %v", err)
	}

	if _, err := os.Stat(db.BlobPath(hash)); err != nil {
		t.Fatalf("BlobPath does not name the file writeBlob wrote: %v", err)
	}

	ctx := context.Background()
	seq, inserted, err := db.PutSession(ctx, hash, store.SessionRecord{
		Source: "claude_code", SourceKey: hash, Title: "pointer check",
		Digest: "checking the pointer", EndedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("put session: %v", err)
	}
	if !inserted {
		t.Fatal("first insert reported a duplicate")
	}

	info, err := db.Session(ctx, seq)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}

	// The pointer the log recorded has to be openable. This is the assertion that
	// failed silently in production for every session ever captured.
	pointer, err := db.PointerFor(ctx, seq)
	if err != nil {
		t.Fatalf("read pointer: %v", err)
	}
	if pointer != db.BlobPath(hash) {
		t.Errorf("recorded pointer %q, but the blob is at %q", pointer, db.BlobPath(hash))
	}
	if _, err := os.Stat(pointer); err != nil {
		t.Errorf("recorded pointer does not exist on disk: %v", err)
	}

	// And the bytes have to come back through the content hash, which is how
	// everything downstream of a ref actually resolves a blob.
	lines, err := db.ReadBlob(info.ContentHash)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if len(lines) != 1 || lines[0] != body {
		t.Errorf("round-trip mismatch: got %q", lines)
	}
}

// Blobs are content-addressed, so re-writing identical bytes must be a no-op
// rather than a rewrite. This broke quietly when the suffix moved: the existence
// check and the rename have to agree on the name, or every capture rewrites
// every blob it has already stored.
func TestWriteBlobSkipsContentItAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	const hash = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	path := db.BlobPath(hash)

	if err := writeBlob(path, "first"); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Same hash, different body: a real caller can never do this, but it proves
	// the skip happened rather than a rewrite that coincidentally matched.
	if err := writeBlob(path, "second"); err != nil {
		t.Fatalf("rewrite blob: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Error("writeBlob rewrote content it already had")
	}

	// No temp file may survive a successful write.
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Error("writeBlob left a .tmp file behind")
	}
}
