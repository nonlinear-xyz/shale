package pack

import (
	"context"
	"math"
	"sort"
	"strconv"
	"time"
)

// Candidates is the immutable input to packing, also used by paired benchmarks.
// Packet sections contain all eligible candidates, not a budgeted response.
type Candidates struct {
	Packet      Packet  `json:"packet"`
	RetrievalMS float64 `json:"retrievalMs"`
}

type Relevance struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

type RankingReport struct {
	Provider       string            `json:"provider"`
	Model          string            `json:"model,omitempty"`
	PromptVersion  string            `json:"promptVersion,omitempty"`
	Status         string            `json:"status"`
	Reason         string            `json:"reason,omitempty"`
	CandidateCount int               `json:"candidateCount"`
	RequestBytes   int               `json:"requestBytes"`
	InputTokens    int               `json:"inputTokens"`
	OutputTokens   int               `json:"outputTokens"`
	PreparationMS  float64           `json:"preparationMs"`
	HTTPMS         float64           `json:"httpMs"`
	RetrievalMS    float64           `json:"retrievalMs"`
	PackingMS      float64           `json:"packingMs"`
	ElapsedMS      float64           `json:"elapsedMs"`
	Sections       map[string]string `json:"sections,omitempty"`
}

// Rank returns decisions keyed by exact ref. Errors should be represented by
// safe reason codes in the report; only caller cancellation aborts assembly.
type Reranker interface {
	Rank(context.Context, string, []Evidence) (map[string]Relevance, RankingReport, error)
}

func sectionPointers(p *Packet) []*[]Evidence {
	return []*[]Evidence{&p.Sections.Checkpoints, &p.Sections.Memories, &p.Sections.Runbooks, &p.Sections.Corrections, &p.Sections.Evidence}
}

var sectionNames = []string{"checkpoints", "memories", "runbooks", "corrections", "evidence"}

// RankingCandidates deduplicates the four rerankable sections, preserving order.
func RankingCandidates(c *Candidates) []Evidence {
	seen := map[string]bool{}
	out := []Evidence{}
	for _, section := range sectionPointers(&c.Packet)[1:] {
		for _, e := range *section {
			if !seen[e.Ref] {
				seen[e.Ref] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// Assemble applies optional relevance ordering, then the original section budgets.
// It never mutates the frozen candidates, including on partial provider failure.
func Assemble(ctx context.Context, c *Candidates, ranker Reranker) (*Packet, error) {
	return assemble(ctx, c, ranker, .6)
}

// AssembleUngated is an evaluation-only policy. Production callers use Assemble.
// It bypasses the confidence veto, never validation, scope, or budget checks.
func AssembleUngated(ctx context.Context, c *Candidates, ranker Reranker) (*Packet, error) {
	return assemble(ctx, c, ranker, 0)
}

func assemble(ctx context.Context, c *Candidates, ranker Reranker, minimumConfidence float64) (*Packet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started := time.Now()
	p := c.Packet
	p.PacketID = "pkt_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	p.Citations = []string{}
	p.Budget.UsedTokens = 0
	p.Budget.Truncated = []Truncation{}
	p.Reranking = nil
	sections := sectionPointers(&p)
	for _, section := range sections {
		*section = append([]Evidence{}, (*section)...)
		for i := range *section {
			(*section)[i].Relevance = nil
		}
	}
	if ranker != nil {
		candidates := RankingCandidates(c)
		report := RankingReport{Provider: "jev", Status: "skipped", Reason: "no_candidates", Sections: map[string]string{}}
		if len(candidates) > 0 {
			scores, r, err := ranker.Rank(ctx, p.Task, candidates)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			report = r
			report.CandidateCount = len(candidates)
			report.Sections = map[string]string{}
			valid := err == nil && len(scores) == len(candidates)
			for _, e := range candidates {
				v, ok := scores[e.Ref]
				if !ok || !ValidRelevance(v) {
					valid = false
				}
			}
			if !valid {
				report.Status = "fallback"
				if report.Reason == "" {
					report.Reason = "invalid_response"
				}
			} else {
				applied, fallback := 0, 0
				for i, section := range sections[1:] {
					if len(*section) == 0 {
						continue
					}
					confident := true
					for j := range *section {
						v := scores[(*section)[j].Ref]
						(*section)[j].Relevance = &v
						if v.Confidence < minimumConfidence {
							confident = false
						}
					}
					if confident {
						sort.SliceStable(*section, func(a, b int) bool { return (*section)[a].Relevance.Score > (*section)[b].Relevance.Score })
						report.Sections[sectionNames[i+1]] = "applied"
						applied++
					} else {
						report.Sections[sectionNames[i+1]] = "low_confidence"
						fallback++
					}
				}
				report.Status, report.Reason = "applied", ""
				if fallback > 0 {
					report.Status, report.Reason = "partial", "low_confidence"
				}
				if applied == 0 {
					report.Status, report.Reason = "fallback", "low_confidence"
				}
			}
		}
		p.Reranking = &report
	}
	packing := time.Now()
	fill := int(float64(p.Budget.MaxTokens) * fillRatio)
	shares := []float64{shareCheckpoints, shareMemories, shareRunbooks, shareCorrections, shareEvidence}
	carry := 0
	served := map[string]bool{}
	for i, section := range sections {
		cap := int(float64(fill)*shares[i]) + carry
		var trunc *Truncation
		*section, trunc = packSection(*section, sectionNames[i], cap, i < 3, served)
		if trunc != nil {
			p.Budget.Truncated = append(p.Budget.Truncated, *trunc)
		}
		used := sumTokens(*section)
		carry = cap - used
		p.Budget.UsedTokens += used
		for _, e := range *section {
			p.Citations = append(p.Citations, e.Ref)
		}
	}
	if p.Reranking != nil {
		p.Reranking.RetrievalMS = c.RetrievalMS
		p.Reranking.PackingMS = float64(time.Since(packing).Microseconds()) / 1000
		p.Reranking.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000
	}
	return &p, nil
}

func ValidRelevance(v Relevance) bool {
	return !math.IsNaN(v.Score) && !math.IsInf(v.Score, 0) && v.Score >= 0 && v.Score <= 3 &&
		!math.IsNaN(v.Confidence) && !math.IsInf(v.Confidence, 0) && v.Confidence >= 0 && v.Confidence <= 1
}

func packSection(hits []Evidence, name string, cap int, artifact bool, served map[string]bool) ([]Evidence, *Truncation) {
	out := []Evidence{}
	used, dropped, excerpted := 0, 0, 0
	for _, e := range hits {
		if served[e.Ref] {
			continue
		}
		if artifact {
			var shortened, fits bool
			e, shortened, fits = fitArtifactEvidence(e, cap-used)
			if !fits {
				dropped++
				continue
			}
			if shortened {
				excerpted++
			}
		}
		cost := EstimateTokens(e.Content) + EstimateTokens(e.Title) + 16
		if used+cost > cap {
			dropped++
			continue
		}
		out = append(out, e)
		served[e.Ref] = true
		used += cost
	}
	if dropped > 0 || excerpted > 0 {
		return out, &Truncation{Section: name, Included: len(out), Dropped: dropped, Excerpted: excerpted}
	}
	return out, nil
}
