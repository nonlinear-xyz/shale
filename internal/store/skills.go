package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// skillSchema is schema version 2. Absolute paths are deliberately confined to
// local projection columns. The append-only events written for these tables use
// portable library keys, skill names, hashes, and change IDs only.
const skillSchema = `
CREATE TABLE IF NOT EXISTS skill_libraries (
  id          TEXT PRIMARY KEY,
  key         TEXT NOT NULL UNIQUE,
  owner_kind  TEXT NOT NULL DEFAULT 'user',
  owner_key   TEXT NOT NULL DEFAULT 'local',
  kind        TEXT NOT NULL,
  source_path TEXT NOT NULL DEFAULT '',
  skills_root TEXT NOT NULL DEFAULT '.',
  remote      TEXT NOT NULL DEFAULT '',
  head        TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skills (
  library_id       TEXT NOT NULL,
  name             TEXT NOT NULL,
  status           TEXT NOT NULL,
  description      TEXT NOT NULL DEFAULT '',
  current_tree_hash TEXT NOT NULL,
  current_event_seq INTEGER NOT NULL,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL,
  PRIMARY KEY (library_id, name),
  FOREIGN KEY (library_id) REFERENCES skill_libraries(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS skills_status_idx ON skills(status, updated_at DESC);

CREATE TABLE IF NOT EXISTS skill_revisions (
  library_id      TEXT NOT NULL,
  skill_name      TEXT NOT NULL,
  tree_hash       TEXT NOT NULL,
  parent_tree_hash TEXT NOT NULL DEFAULT '',
  source_head     TEXT NOT NULL DEFAULT '',
  event_seq       INTEGER NOT NULL,
  created_at      TEXT NOT NULL,
  PRIMARY KEY (library_id, skill_name, tree_hash),
  FOREIGN KEY (library_id, skill_name) REFERENCES skills(library_id, name) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS skill_revisions_hash_idx ON skill_revisions(tree_hash);

CREATE TABLE IF NOT EXISTS skill_revision_files (
  library_id TEXT NOT NULL,
  skill_name TEXT NOT NULL,
  tree_hash  TEXT NOT NULL,
  path       TEXT NOT NULL,
  blob_hash  TEXT NOT NULL,
  mode       INTEGER NOT NULL,
  size       INTEGER NOT NULL,
  PRIMARY KEY (library_id, skill_name, tree_hash, path),
  FOREIGN KEY (library_id, skill_name, tree_hash)
    REFERENCES skill_revisions(library_id, skill_name, tree_hash) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS skill_revision_files_blob_idx ON skill_revision_files(blob_hash);

CREATE TABLE IF NOT EXISTS skill_changes (
  id                   TEXT PRIMARY KEY,
  library_id           TEXT NOT NULL,
  skill_name           TEXT NOT NULL,
  base_tree_hash       TEXT NOT NULL,
  result_tree_hash     TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL,
  lesson               TEXT,
  rationale            TEXT,
  evidence_refs        TEXT,
  replacement_blob_hash TEXT,
  source               TEXT NOT NULL,
  actor                TEXT NOT NULL,
  materialized_path    TEXT NOT NULL DEFAULT '',
  materialized_branch  TEXT NOT NULL DEFAULT '',
  current_event_seq    INTEGER NOT NULL,
  created_at           TEXT NOT NULL,
  updated_at           TEXT NOT NULL,
  FOREIGN KEY (library_id, skill_name) REFERENCES skills(library_id, name) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS skill_changes_status_idx ON skill_changes(status, updated_at DESC);
CREATE INDEX IF NOT EXISTS skill_changes_skill_idx ON skill_changes(library_id, skill_name, updated_at DESC);

CREATE TABLE IF NOT EXISTS skill_targets (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,
  path       TEXT NOT NULL UNIQUE,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skill_installations (
  target_id      TEXT NOT NULL,
  library_id     TEXT NOT NULL,
  skill_name     TEXT NOT NULL,
  tree_hash      TEXT NOT NULL,
  installed_path TEXT NOT NULL,
  installed_at   TEXT NOT NULL,
  PRIMARY KEY (target_id, library_id, skill_name),
  FOREIGN KEY (target_id) REFERENCES skill_targets(id) ON DELETE CASCADE,
  FOREIGN KEY (library_id, skill_name, tree_hash)
    REFERENCES skill_revisions(library_id, skill_name, tree_hash)
);

CREATE VIRTUAL TABLE IF NOT EXISTS skills_fts USING fts5(
  name, description, body,
  library_id UNINDEXED, library_key UNINDEXED,
  status UNINDEXED, tree_hash UNINDEXED, path UNINDEXED, updated_at UNINDEXED,
  tokenize='porter unicode61'
);
`

