package main

import (
	"context"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/pack"
)

type countingRanker struct {
	calls int
	fail  bool
}

func (r *countingRanker) Rank(_ context.Context, _ string, es []pack.Evidence) (map[string]pack.Relevance, pack.RankingReport, error) {
	r.calls++
	report := pack.RankingReport{Provider: "jev", Status: "applied", HTTPMS: 100, InputTokens: 123}
	if r.fail {
		report.Status = "fallback"
		report.Reason = "http_429"
		return nil, report, errors.New("http_429")
	}
	out := map[string]pack.Relevance{}
	for _, e := range es {
		out[e.Ref] = pack.Relevance{Score: 1, Confidence: .9}
	}
	out["memory:needed"] = pack.Relevance{Score: 3, Confidence: .4}
	return out, report, nil
}
func comparisonFixture() benchmark.Case {
	c := benchmark.Case{ID: "task", Split: "tuning", Mode: "test", Reviewed: true, TaskReviewed: true, Required: []string{"memory:needed"}, Critical: []string{"memory:needed"}, Irrelevant: []string{"memory:noise"}}
	c.Candidates.Packet.Task = "fix signing"
	c.Candidates.Packet.Budget.MaxTokens = 500
	c.Candidates.Packet.Sections.Memories = []pack.Evidence{{Ref: "memory:noise", Content: "distracting overview"}, {Ref: "memory:needed", Content: "use the correct signing certificate"}}
	return c
}
func TestComparisonReusesExactlyOneCallAndRealConfidence(t *testing.T) {
	c := comparisonFixture()
	ranker := &countingRanker{}
	trial, err := compareCase(context.Background(), c, ranker, rand.New(rand.NewSource(1)), 0)
	if err != nil || ranker.calls != 1 || len(trial.Results) != 3 {
		t.Fatal(err, ranker.calls)
	}
	results := map[string]policyResult{}
	for _, r := range trial.Results {
		results[r.Policy] = r
	}
	if !reflect.DeepEqual(results["baseline"].Citations, results["jev_gated"].Citations) {
		t.Fatal("gated policy changed")
	}
	if results["jev_ungated"].Citations[0] != "memory:needed" || results["jev_gated"].Status != "fallback" || results["jev_ungated"].Status != "applied" {
		t.Fatal(results)
	}
	if trial.Scores["memory:needed"].Confidence != .4 {
		t.Fatal("confidence overwritten")
	}
	if c.Candidates.Packet.Sections.Memories[0].Ref != "memory:noise" {
		t.Fatal("frozen candidates mutated")
	}
	summary := comparisonSummary([]comparisonTrial{trial}).(map[string]any)
	if summary["scoringCalls"] != 1 || summary["networkAttempts"] != 1 || summary["inputTokens"] != 123 {
		t.Fatal("double-counted call", summary)
	}
	lat := summary["httpLatency"].(map[string]benchmark.Latency)
	if lat["all_network_attempts"].N != 1 {
		t.Fatal("double-counted latency")
	}
	for _, v := range summary["reviewedAccuracy"].(map[string]any) {
		if v.(map[string]any)["interpretation"] != "tuning_only" {
			t.Fatal("tuning presented as validation")
		}
	}
}
func TestUngatedNeverBypassesProviderFailure(t *testing.T) {
	ranker := &countingRanker{fail: true}
	trial, err := compareCase(context.Background(), comparisonFixture(), ranker, rand.New(rand.NewSource(1)), 0)
	if err != nil || ranker.calls != 1 {
		t.Fatal(err)
	}
	for _, r := range trial.Results {
		if r.Policy != "baseline" && (r.Status != "fallback" || r.Reason != "http_429") {
			t.Fatal(r)
		}
	}
}
func TestComparisonCancellationSkipsProvider(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &countingRanker{}
	if _, err := compareCase(ctx, comparisonFixture(), r, rand.New(rand.NewSource(1)), 0); err != context.Canceled || r.calls != 0 {
		t.Fatal(err, r.calls)
	}
}
