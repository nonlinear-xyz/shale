package pack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/nonlinear-xyz/shale/internal/store"
	"reflect"
	"strings"
	"testing"
)

type rankFunc func(context.Context, string, []Evidence) (map[string]Relevance, RankingReport, error)

func (f rankFunc) Rank(ctx context.Context, t string, e []Evidence) (map[string]Relevance, RankingReport, error) {
	return f(ctx, t, e)
}
func candidatesFixture() *Candidates {
	c := &Candidates{Packet: Packet{Task: "release signing", Budget: Budget{MaxTokens: 500}, Citations: []string{}, Retrieval: "match"}}
	c.Packet.Sections.Checkpoints = []Evidence{{Ref: "checkpoint:a", Content: "Resume release"}}
	c.Packet.Sections.Memories = []Evidence{{Ref: "memory:distractor", Content: strings.Repeat("release overview ", 50)}, {Ref: "memory:fix", Content: "Set APPLE_DEVELOPER_ID"}}
	c.Packet.Sections.Runbooks = []Evidence{}
	c.Packet.Sections.Corrections = []Evidence{}
	c.Packet.Sections.Evidence = []Evidence{{Ref: "chunk:1:0", Content: "signing issue"}, {Ref: "chunk:2:0", Content: "certificate fix"}}
	return c
}
func TestRankingPacksUsefulMemoryAndPreservesFrozenInput(t *testing.T) {
	c := candidatesFixture()
	before, _ := json.Marshal(c)
	calls := 0
	rank := rankFunc(func(_ context.Context, _ string, es []Evidence) (map[string]Relevance, RankingReport, error) {
		calls++
		out := map[string]Relevance{}
		for _, e := range es {
			if strings.HasPrefix(e.Ref, "checkpoint:") {
				t.Fatal("checkpoint sent")
			}
			out[e.Ref] = Relevance{1, 1}
		}
		out["memory:fix"] = Relevance{3, 1}
		return out, RankingReport{Provider: "jev"}, nil
	})
	baseline, _ := Assemble(context.Background(), c, nil)
	p, e := Assemble(context.Background(), c, rank)
	if e != nil || calls != 1 || p.Sections.Memories[0].Ref != "memory:fix" || p.Reranking.Status != "applied" {
		t.Fatal(p, e)
	}
	if baseline.Sections.Memories[0].Ref != "memory:distractor" {
		t.Fatal("fixture not meaningful")
	}
	if p.Sections.Evidence[0].Ref != "chunk:1:0" {
		t.Fatal("unstable ties")
	}
	if p.Budget.UsedTokens > p.Budget.MaxTokens {
		t.Fatal("over budget")
	}
	after, _ := json.Marshal(c)
	if string(before) != string(after) {
		t.Fatal("mutated candidates")
	}
}
func TestRankingFallbacks(t *testing.T) {
	for _, mode := range []string{"error", "missing", "invalid", "confidence"} {
		t.Run(mode, func(t *testing.T) {
			c := candidatesFixture()
			base, _ := Assemble(context.Background(), c, nil)
			rank := rankFunc(func(_ context.Context, _ string, es []Evidence) (map[string]Relevance, RankingReport, error) {
				m := map[string]Relevance{}
				for _, e := range es {
					m[e.Ref] = Relevance{3, 1}
				}
				switch mode {
				case "error":
					return nil, RankingReport{Reason: "timeout"}, errors.New("unsafe provider text")
				case "missing":
					delete(m, es[0].Ref)
				case "invalid":
					m[es[0].Ref] = Relevance{99, 1}
				case "confidence":
					for k := range m {
						m[k] = Relevance{3, .5}
					}
				}
				return m, RankingReport{}, nil
			})
			p, err := Assemble(context.Background(), c, rank)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(base.Citations, p.Citations) || p.Reranking.Status != "fallback" {
				t.Fatalf("%v %+v", p.Citations, p.Reranking)
			}
		})
	}
}
func TestDeduplicateAndCancel(t *testing.T) {
	c := candidatesFixture()
	c.Packet.Sections.Corrections = append(c.Packet.Sections.Corrections, c.Packet.Sections.Evidence[0])
	if len(RankingCandidates(c)) != 4 {
		t.Fatal("duplicate candidate")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := Assemble(ctx, c, nil); e != context.Canceled {
		t.Fatal(e)
	}
}
func BenchmarkAssemble(b *testing.B) {
	c := candidatesFixture()
	b.ReportAllocs()
	for b.Loop() {
		if _, e := Assemble(context.Background(), c, nil); e != nil {
			b.Fatal(e)
		}
	}
}

func TestRerankerOnlySeesEligibleArtifacts(t *testing.T) {
	db := seedCorpus(t)
	ctx := context.Background()
	for _, in := range []store.ArtifactInput{
		{Kind: store.ArtifactMemory, ScopeKind: store.ScopeRepo, Repo: "acme/app", Title: "Signing", Content: store.ArtifactContent{Text: "signing eligible"}},
		{Kind: store.ArtifactMemory, Status: store.ArtifactPending, ScopeKind: store.ScopeRepo, Repo: "acme/app", Title: "Signing", Content: store.ArtifactContent{Text: "signing forbidden pending"}},
		{Kind: store.ArtifactMemory, ScopeKind: store.ScopeRepo, Repo: "acme/other", Title: "Signing", Content: store.ArtifactContent{Text: "signing forbidden other repo"}},
	} {
		if _, _, e := db.PutArtifact(ctx, in); e != nil {
			t.Fatal(e)
		}
	}
	called := false
	rank := rankFunc(func(_ context.Context, _ string, es []Evidence) (map[string]Relevance, RankingReport, error) {
		called = true
		out := map[string]Relevance{}
		found := false
		for _, e := range es {
			if strings.Contains(e.Content, "forbidden") {
				t.Fatal("ineligible content sent")
			}
			if strings.Contains(e.Content, "eligible") {
				found = true
			}
			out[e.Ref] = Relevance{1, 1}
		}
		if !found {
			t.Fatal("eligible memory missing")
		}
		return out, RankingReport{}, nil
	})
	if _, e := Build(ctx, db, Input{Task: "signing", Repo: "acme/app", Reranker: rank}); e != nil {
		t.Fatal(e)
	}
	if !called {
		t.Fatal("ranker not invoked")
	}
}

func TestUngatedRetainsValidationAndLeavesProductionGated(t *testing.T) {
	c := candidatesFixture()
	for _, invalid := range []bool{false, true} {
		rank := rankFunc(func(_ context.Context, _ string, es []Evidence) (map[string]Relevance, RankingReport, error) {
			m := map[string]Relevance{}
			for _, e := range es {
				m[e.Ref] = Relevance{1, .1}
			}
			m["memory:fix"] = Relevance{3, .1}
			if invalid {
				m["memory:fix"] = Relevance{99, .1}
			}
			return m, RankingReport{}, nil
		})
		normal, _ := Assemble(context.Background(), c, rank)
		ungated, _ := AssembleUngated(context.Background(), c, rank)
		if normal.Reranking.Status != "fallback" {
			t.Fatal("production gating changed")
		}
		if invalid {
			if ungated.Reranking.Status != "fallback" {
				t.Fatal("invalid score applied")
			}
		} else {
			if ungated.Sections.Memories[0].Ref != "memory:fix" || ungated.Sections.Memories[0].Relevance.Confidence != .1 {
				t.Fatal("ungated did not preserve scores/confidence")
			}
		}
	}
}
