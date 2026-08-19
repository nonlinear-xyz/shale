package artifacts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nonlinear-xyz/shale/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshIndexesHarnessMemoryAndInstructionsWithoutChangingSources(t *testing.T) {
	db := testDB(t)
	home := t.TempDir()
	codexMemory := filepath.Join(home, ".codex", "memories", "durable.md")
	claudeMemory := filepath.Join(home, "custom-claude-memory", "MEMORY.md")
	write(t, codexMemory, "Always include a rollback step.")
	write(t, claudeMemory, "The integration test needs Redis.")
	write(t, filepath.Join(home, ".claude", "settings.json"),
		`{"autoMemoryDirectory":"~/custom-claude-memory"}`)
	write(t, filepath.Join(home, ".codex", "AGENTS.md"), "Use focused tests first.")

	before, err := os.ReadFile(codexMemory)
	if err != nil {
		t.Fatal(err)
	}
	result := Refresh(context.Background(), db, Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"),
		ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: home,
	})
	if len(result.Errors) != 0 {
		t.Fatalf("refresh errors: %v", result.Errors)
	}
	if result.Indexed != 3 {
		t.Fatalf("indexed = %d, want 3 (two memories and one instruction); skips=%v", result.Indexed, result.Skipped)
	}
	memories, err := db.ListArtifacts(context.Background(), store.ArtifactFilter{
		Kind: store.ArtifactMemory, Status: store.ArtifactActive,
	})
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories=%d err=%v", len(memories), err)
	}
	instructions, err := db.ListArtifacts(context.Background(), store.ArtifactFilter{
		Kind: store.ArtifactInstruction, Status: store.ArtifactActive,
	})
	if err != nil || len(instructions) != 1 {
		t.Fatalf("instructions=%d err=%v", len(instructions), err)
	}
	after, _ := os.ReadFile(codexMemory)
	if string(after) != string(before) {
		t.Fatal("refresh modified a canonical harness memory")
	}
}

func TestRefreshRevisesThenRetractsMissingSource(t *testing.T) {
	db := testDB(t)
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "memories", "memory.md")
	write(t, path, "Use the blue deployment pool.")
	opts := Options{HomeDir: home, CodexHome: filepath.Join(home, ".codex"), ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: home}

	first := Refresh(context.Background(), db, opts)
	if first.Indexed != 1 || len(first.Errors) != 0 {
		t.Fatalf("first refresh: %+v", first)
	}
	items, _ := db.ListArtifacts(context.Background(), store.ArtifactFilter{Kind: store.ArtifactMemory})
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	firstSeq := items[0].EventSeq

	write(t, path, "Use the green deployment pool.")
	second := Refresh(context.Background(), db, opts)
	if second.Indexed != 1 || len(second.Errors) != 0 {
		t.Fatalf("second refresh: %+v", second)
	}
	updated, _ := db.Artifact(context.Background(), items[0].ID)
	if updated.EventSeq == firstSeq || updated.Content.Text != "Use the green deployment pool." {
		t.Fatalf("source was not revised: %+v", updated)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	third := Refresh(context.Background(), db, opts)
	if third.Removed != 1 || len(third.Errors) != 0 {
		t.Fatalf("third refresh: %+v", third)
	}
	removed, _ := db.Artifact(context.Background(), items[0].ID)
	if removed.Status != store.ArtifactRetracted {
		t.Fatalf("missing source status = %s", removed.Status)
	}
}

func TestRegisteredRunbookStaysGitBackedAndGetsRevisions(t *testing.T) {
	db := testDB(t)
	repo := t.TempDir()
	git(t, repo, "init")
	path := filepath.Join(repo, "docs", "release.md")
	write(t, path, "# Release\n\nRun tests.")
	source, err := RegisterRunbook(context.Background(), db, path, "")
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{HomeDir: t.TempDir(), CurrentDir: repo}
	first := Refresh(context.Background(), db, opts)
	if first.Indexed != 1 || len(first.Errors) != 0 {
		t.Fatalf("first refresh: %+v", first)
	}
	a, err := db.Artifact(context.Background(), source.ArtifactID)
	canonicalDir, canonicalErr := filepath.EvalSymlinks(filepath.Dir(path))
	canonicalPath := filepath.Join(canonicalDir, filepath.Base(path))
	if canonicalErr != nil || err != nil || a.Kind != store.ArtifactRunbook || a.SourcePointer != canonicalPath {
		t.Fatalf("runbook = %+v err=%v", a, err)
	}
	write(t, path, "# Release\n\nRun tests and build.")
	second := Refresh(context.Background(), db, opts)
	if second.Indexed != 1 || len(second.Errors) != 0 {
		t.Fatalf("second refresh: %+v", second)
	}
	current, _ := db.Artifact(context.Background(), source.ArtifactID)
	if current.EventSeq == a.EventSeq || current.Content.Text == a.Content.Text {
		t.Fatal("registered runbook did not revision when its file changed")
	}
}