type SkillLibraryKind string

const (
	SkillLibraryNative SkillLibraryKind = "native"
	SkillLibraryGit    SkillLibraryKind = "git"
)

type SkillStatus string

const (
	SkillDraft     SkillStatus = "draft"
	SkillActive    SkillStatus = "active"
	SkillRetracted SkillStatus = "retracted"
)

type SkillLibrary struct {
	ID         string
	Key        string
	OwnerKind  string
	OwnerKey   string
	Kind       SkillLibraryKind
	SourcePath string // local-only projection field
	SkillsRoot string
	Remote     string
	Head       string
	CreatedAt  string
	UpdatedAt  string
}

type SkillLibraryInput struct {
	Key        string
	OwnerKind  string
	OwnerKey   string
	Kind       SkillLibraryKind
	SourcePath string
	SkillsRoot string
	Remote     string
	Head       string
	Actor      string
}

type Skill struct {
	LibraryID  string
	LibraryKey string
	Name       string
	Status     SkillStatus
	Description string
	TreeHash   string
	EventSeq   int64
	CreatedAt  string
	UpdatedAt  string
}

func (s Skill) Ref() string { return "skill:" + s.LibraryKey + "/" + s.Name }

func (s Skill) VersionedRef() string {
	if s.TreeHash == "" {
		return s.Ref()
	}
	return s.Ref() + "@" + s.TreeHash
}

type SkillRevision struct {
	LibraryID     string
	LibraryKey    string
	SkillName     string
	TreeHash      string
	ParentTreeHash string
	SourceHead    string
	EventSeq      int64
	CreatedAt     string
}

type SkillFileInput struct {
	Path    string
	Content []byte
	Mode    os.FileMode
}

type SkillRevisionInput struct {
	LibraryID     string
	Name          string
	Status        SkillStatus
	Description   string
	ParentTreeHash string
	SourceHead    string
	Actor         string
	Files         []SkillFileInput
}

type SkillRevisionFile struct {
	Path     string
	BlobHash string
	Mode     os.FileMode
	Size     int64
}

type SkillDetail struct {
	Skill
	Revision SkillRevision
	Files    []SkillRevisionFile
}

type SkillRef struct {
	LibraryKey string
	Name       string
	TreeHash   string
}

// SkillFileRef addresses one exact file in a skill tree. Search results mint
// versioned refs so following a reference cannot silently read a newer skill.
// The fragment form keeps the portable skill identity intact:
//
//   skill:owner/library/name@<tree-hash>#references/details.md
type SkillFileRef struct {
	SkillRef
	Path string
}

func (r SkillFileRef) String() string { return r.SkillRef.String() + "#" + r.Path }

func (r SkillRef) String() string {
	base := "skill:" + r.LibraryKey + "/" + r.Name
	if r.TreeHash != "" {
		return base + "@" + r.TreeHash
	}
	return base
}

var (
	ErrSkillLibraryNotFound = errors.New("skill library not found")
	ErrSkillNotFound        = errors.New("skill not found")
)

// StateDir returns Shale's private local state directory. Callers may use it
// for operational worktrees and staging, but must never place it in events.
func (d *DB) StateDir() string { return d.root }

func ValidSkillName(name string) bool {
	if name == "" || len(name) >= 64 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func ValidLibraryKey(key string) bool {
	if key == "" || len(key) > 240 || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
				continue
			}
			return false
		}
	}
	return true
}

