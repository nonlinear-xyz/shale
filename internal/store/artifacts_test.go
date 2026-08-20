package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactMigrationUpgradesVersionZeroStore(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "shale.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schema); err != nil {
		t.Fatalf("seed v0 schema: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer db.Close()
	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	if _, _, err := db.PutArtifact(context.Background(), ArtifactInput{
		Kind: ArtifactMemory, ScopeKind: ScopeUser,
		Content: ArtifactContent{Text: "prefer compact status output"},
	}); err != nil {
		t.Fatalf("new schema is not usable: %v", err)
	}
}

func TestMemoryLifecycleAndVersionedRefs(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	created, inserted, err := db.PutArtifact(ctx, ArtifactInput{
		Kind: ArtifactMemory, ScopeKind: ScopeRepo, Repo: "acme/app",
		Title: "package manager", Content: ArtifactContent{Text: "Use pnpm for this repository."},
	})
	if err != nil || !inserted {
		t.Fatalf("put memory: inserted=%v err=%v", inserted, err)
	}
	hits, err := db.SearchArtifacts(ctx, ArtifactSearch{Query: "pnpm", Kind: ArtifactMemory, Repo: "acme/app"})
	if err != nil || len(hits) != 1 {
		t.Fatalf("search active memory: hits=%d err=%v", len(hits), err)
	}

	oldRef, err := ParseArtifactRef(created.VersionedRef())
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.SupersedeMemory(ctx, created.ID,
		ArtifactContent{Text: "Use bun for this repository."}, "package manager", "human")
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if updated.EventSeq == created.EventSeq {
		t.Fatal("supersede did not create a new version")
	}
	old, err := db.ResolveArtifactRef(ctx, oldRef)
	if err != nil || old.Content.Text != "Use pnpm for this repository." {
		t.Fatalf("old version did not resolve: %+v err=%v", old, err)
	}
	current, err := db.ResolveArtifactRef(ctx, ArtifactRef{Kind: ArtifactMemory, ID: created.ID})
	if err != nil || current.Content.Text != "Use bun for this repository." {
		t.Fatalf("current version did not resolve: %+v err=%v", current, err)
	}

	retracted, err := db.RetractArtifact(ctx, created.ID, "human")
	if err != nil {
		t.Fatalf("retract: %v", err)
	}
	retractedExact, err := db.ResolveArtifactRef(ctx, ArtifactRef{
		Kind: ArtifactMemory, ID: created.ID, EventSeq: retracted.EventSeq,
	})
	if err != nil || !retractedExact.ContentPresent || retractedExact.Content.Text != "Use bun for this repository." {
		t.Fatalf("retraction version did not resolve retained content: %+v err=%v", retractedExact, err)
	}
	hits, err = db.SearchArtifacts(ctx, ArtifactSearch{Query: "bun", Kind: ArtifactMemory, Repo: "acme/app"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("retracted memory remained searchable: hits=%d err=%v", len(hits), err)
	}
}

func TestArtifactIdentityCannotChangeKind(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, _, err := db.PutArtifact(ctx, ArtifactInput{
		ID: "stable-kind", Kind: ArtifactMemory, ScopeKind: ScopeUser,
		Content: ArtifactContent{Text: "Keep this identity a memory."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.PutArtifact(ctx, ArtifactInput{
		ID: a.ID, Kind: ArtifactRunbook, ScopeKind: ScopeUser,
		Content: ArtifactContent{Text: "Do not turn it into a runbook."},
	}); err == nil {
		t.Fatal("artifact kind changed under a stable ref")
	}
	current, err := db.Artifact(ctx, a.ID)
	if err != nil || current.Kind != ArtifactMemory || current.Content.Text != "Keep this identity a memory." {
		t.Fatalf("artifact was corrupted by kind change: %+v err=%v", current, err)
	}
}

func TestProposalsAreExcludedUntilAccepted(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	proposal, _, err := db.PutArtifact(ctx, ArtifactInput{
		Kind: ArtifactMemory, Status: ArtifactPending,
		ScopeKind: ScopeRepo, Repo: "acme/app", Actor: "agent",
		Authority: "proposed", Source: "codex",
		Content: ArtifactContent{Text: "The signing job requires a notarization key."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.SearchArtifacts(ctx, ArtifactSearch{Query: "notarization", Repo: "acme/app"}); len(hits) != 0 {
		t.Fatal("pending proposal leaked into recall")
	}
	accepted, err := db.AcceptMemory(ctx, proposal.ID, nil, "human")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Status != ArtifactActive || accepted.Authority != "asserted" {
		t.Fatalf("accepted memory = %+v", accepted)
	}
	if hits, _ := db.SearchArtifacts(ctx, ArtifactSearch{Query: "notarization", Repo: "acme/app"}); len(hits) != 1 {
		t.Fatalf("accepted memory not recalled: %d hits", len(hits))
	}
}

func TestArtifactScopesDoNotLeakAcrossRepositories(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, in := range []ArtifactInput{
		{Kind: ArtifactMemory, ScopeKind: ScopeUser, Content: ArtifactContent{Text: "Prefer concise commit messages."}},
		{Kind: ArtifactMemory, ScopeKind: ScopeRepo, Repo: "acme/a", Content: ArtifactContent{Text: "Run frobnicate before deploy."}},
		{Kind: ArtifactMemory, ScopeKind: ScopeTask, ScopeKey: "release", Repo: "acme/a", Content: ArtifactContent{Text: "The release task needs a canary."}},
	} {
		if _, _, err := db.PutArtifact(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	if hits, _ := db.SearchArtifacts(ctx, ArtifactSearch{Query: "frobnicate", Repo: "acme/b"}); len(hits) != 0 {
		t.Fatalf("repo memory leaked across scope: %d hits", len(hits))
	}
	if hits, _ := db.SearchArtifacts(ctx, ArtifactSearch{Query: "concise", Repo: "acme/b"}); len(hits) != 1 {
		t.Fatalf("user memory should be cross-repo: %d hits", len(hits))
	}
	if hits, _ := db.SearchArtifacts(ctx, ArtifactSearch{Query: "canary", Repo: "acme/a"}); len(hits) != 1 {
		t.Fatalf("task memory should be visible in its repo: %d hits", len(hits))
	}
}

func TestRecallRequiresExactTaskScope(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	for _, in := range []ArtifactInput{
		{ID: "task-a", Kind: ArtifactMemory, ScopeKind: ScopeTask, ScopeKey: "REL-42", Repo: "acme/a", Content: ArtifactContent{Text: "Use the violet release lane."}},
		{ID: "task-b", Kind: ArtifactMemory, ScopeKind: ScopeTask, ScopeKey: "REL-42", Repo: "acme/b", Content: ArtifactContent{Text: "Use the violet release lane."}},
		{ID: "task-other", Kind: ArtifactMemory, ScopeKind: ScopeTask, ScopeKey: "REL-99", Repo: "acme/a", Content: ArtifactContent{Text: "Use the violet release lane."}},
	} {
		if _, _, err := db.PutArtifact(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := db.SearchArtifacts(ctx, ArtifactSearch{
		Query: "violet release", Repo: "acme/a", TaskKey: "REL-42", Recall: true,
	})
	if err != nil || len(hits) != 1 || hits[0].ID != "task-a" {
		t.Fatalf("exact task recall = %+v err=%v", hits, err)
	}
	hits, err = db.SearchArtifacts(ctx, ArtifactSearch{
		Query: "violet release", Repo: "acme/a", Recall: true,
	})
	if err != nil || len(hits) != 0 {
		t.Fatalf("repo-only recall leaked task state: %+v err=%v", hits, err)
	}
	// An explicit CLI-style audit is allowed to list all task artifacts attached
	// to a repository; recall and inventory are intentionally different queries.
	hits, err = db.SearchArtifacts(ctx, ArtifactSearch{Query: "violet release", Repo: "acme/a"})
	if err != nil || len(hits) != 2 {
		t.Fatalf("repository audit = %+v err=%v", hits, err)
	}
}

func TestArtifactContentIsScrubbedBeforeHashingAndStorage(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz123456"
	a, _, err := db.PutArtifact(ctx, ArtifactInput{
		Kind: ArtifactMemory, ScopeKind: ScopeUser,
		Content: ArtifactContent{Text: "token " + secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.Content.Text, secret) || !strings.Contains(a.Content.Text, "REDACTED") {
		t.Fatalf("returned content was not scrubbed: %q", a.Content.Text)
	}
	raw, err := os.ReadFile(db.ArtifactBlobPath(a.ContentHash))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("secret remained in compressed blob bytes")
	}
}

func TestPurgeDestroysEveryVersionBody(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	a, _, err := db.PutArtifact(ctx, ArtifactInput{
		Kind: ArtifactMemory, ScopeKind: ScopeUser,
		Content: ArtifactContent{Text: "first-purge-marker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPath := db.ArtifactBlobPath(a.ContentHash)
	a, err = db.SupersedeMemory(ctx, a.ID,
		ArtifactContent{Text: "second-purge-marker"}, "purge test", "human")
	if err != nil {
		t.Fatal(err)
	}
	secondPath := db.ArtifactBlobPath(a.ContentHash)
	if _, err := db.PurgeArtifact(ctx, a.ID, "human"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, path := range []string{firstPath, secondPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("artifact body still exists at %s", path)
		}
	}
	for _, query := range []string{"first-purge-marker", "second-purge-marker"} {
		if hits, err := db.SearchArtifacts(ctx, ArtifactSearch{Query: query}); err != nil || len(hits) != 0 {
			t.Errorf("purged content searchable for %q: hits=%d err=%v", query, len(hits), err)
		}
	}
	current, err := db.Artifact(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != ArtifactPurged || current.ContentPresent || current.ContentHash != "" {
		t.Fatalf("purged projection retains content: %+v", current)
	}
}

func TestPurgeMakesRefsUnreadableWhenAnotherArtifactSharesTheBlob(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	var items []Artifact
	for _, id := range []string{"shared-a", "shared-b"} {
		a, _, err := db.PutArtifact(ctx, ArtifactInput{
			ID: id, Kind: ArtifactMemory, ScopeKind: ScopeUser,
			Content: ArtifactContent{Text: "identical shared body"},
		})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, a)
	}
	if items[0].ContentHash != items[1].ContentHash {
		t.Fatal("test setup did not deduplicate identical content")
	}
	oldRef := ArtifactRef{Kind: ArtifactMemory, ID: items[0].ID, EventSeq: items[0].EventSeq}
	if _, err := db.PurgeArtifact(ctx, items[0].ID, "human"); err != nil {
		t.Fatal(err)
	}
	purgedVersion, err := db.ResolveArtifactRef(ctx, oldRef)
	if err != nil {
		t.Fatal(err)
	}
	if purgedVersion.ContentPresent || purgedVersion.Content.Text != "" {
		t.Fatalf("purged ref recovered a shared blob: %+v", purgedVersion)
	}
	remaining, err := db.Artifact(ctx, items[1].ID)
	if err != nil || !remaining.ContentPresent || remaining.Content.Text != "identical shared body" {
		t.Fatalf("purge broke the artifact that still owns the blob: %+v err=%v", remaining, err)
	}
}
