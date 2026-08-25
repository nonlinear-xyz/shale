package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/scrub"
)

type SkillChangeStatus string

const (
	SkillChangePending      SkillChangeStatus = "pending"
	SkillChangeAccepted     SkillChangeStatus = "accepted"
	SkillChangeMaterialized SkillChangeStatus = "materialized"
	SkillChangeApplied      SkillChangeStatus = "applied"
	SkillChangeRejected     SkillChangeStatus = "rejected"
	SkillChangeStale        SkillChangeStatus = "stale"
)

type SkillChange struct {
	ID                  string
	LibraryID           string
	LibraryKey          string
	SkillName           string
	BaseTreeHash        string
	BaseSourceHead      string
	ResultTreeHash      string
	Status              SkillChangeStatus
	Lesson              string
	Rationale           string
	EvidenceRefs        []string
	ReplacementBlobHash string
	Source              string
	Actor               string
	MaterializedPath    string // local-only projection field
	MaterializedBranch  string // local-only projection field
	EventSeq            int64
	CreatedAt           string
	UpdatedAt           string
}

func (c SkillChange) Ref() string { return "skill-change:" + c.ID }

func (c SkillChange) SkillRef() string {
	return SkillRef{LibraryKey: c.LibraryKey, Name: c.SkillName, TreeHash: c.BaseTreeHash}.String()
}

type SkillChangeRef struct{ ID string }

func (r SkillChangeRef) String() string { return "skill-change:" + r.ID }

func ParseSkillChangeRef(value string) (SkillChangeRef, error) {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "skill-change:") {
		return SkillChangeRef{}, fmt.Errorf("not a skill change ref: %q", value)
	}
	id := strings.TrimPrefix(value, "skill-change:")
	if !validArtifactID(id) {
		return SkillChangeRef{}, fmt.Errorf("bad skill change id in %q", value)
	}
	return SkillChangeRef{ID: id}, nil
}

type SkillChangeInput struct {
	BaseRef      SkillRef
	Lesson       string
	Rationale    string
	EvidenceRefs []string
	Replacement  []byte
	Source       string
	Actor        string
}

var ErrSkillChangeNotFound = errors.New("skill change not found")