func ParseSkillRef(value string) (SkillRef, error) {
	value = strings.TrimSpace(value)
	if strings.Contains(value, "#") {
		return SkillRef{}, fmt.Errorf("skill file ref requires ParseSkillFileRef: %q", value)
	}
	if !strings.HasPrefix(value, "skill:") {
		return SkillRef{}, fmt.Errorf("not a skill ref: %q", value)
	}
	body := strings.TrimPrefix(value, "skill:")
	var tree string
	if at := strings.LastIndexByte(body, '@'); at >= 0 {
		tree, body = body[at+1:], body[:at]
		if !validSHA256(tree) {
			return SkillRef{}, fmt.Errorf("bad skill revision in %q", value)
		}
	}
	slash := strings.LastIndexByte(body, '/')
	if slash <= 0 || slash == len(body)-1 {
		return SkillRef{}, fmt.Errorf("bad skill ref %q", value)
	}
	r := SkillRef{LibraryKey: body[:slash], Name: body[slash+1:], TreeHash: tree}
	if !ValidLibraryKey(r.LibraryKey) || !ValidSkillName(r.Name) {
		return SkillRef{}, fmt.Errorf("bad skill ref %q", value)
	}
	return r, nil
}

func ParseSkillFileRef(value string) (SkillFileRef, error) {
	value = strings.TrimSpace(value)
	hash := strings.IndexByte(value, '#')
	if hash < 0 {
		return SkillFileRef{}, fmt.Errorf("not a skill file ref: %q", value)
	}
	base, err := ParseSkillRef(value[:hash])
	if err != nil {
		return SkillFileRef{}, err
	}
	path := filepath.ToSlash(filepath.Clean(value[hash+1:]))
	if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || strings.ContainsRune(path, 0) {
		return SkillFileRef{}, fmt.Errorf("unsafe skill file ref %q", value)
	}
	return SkillFileRef{SkillRef: base, Path: path}, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (d *DB) SkillBlobPath(hash string) string {
	if len(hash) < 2 {
		return filepath.Join(d.root, "skill-blobs", hash)
	}
	return filepath.Join(d.root, "skill-blobs", hash[:2], hash)
}

func (d *DB) ReadSkillBlob(hash string) ([]byte, error) {
	if !validSHA256(hash) {
		return nil, errors.New("invalid skill blob hash")
	}
	f, err := os.Open(d.SkillBlobPath(hash))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxBlob = 16 << 20
	body, err := io.ReadAll(io.LimitReader(f, maxBlob+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBlob {
		return nil, errors.New("skill blob exceeds 16 MiB")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, errors.New("skill blob hash mismatch")
	}
	return body, nil
}

func writeExactFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == string(body) {
			return nil
		}
		return errors.New("content-addressed skill blob collision")
	} else if !os.IsNotExist(err) {
		return err
	}
	tmp := path + ".tmp-" + randomID()
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		if existing, readErr := os.ReadFile(path); readErr == nil && string(existing) == string(body) {
			return nil
		}
		return err
	}
	return nil
}

