package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillMigrationUpgradesV1AndPreservesMemories(t *testing.T) {
	dir := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "shale.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(artifactSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		INSERT INTO artifacts
		  (id, kind, status, scope_kind, scope_key, repo, title, origin,
		   authority, source, current_event_seq, created_at, updated_at)
		VALUES ('kept-memory', 'memory', 'active', 'user', 'local', '',
		        'Keep me', 'native', 'asserted', 'cli', 1,
		        '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version = %d, want 2", version)
	}
	memory, err := db.Artifact(context.Background(), "kept-memory")
	if err != nil || memory.Title != "Keep me" || memory.Status != ArtifactActive {
		t.Fatalf("v1 memory was not preserved: %+v err=%v", memory, err)
	}
}

func TestSkillRefsAndFileRefsArePortable(t *testing.T) {
	hash := strings.Repeat("a", 64)
	value := "skill:nonlinear-xyz/factory-kit/factory-testing@" + hash
	ref, err := ParseSkillRef(value)
	if err != nil {
		t.Fatal(err)
	}
	if ref.LibraryKey != "nonlinear-xyz/factory-kit" || ref.Name != "factory-testing" || ref.String() != value {
		t.Fatalf("skill ref round trip = %+v", ref)
	}
	fileValue := value + "#references/testing.md"
	fileRef, err := ParseSkillFileRef(fileValue)
	if err != nil || fileRef.Path != "references/testing.md" || fileRef.String() != fileValue {
		t.Fatalf("file ref round trip = %+v err=%v", fileRef, err)
	}
	if _, err := ParseSkillFileRef(value + "#../secret"); err == nil {
		t.Fatal("traversing file ref was accepted")
	}
}

func TestSkillRevisionsPreserveExactBytesAndSearchByFile(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lib, _, err := db.RegisterSkillLibrary(ctx, SkillLibraryInput{Key: "personal", Kind: SkillLibraryNative})
	if err != nil {
		t.Fatal(err)
	}
	secretLike := "sk-ant-abcdefghijklmnopqrstuvwxyz123456"
	core := validSkillMD("release-guide", "Guide releases", "Read [details](references/details.md).")
	detailBody := []byte("Use exact marker " + secretLike + " during notarization.\n")
	skill, _, inserted, err := db.PutSkillRevision(ctx, SkillRevisionInput{
		LibraryID: lib.ID, Name: "release-guide", Status: SkillActive,
		Description: "Guide releases", Files: []SkillFileInput{
			{Path: "SKILL.md", Content: core, Mode: 0o644},
			{Path: "references/details.md", Content: detailBody, Mode: 0o644},
			{Path: "scripts/check.sh", Content: []byte("#!/bin/sh\nexit 0\n"), Mode: 0o755},
		},
	})
	if err != nil || !inserted {
		t.Fatalf("put revision: inserted=%v err=%v", inserted, err)
	}
	body, err := db.ReadSkillFile(ctx, SkillRef{LibraryKey: "personal", Name: skill.Name, TreeHash: skill.TreeHash}, "references/details.md")
	if err != nil || string(body) != string(detailBody) {
		t.Fatalf("exact bytes changed: %q err=%v", body, err)
	}
	hits, err := db.SearchSkills(ctx, "notarization", "personal", SkillActive, 10)
	if err != nil || len(hits) != 1 || hits[0].FilePath != "references/details.md" {
		t.Fatalf("per-file search = %+v err=%v", hits, err)
	}
	if !strings.Contains(hits[0].FileRef(), "#references/details.md") {
		t.Fatalf("search did not mint an exact file ref: %+v", hits[0])
	}
}