func TestRegisterRunbookRetractsPriorInstructionIdentityForSamePath(t *testing.T) {
	db := testDB(t)
	repo := t.TempDir()
	git(t, repo, "init")
	path := filepath.Join(repo, "AGENTS.md")
	write(t, path, "Run the release checks.")
	home := t.TempDir()
	opts := Options{HomeDir: home, CodexHome: filepath.Join(home, ".codex"), ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: repo}
	first := Refresh(context.Background(), db, opts)
	if first.Indexed != 1 || len(first.Errors) != 0 {
		t.Fatalf("instruction refresh: %+v", first)
	}
	instructions, _ := db.ListArtifacts(context.Background(), store.ArtifactFilter{Kind: store.ArtifactInstruction})
	if len(instructions) != 1 || instructions[0].Status != store.ArtifactActive {
		t.Fatalf("instruction setup: %+v", instructions)
	}
	if sources, err := db.ArtifactSources(context.Background()); err != nil || len(sources) != 1 {
		t.Fatalf("source setup: %+v err=%v", sources, err)
	}

	source, err := RegisterRunbook(context.Background(), db, path, "")
	if err != nil {
		t.Fatal(err)
	}
	second := Refresh(context.Background(), db, opts)
	if len(second.Errors) != 0 {
		t.Fatalf("runbook refresh: %+v", second)
	}
	old, err := db.Artifact(context.Background(), instructions[0].ID)
	if err != nil || old.Status != store.ArtifactRetracted {
		t.Fatalf("old instruction was not retracted: %+v err=%v", old, err)
	}
	runbook, err := db.Artifact(context.Background(), source.ArtifactID)
	if err != nil || runbook.Status != store.ArtifactActive || runbook.Kind != store.ArtifactRunbook {
		t.Fatalf("registered runbook = %+v err=%v", runbook, err)
	}
}

func TestRefreshUsesCodexOverrideAndFindsDotClaudeInstructions(t *testing.T) {
	db := testDB(t)
	repo := t.TempDir()
	git(t, repo, "init")
	write(t, filepath.Join(repo, "AGENTS.md"), "This base file is shadowed.")
	home := t.TempDir()
	opts := Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"),
		ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: repo,
	}
	first := Refresh(context.Background(), db, opts)
	if first.Indexed != 1 || len(first.Errors) != 0 {
		t.Fatalf("first refresh: %+v", first)
	}
	// Codex ignores an empty override and continues to the base AGENTS.md.
	write(t, filepath.Join(repo, "AGENTS.override.md"), "")
	emptyOverride := Refresh(context.Background(), db, opts)
	if emptyOverride.Unchanged != 1 || emptyOverride.Removed != 0 || len(emptyOverride.Errors) != 0 {
		t.Fatalf("empty override shadowed the base file: %+v", emptyOverride)
	}

	// Adding an override later must retract the previously indexed base file;
	// the source registry must not keep a now-shadowed instruction alive.
	write(t, filepath.Join(repo, "AGENTS.override.md"), "Use the override instruction.")
	write(t, filepath.Join(repo, ".claude", "CLAUDE.md"), "Use the project Claude instruction.")
	result := Refresh(context.Background(), db, opts)
	if len(result.Errors) != 0 {
		t.Fatalf("refresh errors: %v", result.Errors)
	}
	if result.Removed != 1 {
		t.Fatalf("shadowed base removal = %d, want 1", result.Removed)
	}
	items, err := db.ListArtifacts(context.Background(), store.ArtifactFilter{
		Kind: store.ArtifactInstruction, Status: store.ArtifactActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("instructions = %d, want override + .claude/CLAUDE.md; items=%+v", len(items), items)
	}
	var joined string
	for _, item := range items {
		joined += item.Content.Text
	}
	if !strings.Contains(joined, "override instruction") || !strings.Contains(joined, "project Claude instruction") {
		t.Fatalf("expected instructions missing: %q", joined)
	}
	if strings.Contains(joined, "base file") {
		t.Fatalf("shadowed AGENTS.md was indexed: %q", joined)
	}
}

func TestRefreshRejectsSymlinkOutsideTrustedRoot(t *testing.T) {
	db := testDB(t)
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	write(t, outside, "do not index me")
	link := filepath.Join(home, ".codex", "memories", "escape.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	result := Refresh(context.Background(), db, Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"),
		ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: home,
	})
	if len(result.Skipped) != 1 || result.Skipped[0].Reason != "symlink escapes its trusted root" {
		t.Fatalf("skips = %+v", result.Skipped)
	}
	items, _ := db.ListArtifacts(context.Background(), store.ArtifactFilter{})
	if len(items) != 0 {
		t.Fatalf("escaped source was indexed: %+v", items)
	}
}

func TestUnmappedClaudeProjectMemoryIsNotPromotedToUserScope(t *testing.T) {
	db := testDB(t)
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "unknown-project", "memory", "MEMORY.md")
	write(t, path, "This belongs only to an unmapped project.")
	result := Refresh(context.Background(), db, Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"),
		ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: home,
	})
	if len(result.Errors) != 0 || result.Indexed != 1 {
		t.Fatalf("refresh: %+v", result)
	}
	items, err := db.ListArtifacts(context.Background(), store.ArtifactFilter{Kind: store.ArtifactMemory})
	if err != nil || len(items) != 1 {
		t.Fatalf("memories=%d err=%v", len(items), err)
	}
	if items[0].ScopeKind != store.ScopeRepo || items[0].Repo != "claude-project:unknown-project" {
		t.Fatalf("unmapped project memory escaped into global scope: %+v", items[0])
	}
}

func TestRefreshIgnoresNonTextFilesInMemoryDirectories(t *testing.T) {
	db := testDB(t)
	home := t.TempDir()
	write(t, filepath.Join(home, ".codex", "memories", "memory.md"), "Index this memory.")
	write(t, filepath.Join(home, ".codex", "memories", "cache.bin"), "Do not index arbitrary cache files.")
	result := Refresh(context.Background(), db, Options{
		HomeDir: home, CodexHome: filepath.Join(home, ".codex"),
		ClaudeHome: filepath.Join(home, ".claude"), CurrentDir: home,
	})
	if result.Indexed != 1 || len(result.Errors) != 0 {
		t.Fatalf("refresh: %+v", result)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
