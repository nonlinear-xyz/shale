package benchmark

import (
	"github.com/nonlinear-xyz/shale/internal/pack"
	"math"
	"testing"
)

func fixture() Case {
	c := Case{ID: "task", Split: "heldout", TaskReviewed: true, Reviewed: true, Required: []string{"memory:needed", "memory:missing"}, Critical: []string{"memory:needed"}, Irrelevant: []string{"memory:noise"}}
	c.Candidates.Packet.Task = "fix release"
	c.Candidates.Packet.Budget.MaxTokens = 500
	c.Candidates.Packet.Sections.Memories = []pack.Evidence{{Ref: "memory:needed", Content: "abc"}, {Ref: "memory:noise", Content: "abcdefghi"}}
	return c
}
func TestAccuracySeparatesRetrievalPackingAndCriticalLoss(t *testing.T) {
	c := fixture()
	if e := Validate(c); e != nil {
		t.Fatal(e)
	}
	p := c.Candidates.Packet
	p.Sections.Memories = p.Sections.Memories[1:]
	a := Measure(c, &p)
	if a.RequiredRecall != 0 || a.CandidateRecall != .5 || !a.MissingRequired || len(a.MissingCritical) != 1 || a.IrrelevantTokenFraction != 1 || a.LabeledTokenFraction != 1 {
		t.Fatalf("%+v", a)
	}
	p = c.Candidates.Packet
	a = Measure(c, &p)
	if a.RequiredRecall != .5 || math.Abs(a.IrrelevantTokenFraction-.75) > 1e-9 {
		t.Fatal(a)
	}
}
func TestReviewedCasesNeedIndependentCompleteLabels(t *testing.T) {
	c := fixture()
	c.Irrelevant = nil
	if Validate(c) == nil {
		t.Fatal("unlabeled reviewed candidate accepted")
	}
	c = fixture()
	c.TaskReviewed = false
	if Validate(c) == nil {
		t.Fatal("unreviewed task accepted")
	}
	c = fixture()
	c.Critical = []string{"memory:noise"}
	if Validate(c) == nil {
		t.Fatal("critical not required")
	}
}
func TestQuantilesAndTaskBootstrap(t *testing.T) {
	if Quantile([]float64{5, 1, 4, 2, 3}, .5) != 3 || Quantile([]float64{5, 1, 4, 2, 3}, .95) != 5 {
		t.Fatal("quantile")
	}
	lo, hi := BootstrapDifference([]float64{.5, .5, .5})
	if lo != .5 || hi != .5 {
		t.Fatal(lo, hi)
	}
	lo, hi = BootstrapDifference([]float64{-1, 1})
	if lo >= 0 || hi <= 0 {
		t.Fatal("uncertainty omitted")
	}
}
