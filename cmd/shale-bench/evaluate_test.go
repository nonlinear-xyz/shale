package main

import (
	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/pack"
	"testing"
)

func TestFastFallbacksDoNotCountAsJevSuccess(t *testing.T) {
	rows := []benchmark.Attempt{
		{CaseID: "one", Variant: "jev", AssemblyMS: 1, Reranking: &pack.RankingReport{Status: "fallback", Reason: "http_429", HTTPMS: 1}},
		{CaseID: "one", Variant: "jev", AssemblyMS: 200, Reranking: &pack.RankingReport{Status: "applied", HTTPMS: 199}},
	}
	s := summarize(rows).(map[string]any)
	if s["fullyApplied"] != 1 || s["fallbackOrSkipped"] != 1 {
		t.Fatal(s)
	}
	latency := s["assemblyLatency"].(map[string]benchmark.Latency)
	if latency["jev/success_only"].N != 1 || latency["jev/success_only"].P50 != 200 {
		t.Fatal(latency)
	}
	if len(s["primaryAccuracy"].(map[string]any)) != 0 {
		t.Fatal("unreviewed accuracy claim")
	}
}
func TestReviewedCriticalRegressionOverridesRecallGain(t *testing.T) {
	rows := []benchmark.Attempt{
		{CaseID: "one", Split: "heldout", Mode: "historical", Reviewed: true, Variant: "baseline", Accuracy: &benchmark.Accuracy{RequiredRecall: .5}},
		{CaseID: "one", Split: "heldout", Mode: "historical", Reviewed: true, Variant: "jev", Accuracy: &benchmark.Accuracy{RequiredRecall: .8, MissingCritical: []string{"critical"}}},
	}
	s := summarize(rows).(map[string]any)["primaryAccuracy"].(map[string]any)["historical"].(map[string]any)
	if s["verdict"] != "regressive" {
		t.Fatal(s)
	}
}
