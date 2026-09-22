package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/store"
)

func TestPrepareExcludesFutureTargetVersionsAndExpiredEvidence(t *testing.T) {
	dir := t.TempDir()
	db, e := store.Open(filepath.Join(dir, "store"))
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Truncate(time.Second)
	rows := []struct {
		key, text string
		end       time.Time
	}{
		{"prior", "signing certificate prior fix", now.Add(-time.Hour)},
		{"target", "signing forbidden earlier capture of target", now.Add(-2 * time.Hour)},
		{"future", "signing forbidden future", now.Add(time.Hour)},
		{"expired", "signing forbidden expired", now.AddDate(0, 0, -31)},
	}
	for i, r := range rows {
		_, _, e = db.PutSession(context.Background(), r.key, store.SessionRecord{Source: "test", SourceKey: r.key, Repo: "acme/app", StartedAt: r.end.Add(-time.Minute), EndedAt: r.end}, []store.ChunkRow{{Index: 0, LineStart: 1, LineEnd: 1, Text: r.text}, {Index: 1, LineStart: 2, LineEnd: 2, Text: r.text + " detail"}})
		if e != nil {
			t.Fatal(i, e)
		}
	}
	db.Close()
	tasks := map[string]any{"corpusFingerprint": "test", "mode": "historical_transcript_only", "cases": []map[string]any{{"id": "case", "task": "signing", "repo": "acme/app", "sourceKey": "target", "at": now.Format(time.RFC3339Nano), "split": "heldout", "taskSourceRef": "chunk:2:0"}}}
	if e = writeJSON(filepath.Join(dir, "tasks.json"), tasks); e != nil {
		t.Fatal(e)
	}
	if e = prepare([]string{"--dir", dir}); e != nil {
		t.Fatal(e)
	}
	suite, e := benchmark.ReadSuite(filepath.Join(dir, "suite.json"))
	if e != nil {
		t.Fatal(e)
	}
	if len(suite.Cases) != 1 {
		t.Fatalf("cases: %d", len(suite.Cases))
	}
	for _, entry := range benchmark.Entries(&suite.Cases[0].Candidates.Packet) {
		if strings.Contains(entry.Content, "forbidden") {
			t.Fatal("historical leakage", entry.Ref)
		}
	}
	if suite.Cases[0].Reviewed {
		t.Fatal("draft labeled as reviewed")
	}
}
