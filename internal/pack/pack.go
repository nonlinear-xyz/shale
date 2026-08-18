// Package pack assembles a bounded, provenance-tagged context packet.
//
// This is the shape an agent receives at the start of a task. The contract is
// carried over from Observatory's server-side packet builder deliberately: the
// local binary and a hub must be interchangeable from the agent's point of view,
// so an agent wired to one can be pointed at the other without changing how it
// reads the response.
//
// Three properties matter more than the retrieval quality:
//
//   - The packet says how it found things (Retrieval), so an agent can weigh a
//     recency fallback differently from an exact match.
//   - The packet says what it left out (Budget.Truncated), so it never silently
//     pretends to be complete.
//   - Every item carries provenance, so a claim can be checked against bytes on
//     disk rather than trusted.
package pack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nonlinear-xyz/shale/internal/store"
)

const (
	DefaultBudget = 8000
	MinBudget     = 500
	MaxBudget     = 24000
	DefaultSince  = 30

	// fillRatio leaves room for the JSON envelope around the content.
	fillRatio = 0.9

	shareCheckpoints = 0.15
	shareMemories    = 0.20
	shareRunbooks    = 0.15
	shareCorrections = 0.20
	shareEvidence    = 0.30

	// adjacentScoreFactor is how much of its anchor's score a window-expanded
	// neighbour inherits. Below 1 so real matches always outrank context.
	adjacentScoreFactor = 0.9
)

// EstimateTokens approximates a token count from bytes.
//
// Deliberately NOT a real tokenizer. An exact count needs the model's own
// vocabulary; this binary cannot know which model is connected, MCP does not
// expose it, and vocabularies version faster than this code will. Precision
// against the wrong vocabulary is precision theater.
//
// bytes/3 is chosen to always OVER-estimate against both cl100k and o200k, so
// the ceiling can never be breached. The cost is 70-80% budget utilization, which
// is the right trade: under-filling wastes context, over-filling truncates the
// agent's prompt somewhere it cannot see or control.
func EstimateTokens(s string) int { return (len(s) + 2) / 3 }

// Provenance is how an item came to be known, and how stale it is.
type Provenance struct {
	Source     string `json:"source"`
	Repo       string `json:"repo,omitempty"`
	OccurredAt string `json:"occurredAt"`
	// Epistemic is "observed" for transcript evidence and the stored authority
	// for artifacts (asserted, proposed, or external_generated). Pending proposals
	// never reach a packet, but the field remains explicit so a harness snapshot
	// is not presented as a user assertion.
	Epistemic string `json:"epistemic"`
	Freshness string `json:"freshness"`
	ScopeKind string `json:"scopeKind,omitempty"`
	ScopeKey  string `json:"scopeKey,omitempty"`
	Origin    string `json:"origin,omitempty"`
	Status    string `json:"status,omitempty"`
	// Lines is the inclusive 1-based range in the stored transcript, so a citation
	// points at bytes rather than at a claim.
	Lines [2]int `json:"lines"`
}

// Evidence is one retrieved passage.
type Evidence struct {
	Ref      string     `json:"ref"`
	Title    string     `json:"title"`
	Content  string     `json:"content"`
	Score    float64    `json:"score"`
	Adjacent bool       `json:"adjacent,omitempty"`
	Prov     Provenance `json:"provenance"`
}

// Truncation records a section that did not fit.
type Truncation struct {
	Section   string `json:"section"`
	Included  int    `json:"included"`
	Dropped   int    `json:"dropped"`
	Excerpted int    `json:"excerpted,omitempty"`
}

// Budget reports what the packet was allowed and what it used.
type Budget struct {
	MaxTokens  int          `json:"maxTokens"`
	UsedTokens int          `json:"usedTokens"`
	Truncated  []Truncation `json:"truncated"`
}

