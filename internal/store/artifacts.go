package store

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/scrub"
)

// artifactSchema is schema version 1. The events table remains the append-only
// authority; artifacts and their FTS table are rebuildable projections of the
// current state. Artifact bodies deliberately live outside SQLite so a purge can
// destroy content without rewriting the event log.
const artifactSchema = `
CREATE TABLE IF NOT EXISTS artifacts (
  id                TEXT PRIMARY KEY,
  kind              TEXT NOT NULL,
  status            TEXT NOT NULL,
  scope_kind        TEXT NOT NULL,
  scope_key         TEXT NOT NULL DEFAULT '',
  repo              TEXT NOT NULL DEFAULT '',
  title             TEXT NOT NULL,
  origin            TEXT NOT NULL,
  authority         TEXT NOT NULL,
  source            TEXT NOT NULL,
  source_pointer    TEXT,
  current_event_seq INTEGER NOT NULL,
  content_hash      TEXT,
  created_at        TEXT NOT NULL,
  updated_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS artifacts_kind_status_idx
  ON artifacts(kind, status, updated_at DESC);
CREATE INDEX IF NOT EXISTS artifacts_scope_idx
  ON artifacts(scope_kind, scope_key, repo);
CREATE INDEX IF NOT EXISTS artifacts_pointer_idx
  ON artifacts(source_pointer);

CREATE TABLE IF NOT EXISTS artifact_versions (
  artifact_id  TEXT NOT NULL,
  event_seq    INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  PRIMARY KEY (artifact_id, event_seq)
);
CREATE INDEX IF NOT EXISTS artifact_versions_hash_idx
  ON artifact_versions(content_hash);

CREATE TABLE IF NOT EXISTS artifact_sources (
  path          TEXT PRIMARY KEY,
  artifact_id   TEXT NOT NULL,
  kind          TEXT NOT NULL,
  scope_kind    TEXT NOT NULL,
  scope_key     TEXT NOT NULL DEFAULT '',
  repo          TEXT NOT NULL DEFAULT '',
  source        TEXT NOT NULL,
  origin        TEXT NOT NULL,
  last_hash     TEXT,
  last_seen_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS artifact_sources_artifact_idx
  ON artifact_sources(artifact_id);

CREATE VIRTUAL TABLE IF NOT EXISTS artifacts_fts USING fts5(
  title, body,
  artifact_id UNINDEXED, event_seq UNINDEXED,
  kind UNINDEXED, status UNINDEXED,
  scope_kind UNINDEXED, scope_key UNINDEXED, repo UNINDEXED,
  origin UNINDEXED, authority UNINDEXED, source UNINDEXED,
  occurred_at UNINDEXED,
  tokenize='porter unicode61'
);
`

type ArtifactKind string

const (
	ArtifactMemory      ArtifactKind = "memory"
	ArtifactCheckpoint  ArtifactKind = "checkpoint"
	ArtifactRunbook     ArtifactKind = "runbook"
	ArtifactInstruction ArtifactKind = "instruction"
)

type ArtifactStatus string

const (
	ArtifactPending   ArtifactStatus = "pending"
	ArtifactActive    ArtifactStatus = "active"
	ArtifactRetracted ArtifactStatus = "retracted"
	ArtifactRejected  ArtifactStatus = "rejected"
	ArtifactPurged    ArtifactStatus = "purged"
)

type ScopeKind string

const (
	ScopeUser ScopeKind = "user"
	ScopeRepo ScopeKind = "repo"
	ScopeTask ScopeKind = "task"
)

const ArtifactContentMax = 64 << 10

// ArtifactContent is the typed body stored in an external content-addressed
// blob. Fields are intentionally shared and sparse: the query-driving shape is
// in the artifacts projection, while this document can evolve without a schema
// migration.
type ArtifactContent struct {
	Text               string   `json:"text,omitempty"`
	Trigger            string   `json:"trigger,omitempty"`
	TaskKey            string   `json:"taskKey,omitempty"`
	Goal               string   `json:"goal,omitempty"`
	Summary            string   `json:"summary,omitempty"`
	Decisions          []string `json:"decisions,omitempty"`
	Artifacts          []string `json:"artifacts,omitempty"`
	OpenLoops          []string `json:"openLoops,omitempty"`
	NextActions        []string `json:"nextActions,omitempty"`
	EvidenceRefs       []string `json:"evidenceRefs,omitempty"`
	PreviousCheckpoint string   `json:"previousCheckpoint,omitempty"`
}

// SearchText is the FTS body. Retrieval resolves the typed blob again before it
// renders a result, so query hints do not leak into presentation as unlabeled
// prose.
func (c ArtifactContent) SearchText() string {
	parts := []string{c.Trigger, c.TaskKey, c.Goal, c.Summary, c.Text}
	parts = append(parts, c.Decisions...)
	parts = append(parts, c.Artifacts...)
	parts = append(parts, c.OpenLoops...)
	parts = append(parts, c.NextActions...)
	return strings.TrimSpace(strings.Join(nonEmpty(parts), "\n"))
}