func TestSkillChangeHumanBoundaryAndRejectPurgesContent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lib, skill := seedSkill(t, db, "personal", "release-guide")
	_ = lib
	replacement := validSkillMD("release-guide", "Guide releases", "Always verify notarization twice.")
	secret := "sk-ant-abcdefghijklmnopqrstuvwxyz123456"
	change, err := db.ProposeSkillChange(ctx, SkillChangeInput{
		BaseRef:     SkillRef{LibraryKey: "personal", Name: skill.Name, TreeHash: skill.TreeHash},
		Lesson:      "Observed token " + secret + " while testing notarization.",
		Replacement: replacement, Source: "mcp", Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(change.Lesson, secret) || !strings.Contains(change.Lesson, "REDACTED") {
		t.Fatalf("proposal prose was not scrubbed: %q", change.Lesson)
	}
	gotReplacement, err := db.ReadSkillChangeReplacement(ctx, change.ID)
	if err != nil || string(gotReplacement) != string(replacement) {
		t.Fatalf("replacement bytes were not exact: err=%v", err)
	}
	if _, err := db.AcceptSkillChange(ctx, change.ID, "agent"); err == nil {
		t.Fatal("agent accepted its own proposal")
	}
	accepted, err := db.AcceptSkillChange(ctx, change.ID, "human")
	if err != nil || accepted.Status != SkillChangeAccepted {
		t.Fatalf("human accept: %+v err=%v", accepted, err)
	}
	blobPath := db.SkillBlobPath(accepted.ReplacementBlobHash)
	rejected, err := db.RejectSkillChange(ctx, change.ID, "human")
	if err != nil {
		t.Fatal(err)
	}
	if rejected.Status != SkillChangeRejected || rejected.Lesson != "" || rejected.ReplacementBlobHash != "" {
		t.Fatalf("rejected tombstone retained content: %+v", rejected)
	}
	if _, err := os.Stat(blobPath); !os.IsNotExist(err) {
		t.Fatalf("rejected replacement blob still exists: %v", err)
	}
}

func TestLessonOnlyProposalCanBeAcceptedWithoutChangingSkill(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	_, skill := seedSkill(t, db, "personal", "release-guide")
	change, err := db.ProposeSkillChange(ctx, SkillChangeInput{
		BaseRef: SkillRef{LibraryKey: "personal", Name: skill.Name},
		Lesson:  "The release check should mention the signing order.", Source: "mcp", Actor: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := db.AcceptSkillChange(ctx, change.ID, "human")
	if err != nil || accepted.Status != SkillChangeAccepted || accepted.ReplacementBlobHash != "" {
		t.Fatalf("lesson-only accept = %+v err=%v", accepted, err)
	}
	current, err := db.ResolveSkillRef(ctx, SkillRef{LibraryKey: "personal", Name: skill.Name})
	if err != nil || current.TreeHash != skill.TreeHash {
		t.Fatalf("accepting lesson changed behavior: %+v err=%v", current, err)
	}
}

func TestStableSkillRefKeepsRetractionWhileExactRefKeepsRevisionState(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	lib, skill := seedSkill(t, db, "personal", "release-guide")
	if _, err := db.RetractMissingSkills(ctx, lib.ID, nil, "human"); err != nil {
		t.Fatal(err)
	}
	stable, err := db.ResolveSkillRef(ctx, SkillRef{LibraryKey: lib.Key, Name: skill.Name})
	if err != nil || stable.Status != SkillRetracted {
		t.Fatalf("stable ref = %+v err=%v", stable, err)
	}
	exact, err := db.ResolveSkillRef(ctx, SkillRef{LibraryKey: lib.Key, Name: skill.Name, TreeHash: skill.TreeHash})
	if err != nil || exact.Status != SkillActive {
		t.Fatalf("exact historical ref = %+v err=%v", exact, err)
	}
}

func TestSkillEventsNeverContainAbsoluteLocalPaths(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	localRoot := filepath.Join(t.TempDir(), "canonical")
	lib, _, err := db.RegisterSkillLibrary(ctx, SkillLibraryInput{
		Key: "acme/skills", Kind: SkillLibraryGit, SourcePath: localRoot,
		SkillsRoot: "skills", Remote: "file://" + localRoot, Head: strings.Repeat("b", 40),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lib.Remote != "" {
		t.Fatalf("local Git remote leaked into portable library provenance: %q", lib.Remote)
	}
	core := validSkillMD("release-guide", "Guide releases", "Do the work.")
	skill, _, _, err := db.PutSkillRevision(ctx, SkillRevisionInput{
		LibraryID: lib.ID, Name: "release-guide", Status: SkillActive,
		Description: "Guide releases", SourceHead: lib.Head,
		Files: []SkillFileInput{{Path: "SKILL.md", Content: core, Mode: 0o644}},
	})
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(t.TempDir(), "targets")
	target, err := db.AddSkillTarget(ctx, "codex", targetPath, "human")
	if err != nil {
		t.Fatal(err)
	}
	detail, err := db.ResolveSkillRef(ctx, SkillRef{LibraryKey: lib.Key, Name: skill.Name, TreeHash: skill.TreeHash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordSkillInstallation(ctx, target, detail, filepath.Join(targetPath, skill.Name), "human"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.sql.QueryContext(ctx, `SELECT pointer, payload FROM events WHERE kind LIKE 'skill.%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var pointer sql.NullString
		var payload string
		if err := rows.Scan(&pointer, &payload); err != nil {
			t.Fatal(err)
		}
		if pointer.Valid {
			t.Errorf("skill event carried local pointer %q", pointer.String)
		}
		for _, forbidden := range []string{localRoot, targetPath} {
			if strings.Contains(payload, forbidden) {
				t.Errorf("skill event payload leaked absolute path %q: %s", forbidden, payload)
			}
		}
	}
}

func seedSkill(t *testing.T, db *DB, libraryKey, name string) (SkillLibrary, Skill) {
	t.Helper()
	ctx := context.Background()
	lib, _, err := db.RegisterSkillLibrary(ctx, SkillLibraryInput{Key: libraryKey, Kind: SkillLibraryNative})
	if err != nil {
		t.Fatal(err)
	}
	skill, _, _, err := db.PutSkillRevision(ctx, SkillRevisionInput{
		LibraryID: lib.ID, Name: name, Status: SkillActive, Description: "Guide releases",
		Files: []SkillFileInput{{Path: "SKILL.md", Content: validSkillMD(name, "Guide releases", "Do the work."), Mode: 0o644}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return lib, skill
}

func validSkillMD(name, description, body string) []byte {
	return []byte("---\nname: " + name + "\ndescription: " + description + "\n---\n\n# Instructions\n\n" + body + "\n")
}

func TestInvalidSkillEvidenceRefIsRejected(t *testing.T) {
	db := newTestDB(t)
	_, skill := seedSkill(t, db, "personal", "release-guide")
	_, err := db.ProposeSkillChange(context.Background(), SkillChangeInput{
		BaseRef: SkillRef{LibraryKey: "personal", Name: skill.Name},
		Lesson:  "lesson", EvidenceRefs: []string{"/absolute/local/path"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid evidence") {
		t.Fatalf("invalid evidence error = %v", err)
	}
}

func TestSkillNotFoundErrorsAreStable(t *testing.T) {
	db := newTestDB(t)
	_, err := db.ResolveSkillRef(context.Background(), SkillRef{LibraryKey: "missing", Name: "release-guide"})
	if !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("error = %v, want ErrSkillNotFound", err)
	}
}
