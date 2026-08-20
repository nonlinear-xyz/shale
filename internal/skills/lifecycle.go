package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/store"
)

type CreateInput struct {
	LibraryKey  string
	Name        string
	Description string
	Body        []byte
}

func CreateSkill(ctx context.Context, db *store.DB, in CreateInput) (store.SkillDetail, []string, error) {
	lib, err := db.SkillLibraryByKey(ctx, strings.TrimSpace(in.LibraryKey))
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	if lib.Kind != store.SkillLibraryNative {
		return store.SkillDetail{}, nil, errors.New("new skills can only be created directly in a Shale-native library")
	}
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if !store.ValidSkillName(in.Name) || in.Description == "" {
		return store.SkillDetail{}, nil, errors.New("create requires a valid --name and non-empty --description")
	}
	if _, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: lib.Key, Name: in.Name}); err == nil {
		return store.SkillDetail{}, nil, fmt.Errorf("skill %s/%s already exists", lib.Key, in.Name)
	} else if !errors.Is(err, store.ErrSkillNotFound) {
		return store.SkillDetail{}, nil, err
	}
	body := append([]byte(nil), in.Body...)
	meta, warnings, validateErr := ValidateSkillMD(body, in.Name)
	if validateErr != nil {
		if bytes.HasPrefix(bytes.TrimSpace(body), []byte("---")) {
			return store.SkillDetail{}, nil, validateErr
		}
		body = WrapSkillBody(in.Name, in.Description, body)
		meta, warnings, validateErr = ValidateSkillMD(body, in.Name)
	}
	if validateErr != nil {
		return store.SkillDetail{}, nil, validateErr
	}
	if meta.Description != in.Description {
		return store.SkillDetail{}, nil, errors.New("--description must match the description in SKILL.md")
	}
	snapshot, err := ValidateFiles([]store.SkillFileInput{{Path: "SKILL.md", Content: body, Mode: 0o644}}, in.Name)
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	skill, _, _, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
		LibraryID: lib.ID, Name: in.Name, Status: store.SkillActive,
		Description: meta.Description, Actor: "human", Files: snapshot.Files,
	})
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	detail, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: lib.Key, Name: skill.Name, TreeHash: skill.TreeHash})
	return detail, warnings, err
}

func ActivateDraft(ctx context.Context, db *store.DB, ref store.SkillRef, description string) (store.SkillDetail, []string, error) {
	detail, err := db.ResolveSkillRef(ctx, ref)
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	if detail.Status != store.SkillDraft {
		return store.SkillDetail{}, nil, fmt.Errorf("%s is not a draft", detail.Ref())
	}
	lib, err := db.SkillLibraryByID(ctx, detail.LibraryID)
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	if lib.Kind != store.SkillLibraryNative {
		return store.SkillDetail{}, nil, errors.New("only a Shale-native draft can be activated")
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return store.SkillDetail{}, nil, errors.New("--description is required to activate a draft")
	}
	files, err := db.SkillRevisionInputs(ctx, detail)
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	var current []byte
	for _, file := range files {
		if filepath.ToSlash(file.Path) == "SKILL.md" {
			current = file.Content
			break
		}
	}
	replacement := current
	meta, warnings, validateErr := ValidateSkillMD(replacement, detail.Name)
	if validateErr != nil {
		replacement = WrapSkillBody(detail.Name, description, current)
		meta, warnings, validateErr = ValidateSkillMD(replacement, detail.Name)
	}
	if validateErr != nil {
		return store.SkillDetail{}, nil, validateErr
	}
	if meta.Description != description {
		return store.SkillDetail{}, nil, errors.New("--description must match the description already present in the draft")
	}
	files = ReplaceSkillMD(files, replacement)
	if _, err := ValidateFiles(files, detail.Name); err != nil {
		return store.SkillDetail{}, nil, err
	}
	skill, _, _, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
		LibraryID: detail.LibraryID, Name: detail.Name, Status: store.SkillActive,
		Description: description, ParentTreeHash: detail.TreeHash, Actor: "human", Files: files,
	})
	if err != nil {
		return store.SkillDetail{}, nil, err
	}
	activated, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: skill.TreeHash})
	return activated, warnings, err
}

type ProposalInput struct {
	Ref          store.SkillRef
	Lesson       string
	Rationale    string
	EvidenceRefs []string
	Replacement  []byte
	Source       string
	Actor        string
}