// RenderText gives checkpoints a readable handoff shape while leaving Markdown
// memories, instructions and runbooks untouched.
func (c ArtifactContent) RenderText(kind ArtifactKind) string {
	if kind != ArtifactCheckpoint {
		return c.Text
	}
	var b strings.Builder
	section := func(name, value string) {
		if strings.TrimSpace(value) != "" {
			fmt.Fprintf(&b, "## %s\n%s\n\n", name, strings.TrimSpace(value))
		}
	}
	list := func(name string, values []string) {
		if len(values) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s\n", name)
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(value))
			}
		}
		b.WriteString("\n")
	}
	section("Goal", c.Goal)
	section("Summary", c.Summary)
	list("Decisions", c.Decisions)
	list("Artifacts", c.Artifacts)
	list("Open loops", c.OpenLoops)
	list("Next actions", c.NextActions)
	list("Evidence", c.EvidenceRefs)
	section("Previous checkpoint", c.PreviousCheckpoint)
	return strings.TrimSpace(b.String())
}

type Artifact struct {
	ID             string
	Kind           ArtifactKind
	Status         ArtifactStatus
	ScopeKind      ScopeKind
	ScopeKey       string
	Repo           string
	Title          string
	Origin         string
	Authority      string
	Source         string
	SourcePointer  string
	EventSeq       int64
	ContentHash    string
	CreatedAt      string
	UpdatedAt      string
	Content        ArtifactContent
	ContentPresent bool
}

func (a Artifact) Ref() string { return fmt.Sprintf("%s:%s", a.Kind, a.ID) }

func (a Artifact) VersionedRef() string {
	if a.EventSeq <= 0 {
		return a.Ref()
	}
	return fmt.Sprintf("%s@%d", a.Ref(), a.EventSeq)
}

type ArtifactInput struct {
	ID            string
	Kind          ArtifactKind
	Status        ArtifactStatus
	ScopeKind     ScopeKind
	ScopeKey      string
	Repo          string
	Title         string
	Origin        string
	Authority     string
	Source        string
	SourcePointer string
	Actor         string
	EventKind     string
	OccurredAt    time.Time
	Content       ArtifactContent
}

type artifactEventPayload struct {
	ArtifactID      string         `json:"artifactId"`
	Kind            ArtifactKind   `json:"artifactKind"`
	Status          ArtifactStatus `json:"status"`
	ScopeKind       ScopeKind      `json:"scopeKind"`
	ScopeKey        string         `json:"scopeKey,omitempty"`
	Repo            string         `json:"repo,omitempty"`
	Title           string         `json:"title"`
	Origin          string         `json:"origin"`
	Authority       string         `json:"authority"`
	Source          string         `json:"source"`
	SourcePointer   string         `json:"sourcePointer,omitempty"`
	EvidenceRefs    []string       `json:"evidenceRefs,omitempty"`
	Redactions      map[string]int `json:"redactions,omitempty"`
	PreviousVersion int64          `json:"previousVersion,omitempty"`
}

func (d *DB) ArtifactBlobPath(contentHash string) string {
	if len(contentHash) < 2 {
		return filepath.Join(d.root, "artifact-blobs", contentHash+".json.gz")
	}
	return filepath.Join(d.root, "artifact-blobs", contentHash[:2], contentHash+".json.gz")
}

