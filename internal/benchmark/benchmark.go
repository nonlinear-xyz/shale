// Package benchmark evaluates frozen packets. Accuracy labels are independent
// of Jev output, and only human-reviewed cases support primary accuracy claims.
package benchmark

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"

	"github.com/nonlinear-xyz/shale/internal/pack"
)

type Case struct {
	ID            string          `json:"id"`
	Split         string          `json:"split"`
	Mode          string          `json:"mode"`
	TaskSourceRef string          `json:"taskSourceRef"`
	TaskReviewed  bool            `json:"taskReviewed"`
	Reviewed      bool            `json:"reviewed"`
	LabelNotes    string          `json:"labelNotes"`
	Required      []string        `json:"required"`
	Critical      []string        `json:"critical"`
	Helpful       []string        `json:"helpful"`
	Irrelevant    []string        `json:"irrelevant"`
	Candidates    pack.Candidates `json:"candidates"`
}

type Suite struct {
	Version           int    `json:"version"`
	CorpusFingerprint string `json:"corpusFingerprint"`
	Cases             []Case `json:"cases"`
}

type Accuracy struct {
	RequiredRecall          float64  `json:"requiredRecall"`
	CandidateRecall         float64  `json:"candidateRecall"`
	MissingRequired         bool     `json:"missingRequired"`
	MissingCritical         []string `json:"missingCritical"`
	IrrelevantTokenFraction float64  `json:"irrelevantTokenFraction"`
	LabeledTokenFraction    float64  `json:"labeledTokenFraction"`
}

func Entries(p *pack.Packet) []pack.Evidence {
	out := []pack.Evidence{}
	for _, s := range [][]pack.Evidence{p.Sections.Checkpoints, p.Sections.Memories, p.Sections.Runbooks, p.Sections.Corrections, p.Sections.Evidence} {
		out = append(out, s...)
	}
	return out
}

func Validate(c Case) error {
	if c.ID == "" || c.Candidates.Packet.Task == "" {
		return fmt.Errorf("case requires id and task")
	}
	if c.Split != "heldout" && c.Split != "tuning" {
		return fmt.Errorf("%s: invalid split", c.ID)
	}
	b := c.Candidates.Packet.Budget.MaxTokens
	if b < pack.MinBudget || b > pack.MaxBudget {
		return fmt.Errorf("%s: invalid budget", c.ID)
	}
	labels := map[string]bool{}
	for _, list := range [][]string{c.Required, c.Helpful, c.Irrelevant} {
		for _, r := range list {
			if labels[r] {
				return fmt.Errorf("%s: duplicate label %s", c.ID, r)
			}
			labels[r] = true
		}
	}
	required := map[string]bool{}
	for _, r := range c.Required {
		required[r] = true
	}
	for _, r := range c.Critical {
		if !required[r] {
			return fmt.Errorf("%s: critical ref must also be required", c.ID)
		}
	}
	if c.Reviewed {
		if !c.TaskReviewed || len(c.Required) == 0 {
			return fmt.Errorf("%s: reviewed case needs reviewed task and required labels", c.ID)
		}
		for _, e := range Entries(&c.Candidates.Packet) {
			if !labels[e.Ref] {
				return fmt.Errorf("%s: unlabeled candidate %s", c.ID, e.Ref)
			}
		}
	}
	return nil
}

func Measure(c Case, p *pack.Packet) Accuracy {
	included, pool := map[string]bool{}, map[string]bool{}
	for _, e := range Entries(p) {
		included[e.Ref] = true
	}
	for _, e := range Entries(&c.Candidates.Packet) {
		pool[e.Ref] = true
	}
	a := Accuracy{MissingCritical: []string{}}
	if len(c.Required) > 0 {
		for _, r := range c.Required {
			if included[r] {
				a.RequiredRecall++
			}
			if pool[r] {
				a.CandidateRecall++
			}
		}
		a.RequiredRecall /= float64(len(c.Required))
		a.CandidateRecall /= float64(len(c.Required))
		a.MissingRequired = a.RequiredRecall < 1
	}
	for _, r := range c.Critical {
		if !included[r] {
			a.MissingCritical = append(a.MissingCritical, r)
		}
	}
	irrelevant, labeled := map[string]bool{}, map[string]bool{}
	for _, r := range c.Irrelevant {
		irrelevant[r] = true
	}
	for _, list := range [][]string{c.Required, c.Helpful, c.Irrelevant} {
		for _, r := range list {
			labeled[r] = true
		}
	}
	total, bad, known := 0, 0, 0
	for _, e := range Entries(p) {
		n := pack.EstimateTokens(e.Content)
		total += n
		if irrelevant[e.Ref] {
			bad += n
		}
		if labeled[e.Ref] {
			known += n
		}
	}
	if total > 0 {
		a.IrrelevantTokenFraction = float64(bad) / float64(total)
		a.LabeledTokenFraction = float64(known) / float64(total)
	}
	return a
}

type Attempt struct {
	CaseID                string              `json:"caseId"`
	Split                 string              `json:"split"`
	Mode                  string              `json:"mode"`
	Reviewed              bool                `json:"reviewed"`
	Repeat                int                 `json:"repeat"`
	Variant               string              `json:"variant"`
	InitialConnection     bool                `json:"initialConnection"`
	CandidateCount        int                 `json:"candidateCount"`
	RetrievalMS           float64             `json:"retrievalMs"`
	AssemblyMS            float64             `json:"assemblyMs"`
	ReconstructedPacketMS float64             `json:"reconstructedPacketMs"`
	Accuracy              *Accuracy           `json:"accuracy,omitempty"`
	Citations             []string            `json:"citations"`
	DecisionOrder         []string            `json:"decisionOrder,omitempty"`
	UsedTokens            int                 `json:"usedTokens"`
	Reranking             *pack.RankingReport `json:"reranking,omitempty"`
}

func Quantile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	v := append([]float64{}, values...)
	sort.Float64s(v)
	i := int(math.Ceil(p*float64(len(v)))) - 1
	if i < 0 {
		i = 0
	}
	return v[i]
}

type Latency struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50Ms"`
	P95 float64 `json:"p95Ms"`
}

func Latencies(v []float64) Latency { return Latency{len(v), Quantile(v, .5), Quantile(v, .95)} }

// BootstrapDifference resamples tasks, not their correlated repetitions.
func BootstrapDifference(differences []float64) (float64, float64) {
	if len(differences) == 0 {
		return 0, 0
	}
	rng := rand.New(rand.NewSource(7))
	samples := make([]float64, 2000)
	for i := range samples {
		for range differences {
			samples[i] += differences[rng.Intn(len(differences))]
		}
		samples[i] /= float64(len(differences))
	}
	return Quantile(samples, .025), Quantile(samples, .975)
}

func ReadSuite(path string) (Suite, error) {
	var s Suite
	b, e := os.ReadFile(path)
	if e != nil {
		return s, e
	}
	if e = json.Unmarshal(b, &s); e != nil {
		return s, e
	}
	if s.Version != 1 {
		return s, fmt.Errorf("unsupported suite version")
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if seen[c.ID] {
			return s, fmt.Errorf("duplicate case id")
		}
		seen[c.ID] = true
		if e = Validate(c); e != nil {
			return s, e
		}
	}
	return s, nil
}