func ProposeChange(ctx context.Context, db *store.DB, in ProposalInput) (store.SkillChange, []string, error) {
	detail, err := db.ResolveSkillRef(ctx, in.Ref)
	if err != nil {
		return store.SkillChange{}, nil, err
	}
	var warnings []string
	if len(in.Replacement) > 0 {
		warnings, err = ValidateReplacement(ctx, db, detail, in.Replacement)
		if err != nil {
			return store.SkillChange{}, nil, err
		}
	}
	change, err := db.ProposeSkillChange(ctx, store.SkillChangeInput{
		BaseRef: store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash},
		Lesson:  in.Lesson, Rationale: in.Rationale, EvidenceRefs: in.EvidenceRefs,
		Replacement: in.Replacement, Source: in.Source, Actor: in.Actor,
	})
	return change, warnings, err
}

func ValidateReplacement(ctx context.Context, db *store.DB, detail store.SkillDetail, replacement []byte) ([]string, error) {
	meta, warnings, err := ValidateSkillMD(replacement, detail.Name)
	if err != nil {
		return nil, err
	}
	if detail.Status == store.SkillActive && meta.Description == "" {
		return nil, errors.New("replacement must retain a description")
	}
	files, err := db.SkillRevisionInputs(ctx, detail)
	if err != nil {
		return nil, err
	}
	files = ReplaceSkillMD(files, replacement)
	validated, err := ValidateFiles(files, detail.Name)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, validated.Warnings...)
	return uniqueStrings(warnings), nil
}

type ApplyResult struct {
	Change   store.SkillChange
	Skill    *store.SkillDetail
	Worktree string
	Branch   string
	Guidance []string
}

func ApplyChange(ctx context.Context, db *store.DB, changeID string, replacement []byte) (ApplyResult, error) {
	c, err := db.SkillChange(ctx, changeID)
	if err != nil {
		return ApplyResult{}, err
	}
	if c.Status != store.SkillChangeAccepted {
		return ApplyResult{}, fmt.Errorf("%s must be accepted before apply (current status: %s)", c.Ref(), c.Status)
	}
	base, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: c.LibraryKey, Name: c.SkillName, TreeHash: c.BaseTreeHash})
	if err != nil {
		return ApplyResult{}, err
	}
	current, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: c.LibraryKey, Name: c.SkillName})
	if err != nil {
		return ApplyResult{}, err
	}
	if current.TreeHash != c.BaseTreeHash {
		stale, transitionErr := db.MarkSkillChangeStale(ctx, c.ID, "human")
		if transitionErr == nil {
			c = stale
		}
		return ApplyResult{Change: c}, errors.New("proposal base is stale; create a superseding proposal against the current skill revision")
	}
	if len(replacement) > 0 {
		if _, err := ValidateReplacement(ctx, db, base, replacement); err != nil {
			return ApplyResult{}, err
		}
		c, err = db.SetSkillChangeReplacement(ctx, c.ID, replacement, "human")
		if err != nil {
			return ApplyResult{}, err
		}
	}
	if c.ReplacementBlobHash == "" {
		return ApplyResult{}, errors.New("accepted lesson has no replacement yet; pass --replacement with a complete SKILL.md")
	}
	replacement, err = db.ReadSkillChangeReplacement(ctx, c.ID)
	if err != nil {
		return ApplyResult{}, err
	}
	if _, err := ValidateReplacement(ctx, db, base, replacement); err != nil {
		return ApplyResult{}, err
	}
	files, err := db.SkillRevisionInputs(ctx, base)
	if err != nil {
		return ApplyResult{}, err
	}
	files = ReplaceSkillMD(files, replacement)
	meta, _, _ := ValidateSkillMD(replacement, base.Name)
	lib, err := db.SkillLibraryByID(ctx, base.LibraryID)
	if err != nil {
		return ApplyResult{}, err
	}
	if lib.Kind == store.SkillLibraryNative {
		skill, _, _, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
			LibraryID: base.LibraryID, Name: base.Name, Status: store.SkillActive,
			Description: meta.Description, ParentTreeHash: base.TreeHash,
			Actor: "human", Files: files,
		})
		if err != nil {
			return ApplyResult{}, err
		}
		applied, err := db.MarkSkillChangeApplied(ctx, c.ID, skill.TreeHash, "human")
		if err != nil {
			return ApplyResult{}, err
		}
		detail, err := db.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: lib.Key, Name: skill.Name, TreeHash: skill.TreeHash})
		if err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{Change: applied, Skill: &detail}, nil
	}
	return materializeGitChange(ctx, db, lib, base, c, replacement)
}