// Packet is the whole response.
type Packet struct {
	PacketID  string  `json:"packetId"`
	Task      string  `json:"task"`
	Repo      *string `json:"repo"`
	SinceDays int     `json:"sinceDays"`
	// Retrieval is how the evidence was found: "match" (all terms), "match_or"
	// (any term, after the strict pass came up short), or "recency_fallback"
	// (nothing matched, so the most recent work is offered instead). The agent is
	// told which, because a recency fallback deserves far less trust.
	Retrieval string `json:"retrieval"`
	// SectionRetrieval reports the independent artifact ladders without changing
	// Retrieval's established meaning for transcript evidence.
	SectionRetrieval map[string]string `json:"sectionRetrieval"`
	Budget           Budget            `json:"budget"`
	Sections         struct {
		Checkpoints []Evidence `json:"checkpoints"`
		Memories    []Evidence `json:"memories"`
		Runbooks    []Evidence `json:"runbooks"`
		Corrections []Evidence `json:"corrections"`
		Evidence    []Evidence `json:"evidence"`
	} `json:"sections"`
	Citations []string `json:"citations"`
}

// Input parameterizes packet assembly.
type Input struct {
	Task        string
	TaskKey     string
	Repo        string
	SinceDays   int
	TokenBudget int
	Now         time.Time
}

// Build assembles a packet from the local store.
func Build(ctx context.Context, db *store.DB, in Input) (*Packet, error) {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	budget := clamp(in.TokenBudget, MinBudget, MaxBudget, DefaultBudget)
	since := in.SinceDays
	if since <= 0 {
		since = DefaultSince
	}

	p := &Packet{
		PacketID:  fmt.Sprintf("pkt_%d", now.UnixNano()),
		Task:      in.Task,
		SinceDays: since,
		Retrieval: "match",
		SectionRetrieval: map[string]string{
			"checkpoints": "none", "memories": "none", "runbooks": "none",
		},
		Budget:    Budget{MaxTokens: budget, Truncated: []Truncation{}},
		Citations: []string{},
	}
	if in.Repo != "" {
		r := in.Repo
		p.Repo = &r
	}
	p.Sections.Checkpoints = []Evidence{}
	p.Sections.Memories = []Evidence{}
	p.Sections.Runbooks = []Evidence{}
	p.Sections.Corrections = []Evidence{}
	p.Sections.Evidence = []Evidence{}

	fill := int(float64(budget) * fillRatio)
	served := map[string]bool{}

	terms := DistillQuery(in.Task, 8)
	query := strings.Join(terms, " AND ")
	looseQuery := strings.Join(terms, " OR ")

	checkpointHits, checkpointRetrieval, err := artifactHits(ctx, db, in, store.ArtifactCheckpoint, query, looseQuery, 4)
	if err != nil {
		return nil, err
	}
	memoryHits, memoryRetrieval, err := artifactHits(ctx, db, in, store.ArtifactMemory, query, looseQuery, 16)
	if err != nil {
		return nil, err
	}
	runbookHits, runbookRetrieval, err := artifactHits(ctx, db, in, store.ArtifactRunbook, query, looseQuery, 8)
	if err != nil {
		return nil, err
	}
	p.SectionRetrieval["checkpoints"] = checkpointRetrieval
	p.SectionRetrieval["memories"] = memoryRetrieval
	p.SectionRetrieval["runbooks"] = runbookRetrieval

	// Each section gets a floor, then hands unused space to the next. By the time
	// evidence fills, every byte not needed by durable state is available to raw
	// transcript passages.
	carry := 0
	p.Sections.Checkpoints, p.Budget.Truncated = fillArtifactSection(
		"checkpoints", checkpointHits, int(float64(fill)*shareCheckpoints)+carry, now, served, p.Budget.Truncated)
	carry = int(float64(fill)*shareCheckpoints) + carry - sumTokens(p.Sections.Checkpoints)
	p.Sections.Memories, p.Budget.Truncated = fillArtifactSection(
		"memories", memoryHits, int(float64(fill)*shareMemories)+carry, now, served, p.Budget.Truncated)
	carry = int(float64(fill)*shareMemories) + carry - sumTokens(p.Sections.Memories)
	p.Sections.Runbooks, p.Budget.Truncated = fillArtifactSection(
		"runbooks", runbookHits, int(float64(fill)*shareRunbooks)+carry, now, served, p.Budget.Truncated)
	carry = int(float64(fill)*shareRunbooks) + carry - sumTokens(p.Sections.Runbooks)

	var hits, corrections []store.ChunkHit
	if len(terms) == 0 {
		p.Retrieval = "recency_fallback"
		hits, err = db.RecentChunks(ctx, in.Repo, since, 8)
		if err != nil {
			return nil, err
		}
	} else {
		// The transcript retrieval ladder remains unchanged: strict AND first,
		// loosen to OR only when it genuinely finds more.
		hits, err = db.SearchChunks(ctx, query, in.Repo, "", since, 24)
		if err != nil {
			return nil, err
		}
		if len(hits) < 3 {
			loose, err := db.SearchChunks(ctx, looseQuery, in.Repo, "", since, 24)
			if err != nil {
				return nil, err
			}
			if len(loose) > len(hits) {
				hits, p.Retrieval = loose, "match_or"
			}
		}
		corrections, err = db.SearchChunks(ctx, looseQuery, in.Repo, "error", since, 8)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 && len(corrections) == 0 {
			p.Retrieval = "recency_fallback"
			hits, err = db.RecentChunks(ctx, in.Repo, since, 8)
			if err != nil {
				return nil, err
			}
		}
	}

	if p.Retrieval != "recency_fallback" {
		hits, err = db.ExpandWindow(ctx, hits, adjacentScoreFactor)
		if err != nil {
			return nil, err
		}
	}

	correctionCap := int(float64(fill)*shareCorrections) + carry
	p.Sections.Corrections, p.Budget.Truncated = fillSection(
		"corrections", corrections, correctionCap, now, served, p.Budget.Truncated)
	carry = correctionCap - sumTokens(p.Sections.Corrections)

	evidenceCap := int(float64(fill)*shareEvidence) + carry
	p.Sections.Evidence, p.Budget.Truncated = fillSection(
		"evidence", hits, evidenceCap, now, served, p.Budget.Truncated)

	all := [][]Evidence{
		p.Sections.Checkpoints, p.Sections.Memories, p.Sections.Runbooks,
		p.Sections.Corrections, p.Sections.Evidence,
	}
	for _, section := range all {
		for _, e := range section {
			p.Citations = append(p.Citations, e.Ref)
		}
		p.Budget.UsedTokens += sumTokens(section)
	}
	return p, nil
}