func (d *DB) ProposeSkillChange(ctx context.Context, in SkillChangeInput) (SkillChange, error) {
	if in.BaseRef.TreeHash == "" {
		detail, err := d.ResolveSkillRef(ctx, in.BaseRef)
		if err != nil {
			return SkillChange{}, err
		}
		in.BaseRef.TreeHash = detail.TreeHash
	}
	detail, err := d.ResolveSkillRef(ctx, in.BaseRef)
	if err != nil {
		return SkillChange{}, err
	}
	if detail.Status == SkillRetracted {
		return SkillChange{}, errors.New("cannot propose a change to a retracted skill")
	}
	if in.Source == "" {
		in.Source = "cli"
	}
	if in.Actor == "" {
		in.Actor = "agent"
	}
	if strings.TrimSpace(in.Lesson) == "" {
		return SkillChange{}, errors.New("skill change lesson is required")
	}
	if len(in.Lesson) > ArtifactContentMax || len(in.Rationale) > ArtifactContentMax {
		return SkillChange{}, errors.New("skill change prose exceeds 64 KiB")
	}
	if len(in.Replacement) > 1<<20 {
		return SkillChange{}, errors.New("replacement SKILL.md exceeds 1 MiB")
	}
	if err := validatePortableEvidenceRefs(in.EvidenceRefs); err != nil {
		return SkillChange{}, err
	}
	sc, _ := scrub.New()
	lesson := strings.TrimSpace(sc.String(in.Lesson))
	rationale := strings.TrimSpace(sc.String(in.Rationale))
	evidence, _ := json.Marshal(in.EvidenceRefs)
	replacementHash := ""
	if len(in.Replacement) > 0 {
		sum := sha256Bytes(in.Replacement)
		replacementHash = sum
		if err := writeExactFile(d.SkillBlobPath(sum), in.Replacement); err != nil {
			return SkillChange{}, err
		}
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	c := SkillChange{
		ID: randomID(), LibraryID: detail.LibraryID, LibraryKey: detail.LibraryKey,
		SkillName: detail.Name, BaseTreeHash: detail.TreeHash, Status: SkillChangePending,
		Lesson: lesson, Rationale: rationale, EvidenceRefs: append([]string(nil), in.EvidenceRefs...),
		ReplacementBlobHash: replacementHash, Source: in.Source, Actor: in.Actor,
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	if lib, libErr := d.SkillLibraryByID(ctx, detail.LibraryID); libErr == nil && lib.Kind == SkillLibraryGit {
		c.BaseSourceHead = lib.Head
	}
	payload := skillChangeEventPayload(c)
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillChange{}, err
	}
	defer tx.Rollback()
	seq, err := appendSkillEvent(ctx, tx, KindSkillChangeProposed, in.Actor, detail.LibraryKey, replacementHash, payload)
	if err != nil {
		return SkillChange{}, err
	}
	c.EventSeq = seq
	_, err = tx.ExecContext(ctx, `
		INSERT INTO skill_changes
		  (id, library_id, skill_name, base_tree_hash, base_source_head, result_tree_hash, status,
		   lesson, rationale, evidence_refs, replacement_blob_hash, source, actor,
		   materialized_path, materialized_branch, current_event_seq, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, ?)`,
		c.ID, c.LibraryID, c.SkillName, c.BaseTreeHash, c.BaseSourceHead, c.Status, c.Lesson,
		c.Rationale, string(evidence), nullable(c.ReplacementBlobHash), c.Source,
		c.Actor, c.EventSeq, stamp, stamp)
	if err != nil {
		return SkillChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillChange{}, err
	}
	return c, nil
}

func sha256Bytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func skillChangeEventPayload(c SkillChange) map[string]any {
	return map[string]any{
		"changeId": c.ID, "libraryId": c.LibraryID, "libraryKey": c.LibraryKey,
		"skillName": c.SkillName, "baseTreeHash": c.BaseTreeHash,
		"baseSourceHead": c.BaseSourceHead,
		"resultTreeHash": c.ResultTreeHash, "status": c.Status,
		"hasReplacement": c.ReplacementBlobHash != "",
	}
}

func (d *DB) SkillChange(ctx context.Context, id string) (SkillChange, error) {
	var c SkillChange
	var lesson, rationale, evidence, replacement sql.NullString
	err := d.sql.QueryRowContext(ctx, `
		SELECT c.id, c.library_id, l.key, c.skill_name, c.base_tree_hash,
		       c.base_source_head, c.result_tree_hash, c.status, c.lesson, c.rationale, c.evidence_refs,
		       c.replacement_blob_hash, c.source, c.actor, c.materialized_path,
		       c.materialized_branch, c.current_event_seq, c.created_at, c.updated_at
		FROM skill_changes c JOIN skill_libraries l ON l.id = c.library_id
		WHERE c.id = ?`, id).
		Scan(&c.ID, &c.LibraryID, &c.LibraryKey, &c.SkillName, &c.BaseTreeHash,
			&c.BaseSourceHead, &c.ResultTreeHash, &c.Status, &lesson, &rationale, &evidence,
			&replacement, &c.Source, &c.Actor, &c.MaterializedPath,
			&c.MaterializedBranch, &c.EventSeq, &c.CreatedAt, &c.UpdatedAt)
	if err == sql.ErrNoRows {
		return c, ErrSkillChangeNotFound
	}
	if err != nil {
		return c, err
	}
	c.Lesson, c.Rationale, c.ReplacementBlobHash = lesson.String, rationale.String, replacement.String
	if evidence.Valid && evidence.String != "" {
		if err := json.Unmarshal([]byte(evidence.String), &c.EvidenceRefs); err != nil {
			return c, err
		}
	}
	return c, nil
}

type SkillChangeFilter struct {
	Status     SkillChangeStatus
	LibraryKey string
	Limit      int
}

func (d *DB) ListSkillChanges(ctx context.Context, f SkillChangeFilter) ([]SkillChange, error) {
	query := `SELECT c.id FROM skill_changes c JOIN skill_libraries l ON l.id = c.library_id WHERE 1=1`
	var args []any
	if f.Status != "" {
		query += ` AND c.status = ?`
		args = append(args, f.Status)
	}
	if f.LibraryKey != "" {
		query += ` AND l.key = ?`
		args = append(args, f.LibraryKey)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query += ` ORDER BY c.updated_at DESC LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]SkillChange, 0, len(ids))
	for _, id := range ids {
		c, err := d.SkillChange(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (d *DB) ReadSkillChangeReplacement(ctx context.Context, id string) ([]byte, error) {
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.ReplacementBlobHash == "" {
		return nil, os.ErrNotExist
	}
	return d.ReadSkillBlob(c.ReplacementBlobHash)
}

func requireHuman(actor string) error {
	if actor != "human" {
		return errors.New("this skill change transition requires a human actor")
	}
	return nil
}

func (d *DB) AcceptSkillChange(ctx context.Context, id, actor string) (SkillChange, error) {
	if err := requireHuman(actor); err != nil {
		return SkillChange{}, err
	}
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if c.Status != SkillChangePending {
		return SkillChange{}, fmt.Errorf("%s is not pending", c.Ref())
	}
	return d.transitionSkillChange(ctx, c, SkillChangeAccepted, KindSkillChangeAccepted, actor, "", "", "")
}

func (d *DB) SetSkillChangeReplacement(ctx context.Context, id string, replacement []byte, actor string) (SkillChange, error) {
	if err := requireHuman(actor); err != nil {
		return SkillChange{}, err
	}
	if len(replacement) == 0 || len(replacement) > 1<<20 {
		return SkillChange{}, errors.New("replacement SKILL.md must be between 1 byte and 1 MiB")
	}
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if c.Status != SkillChangeAccepted {
		return SkillChange{}, fmt.Errorf("%s must be accepted before attaching a replacement", c.Ref())
	}
	hash := sha256Bytes(replacement)
	if err := writeExactFile(d.SkillBlobPath(hash), replacement); err != nil {
		return SkillChange{}, err
	}
	old := c.ReplacementBlobHash
	c.ReplacementBlobHash = hash
	c, err = d.transitionSkillChange(ctx, c, SkillChangeAccepted, KindSkillChangeReplacement, actor, "", "", "")
	if err != nil {
		return SkillChange{}, err
	}
	if old != "" && old != hash {
		_ = d.removeUnreferencedSkillBlob(ctx, old)
	}
	return c, nil
}

func (d *DB) RejectSkillChange(ctx context.Context, id, actor string) (SkillChange, error) {
	if err := requireHuman(actor); err != nil {
		return SkillChange{}, err
	}
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if c.Status != SkillChangePending && c.Status != SkillChangeAccepted {
		return SkillChange{}, fmt.Errorf("%s cannot be rejected from status %s", c.Ref(), c.Status)
	}
	oldHash := c.ReplacementBlobHash
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	c.Status, c.Lesson, c.Rationale, c.EvidenceRefs, c.ReplacementBlobHash = SkillChangeRejected, "", "", nil, ""
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillChange{}, err
	}
	defer tx.Rollback()
	seq, err := appendSkillEvent(ctx, tx, KindSkillChangeRejected, actor, c.LibraryKey, "", skillChangeEventPayload(c))
	if err == nil {
		_, err = tx.ExecContext(ctx, `
			UPDATE skill_changes SET status = ?, lesson = NULL, rationale = NULL,
			  evidence_refs = NULL, replacement_blob_hash = NULL,
			  current_event_seq = ?, updated_at = ? WHERE id = ?`, c.Status, seq, stamp, c.ID)
	}
	if err != nil {
		return SkillChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillChange{}, err
	}
	c.EventSeq, c.UpdatedAt = seq, stamp
	if oldHash != "" {
		if err := d.removeUnreferencedSkillBlob(ctx, oldHash); err != nil {
			return c, err
		}
	}
	if _, err := d.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return c, err
	}
	return c, nil
}

func (d *DB) MarkSkillChangeStale(ctx context.Context, id, actor string) (SkillChange, error) {
	if err := requireHuman(actor); err != nil {
		return SkillChange{}, err
	}
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if c.Status != SkillChangeAccepted {
		return SkillChange{}, fmt.Errorf("%s cannot become stale from status %s", c.Ref(), c.Status)
	}
	return d.transitionSkillChange(ctx, c, SkillChangeStale, KindSkillChangeStale, actor, "", "", "")
}

func (d *DB) MarkSkillChangeMaterialized(ctx context.Context, id, path, branch, actor string) (SkillChange, error) {
	if err := requireHuman(actor); err != nil {
		return SkillChange{}, err
	}
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if c.Status != SkillChangeAccepted {
		return SkillChange{}, fmt.Errorf("%s cannot be materialized from status %s", c.Ref(), c.Status)
	}
	if !filepath.IsAbs(path) || strings.TrimSpace(branch) == "" {
		return SkillChange{}, errors.New("materialized change requires an absolute worktree path and branch")
	}
	return d.transitionSkillChange(ctx, c, SkillChangeMaterialized, KindSkillChangeMaterialized, actor, path, branch, "")
}

func (d *DB) MarkSkillChangeApplied(ctx context.Context, id, resultTreeHash, actor string) (SkillChange, error) {
	c, err := d.SkillChange(ctx, id)
	if err != nil {
		return SkillChange{}, err
	}
	if actor != "human" && actor != "system" {
		return SkillChange{}, errors.New("applying a skill change requires a human action or canonical-source observation")
	}
	if c.Status != SkillChangeAccepted && c.Status != SkillChangeMaterialized {
		return SkillChange{}, fmt.Errorf("%s cannot be applied from status %s", c.Ref(), c.Status)
	}
	if !validSHA256(resultTreeHash) {
		return SkillChange{}, errors.New("applied skill change requires a valid result tree hash")
	}
	return d.transitionSkillChange(ctx, c, SkillChangeApplied, KindSkillChangeApplied, actor, c.MaterializedPath, c.MaterializedBranch, resultTreeHash)
}

func (d *DB) transitionSkillChange(ctx context.Context, c SkillChange, status SkillChangeStatus, eventKind, actor, path, branch, resultHash string) (SkillChange, error) {
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	c.Status = status
	if path != "" {
		c.MaterializedPath = path
	}
	if branch != "" {
		c.MaterializedBranch = branch
	}
	if resultHash != "" {
		c.ResultTreeHash = resultHash
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillChange{}, err
	}
	defer tx.Rollback()
	seq, err := appendSkillEvent(ctx, tx, eventKind, actor, c.LibraryKey, resultHash, skillChangeEventPayload(c))
	if err == nil {
		_, err = tx.ExecContext(ctx, `
			UPDATE skill_changes SET status = ?, result_tree_hash = ?,
			  replacement_blob_hash = ?, materialized_path = ?, materialized_branch = ?,
			  current_event_seq = ?, updated_at = ? WHERE id = ?`,
			c.Status, c.ResultTreeHash, nullable(c.ReplacementBlobHash),
			c.MaterializedPath, c.MaterializedBranch, seq, stamp, c.ID)
	}
	if err != nil {
		return SkillChange{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillChange{}, err
	}
	c.EventSeq, c.UpdatedAt = seq, stamp
	return c, nil
}

// ObserveMaterializedSkillChanges marks a reviewed Git proposal applied only
// after refresh sees its exact replacement in the canonical source tree.
func (d *DB) ObserveMaterializedSkillChanges(ctx context.Context, libraryID, skillName, treeHash string) (int, error) {
	var skillBlob string
	err := d.sql.QueryRowContext(ctx, `
		SELECT blob_hash FROM skill_revision_files
		WHERE library_id = ? AND skill_name = ? AND tree_hash = ? AND path = 'SKILL.md'`,
		libraryID, skillName, treeHash).Scan(&skillBlob)
	if err != nil {
		return 0, err
	}
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id FROM skill_changes WHERE library_id = ? AND skill_name = ?
		  AND status = 'materialized' AND replacement_blob_hash = ?`, libraryID, skillName, skillBlob)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err := d.MarkSkillChangeApplied(ctx, id, treeHash, "system"); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

func validatePortableEvidenceRefs(refs []string) error {
	if len(refs) > 100 {
		return errors.New("skill change has more than 100 evidence refs")
	}
	for _, value := range refs {
		value = strings.TrimSpace(value)
		if _, err := ParseArtifactRef(value); err == nil {
			continue
		}
		if _, err := ParseRef(value); err == nil {
			continue
		}
		if _, err := ParseSkillRef(value); err == nil {
			continue
		}
		if _, err := ParseSkillFileRef(value); err == nil {
			continue
		}
		if _, err := ParseSkillChangeRef(value); err == nil {
			continue
		}
		return fmt.Errorf("invalid evidence ref %q", value)
	}
	return nil
}

func (d *DB) removeUnreferencedSkillBlob(ctx context.Context, hash string) error {
	var revisions, changes int
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM skill_revision_files WHERE blob_hash = ?`, hash).Scan(&revisions); err != nil {
		return err
	}
	if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM skill_changes WHERE replacement_blob_hash = ?`, hash).Scan(&changes); err != nil {
		return err
	}
	if revisions+changes == 0 {
		if err := os.Remove(d.SkillBlobPath(hash)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

type SkillTarget struct {
	ID        string
	Name      string
	Path      string // local-only projection field
	CreatedAt string
	UpdatedAt string
}

func (d *DB) AddSkillTarget(ctx context.Context, name, path, actor string) (SkillTarget, error) {
	name, path = strings.TrimSpace(name), filepath.Clean(strings.TrimSpace(path))
	if !validArtifactID(name) {
		return SkillTarget{}, errors.New("target name may contain only letters, digits, dot, dash, and underscore")
	}
	if !filepath.IsAbs(path) {
		return SkillTarget{}, errors.New("skill target path must be absolute")
	}
	if actor != "human" {
		return SkillTarget{}, errors.New("adding a skill target requires a human actor")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	target := SkillTarget{ID: randomID(), Name: name, Path: path, CreatedAt: stamp, UpdatedAt: stamp}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillTarget{}, err
	}
	defer tx.Rollback()
	payload := map[string]any{"targetId": target.ID, "targetName": target.Name}
	if _, err := appendSkillEvent(ctx, tx, KindSkillTargetAdded, actor, "personal", "", payload); err != nil {
		return SkillTarget{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO skill_targets (id, name, path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, target.ID, target.Name, target.Path, stamp, stamp)
	if err != nil {
		return SkillTarget{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillTarget{}, err
	}
	return target, nil
}

func (d *DB) SkillTargetByName(ctx context.Context, name string) (SkillTarget, error) {
	var t SkillTarget
	err := d.sql.QueryRowContext(ctx, `SELECT id, name, path, created_at, updated_at FROM skill_targets WHERE name = ?`, name).
		Scan(&t.ID, &t.Name, &t.Path, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return t, errors.New("skill target not found")
	}
	return t, err
}

func (d *DB) ListSkillTargets(ctx context.Context) ([]SkillTarget, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, name, path, created_at, updated_at FROM skill_targets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillTarget
	for rows.Next() {
		var t SkillTarget
		if err := rows.Scan(&t.ID, &t.Name, &t.Path, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type SkillInstallation struct {
	TargetID      string
	TargetName    string
	LibraryID     string
	LibraryKey    string
	SkillName     string
	TreeHash      string
	InstalledPath string // local-only projection field
	InstalledAt   string
}

func (d *DB) SkillInstallation(ctx context.Context, targetID, libraryID, skillName string) (SkillInstallation, error) {
	var i SkillInstallation
	err := d.sql.QueryRowContext(ctx, `
		SELECT i.target_id, t.name, i.library_id, l.key, i.skill_name,
		       i.tree_hash, i.installed_path, i.installed_at
		FROM skill_installations i
		JOIN skill_targets t ON t.id = i.target_id
		JOIN skill_libraries l ON l.id = i.library_id
		WHERE i.target_id = ? AND i.library_id = ? AND i.skill_name = ?`,
		targetID, libraryID, skillName).
		Scan(&i.TargetID, &i.TargetName, &i.LibraryID, &i.LibraryKey, &i.SkillName,
			&i.TreeHash, &i.InstalledPath, &i.InstalledAt)
	if err == sql.ErrNoRows {
		return i, os.ErrNotExist
	}
	return i, err
}

func (d *DB) RecordSkillInstallation(ctx context.Context, target SkillTarget, detail SkillDetail, installedPath, actor string) (SkillInstallation, error) {
	if actor != "human" {
		return SkillInstallation{}, errors.New("installing a skill requires a human actor")
	}
	if !filepath.IsAbs(installedPath) {
		return SkillInstallation{}, errors.New("installed path must be absolute")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	i := SkillInstallation{TargetID: target.ID, TargetName: target.Name,
		LibraryID: detail.LibraryID, LibraryKey: detail.LibraryKey,
		SkillName: detail.Name, TreeHash: detail.TreeHash,
		InstalledPath: installedPath, InstalledAt: stamp}
	payload := map[string]any{
		"targetId": target.ID, "targetName": target.Name,
		"libraryId": detail.LibraryID, "libraryKey": detail.LibraryKey,
		"skillName": detail.Name, "treeHash": detail.TreeHash,
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillInstallation{}, err
	}
	defer tx.Rollback()
	if _, err := appendSkillEvent(ctx, tx, KindSkillInstalled, actor, detail.LibraryKey, detail.TreeHash, payload); err != nil {
		return SkillInstallation{}, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO skill_installations
		  (target_id, library_id, skill_name, tree_hash, installed_path, installed_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(target_id, library_id, skill_name) DO UPDATE SET
		  tree_hash=excluded.tree_hash, installed_path=excluded.installed_path,
		  installed_at=excluded.installed_at`, target.ID, detail.LibraryID,
		detail.Name, detail.TreeHash, installedPath, stamp)
	if err != nil {
		return SkillInstallation{}, err
	}
	if err := tx.Commit(); err != nil {
		return SkillInstallation{}, err
	}
	return i, nil
}

func (d *DB) ListSkillInstallations(ctx context.Context) ([]SkillInstallation, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT i.target_id, t.name, i.library_id, l.key, i.skill_name,
		       i.tree_hash, i.installed_path, i.installed_at
		FROM skill_installations i
		JOIN skill_targets t ON t.id = i.target_id
		JOIN skill_libraries l ON l.id = i.library_id
		ORDER BY t.name, l.key, i.skill_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillInstallation
	for rows.Next() {
		var i SkillInstallation
		if err := rows.Scan(&i.TargetID, &i.TargetName, &i.LibraryID, &i.LibraryKey,
			&i.SkillName, &i.TreeHash, &i.InstalledPath, &i.InstalledAt); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}