func (d *DB) RegisterSkillLibrary(ctx context.Context, in SkillLibraryInput) (SkillLibrary, bool, error) {
	in.Key = strings.TrimSpace(in.Key)
	in.OwnerKind = strings.TrimSpace(in.OwnerKind)
	in.OwnerKey = strings.TrimSpace(in.OwnerKey)
	in.SkillsRoot = filepath.ToSlash(filepath.Clean(strings.TrimSpace(in.SkillsRoot)))
	if in.OwnerKind == "" {
		in.OwnerKind = "user"
	}
	if in.OwnerKey == "" {
		in.OwnerKey = "local"
	}
	if in.SkillsRoot == "" {
		in.SkillsRoot = "."
	}
	if in.Actor == "" {
		in.Actor = "human"
	}
	if !ValidLibraryKey(in.Key) {
		return SkillLibrary{}, false, fmt.Errorf("invalid library key %q", in.Key)
	}
	if in.OwnerKind != "user" && in.OwnerKind != "team" {
		return SkillLibrary{}, false, errors.New("library owner kind must be user or team")
	}
	if in.OwnerKey == "" || len(in.OwnerKey) > 240 {
		return SkillLibrary{}, false, errors.New("library owner key is invalid")
	}
	if in.Kind != SkillLibraryNative && in.Kind != SkillLibraryGit {
		return SkillLibrary{}, false, errors.New("library kind must be native or git")
	}
	if in.Kind == SkillLibraryGit {
		if !filepath.IsAbs(in.SourcePath) {
			return SkillLibrary{}, false, errors.New("git library source path must be absolute")
		}
		if filepath.IsAbs(in.SkillsRoot) || in.SkillsRoot == ".." || strings.HasPrefix(in.SkillsRoot, "../") {
			return SkillLibrary{}, false, errors.New("git library skills root must be relative")
		}
	} else {
		// A native library is canonically represented by its immutable blobs, not
		// by the directory it was imported from.
		in.SourcePath, in.SkillsRoot = "", "."
	}
	if existing, err := d.SkillLibraryByKey(ctx, in.Key); err == nil {
		if existing.Kind != in.Kind || existing.SourcePath != in.SourcePath || existing.SkillsRoot != in.SkillsRoot {
			return SkillLibrary{}, false, fmt.Errorf("library key %q is already registered to a different source", in.Key)
		}
		return existing, false, nil
	} else if !errors.Is(err, ErrSkillLibraryNotFound) {
		return SkillLibrary{}, false, err
	}

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	lib := SkillLibrary{
		ID: randomID(), Key: in.Key, OwnerKind: in.OwnerKind, OwnerKey: in.OwnerKey,
		Kind: in.Kind, SourcePath: in.SourcePath, SkillsRoot: in.SkillsRoot,
		Remote: strings.TrimSpace(in.Remote), Head: strings.TrimSpace(in.Head),
		CreatedAt: stamp, UpdatedAt: stamp,
	}
	eventKind := KindSkillLibraryImported
	if in.Kind == SkillLibraryGit {
		eventKind = KindSkillLibraryRegistered
	}
	payload := map[string]any{
		"libraryId": lib.ID, "libraryKey": lib.Key, "ownerKind": lib.OwnerKind,
		"ownerKey": lib.OwnerKey, "libraryKind": lib.Kind, "skillsRoot": lib.SkillsRoot,
		"remote": lib.Remote, "head": lib.Head,
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return SkillLibrary{}, false, err
	}
	defer tx.Rollback()
	if _, err := appendSkillEvent(ctx, tx, eventKind, in.Actor, lib.Key, "", payload); err != nil {
		return SkillLibrary{}, false, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO skill_libraries
		  (id, key, owner_kind, owner_key, kind, source_path, skills_root, remote, head, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lib.ID, lib.Key, lib.OwnerKind, lib.OwnerKey, lib.Kind, lib.SourcePath,
		lib.SkillsRoot, lib.Remote, lib.Head, stamp, stamp)
	if err != nil {
		return SkillLibrary{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return SkillLibrary{}, false, err
	}
	return lib, true, nil
}

func (d *DB) SkillLibraryByKey(ctx context.Context, key string) (SkillLibrary, error) {
	return d.skillLibrary(ctx, `SELECT id, key, owner_kind, owner_key, kind, source_path, skills_root, remote, head, created_at, updated_at FROM skill_libraries WHERE key = ?`, key)
}

func (d *DB) SkillLibraryByID(ctx context.Context, id string) (SkillLibrary, error) {
	return d.skillLibrary(ctx, `SELECT id, key, owner_kind, owner_key, kind, source_path, skills_root, remote, head, created_at, updated_at FROM skill_libraries WHERE id = ?`, id)
}

func (d *DB) skillLibrary(ctx context.Context, query string, arg any) (SkillLibrary, error) {
	var lib SkillLibrary
	err := d.sql.QueryRowContext(ctx, query, arg).Scan(
		&lib.ID, &lib.Key, &lib.OwnerKind, &lib.OwnerKey, &lib.Kind, &lib.SourcePath,
		&lib.SkillsRoot, &lib.Remote, &lib.Head, &lib.CreatedAt, &lib.UpdatedAt)
	if err == sql.ErrNoRows {
		return lib, ErrSkillLibraryNotFound
	}
	return lib, err
}

func (d *DB) ListSkillLibraries(ctx context.Context) ([]SkillLibrary, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT id, key, owner_kind, owner_key, kind, source_path, skills_root, remote, head, created_at, updated_at
		FROM skill_libraries ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillLibrary
	for rows.Next() {
		var lib SkillLibrary
		if err := rows.Scan(&lib.ID, &lib.Key, &lib.OwnerKind, &lib.OwnerKey, &lib.Kind,
			&lib.SourcePath, &lib.SkillsRoot, &lib.Remote, &lib.Head,
			&lib.CreatedAt, &lib.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

func (d *DB) UpdateSkillLibraryHead(ctx context.Context, id, head string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE skill_libraries SET head = ?, updated_at = ? WHERE id = ?`,
		strings.TrimSpace(head), time.Now().UTC().Format(time.RFC3339Nano), id)
	return err
}

func normalizeRevisionFiles(files []SkillFileInput) ([]SkillFileInput, error) {
	if len(files) == 0 || len(files) > 4096 {
		return nil, errors.New("skill revision must contain between 1 and 4096 files")
	}
	out := make([]SkillFileInput, len(files))
	copy(out, files)
	total := 0
	seen := map[string]bool{}
	hasSkill := false
	for i := range out {
		path := filepath.ToSlash(filepath.Clean(out[i].Path))
		if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") || strings.ContainsRune(path, 0) {
			return nil, fmt.Errorf("unsafe skill file path %q", out[i].Path)
		}
		if seen[path] {
			return nil, fmt.Errorf("duplicate skill file path %q", path)
		}
		seen[path] = true
		out[i].Path = path
		if path == "SKILL.md" {
			hasSkill = true
			if !utf8.Valid(out[i].Content) {
				return nil, errors.New("SKILL.md must be UTF-8")
			}
			if len(out[i].Content) > 1<<20 {
				return nil, errors.New("SKILL.md exceeds 1 MiB")
			}
		}
		if len(out[i].Content) > 16<<20 {
			return nil, fmt.Errorf("skill file %q exceeds 16 MiB", path)
		}
		total += len(out[i].Content)
		if total > 64<<20 {
			return nil, errors.New("skill tree exceeds 64 MiB")
		}
		if out[i].Mode&0o111 != 0 {
			out[i].Mode = 0o755
		} else {
			out[i].Mode = 0o644
		}
	}
	if !hasSkill {
		return nil, errors.New("skill revision is missing SKILL.md")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func revisionHashes(files []SkillFileInput) (string, []SkillRevisionFile) {
	h := sha256.New()
	h.Write([]byte("shale-skill-tree-v1\n"))
	rows := make([]SkillRevisionFile, 0, len(files))
	for _, file := range files {
		sum := sha256.Sum256(file.Content)
		blobHash := hex.EncodeToString(sum[:])
		mode := file.Mode
		fmt.Fprintf(h, "%s\x00%04o\x00%s\n", file.Path, mode.Perm(), blobHash)
		rows = append(rows, SkillRevisionFile{Path: file.Path, BlobHash: blobHash, Mode: mode, Size: int64(len(file.Content))})
	}
	return hex.EncodeToString(h.Sum(nil)), rows
}

func (d *DB) PutSkillRevision(ctx context.Context, in SkillRevisionInput) (Skill, SkillRevision, bool, error) {
	in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
	if in.Actor == "" {
		in.Actor = "human"
	}
	if !ValidSkillName(in.Name) {
		return Skill{}, SkillRevision{}, false, fmt.Errorf("invalid skill name %q", in.Name)
	}
	if in.Status != SkillActive && in.Status != SkillDraft {
		return Skill{}, SkillRevision{}, false, errors.New("new skill revisions must be active or draft")
	}
	if in.Status == SkillActive && in.Description == "" {
		return Skill{}, SkillRevision{}, false, errors.New("active skill requires a description")
	}
	if len(in.Description) > 16<<10 {
		return Skill{}, SkillRevision{}, false, errors.New("skill description exceeds 16 KiB")
	}
	lib, err := d.SkillLibraryByID(ctx, in.LibraryID)
	if err != nil {
		return Skill{}, SkillRevision{}, false, err
	}
	files, err := normalizeRevisionFiles(in.Files)
	if err != nil {
		return Skill{}, SkillRevision{}, false, err
	}
	treeHash, rows := revisionHashes(files)
	for i, file := range files {
		if err := writeExactFile(d.SkillBlobPath(rows[i].BlobHash), file.Content); err != nil {
			return Skill{}, SkillRevision{}, false, fmt.Errorf("write skill blob %s: %w", file.Path, err)
		}
	}

	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	var existingSeq int64
	err = d.sql.QueryRowContext(ctx, `
		SELECT event_seq FROM skill_revisions WHERE library_id = ? AND skill_name = ? AND tree_hash = ?`,
		in.LibraryID, in.Name, treeHash).Scan(&existingSeq)
	if err != nil && err != sql.ErrNoRows {
		return Skill{}, SkillRevision{}, false, err
	}
	inserted := err == sql.ErrNoRows
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Skill{}, SkillRevision{}, false, err
	}
	defer tx.Rollback()
	eventSeq := existingSeq
	if inserted {
		payload := map[string]any{
			"libraryId": lib.ID, "libraryKey": lib.Key, "skillName": in.Name,
			"treeHash": treeHash, "parentTreeHash": in.ParentTreeHash,
			"status": in.Status, "sourceHead": in.SourceHead,
		}
		eventSeq, err = appendSkillEvent(ctx, tx, KindSkillRevisionIndexed, in.Actor, lib.Key, treeHash, payload)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
		// Insert the skill projection before the revision because the revision's
		// composite FK names the stable skill identity.
		_, err = tx.ExecContext(ctx, `
			INSERT INTO skills
			  (library_id, name, status, description, current_tree_hash, current_event_seq, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(library_id, name) DO UPDATE SET
			  status=excluded.status, description=excluded.description,
			  current_tree_hash=excluded.current_tree_hash,
			  current_event_seq=excluded.current_event_seq, updated_at=excluded.updated_at`,
			in.LibraryID, in.Name, in.Status, in.Description, treeHash, eventSeq, stamp, stamp)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO skill_revisions
			  (library_id, skill_name, tree_hash, parent_tree_hash, source_head, event_seq, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, in.LibraryID, in.Name, treeHash,
			in.ParentTreeHash, in.SourceHead, eventSeq, stamp)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
		for _, file := range rows {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO skill_revision_files
				  (library_id, skill_name, tree_hash, path, blob_hash, mode, size)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, in.LibraryID, in.Name, treeHash,
				file.Path, file.BlobHash, file.Mode.Perm(), file.Size); err != nil {
				return Skill{}, SkillRevision{}, false, err
			}
		}
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE skills SET status = ?, description = ?, current_tree_hash = ?,
			  current_event_seq = ?, updated_at = ? WHERE library_id = ? AND name = ?`,
			in.Status, in.Description, treeHash, eventSeq, stamp, in.LibraryID, in.Name)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM skills_fts WHERE library_id = ? AND name = ?`, in.LibraryID, in.Name); err != nil {
		return Skill{}, SkillRevision{}, false, err
	}
	indexed := 0
	for _, file := range files {
		if in.Status == SkillRetracted || !utf8.Valid(file.Content) || len(file.Content) > 1<<20 {
			continue
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO skills_fts
			  (name, description, body, library_id, library_key, status, tree_hash, path, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, in.Description, string(file.Content),
			in.LibraryID, lib.Key, in.Status, treeHash, file.Path, stamp)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
		indexed++
	}
	if indexed == 0 && in.Status != SkillRetracted {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO skills_fts
			  (name, description, body, library_id, library_key, status, tree_hash, path, updated_at)
			VALUES (?, ?, '', ?, ?, ?, ?, 'SKILL.md', ?)`, in.Name, in.Description,
			in.LibraryID, lib.Key, in.Status, treeHash, stamp)
		if err != nil {
			return Skill{}, SkillRevision{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Skill{}, SkillRevision{}, false, err
	}
	skill := Skill{LibraryID: in.LibraryID, LibraryKey: lib.Key, Name: in.Name,
		Status: in.Status, Description: in.Description, TreeHash: treeHash,
		EventSeq: eventSeq, CreatedAt: stamp, UpdatedAt: stamp}
	_ = d.sql.QueryRowContext(ctx, `SELECT created_at FROM skills WHERE library_id = ? AND name = ?`, in.LibraryID, in.Name).Scan(&skill.CreatedAt)
	revision := SkillRevision{LibraryID: in.LibraryID, LibraryKey: lib.Key,
		SkillName: in.Name, TreeHash: treeHash, ParentTreeHash: in.ParentTreeHash,
		SourceHead: in.SourceHead, EventSeq: eventSeq, CreatedAt: stamp}
	return skill, revision, inserted, nil
}

type SkillFilter struct {
	LibraryKey string
	Status     SkillStatus
	Limit      int
}

func (d *DB) ListSkills(ctx context.Context, f SkillFilter) ([]Skill, error) {
	query := `
		SELECT s.library_id, l.key, s.name, s.status, s.description,
		       s.current_tree_hash, s.current_event_seq, s.created_at, s.updated_at
		FROM skills s JOIN skill_libraries l ON l.id = s.library_id WHERE 1=1`
	var args []any
	if f.LibraryKey != "" {
		query += ` AND l.key = ?`
		args = append(args, f.LibraryKey)
	}
	if f.Status != "" {
		query += ` AND s.status = ?`
		args = append(args, f.Status)
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	query += ` ORDER BY l.key, s.name LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		var s Skill
		if err := rows.Scan(&s.LibraryID, &s.LibraryKey, &s.Name, &s.Status,
			&s.Description, &s.TreeHash, &s.EventSeq, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) ResolveSkillRef(ctx context.Context, ref SkillRef) (SkillDetail, error) {
	lib, err := d.SkillLibraryByKey(ctx, ref.LibraryKey)
	if err != nil {
		return SkillDetail{}, ErrSkillNotFound
	}
	var s Skill
	err = d.sql.QueryRowContext(ctx, `
		SELECT library_id, ?, name, status, description, current_tree_hash,
		       current_event_seq, created_at, updated_at
		FROM skills WHERE library_id = ? AND name = ?`, lib.Key, lib.ID, ref.Name).
		Scan(&s.LibraryID, &s.LibraryKey, &s.Name, &s.Status, &s.Description,
			&s.TreeHash, &s.EventSeq, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return SkillDetail{}, ErrSkillNotFound
	}
	if err != nil {
		return SkillDetail{}, err
	}
	tree := ref.TreeHash
	if tree == "" {
		tree = s.TreeHash
	}
	var rev SkillRevision
	err = d.sql.QueryRowContext(ctx, `
		SELECT library_id, ?, skill_name, tree_hash, parent_tree_hash,
		       source_head, event_seq, created_at
		FROM skill_revisions WHERE library_id = ? AND skill_name = ? AND tree_hash = ?`,
		lib.Key, lib.ID, ref.Name, tree).
		Scan(&rev.LibraryID, &rev.LibraryKey, &rev.SkillName, &rev.TreeHash,
			&rev.ParentTreeHash, &rev.SourceHead, &rev.EventSeq, &rev.CreatedAt)
	if err == sql.ErrNoRows {
		return SkillDetail{}, ErrSkillNotFound
	}
	if err != nil {
		return SkillDetail{}, err
	}
	files, err := d.SkillRevisionFiles(ctx, lib.ID, ref.Name, tree)
	if err != nil {
		return SkillDetail{}, err
	}
	// An exact ref describes that revision even when it is not current.
	s.TreeHash, s.EventSeq = tree, rev.EventSeq
	return SkillDetail{Skill: s, Revision: rev, Files: files}, nil
}

func (d *DB) SkillRevisionFiles(ctx context.Context, libraryID, name, tree string) ([]SkillRevisionFile, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT path, blob_hash, mode, size FROM skill_revision_files
		WHERE library_id = ? AND skill_name = ? AND tree_hash = ? ORDER BY path`,
		libraryID, name, tree)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillRevisionFile
	for rows.Next() {
		var f SkillRevisionFile
		var mode uint32
		if err := rows.Scan(&f.Path, &f.BlobHash, &mode, &f.Size); err != nil {
			return nil, err
		}
		f.Mode = os.FileMode(mode)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrSkillNotFound
	}
	return out, nil
}

func (d *DB) SkillRevisionInputs(ctx context.Context, detail SkillDetail) ([]SkillFileInput, error) {
	out := make([]SkillFileInput, 0, len(detail.Files))
	for _, file := range detail.Files {
		body, err := d.ReadSkillBlob(file.BlobHash)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file.Path, err)
		}
		out = append(out, SkillFileInput{Path: file.Path, Content: body, Mode: file.Mode})
	}
	return out, nil
}

func (d *DB) ReadSkillFile(ctx context.Context, ref SkillRef, path string) ([]byte, error) {
	detail, err := d.ResolveSkillRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	path = filepath.ToSlash(filepath.Clean(path))
	for _, file := range detail.Files {
		if file.Path == path {
			return d.ReadSkillBlob(file.BlobHash)
		}
	}
	return nil, os.ErrNotExist
}

type SkillHit struct {
	Skill
	FilePath string
	Excerpt string
	Score   float64
}

func (h SkillHit) FileRef() string {
	return SkillFileRef{SkillRef: SkillRef{LibraryKey: h.LibraryKey, Name: h.Name, TreeHash: h.TreeHash}, Path: h.FilePath}.String()
}

func (d *DB) SearchSkills(ctx context.Context, query, libraryKey string, status SkillStatus, limit int) ([]SkillHit, error) {
	match := BuildMatchQuery(query)
	if match == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	sqlText := `
		SELECT library_id, library_key, name, status, description, tree_hash,
		       path, updated_at, snippet(skills_fts, 2, '', '', ' … ', 24), bm25(skills_fts)
		FROM skills_fts WHERE skills_fts MATCH ?`
	args := []any{match}
	if libraryKey != "" {
		sqlText += ` AND library_key = ?`
		args = append(args, libraryKey)
	}
	if status == "" {
		status = SkillActive
	}
	sqlText += ` AND status = ? ORDER BY bm25(skills_fts) LIMIT ?`
	args = append(args, status, limit)
	rows, err := d.sql.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillHit
	for rows.Next() {
		var h SkillHit
		if err := rows.Scan(&h.LibraryID, &h.LibraryKey, &h.Name, &h.Status,
			&h.Description, &h.TreeHash, &h.FilePath, &h.UpdatedAt, &h.Excerpt, &h.Score); err != nil {
			return nil, err
		}
		h.Score = -h.Score
		out = append(out, h)
	}
	return out, rows.Err()
}

func (d *DB) RetractMissingSkills(ctx context.Context, libraryID string, keepNames []string, actor string) (int, error) {
	if actor == "" {
		actor = "human"
	}
	keep := map[string]bool{}
	for _, name := range keepNames {
		keep[name] = true
	}
	lib, err := d.SkillLibraryByID(ctx, libraryID)
	if err != nil {
		return 0, err
	}
	items, err := d.ListSkills(ctx, SkillFilter{LibraryKey: lib.Key, Limit: 1000})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, item := range items {
		if keep[item.Name] || item.Status == SkillRetracted {
			continue
		}
		stamp := time.Now().UTC().Format(time.RFC3339Nano)
		tx, err := d.sql.BeginTx(ctx, nil)
		if err != nil {
			return count, err
		}
		payload := map[string]any{"libraryId": lib.ID, "libraryKey": lib.Key, "skillName": item.Name, "treeHash": item.TreeHash}
		seq, err := appendSkillEvent(ctx, tx, KindSkillRetracted, actor, lib.Key, item.TreeHash, payload)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE skills SET status = 'retracted', current_event_seq = ?, updated_at = ? WHERE library_id = ? AND name = ?`, seq, stamp, libraryID, item.Name)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM skills_fts WHERE library_id = ? AND name = ?`, libraryID, item.Name)
		}
		if err != nil {
			tx.Rollback()
			return count, err
		}
		if err := tx.Commit(); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func appendSkillEvent(ctx context.Context, tx *sql.Tx, kind, actor, libraryKey, contentHash string, payload any) (int64, error) {
	if actor == "" {
		actor = "human"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, kind, source, actor, occurred_at, scope, pointer, content_hash, payload)
		VALUES (?, ?, 'skills', ?, ?, ?, NULL, ?, ?)`, randomID(), kind, actor,
		stamp, libraryKey, nullable(contentHash), string(body))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