func artifactHits(ctx context.Context, db *store.DB, in Input, kind store.ArtifactKind, strict, loose string, limit int) ([]store.ArtifactHit, string, error) {
	if kind == store.ArtifactCheckpoint && in.TaskKey != "" {
		checkpoint, err := db.LatestCheckpoint(ctx, in.Repo, in.TaskKey)
		if err == nil {
			return []store.ArtifactHit{{
				Artifact: checkpoint, Body: checkpoint.Content.RenderText(checkpoint.Kind), Score: 1,
			}}, "task", nil
		}
		if !errors.Is(err, store.ErrArtifactNotFound) {
			return nil, "none", err
		}
	}
	if strict == "" {
		return nil, "none", nil
	}
	hits, err := db.SearchArtifacts(ctx, store.ArtifactSearch{
		Query: strict, Kind: kind, Repo: in.Repo, TaskKey: in.TaskKey,
		UserOnly: in.Repo == "" && in.TaskKey == "", Recall: true, Limit: limit,
	})
	if err != nil {
		return nil, "none", err
	}
	retrieval := "match"
	if len(hits) == 0 && loose != strict {
		looseHits, err := db.SearchArtifacts(ctx, store.ArtifactSearch{
			Query: loose, Kind: kind, Repo: in.Repo, TaskKey: in.TaskKey,
			UserOnly: in.Repo == "" && in.TaskKey == "", Recall: true, Limit: limit,
		})
		if err != nil {
			return nil, "none", err
		}
		if len(looseHits) > 0 {
			hits, retrieval = looseHits, "match_or"
		}
	}
	if len(hits) == 0 {
		retrieval = "none"
	}
	return hits, retrieval, nil
}