// PutArtifact appends a lifecycle event and updates the current projection in
// one transaction. The scrubbed body is written first: an orphan blob is
// recoverable, while an event pointing at missing content is not.
func (d *DB) PutArtifact(ctx context.Context, in ArtifactInput) (Artifact, bool, error) {
	in = normalizeArtifactInput(in)
	if err := validateArtifactInput(in); err != nil {
		return Artifact{}, false, err
	}
	current, currentErr := d.Artifact(ctx, in.ID)
	if currentErr != nil && !errors.Is(currentErr, ErrArtifactNotFound) {
		return Artifact{}, false, currentErr
	}
	if currentErr == nil && current.Kind != in.Kind {
		return Artifact{}, false, fmt.Errorf("artifact %s is a %s, not a %s", in.ID, current.Kind, in.Kind)
	}

	sc, _ := scrub.New()
	in.Title = sc.String(in.Title)
	in.Content = scrubArtifactContent(sc, in.Content)
	body, err := json.Marshal(in.Content)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("encode artifact content: %w", err)
	}
	if len(body) > ArtifactContentMax {
		return Artifact{}, false, fmt.Errorf("artifact content is %d bytes; maximum is %d", len(body), ArtifactContentMax)
	}
	sum := sha256.Sum256(body)
	contentHash := hex.EncodeToString(sum[:])
	if err := writeGzipFile(d.ArtifactBlobPath(contentHash), body); err != nil {
		return Artifact{}, false, fmt.Errorf("write artifact blob: %w", err)
	}

	if currentErr == nil && artifactUnchanged(current, in, contentHash) {
		return current, false, nil
	}

	stamp := in.OccurredAt
	if stamp.IsZero() {
		stamp = time.Now().UTC()
	}
	stampText := stamp.UTC().Format(time.RFC3339Nano)
	payload := artifactEventPayload{
		ArtifactID: in.ID, Kind: in.Kind, Status: in.Status,
		ScopeKind: in.ScopeKind, ScopeKey: in.ScopeKey, Repo: in.Repo,
		Title: in.Title, Origin: in.Origin, Authority: in.Authority,
		Source: in.Source, SourcePointer: in.SourcePointer,
		EvidenceRefs: append([]string(nil), in.Content.EvidenceRefs...),
		Redactions:   sc.Counts(),
	}
	if currentErr == nil {
		payload.PreviousVersion = current.EventSeq
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("encode artifact event: %w", err)
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, false, err
	}
	defer tx.Rollback()

	scope := in.Repo
	if scope == "" {
		scope = in.ScopeKey
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, kind, source, actor, occurred_at, scope, pointer, content_hash, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		randomID(), in.EventKind, in.Source, in.Actor, stampText, scope,
		d.ArtifactBlobPath(contentHash), contentHash, string(payloadJSON))
	if err != nil {
		return Artifact{}, false, fmt.Errorf("insert artifact event: %w", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Artifact{}, false, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO artifacts (
		  id, kind, status, scope_kind, scope_key, repo, title, origin,
		  authority, source, source_pointer, current_event_seq, content_hash,
		  created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  kind=excluded.kind, status=excluded.status, scope_kind=excluded.scope_kind,
		  scope_key=excluded.scope_key, repo=excluded.repo, title=excluded.title,
		  origin=excluded.origin, authority=excluded.authority, source=excluded.source,
		  source_pointer=excluded.source_pointer,
		  current_event_seq=excluded.current_event_seq,
		  content_hash=excluded.content_hash, updated_at=excluded.updated_at`,
		in.ID, in.Kind, in.Status, in.ScopeKind, in.ScopeKey, in.Repo, in.Title,
		in.Origin, in.Authority, in.Source, nullable(in.SourcePointer), seq,
		contentHash, stampText, stampText)
	if err != nil {
		return Artifact{}, false, fmt.Errorf("project artifact: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO artifact_versions (artifact_id, event_seq, content_hash, created_at)
		VALUES (?, ?, ?, ?)`, in.ID, seq, contentHash, stampText); err != nil {
		return Artifact{}, false, fmt.Errorf("record artifact version: %w", err)
	}
	if err := replaceArtifactFTS(ctx, tx, in.ID, seq, in, in.Content.SearchText(), stampText); err != nil {
		return Artifact{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, false, err
	}

	createdAt := stampText
	if currentErr == nil {
		createdAt = current.CreatedAt
	}
	return Artifact{
		ID: in.ID, Kind: in.Kind, Status: in.Status,
		ScopeKind: in.ScopeKind, ScopeKey: in.ScopeKey, Repo: in.Repo,
		Title: in.Title, Origin: in.Origin, Authority: in.Authority,
		Source: in.Source, SourcePointer: in.SourcePointer, EventSeq: seq,
		ContentHash: contentHash, CreatedAt: createdAt, UpdatedAt: stampText,
		Content: in.Content, ContentPresent: true,
	}, true, nil
}

func replaceArtifactFTS(ctx context.Context, tx *sql.Tx, id string, seq int64, in ArtifactInput, body, stamp string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts_fts WHERE artifact_id = ?`, id); err != nil {
		return fmt.Errorf("clear artifact index: %w", err)
	}
	if in.Status != ArtifactActive {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO artifacts_fts (
		  title, body, artifact_id, event_seq, kind, status, scope_kind,
		  scope_key, repo, origin, authority, source, occurred_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Title, body, id, seq, in.Kind, in.Status, in.ScopeKind,
		in.ScopeKey, in.Repo, in.Origin, in.Authority, in.Source, stamp)
	if err != nil {
		return fmt.Errorf("index artifact: %w", err)
	}
	return nil
}

func normalizeArtifactInput(in ArtifactInput) ArtifactInput {
	if in.ID == "" {
		in.ID = randomID()
	}
	if in.Status == "" {
		in.Status = ArtifactActive
	}
	if in.ScopeKind == "" {
		in.ScopeKind = ScopeRepo
	}
	if in.ScopeKind == ScopeUser && in.ScopeKey == "" {
		in.ScopeKey = "local"
	}
	if in.ScopeKind == ScopeRepo && in.ScopeKey == "" {
		in.ScopeKey = in.Repo
	}
	if in.ScopeKind == ScopeTask && in.Content.TaskKey == "" {
		in.Content.TaskKey = in.ScopeKey
	}
	if in.Origin == "" {
		in.Origin = "native"
	}
	if in.Authority == "" {
		in.Authority = "asserted"
	}
	if in.Source == "" {
		in.Source = "manual"
	}
	if in.Actor == "" {
		in.Actor = "human"
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = deriveTitle(in.Content.SearchText())
	}
	if in.EventKind == "" {
		in.EventKind = defaultArtifactEventKind(in)
	}
	return in
}

func defaultArtifactEventKind(in ArtifactInput) string {
	switch in.Kind {
	case ArtifactMemory:
		if in.Status == ArtifactPending {
			return KindMemoryProposed
		}
		return KindMemoryAsserted
	case ArtifactCheckpoint:
		return KindCheckpointSaved
	case ArtifactRunbook:
		if in.Origin == "file" {
			return KindRunbookRegistered
		}
		return KindRunbookCreated
	default:
		return KindExternalIndexed
	}
}

func validateArtifactInput(in ArtifactInput) error {
	if !validArtifactID(in.ID) {
		return fmt.Errorf("invalid artifact id %q", in.ID)
	}
	switch in.Kind {
	case ArtifactMemory, ArtifactCheckpoint, ArtifactRunbook, ArtifactInstruction:
	default:
		return fmt.Errorf("invalid artifact kind %q", in.Kind)
	}
	if in.Status != ArtifactActive && in.Status != ArtifactPending {
		return fmt.Errorf("cannot write artifact with status %q", in.Status)
	}
	if in.Status == ArtifactPending && in.Kind != ArtifactMemory {
		return errors.New("only memories can be pending")
	}
	switch in.ScopeKind {
	case ScopeUser:
	case ScopeRepo:
		if strings.TrimSpace(in.Repo) == "" {
			return errors.New("repo scope requires repo")
		}
	case ScopeTask:
		if strings.TrimSpace(in.ScopeKey) == "" {
			return errors.New("task scope requires task key")
		}
	default:
		return fmt.Errorf("invalid scope kind %q", in.ScopeKind)
	}
	if in.ScopeKind == ScopeTask && in.Content.TaskKey != in.ScopeKey {
		return errors.New("artifact task key must match its task scope")
	}
	if in.Kind == ArtifactCheckpoint && in.ScopeKind != ScopeTask {
		return errors.New("checkpoints require task scope")
	}
	if strings.TrimSpace(in.Title) == "" {
		return errors.New("artifact title is required")
	}
	if len(in.Title) > 1024 {
		return errors.New("artifact title exceeds 1024 bytes")
	}
	if len(in.ScopeKey) > 4096 || len(in.Repo) > 4096 || len(in.SourcePointer) > 16<<10 {
		return errors.New("artifact metadata exceeds its size limit")
	}
	if len(in.Origin) > 256 || len(in.Authority) > 256 || len(in.Source) > 256 || len(in.Actor) > 256 || len(in.EventKind) > 256 {
		return errors.New("artifact provenance exceeds its size limit")
	}
	if strings.TrimSpace(in.Content.SearchText()) == "" {
		return errors.New("artifact content is required")
	}
	if len(in.Content.EvidenceRefs) > 100 {
		return errors.New("artifact has more than 100 evidence refs")
	}
	for _, value := range in.Content.EvidenceRefs {
		if _, err := ParseArtifactRef(value); err == nil {
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
		if _, err := ParseRef(value); err != nil {
			return fmt.Errorf("invalid evidence ref %q", value)
		}
	}
	if in.Content.PreviousCheckpoint != "" {
		ref, err := ParseArtifactRef(in.Content.PreviousCheckpoint)
		if err != nil || ref.Kind != ArtifactCheckpoint {
			return errors.New("previous checkpoint must be a checkpoint ref")
		}
	}
	if strings.TrimSpace(in.EventKind) == "" {
		return errors.New("artifact event kind is required")
	}
	return nil
}

func scrubArtifactContent(sc *scrub.Scrubber, c ArtifactContent) ArtifactContent {
	c.Text = sc.String(c.Text)
	c.Trigger = sc.String(c.Trigger)
	c.TaskKey = sc.String(c.TaskKey)
	c.Goal = sc.String(c.Goal)
	c.Summary = sc.String(c.Summary)
	for _, list := range []*[]string{&c.Decisions, &c.Artifacts, &c.OpenLoops, &c.NextActions} {
		for i := range *list {
			(*list)[i] = sc.String((*list)[i])
		}
	}
	return c
}

func artifactUnchanged(current Artifact, in ArtifactInput, hash string) bool {
	return current.Kind == in.Kind && current.Status == in.Status &&
		current.ScopeKind == in.ScopeKind && current.ScopeKey == in.ScopeKey &&
		current.Repo == in.Repo && current.Title == in.Title &&
		current.Origin == in.Origin && current.Authority == in.Authority &&
		current.Source == in.Source && current.SourcePointer == in.SourcePointer &&
		current.ContentHash == hash
}

var ErrArtifactNotFound = errors.New("artifact not found")

func (d *DB) Artifact(ctx context.Context, id string) (Artifact, error) {
	var a Artifact
	var pointer, hash sql.NullString
	err := d.sql.QueryRowContext(ctx, `
		SELECT id, kind, status, scope_kind, scope_key, repo, title, origin,
		       authority, source, source_pointer, current_event_seq, content_hash,
		       created_at, updated_at
		FROM artifacts WHERE id = ?`, id).
		Scan(&a.ID, &a.Kind, &a.Status, &a.ScopeKind, &a.ScopeKey, &a.Repo,
			&a.Title, &a.Origin, &a.Authority, &a.Source, &pointer, &a.EventSeq,
			&hash, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return a, ErrArtifactNotFound
	}
	if err != nil {
		return a, fmt.Errorf("load artifact: %w", err)
	}
	a.SourcePointer, a.ContentHash = pointer.String, hash.String
	if a.ContentHash == "" {
		return a, nil
	}
	content, err := d.readArtifactBlob(a.ContentHash)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.Content, a.ContentPresent = content, true
	return a, nil
}

func (d *DB) ArtifactAt(ctx context.Context, id string, eventSeq int64) (Artifact, error) {
	var payloadJSON string
	var occurred string
	err := d.sql.QueryRowContext(ctx, `
		SELECT payload, occurred_at FROM events WHERE seq = ?`, eventSeq).
		Scan(&payloadJSON, &occurred)
	if err == sql.ErrNoRows {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	var p artifactEventPayload
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil || p.ArtifactID != id {
		return Artifact{}, ErrArtifactNotFound
	}
	a := Artifact{
		ID: p.ArtifactID, Kind: p.Kind, Status: p.Status, ScopeKind: p.ScopeKind,
		ScopeKey: p.ScopeKey, Repo: p.Repo, Title: p.Title, Origin: p.Origin,
		Authority: p.Authority, Source: p.Source, SourcePointer: p.SourcePointer,
		EventSeq: eventSeq, UpdatedAt: occurred,
	}
	_ = d.sql.QueryRowContext(ctx, `SELECT created_at FROM artifacts WHERE id = ?`, id).Scan(&a.CreatedAt)
	// The event hash is immutable metadata, not permission to read a body. Purge
	// deletes the version mapping; consulting that mapping here ensures an old ref
	// cannot recover identical bytes retained for another deduplicated artifact.
	var versionHash string
	err = d.sql.QueryRowContext(ctx, `
		SELECT content_hash FROM artifact_versions WHERE artifact_id = ? AND event_seq = ?`,
		id, eventSeq).Scan(&versionHash)
	if err == sql.ErrNoRows {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.ContentHash = versionHash
	content, err := d.readArtifactBlob(versionHash)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return a, err
	}
	a.Content, a.ContentPresent = content, true
	return a, nil
}

type ArtifactFilter struct {
	Kind      ArtifactKind
	Status    ArtifactStatus
	ScopeKind ScopeKind
	ScopeKey  string
	Repo      string
	Limit     int
}

type ArtifactSource struct {
	Path       string
	ArtifactID string
	Kind       ArtifactKind
	ScopeKind  ScopeKind
	ScopeKey   string
	Repo       string
	Source     string
	Origin     string
	LastHash   string
	LastSeenAt string
}

func (d *DB) RegisterArtifactSource(ctx context.Context, s ArtifactSource) error {
	if strings.TrimSpace(s.Path) == "" || !filepath.IsAbs(s.Path) {
		return errors.New("artifact source path must be absolute")
	}
	if !validArtifactID(s.ArtifactID) {
		return errors.New("artifact source requires a valid artifact id")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := d.sql.ExecContext(ctx, `
		INSERT INTO artifact_sources (
		  path, artifact_id, kind, scope_kind, scope_key, repo, source,
		  origin, last_hash, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  artifact_id=excluded.artifact_id, kind=excluded.kind,
		  scope_kind=excluded.scope_kind, scope_key=excluded.scope_key,
		  repo=excluded.repo, source=excluded.source, origin=excluded.origin,
		  last_seen_at=excluded.last_seen_at`,
		s.Path, s.ArtifactID, s.Kind, s.ScopeKind, s.ScopeKey, s.Repo,
		s.Source, s.Origin, nullable(s.LastHash), stamp)
	return err
}

func (d *DB) TouchArtifactSource(ctx context.Context, path, hash string) error {
	_, err := d.sql.ExecContext(ctx, `
		UPDATE artifact_sources SET last_hash = ?, last_seen_at = ? WHERE path = ?`,
		nullable(hash), time.Now().UTC().Format(time.RFC3339Nano), path)
	return err
}

func (d *DB) ArtifactSources(ctx context.Context) ([]ArtifactSource, error) {
	rows, err := d.sql.QueryContext(ctx, `
		SELECT path, artifact_id, kind, scope_kind, scope_key, repo, source,
		       origin, coalesce(last_hash, ''), last_seen_at
		FROM artifact_sources ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ArtifactSource
	for rows.Next() {
		var s ArtifactSource
		if err := rows.Scan(&s.Path, &s.ArtifactID, &s.Kind, &s.ScopeKind,
			&s.ScopeKey, &s.Repo, &s.Source, &s.Origin, &s.LastHash,
			&s.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) RemoveArtifactSource(ctx context.Context, path string) error {
	_, err := d.sql.ExecContext(ctx, `DELETE FROM artifact_sources WHERE path = ?`, path)
	return err
}

func (d *DB) ListArtifacts(ctx context.Context, f ArtifactFilter) ([]Artifact, error) {
	query := `SELECT id FROM artifacts WHERE 1=1`
	var args []any
	if f.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		query += ` AND status = ?`
		args = append(args, f.Status)
	}
	if f.ScopeKind != "" {
		query += ` AND scope_kind = ?`
		args = append(args, f.ScopeKind)
	}
	if f.ScopeKey != "" {
		query += ` AND scope_key = ?`
		args = append(args, f.ScopeKey)
	}
	if f.Repo != "" {
		query += ` AND repo = ?`
		args = append(args, f.Repo)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query += ` ORDER BY updated_at DESC LIMIT ?`
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
	out := make([]Artifact, 0, len(ids))
	for _, id := range ids {
		a, err := d.Artifact(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, a)
	}
	return out, nil
}

type ArtifactHit struct {
	Artifact
	Body    string
	Excerpt string
	Score   float64
}

type ArtifactSearch struct {
	Query    string
	Kind     ArtifactKind
	Repo     string
	TaskKey  string
	UserOnly bool
	Recall   bool
	Limit    int
}

func (d *DB) SearchArtifacts(ctx context.Context, in ArtifactSearch) ([]ArtifactHit, error) {
	match := BuildMatchQuery(in.Query)
	if match == "" {
		return nil, nil
	}
	query := `
		SELECT artifact_id, event_seq, kind, status, scope_kind, scope_key, repo,
		       origin, authority, source, occurred_at, title, body,
		       snippet(artifacts_fts, 1, '', '', ' … ', 24), bm25(artifacts_fts)
		FROM artifacts_fts WHERE artifacts_fts MATCH ? AND status = 'active'`
	args := []any{match}
	if in.Kind != "" {
		query += ` AND kind = ?`
		args = append(args, in.Kind)
	}
	if in.UserOnly {
		query += ` AND scope_kind = 'user'`
	} else if in.TaskKey != "" {
		query += ` AND ((scope_kind = 'task' AND scope_key = ? AND (? = '' OR repo = ?)) OR (scope_kind = 'repo' AND repo = ?) OR scope_kind = 'user')`
		args = append(args, in.TaskKey, in.Repo, in.Repo, in.Repo)
	} else if in.Repo != "" {
		if in.Recall {
			query += ` AND ((scope_kind = 'repo' AND repo = ?) OR scope_kind = 'user')`
		} else {
			query += ` AND (repo = ? OR scope_kind = 'user')`
		}
		args = append(args, in.Repo)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query += ` ORDER BY bm25(artifacts_fts) LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("search artifacts: %w", err)
	}
	defer rows.Close()
	var out []ArtifactHit
	for rows.Next() {
		var h ArtifactHit
		if err := rows.Scan(&h.ID, &h.EventSeq, &h.Kind, &h.Status,
			&h.ScopeKind, &h.ScopeKey, &h.Repo, &h.Origin, &h.Authority,
			&h.Source, &h.UpdatedAt, &h.Title, &h.Body, &h.Excerpt, &h.Score); err != nil {
			return nil, err
		}
		h.Score = -h.Score
		h.ContentPresent = true
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// FTS stores query-driving text, not the presentation contract. Resolve each
	// exact version so packets receive structured checkpoint rendering and memory
	// triggers remain retrieval hints rather than unlabeled prose.
	for i := range out {
		a, err := d.ArtifactAt(ctx, out[i].ID, out[i].EventSeq)
		if err != nil {
			return nil, err
		}
		out[i].Artifact = a
		out[i].Body = a.Content.RenderText(a.Kind)
	}
	return out, nil
}

func (d *DB) LatestCheckpoint(ctx context.Context, repo, taskKey string) (Artifact, error) {
	var id string
	err := d.sql.QueryRowContext(ctx, `
		SELECT id FROM artifacts
		WHERE kind = ? AND status = ? AND scope_kind = ? AND scope_key = ?
		  AND (? = '' OR repo = ?)
		ORDER BY updated_at DESC LIMIT 1`,
		ArtifactCheckpoint, ArtifactActive, ScopeTask, taskKey, repo, repo).Scan(&id)
	if err == sql.ErrNoRows {
		return Artifact{}, ErrArtifactNotFound
	}
	if err != nil {
		return Artifact{}, err
	}
	return d.Artifact(ctx, id)
}

func (d *DB) AcceptMemory(ctx context.Context, id string, edited *ArtifactContent, actor string) (Artifact, error) {
	a, err := d.Artifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	if a.Kind != ArtifactMemory || a.Status != ArtifactPending {
		return Artifact{}, fmt.Errorf("%s is not a pending memory", a.Ref())
	}
	content := a.Content
	if edited != nil {
		content = *edited
	}
	accepted, _, err := d.PutArtifact(ctx, ArtifactInput{
		ID: a.ID, Kind: a.Kind, Status: ArtifactActive,
		ScopeKind: a.ScopeKind, ScopeKey: a.ScopeKey, Repo: a.Repo,
		Title: a.Title, Origin: a.Origin, Authority: "asserted",
		Source: a.Source, SourcePointer: a.SourcePointer, Actor: actor,
		EventKind: KindMemoryAccepted, Content: content,
	})
	return accepted, err
}

func (d *DB) SupersedeMemory(ctx context.Context, id string, content ArtifactContent, title, actor string) (Artifact, error) {
	a, err := d.Artifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	if a.Kind != ArtifactMemory || a.Status != ArtifactActive {
		return Artifact{}, fmt.Errorf("%s is not an active memory", a.Ref())
	}
	if title == "" {
		title = a.Title
	}
	updated, _, err := d.PutArtifact(ctx, ArtifactInput{
		ID: a.ID, Kind: a.Kind, Status: ArtifactActive,
		ScopeKind: a.ScopeKind, ScopeKey: a.ScopeKey, Repo: a.Repo,
		Title: title, Origin: a.Origin, Authority: "asserted",
		Source: a.Source, SourcePointer: a.SourcePointer, Actor: actor,
		EventKind: KindMemorySuperseded, Content: content,
	})
	return updated, err
}

func (d *DB) RejectMemory(ctx context.Context, id, actor string) (Artifact, error) {
	return d.transitionArtifact(ctx, id, ArtifactRejected, KindMemoryRejected, actor, true)
}

func (d *DB) RetractArtifact(ctx context.Context, id, actor string) (Artifact, error) {
	a, err := d.Artifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	eventKind := KindMemoryRetracted
	if a.Kind != ArtifactMemory {
		eventKind = KindArtifactRetracted
	}
	if a.Origin != "native" {
		eventKind = KindExternalRemoved
	}
	return d.transitionArtifact(ctx, id, ArtifactRetracted, eventKind, actor, false)
}

func (d *DB) PurgeArtifact(ctx context.Context, id, actor string) (Artifact, error) {
	a, err := d.Artifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	eventKind := KindArtifactPurged
	if a.Kind == ArtifactMemory {
		eventKind = KindMemoryPurged
	}
	return d.transitionArtifact(ctx, id, ArtifactPurged, eventKind, actor, true)
}

func (d *DB) transitionArtifact(ctx context.Context, id string, status ArtifactStatus, eventKind, actor string, purgeBody bool) (Artifact, error) {
	a, err := d.Artifact(ctx, id)
	if err != nil {
		return Artifact{}, err
	}
	if a.Status == ArtifactPurged {
		return a, nil
	}
	if a.Status == status {
		return a, nil
	}
	if status == ArtifactRejected && (a.Kind != ArtifactMemory || a.Status != ArtifactPending) {
		return Artifact{}, fmt.Errorf("%s is not a pending memory", a.Ref())
	}
	if actor == "" {
		actor = "human"
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	p := artifactEventPayload{
		ArtifactID: a.ID, Kind: a.Kind, Status: status,
		ScopeKind: a.ScopeKind, ScopeKey: a.ScopeKey, Repo: a.Repo,
		Title: a.Title, Origin: a.Origin, Authority: a.Authority,
		Source: a.Source, SourcePointer: a.SourcePointer,
		PreviousVersion: a.EventSeq,
	}
	payload, _ := json.Marshal(p)
	hashes, err := d.artifactVersionHashes(ctx, id)
	if err != nil {
		return Artifact{}, err
	}

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, err
	}
	defer tx.Rollback()
	scope := a.Repo
	if scope == "" {
		scope = a.ScopeKey
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO events (id, kind, source, actor, occurred_at, scope, pointer, content_hash, payload)
		VALUES (?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
		randomID(), eventKind, a.Source, actor, stamp, scope, string(payload))
	if err != nil {
		return Artifact{}, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return Artifact{}, err
	}
	contentHash := any(a.ContentHash)
	if purgeBody {
		contentHash = nil
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE artifacts SET status = ?, current_event_seq = ?, content_hash = ?, updated_at = ?
		WHERE id = ?`, status, seq, contentHash, stamp, id)
	if err != nil {
		return Artifact{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts_fts WHERE artifact_id = ?`, id); err != nil {
		return Artifact{}, err
	}
	if !purgeBody && a.ContentHash != "" {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO artifact_versions (artifact_id, event_seq, content_hash, created_at)
			VALUES (?, ?, ?, ?)`, id, seq, a.ContentHash, stamp); err != nil {
			return Artifact{}, err
		}
	}
	if purgeBody {
		if _, err := tx.ExecContext(ctx, `DELETE FROM artifact_versions WHERE artifact_id = ?`, id); err != nil {
			return Artifact{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Artifact{}, err
	}

	a.Status, a.EventSeq, a.UpdatedAt = status, seq, stamp
	if purgeBody {
		a.ContentHash, a.Content, a.ContentPresent = "", ArtifactContent{}, false
		if err := d.removeUnreferencedArtifactBlobs(ctx, hashes); err != nil {
			return a, err
		}
		// secure_delete covers table cells. Truncate WAL and rewrite database pages
		// so an explicit purge does not leave the FTS body in SQLite freelists.
		if _, err := d.sql.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return a, fmt.Errorf("truncate WAL after purge: %w", err)
		}
		if _, err := d.sql.ExecContext(ctx, `VACUUM`); err != nil {
			return a, fmt.Errorf("vacuum after purge: %w", err)
		}
	}
	return a, nil
}

func (d *DB) artifactVersionHashes(ctx context.Context, id string) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT content_hash FROM artifact_versions WHERE artifact_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		out = append(out, hash)
	}
	return out, rows.Err()
}

func (d *DB) removeUnreferencedArtifactBlobs(ctx context.Context, hashes []string) error {
	for _, hash := range hashes {
		var n int
		if err := d.sql.QueryRowContext(ctx, `SELECT count(*) FROM artifact_versions WHERE content_hash = ?`, hash).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if err := os.Remove(d.ArtifactBlobPath(hash)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove artifact blob: %w", err)
			}
		}
	}
	return nil
}

func (d *DB) readArtifactBlob(hash string) (ArtifactContent, error) {
	f, err := os.Open(d.ArtifactBlobPath(hash))
	if err != nil {
		return ArtifactContent{}, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return ArtifactContent{}, fmt.Errorf("open artifact gzip: %w", err)
	}
	defer zr.Close()
	body, err := io.ReadAll(io.LimitReader(zr, ArtifactContentMax+1))
	if err != nil {
		return ArtifactContent{}, err
	}
	if len(body) > ArtifactContentMax {
		return ArtifactContent{}, errors.New("artifact blob exceeds content limit")
	}
	var content ArtifactContent
	if err := json.Unmarshal(body, &content); err != nil {
		return ArtifactContent{}, fmt.Errorf("decode artifact blob: %w", err)
	}
	return content, nil
}

func writeGzipFile(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp := path + ".tmp-" + randomID()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		f.Close()
		if remove {
			os.Remove(tmp)
		}
	}()
	zw := gzip.NewWriter(f)
	if _, err := zw.Write(body); err != nil {
		zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			remove = true
			return nil
		}
		return err
	}
	remove = false
	return nil
}

type ArtifactRef struct {
	Kind     ArtifactKind
	ID       string
	EventSeq int64
}

func (r ArtifactRef) String() string {
	base := fmt.Sprintf("%s:%s", r.Kind, r.ID)
	if r.EventSeq > 0 {
		return fmt.Sprintf("%s@%d", base, r.EventSeq)
	}
	return base
}

func ParseArtifactRef(value string) (ArtifactRef, error) {
	value = strings.TrimSpace(value)
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return ArtifactRef{}, fmt.Errorf("not an artifact ref: %q", value)
	}
	r := ArtifactRef{Kind: ArtifactKind(parts[0])}
	switch r.Kind {
	case ArtifactMemory, ArtifactCheckpoint, ArtifactRunbook, ArtifactInstruction:
	default:
		return ArtifactRef{}, fmt.Errorf("unknown artifact ref kind %q", parts[0])
	}
	id := parts[1]
	if at := strings.LastIndexByte(id, '@'); at >= 0 {
		seq, err := strconv.ParseInt(id[at+1:], 10, 64)
		if err != nil || seq <= 0 {
			return ArtifactRef{}, fmt.Errorf("bad artifact version in %q", value)
		}
		r.EventSeq = seq
		id = id[:at]
	}
	if !validArtifactID(id) {
		return ArtifactRef{}, fmt.Errorf("bad artifact id in %q", value)
	}
	r.ID = id
	return r, nil
}

func (d *DB) ResolveArtifactRef(ctx context.Context, ref ArtifactRef) (Artifact, error) {
	if ref.EventSeq > 0 {
		a, err := d.ArtifactAt(ctx, ref.ID, ref.EventSeq)
		if err == nil && a.Kind != ref.Kind {
			return Artifact{}, ErrArtifactNotFound
		}
		return a, err
	}
	a, err := d.Artifact(ctx, ref.ID)
	if err == nil && a.Kind != ref.Kind {
		return Artifact{}, ErrArtifactNotFound
	}
	return a, err
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("cannot generate artifact id: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func validArtifactID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func deriveTitle(body string) string {
	line := strings.TrimSpace(strings.SplitN(body, "\n", 2)[0])
	if line == "" {
		return "(untitled)"
	}
	runes := []rune(line)
	if len(runes) > 100 {
		return string(runes[:100]) + " …"
	}
	return line
}

func nonEmpty(values []string) []string {
	out := values[:0:0]
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
