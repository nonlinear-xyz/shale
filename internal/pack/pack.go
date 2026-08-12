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
	"fmt"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/store"
)

const (
	DefaultBudget = 8000
	MinBudget     = 500
	MaxBudget     = 24000
	DefaultSince  = 30

	// fillRatio leaves room for the JSON envelope around the content.
	fillRatio = 0.9

	// shareCorrections is the slice of the budget reserved for things that went
	// wrong. Errors are disproportionately valuable — "we tried this and it broke"
	// saves an agent from repeating it — so they get a guaranteed floor rather
	// than competing with ordinary evidence for space.
	shareCorrections = 0.30
	shareEvidence    = 0.70

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
	// Epistemic is always "observed" locally: everything here is captured
	// transcript, not interpretation. A hub's distiller introduces "inferred" and
	// "asserted"; a binary that never calls a model cannot produce them, and
	// claiming otherwise would be the exact lie the field exists to prevent.
	Epistemic string `json:"epistemic"`
	Freshness string `json:"freshness"`
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
	Section  string `json:"section"`
	Included int    `json:"included"`
	Dropped  int    `json:"dropped"`
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
	Budget    Budget `json:"budget"`
	Sections  struct {
		Corrections []Evidence `json:"corrections"`
		Evidence    []Evidence `json:"evidence"`
	} `json:"sections"`
	Citations []string `json:"citations"`
}

// Input parameterizes packet assembly.
type Input struct {
	Task        string
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
		Budget:    Budget{MaxTokens: budget, Truncated: []Truncation{}},
		Citations: []string{},
	}
	if in.Repo != "" {
		r := in.Repo
		p.Repo = &r
	}
	p.Sections.Corrections = []Evidence{}
	p.Sections.Evidence = []Evidence{}

	terms := DistillQuery(in.Task, 8)
	if len(terms) == 0 {
		p.Retrieval = "recency_fallback"
		return p, nil
	}

	// The retrieval ladder. Start strict, loosen only when strict comes up short,
	// and say which rung was used.
	andQuery := strings.Join(terms, " AND ")
	hits, err := db.SearchChunks(ctx, andQuery, in.Repo, "", 24)
	if err != nil {
		return nil, err
	}
	if len(hits) < 3 {
		orQuery := strings.Join(terms, " OR ")
		loose, err := db.SearchChunks(ctx, orQuery, in.Repo, "", 24)
		if err != nil {
			return nil, err
		}
		// Adopt the looser pass only if it genuinely found more. Swapping in a
		// worse result set just because it ran would make the ladder a lie.
		if len(loose) > len(hits) {
			hits, p.Retrieval = loose, "match_or"
		}
	}

	corrections, err := db.SearchChunks(ctx, strings.Join(terms, " OR "), in.Repo, "error", 8)
	if err != nil {
		return nil, err
	}

	if len(hits) == 0 && len(corrections) == 0 {
		p.Retrieval = "recency_fallback"
		return p, nil
	}

	// Window expansion: pull the chunks either side of each hit so evidence reads
	// as passages rather than fragments. A decision is often stated in one window
	// and its reason in the next.
	hits, err = db.ExpandWindow(ctx, hits, adjacentScoreFactor)
	if err != nil {
		return nil, err
	}

	fill := int(float64(budget) * fillRatio)
	served := map[string]bool{}

	// Corrections are filled first so an error is never crowded out by ordinary
	// evidence that merely scored higher.
	p.Sections.Corrections, p.Budget.Truncated = fillSection(
		"corrections", corrections, int(float64(fill)*shareCorrections), now, served, p.Budget.Truncated)

	// Headroom rolls forward: whatever corrections did not use is available to
	// evidence, so a task with no prior failures still gets a full packet.
	used := sumTokens(p.Sections.Corrections)
	evidenceCap := int(float64(fill)*shareEvidence) + (int(float64(fill)*shareCorrections) - used)
	p.Sections.Evidence, p.Budget.Truncated = fillSection(
		"evidence", hits, evidenceCap, now, served, p.Budget.Truncated)

	for _, e := range append(append([]Evidence{}, p.Sections.Corrections...), p.Sections.Evidence...) {
		p.Citations = append(p.Citations, e.Ref)
	}
	p.Budget.UsedTokens = sumTokens(p.Sections.Corrections) + sumTokens(p.Sections.Evidence)
	return p, nil
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