func fillArtifactSection(name string, hits []store.ArtifactHit, cap int, now time.Time, served map[string]bool, trunc []Truncation) ([]Evidence, []Truncation) {
	out := []Evidence{}
	used, dropped, excerpted := 0, 0, 0
	for _, hit := range hits {
		e := artifactEvidence(hit, now)
		if served[e.Ref] {
			continue
		}
		var shortened bool
		var fits bool
		e, shortened, fits = fitArtifactEvidence(e, cap-used)
		if !fits {
			dropped++
			continue
		}
		if shortened {
			excerpted++
		}
		cost := EstimateTokens(e.Content) + EstimateTokens(e.Title) + 16
		served[e.Ref] = true
		out = append(out, e)
		used += cost
	}
	if dropped > 0 || excerpted > 0 {
		trunc = append(trunc, Truncation{Section: name, Included: len(out), Dropped: dropped, Excerpted: excerpted})
	}
	return out, trunc
}

func fitArtifactEvidence(e Evidence, remaining int) (Evidence, bool, bool) {
	overhead := EstimateTokens(e.Title) + 16
	contentBudget := remaining - overhead
	if contentBudget <= 0 {
		return e, false, false
	}
	if EstimateTokens(e.Content) <= contentBudget {
		return e, false, true
	}
	const suffix = "\n…[read the ref for the full artifact]"
	maxBytes := contentBudget * 3
	if maxBytes <= len(suffix) {
		return e, false, false
	}
	e.Content = truncateUTF8(e.Content, maxBytes-len(suffix)) + suffix
	return e, true, true
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func artifactEvidence(hit store.ArtifactHit, now time.Time) Evidence {
	content := hit.Body
	return Evidence{
		Ref: hit.VersionedRef(), Title: hit.Title, Content: content, Score: hit.Score,
		Prov: Provenance{
			Source: hit.Source, Repo: hit.Repo, OccurredAt: hit.UpdatedAt,
			Epistemic: hit.Authority, Freshness: freshness(hit.UpdatedAt, now),
			ScopeKind: string(hit.ScopeKind), ScopeKey: hit.ScopeKey,
			Origin: hit.Origin, Status: string(hit.Status),
		},
	}
}

// fillSection packs hits into a token cap, recording what was dropped.
//
// Whole chunks only — a bisected chunk retrieves badly and reads worse, and the
// point of chunking was to produce windows that stand on their own. Anything that
// does not fit is reported rather than silently omitted.
func fillSection(name string, hits []store.ChunkHit, cap int, now time.Time, served map[string]bool, trunc []Truncation) ([]Evidence, []Truncation) {
	out := []Evidence{}
	used, dropped := 0, 0

	for _, h := range hits {
		ref := h.Ref()
		// A chunk served as a correction is never repeated as evidence.
		if served[ref] {
			continue
		}
		e := toEvidence(h, now)
		cost := EstimateTokens(e.Content) + EstimateTokens(e.Title) + 16
		if used+cost > cap {
			dropped++
			continue
		}
		served[ref] = true
		out = append(out, e)
		used += cost
	}

	if dropped > 0 {
		trunc = append(trunc, Truncation{Section: name, Included: len(out), Dropped: dropped})
	}
	return out, trunc
}

func toEvidence(h store.ChunkHit, now time.Time) Evidence {
	title := h.Scope
	if title == "" {
		title = h.Source
	}
	return Evidence{
		Ref:      h.Ref(),
		Title:    fmt.Sprintf("%s · lines %d-%d", title, h.LineStart, h.LineEnd),
		Content:  h.Body,
		Score:    h.Score,
		Adjacent: h.Adjacent,
		Prov: Provenance{
			Source:     h.Source,
			Repo:       h.Scope,
			OccurredAt: h.OccurredAt,
			Epistemic:  "observed",
			Freshness:  freshness(h.OccurredAt, now),
			Lines:      [2]int{h.LineStart, h.LineEnd},
		},
	}
}

// freshness buckets age. Coarse on purpose: an agent needs to know whether
// something is current, not its exact age.
func freshness(occurredAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return "stale"
	}
	switch age := now.Sub(t); {
	case age <= 7*24*time.Hour:
		return "fresh"
	case age <= 30*24*time.Hour:
		return "recent"
	default:
		return "stale"
	}
}

func sumTokens(es []Evidence) int {
	n := 0
	for _, e := range es {
		n += EstimateTokens(e.Content) + EstimateTokens(e.Title) + 16
	}
	return n
}

func clamp(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
