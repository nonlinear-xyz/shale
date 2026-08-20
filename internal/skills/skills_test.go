package skills

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nonlinear-xyz/shale/internal/store"
)

func TestSnapshotValidatesFullTreeAndRelativeReferences(t *testing.T) {
	root := filepath.Join(t.TempDir(), "release-guide")
	mustWrite(t, filepath.Join(root, "SKILL.md"), validSkill("release-guide", "Guide releases", "Read [details](references/details.md)."), 0o644)
	mustWrite(t, filepath.Join(root, "references", "details.md"), []byte("# Details\n\nNotarize after signing.\n"), 0o644)
	mustWrite(t, filepath.Join(root, "scripts", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	mustWrite(t, filepath.Join(root, "assets", "bytes.bin"), []byte{0, 1, 2, 3}, 0o644)

	snapshot, err := SnapshotDir(root, "release-guide")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Name != "release-guide" || snapshot.Description != "Guide releases" || len(snapshot.Files) != 4 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	hash1, err := store.ComputeSkillTreeHash(snapshot.Files)
	if err != nil {
		t.Fatal(err)
	}
	snapshot2, err := SnapshotDir(root, "release-guide")
	if err != nil {
		t.Fatal(err)
	}
	hash2, _ := store.ComputeSkillTreeHash(snapshot2.Files)
	if hash1 != hash2 {
		t.Fatalf("stable tree hash changed: %s != %s", hash1, hash2)
	}

	mustWrite(t, filepath.Join(root, "SKILL.md"), validSkill("release-guide", "Guide releases", "Read [missing](references/missing.md)."), 0o644)
	if _, err := SnapshotDir(root, "release-guide"); err == nil || !strings.Contains(err.Error(), "does not resolve") {
		t.Fatalf("missing reference error = %v", err)
	}
}

func TestSnapshotRejectsSymlinksAndNameMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "release-guide")
	mustWrite(t, filepath.Join(root, "SKILL.md"), validSkill("other-name", "Guide releases", "Do it."), 0o644)
	if _, err := SnapshotDir(root, "release-guide"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("name mismatch error = %v", err)
	}
	mustWrite(t, filepath.Join(root, "SKILL.md"), validSkill("release-guide", "Guide releases", "Do it."), 0o644)
	outside := filepath.Join(t.TempDir(), "secret")
	mustWrite(t, outside, []byte("secret"), 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "references.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotDir(root, "release-guide"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestImportCopiesExactTreesAndMakesLooseMarkdownDrafts(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "Do it."), 0o644)
	mustWrite(t, filepath.Join(root, "release-guide", "assets", "sample.bin"), []byte{4, 3, 2, 1}, 0o644)
	loosePath := filepath.Join(root, "New Discovery.md")
	mustWrite(t, loosePath, []byte("# A lesson\n\nTurn this into a skill later.\n"), 0o644)
	original, _ := os.ReadFile(loosePath)

	result, err := ImportLibrary(ctx, db, root, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("imported %d skills, want 2", len(result.Skills))
	}
	draft, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: "personal", Name: "new-discovery"})
	if err != nil || draft.Status != store.SkillDraft || draft.Description != "" {
		t.Fatalf("loose draft = %+v err=%v", draft, err)
	}
	after, _ := os.ReadFile(loosePath)
	if string(after) != string(original) {
		t.Fatal("import modified the original loose Markdown")
	}
	asset, err := db.ReadSkillFile(ctx, store.SkillRef{LibraryKey: "personal", Name: "release-guide"}, "assets/sample.bin")
	if err != nil || string(asset) != string([]byte{4, 3, 2, 1}) {
		t.Fatalf("asset snapshot changed: %v err=%v", asset, err)
	}
}

func TestActivateValidLooseDraftRecordsActivationEvenWhenBytesMatch(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "release-guide.md"), validSkill("release-guide", "Guide releases", "Do it."), 0o644)
	result, err := ImportLibrary(ctx, db, root, "personal")
	if err != nil {
		t.Fatal(err)
	}
	draft := result.Skills[0]
	if draft.Status != store.SkillDraft {
		t.Fatalf("loose file status = %s, want draft", draft.Status)
	}
	active, _, err := ActivateDraft(ctx, db, store.SkillRef{LibraryKey: "personal", Name: draft.Name}, "Guide releases")
	if err != nil {
		t.Fatal(err)
	}
	if active.Status != store.SkillActive || active.TreeHash != draft.TreeHash {
		t.Fatalf("activation = %+v; exact bytes should remain stable", active)
	}
	exact, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: "personal", Name: draft.Name, TreeHash: draft.TreeHash})
	if err != nil || exact.Status != store.SkillActive {
		t.Fatalf("exact activated revision = %+v err=%v", exact, err)
	}
}