func materializeGitChange(ctx context.Context, db *store.DB, lib store.SkillLibrary, base store.SkillDetail, c store.SkillChange, replacement []byte) (ApplyResult, error) {
	if IsPluginCachePath(lib.SourcePath) {
		return ApplyResult{}, errors.New("refusing to materialize a proposal from a plugin cache")
	}
	if err := requireCleanGitRoot(ctx, lib.SourcePath, lib.SkillsRoot); err != nil {
		return ApplyResult{}, err
	}
	head, err := gitOutput(ctx, lib.SourcePath, "rev-parse", "HEAD")
	if err != nil {
		return ApplyResult{}, err
	}
	head = strings.TrimSpace(head)
	if c.BaseSourceHead == "" || head != c.BaseSourceHead {
		stale, transitionErr := db.MarkSkillChangeStale(ctx, c.ID, "human")
		if transitionErr == nil {
			c = stale
		}
		return ApplyResult{Change: c}, errors.New("Git HEAD changed since the proposal base; refresh and create a superseding proposal")
	}
	sourceSkill, relSkill, err := gitSkillPath(lib, base.Name)
	if err != nil {
		return ApplyResult{}, err
	}
	sourceSnapshot, err := SnapshotDir(sourceSkill, base.Name)
	if err != nil {
		return ApplyResult{}, err
	}
	sourceHash, err := store.ComputeSkillTreeHash(sourceSnapshot.Files)
	if err != nil {
		return ApplyResult{}, err
	}
	if sourceHash != c.BaseTreeHash {
		stale, transitionErr := db.MarkSkillChangeStale(ctx, c.ID, "human")
		if transitionErr == nil {
			c = stale
		}
		return ApplyResult{Change: c}, errors.New("canonical skill bytes changed since the proposal base; refresh and supersede the proposal")
	}
	short := c.ID
	if len(short) > 8 {
		short = short[:8]
	}
	branch := "shale/skill-" + short
	worktree := filepath.Join(db.StateDir(), "worktrees", "skill-"+short)
	if _, err := os.Lstat(worktree); err == nil {
		return ApplyResult{}, fmt.Errorf("skill worktree already exists at %s", worktree)
	} else if !os.IsNotExist(err) {
		return ApplyResult{}, err
	}
	if _, err := gitOutput(ctx, lib.SourcePath, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
		return ApplyResult{}, fmt.Errorf("Git branch %s already exists", branch)
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return ApplyResult{}, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", lib.SourcePath, "worktree", "add", "-b", branch, worktree, head)
	if body, err := cmd.CombinedOutput(); err != nil {
		return ApplyResult{}, fmt.Errorf("create Git worktree: %s", strings.TrimSpace(string(body)))
	}
	target := filepath.Join(worktree, filepath.FromSlash(relSkill), "SKILL.md")
	if err := ensureWithin(worktree, target); err != nil {
		return ApplyResult{}, err
	}
	if err := os.WriteFile(target, replacement, 0o644); err != nil {
		return ApplyResult{}, fmt.Errorf("write materialized SKILL.md: %w", err)
	}
	if _, err := SnapshotDir(filepath.Dir(target), base.Name); err != nil {
		return ApplyResult{}, fmt.Errorf("validate materialized worktree: %w", err)
	}
	materialized, err := db.MarkSkillChangeMaterialized(ctx, c.ID, worktree, branch, "human")
	if err != nil {
		return ApplyResult{}, fmt.Errorf("worktree created at %s but proposal state could not be recorded: %w", worktree, err)
	}
	return ApplyResult{Change: materialized, Worktree: worktree, Branch: branch, Guidance: ValidationGuidance(worktree)}, nil
}

func gitSkillPath(lib store.SkillLibrary, name string) (absolute, relative string, err error) {
	root := filepath.Join(lib.SourcePath, filepath.FromSlash(lib.SkillsRoot))
	nested := filepath.Join(root, name)
	if _, statErr := os.Lstat(filepath.Join(nested, "SKILL.md")); statErr == nil {
		rel, err := filepath.Rel(lib.SourcePath, nested)
		return nested, filepath.ToSlash(rel), err
	}
	if _, statErr := os.Lstat(filepath.Join(root, "SKILL.md")); statErr == nil {
		rel, err := filepath.Rel(lib.SourcePath, root)
		return root, filepath.ToSlash(rel), err
	}
	return "", "", fmt.Errorf("canonical source for skill %s/%s is missing", lib.Key, name)
}

func ValidationGuidance(projectRoot string) []string {
	checks := []struct {
		file string
		cmd  string
	}{
		{"pnpm-lock.yaml", "pnpm test"},
		{"yarn.lock", "yarn test"},
		{"package-lock.json", "npm test"},
		{"package.json", "npm test"},
		{"go.mod", "go test ./..."},
		{"pyproject.toml", "pytest"},
		{"Cargo.toml", "cargo test"},
	}
	for _, check := range checks {
		if _, err := os.Stat(filepath.Join(projectRoot, check.file)); err == nil {
			return []string{check.cmd}
		}
	}
	return nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
