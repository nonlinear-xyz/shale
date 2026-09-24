package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/jev"
	"github.com/nonlinear-xyz/shale/internal/pack"
)

func batching(args []string) error {
	fs := flag.NewFlagSet("batch", flag.ContinueOnError)
	path := fs.String("suite", "", "private suite path")
	out := fs.String("output", "", "new private JSON result")
	caseID := fs.String("case", "", "case id (default first with eight candidates)")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *path == "" || *out == "" {
		return errors.New("--suite and --output required")
	}
	if _, e := os.Stat(*out); e == nil {
		return errors.New("output already exists")
	}
	key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	if key == "" {
		return errors.New("live batching benchmark requires TYPESAFE_API_KEY")
	}
	suite, e := benchmark.ReadSuite(*path)
	if e != nil {
		return e
	}
	var selected *benchmark.Case
	for i := range suite.Cases {
		if (*caseID == "" || suite.Cases[i].ID == *caseID) && len(pack.RankingCandidates(&suite.Cases[i].Candidates)) >= 8 {
			selected = &suite.Cases[i]
			break
		}
	}
	if selected == nil {
		return errors.New("need a case with at least eight candidates")
	}
	client := jev.New(key, os.Getenv("SHALE_JEV_MODEL"))
	candidates := pack.RankingCandidates(&selected.Candidates)[:8]
	prepared, e := jev.Prepare(selected.Candidates.Packet.Task, candidates, client.Model)
	if e != nil {
		return e
	}
	type trial struct {
		Repeat    int                       `json:"repeat"`
		Variant   string                    `json:"variant"`
		ElapsedMS float64                   `json:"elapsedMs"`
		Calls     []pack.RankingReport      `json:"calls"`
		Scores    map[string]pack.Relevance `json:"scores"`
		Complete  bool                      `json:"complete"`
	}
	trials := []trial{}
	rng := rand.New(rand.NewSource(42))
	latencies := map[string][]float64{}
	completeLatencies := map[string][]float64{}
	differences := []map[string]any{}
	for i := 0; i < 10; i++ {
		variants := []string{"batch", "sequential"}
		rng.Shuffle(2, func(i, j int) { variants[i], variants[j] = variants[j], variants[i] })
		paired := map[string]trial{}
		for _, variant := range variants {
			t := trial{Repeat: i, Variant: variant, Scores: map[string]pack.Relevance{}, Complete: true}
			start := time.Now()
			count := 1
			if variant == "sequential" {
				count = 8
			}
			for j := 0; j < count; j++ {
				var ids []string
				if variant == "sequential" {
					ids = []string{fmt.Sprintf("c%d", j)}
				}
				scores, r, err := client.Evaluate(context.Background(), prepared, ids)
				t.Calls = append(t.Calls, r)
				if err != nil {
					t.Complete = false
				}
				for k, v := range scores {
					t.Scores[k] = v
				}
			}
			t.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
			trials = append(trials, t)
			paired[variant] = t
			latencies[variant] = append(latencies[variant], t.ElapsedMS)
			if t.Complete {
				completeLatencies[variant] = append(completeLatencies[variant], t.ElapsedMS)
			}
		}
		b, s := paired["batch"], paired["sequential"]
		changed := 0
		delta := 0.0
		if b.Complete && s.Complete {
			for k, v := range b.Scores {
				d := v.Score - s.Scores[k].Score
				if d < 0 {
					d = -d
				}
				delta += d
				if v != s.Scores[k] {
					changed++
				}
			}
			differences = append(differences, map[string]any{"repeat": i, "answersChanged": changed, "meanAbsoluteScoreDifference": delta / 8})
		}
		fmt.Fprintf(os.Stderr, "Completed batching pair %d/10\n", i+1)
	}
	hash, e := hashFile(*path)
	if e != nil {
		return e
	}
	return writeJSON(*out, map[string]any{"sourceFingerprint": sourceFingerprint(), "caseId": selected.ID, "suiteHash": hash, "corpusFingerprint": suite.CorpusFingerprint, "commit": strings.TrimSpace(commit()), "promptVersion": jev.PromptVersion, "configuredModel": client.Model, "startedConnection": "first call in trials; shared client thereafter", "trials": trials, "pairedAnswerDifferences": differences, "batchAllLatency": benchmark.Latencies(latencies["batch"]), "sequentialAllLatency": benchmark.Latencies(latencies["sequential"]), "batchCompleteLatency": benchmark.Latencies(completeLatencies["batch"]), "sequentialCompleteLatency": benchmark.Latencies(completeLatencies["sequential"]), "note": "Eight singleton requests share identical state with the batch; failures remain in all-attempt timing. This is not a comparison with parallel singleton requests."})
}