func TestNativeProposalApplyInstallAndRollback(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "Use the old order."), 0o644)
	mustWrite(t, filepath.Join(root, "release-guide", "references", "details.md"), []byte("details stay exact\n"), 0o644)
	if _, err := ImportLibrary(ctx, db, root, "personal"); err != nil {
		t.Fatal(err)
	}
	old, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: "personal", Name: "release-guide"})
	if err != nil {
		t.Fatal(err)
	}
	replacement := validSkill("release-guide", "Guide releases", "Use the new order and [details](references/details.md).")
	change, _, err := ProposeChange(ctx, db, ProposalInput{
		Ref:    store.SkillRef{LibraryKey: "personal", Name: "release-guide"},
		Lesson: "The new order prevents signing failures.", Replacement: replacement,
		Source: "mcp", Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptSkillChange(ctx, change.ID, "human"); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyChange(ctx, db, change.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Change.Status != store.SkillChangeApplied || result.Skill == nil || result.Skill.TreeHash == old.TreeHash {
		t.Fatalf("apply result = %+v", result)
	}
	targetRoot := filepath.Join(t.TempDir(), "agent-skills")
	if _, err := AddTarget(ctx, db, "codex", targetRoot); err != nil {
		t.Fatal(err)
	}
	newRef := store.SkillRef{LibraryKey: "personal", Name: "release-guide", TreeHash: result.Skill.TreeHash}
	installed, err := Install(ctx, db, newRef, "codex")
	if err != nil {
		t.Fatal(err)
	}
	installedCore, _ := os.ReadFile(filepath.Join(installed.Installation.InstalledPath, "SKILL.md"))
	if !strings.Contains(string(installedCore), "new order") {
		t.Fatalf("new revision was not installed: %s", installedCore)
	}
	oldRef := store.SkillRef{LibraryKey: "personal", Name: "release-guide", TreeHash: old.TreeHash}
	rolledBack, err := Install(ctx, db, oldRef, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.PreviousTree != newRef.TreeHash {
		t.Fatalf("rollback previous tree = %s, want %s", rolledBack.PreviousTree, newRef.TreeHash)
	}
	rolledBackCore, _ := os.ReadFile(filepath.Join(rolledBack.Installation.InstalledPath, "SKILL.md"))
	if !strings.Contains(string(rolledBackCore), "old order") {
		t.Fatalf("old exact revision was not restored: %s", rolledBackCore)
	}

	unmanagedRoot := filepath.Join(t.TempDir(), "unmanaged-target")
	if _, err := AddTarget(ctx, db, "other", unmanagedRoot); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(unmanagedRoot, "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "User-owned copy."), 0o644)
	if _, err := Install(ctx, db, newRef, "other"); err == nil || !strings.Contains(err.Error(), "unmanaged") {
		t.Fatalf("unmanaged overwrite error = %v", err)
	}
}

func TestInstallRefusesDivergedManagedBaselineAndPluginCache(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "Original."), 0o644)
	result, err := ImportLibrary(ctx, db, root, "personal")
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := store.ParseSkillRef(result.Skills[0].VersionedRef())
	targetRoot := filepath.Join(t.TempDir(), "targets")
	if _, err := AddTarget(ctx, db, "codex", targetRoot); err != nil {
		t.Fatal(err)
	}
	installed, err := Install(ctx, db, ref, "codex")
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(installed.Installation.InstalledPath, "SKILL.md"), validSkill("release-guide", "Guide releases", "Locally edited."), 0o644)
	if _, err := Install(ctx, db, ref, "codex"); err == nil || !strings.Contains(err.Error(), "modified outside Shale") {
		t.Fatalf("diverged baseline error = %v", err)
	}
	cache := filepath.Join(t.TempDir(), ".codex", "plugins", "cache", "factory-kit")
	if _, err := AddTarget(ctx, db, "cache", cache); err == nil || !strings.Contains(err.Error(), "plugin cache") {
		t.Fatalf("plugin cache target error = %v", err)
	}
}

func TestGitApplyCreatesIsolatedUncommittedWorktree(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	repo := createGitLibrary(t, "Use the old release order.")
	mustWrite(t, filepath.Join(repo, "package.json"), []byte(`{"scripts":{"test":"touch SHOULD-NOT-EXIST"}}`+"\n"), 0o644)
	runGit(t, repo, "add", "package.json")
	runGit(t, repo, "commit", "-m", "add validation guidance")
	registered, err := RegisterGitLibrary(ctx, db, repo, "skills", "")
	if err != nil {
		t.Fatal(err)
	}
	if registered.Library.Key != "acme/factory-kit" {
		t.Fatalf("derived key = %q", registered.Library.Key)
	}
	base := registered.Skills[0]
	original, _ := os.ReadFile(filepath.Join(repo, "skills", "release-guide", "SKILL.md"))
	headBefore := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD"))
	replacement := validSkill("release-guide", "Guide releases", "Use the safer release order.")
	change, _, err := ProposeChange(ctx, db, ProposalInput{
		Ref:    store.SkillRef{LibraryKey: registered.Library.Key, Name: base.Name},
		Lesson: "The safer order avoids a release failure.", Replacement: replacement,
		Source: "mcp", Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptSkillChange(ctx, change.ID, "human"); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyChange(ctx, db, change.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Change.Status != store.SkillChangeMaterialized || result.Worktree == "" || result.Branch == "" {
		t.Fatalf("materialized result = %+v", result)
	}
	if len(result.Guidance) != 1 || result.Guidance[0] != "npm test" {
		t.Fatalf("validation guidance = %v", result.Guidance)
	}
	if _, err := os.Stat(filepath.Join(result.Worktree, "SHOULD-NOT-EXIST")); !os.IsNotExist(err) {
		t.Fatalf("project-defined validation command was executed: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(repo, "skills", "release-guide", "SKILL.md"))
	if string(after) != string(original) {
		t.Fatal("canonical source worktree was modified")
	}
	if headAfter := strings.TrimSpace(runGit(t, repo, "rev-parse", "HEAD")); headAfter != headBefore {
		t.Fatalf("canonical HEAD changed: %s != %s", headAfter, headBefore)
	}
	worktreeCore, _ := os.ReadFile(filepath.Join(result.Worktree, "skills", "release-guide", "SKILL.md"))
	if !strings.Contains(string(worktreeCore), "safer release order") {
		t.Fatalf("worktree did not contain replacement: %s", worktreeCore)
	}
	if head := strings.TrimSpace(runGit(t, result.Worktree, "rev-parse", "HEAD")); head != headBefore {
		t.Fatalf("apply created a commit: %s != %s", head, headBefore)
	}
	status := runGit(t, result.Worktree, "status", "--short")
	if !strings.Contains(status, "SKILL.md") {
		t.Fatalf("worktree should contain one uncommitted edit: %q", status)
	}
}

func TestGitRegistrationRejectsIgnoredUncommittedSkillFiles(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	repo := createGitLibrary(t, "Original.")
	mustWrite(t, filepath.Join(repo, ".gitignore"), []byte("skills/release-guide/references/private.md\n"), 0o644)
	runGit(t, repo, "add", ".gitignore")
	runGit(t, repo, "commit", "-m", "ignore local skill note")
	mustWrite(t, filepath.Join(repo, "skills", "release-guide", "references", "private.md"), []byte("ignored local note\n"), 0o644)
	if status := strings.TrimSpace(runGit(t, repo, "status", "--short")); status != "" {
		t.Fatalf("fixture should look clean to ordinary status: %q", status)
	}
	_, err := RegisterGitLibrary(ctx, db, repo, "skills", "")
	if err == nil || !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("ignored package file error = %v", err)
	}
}

func TestGitApplyMarksChangedBaseStaleAndDirtyRefreshRetainsSnapshot(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	repo := createGitLibrary(t, "Original.")
	registered, err := RegisterGitLibrary(ctx, db, repo, "skills", "")
	if err != nil {
		t.Fatal(err)
	}
	base := registered.Skills[0]
	change, _, err := ProposeChange(ctx, db, ProposalInput{
		Ref:    store.SkillRef{LibraryKey: registered.Library.Key, Name: base.Name},
		Lesson: "Change it.", Replacement: validSkill("release-guide", "Guide releases", "Proposed."),
		Source: "mcp", Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcceptSkillChange(ctx, change.ID, "human"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(repo, "skills", "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "Uncommitted canonical edit."), 0o644)
	refresh := RefreshLibraries(ctx, db, registered.Library.Key)
	if len(refresh.Skipped) != 1 || refresh.Indexed != 0 {
		t.Fatalf("dirty refresh = %+v", refresh)
	}
	current, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: registered.Library.Key, Name: base.Name})
	if err != nil || current.TreeHash != base.TreeHash {
		t.Fatalf("dirty refresh changed snapshot: %+v err=%v", current, err)
	}
	if _, err := ApplyChange(ctx, db, change.ID, nil); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("dirty apply error = %v", err)
	}
	stored, _ := db.SkillChange(ctx, change.ID)
	if stored.Status != store.SkillChangeAccepted {
		t.Fatalf("dirty worktree should not silently stale proposal: %+v", stored)
	}

	runGit(t, repo, "add", "skills/release-guide/SKILL.md")
	runGit(t, repo, "commit", "-m", "change canonical skill")
	if _, err := ApplyChange(ctx, db, change.ID, nil); err == nil || !strings.Contains(err.Error(), "HEAD changed") {
		t.Fatalf("stale apply error = %v", err)
	}
	stored, _ = db.SkillChange(ctx, change.ID)
	if stored.Status != store.SkillChangeStale {
		t.Fatalf("proposal status = %s, want stale", stored.Status)
	}
}

func TestNormalizeGitRemoteStableAcrossCheckoutPaths(t *testing.T) {
	for _, remote := range []string{
		"git@github.com:Acme/Factory-Kit.git",
		"https://github.com/Acme/Factory-Kit.git",
		"ssh://git@github.com/Acme/Factory-Kit.git",
	} {
		if got := NormalizeGitRemote(remote); got != "acme/factory-kit" {
			t.Errorf("NormalizeGitRemote(%q) = %q", remote, got)
		}
	}
	if got := NormalizeGitRemote("/tmp/local-repo"); got != "" {
		t.Fatalf("local path produced portable key %q", got)
	}
}

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func createGitLibrary(t *testing.T, instruction string) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Shale Test")
	mustWrite(t, filepath.Join(repo, "skills", "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", instruction), 0o644)
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "seed skill")
	runGit(t, repo, "remote", "add", "origin", "https://github.com/acme/factory-kit.git")
	return repo
}

func runGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", directory}, args...)...)
	body, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, body)
	}
	return string(body)
}

func mustWrite(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func validSkill(name, description, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n\n# Instructions\n\n" + body + "\n")
}

func TestAddTargetRequiresAbsolutePath(t *testing.T) {
	db := openTestStore(t)
	_, err := AddTarget(context.Background(), db, "codex", "relative/path")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative target error = %v", err)
	}
}

func TestApplyRequiresAcceptedProposal(t *testing.T) {
	db := openTestStore(t)
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "release-guide", "SKILL.md"), validSkill("release-guide", "Guide releases", "Original."), 0o644)
	result, err := ImportLibrary(context.Background(), db, root, "personal")
	if err != nil {
		t.Fatal(err)
	}
	change, _, err := ProposeChange(context.Background(), db, ProposalInput{
		Ref:    store.SkillRef{LibraryKey: "personal", Name: result.Skills[0].Name},
		Lesson: "Learned.", Replacement: validSkill("release-guide", "Guide releases", "Changed."),
		Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ApplyChange(context.Background(), db, change.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "accepted") {
		t.Fatalf("pending apply error = %v", err)
	}
}

func TestInstallMissingRevision(t *testing.T) {
	db := openTestStore(t)
	_, err := Install(context.Background(), db, store.SkillRef{LibraryKey: "personal", Name: "missing", TreeHash: strings.Repeat("a", 64)}, "codex")
	if err == nil || (!errors.Is(err, store.ErrSkillNotFound) && !strings.Contains(err.Error(), "not found")) {
		t.Fatalf("missing install error = %v", err)
	}
}
