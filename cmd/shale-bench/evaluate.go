package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/benchmark"
	"github.com/nonlinear-xyz/shale/internal/jev"
	"github.com/nonlinear-xyz/shale/internal/pack"
)

type recorder struct {
	client *jev.Client
	scores map[string]pack.Relevance
}

func (r *recorder) Rank(ctx context.Context, t string, e []pack.Evidence) (map[string]pack.Relevance, pack.RankingReport, error) {
	s, p, err := r.client.Rank(ctx, t, e)
	r.scores = s
	return s, p, err
}

type runResult struct {
	StartedAt         time.Time           `json:"startedAt"`
	SourceFingerprint string              `json:"sourceFingerprint"`
	Commit            string              `json:"commit"`
	Platform          string              `json:"platform"`
	Model             string              `json:"configuredModel"`
	PromptVersion     string              `json:"promptVersion"`
	TimeoutMS         int64               `json:"timeoutMs"`
	SuiteHash         string              `json:"suiteHash"`
	CorpusFingerprint string              `json:"corpusFingerprint"`
	Seed              int64               `json:"seed"`
	Attempts          []benchmark.Attempt `json:"attempts"`
	Summary           any                 `json:"summary"`
}

func evaluate(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	suitePath := fs.String("suite", "", "private labeled suite.json")
	output := fs.String("output", "", "new private output JSON file")
	split := fs.String("split", "heldout", "heldout or tuning")
	repeats := fs.Int("repeats", 5, "repeats per task")
	seed := fs.Int64("seed", 42, "randomization seed")
	offline := fs.Bool("baseline-only", false, "run local baseline without a provider")
	unreviewed := fs.Bool("allow-unreviewed", false, "include exploratory cases; never count them as primary accuracy evidence")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *suitePath == "" || *output == "" || *repeats < 1 || (*split != "heldout" && *split != "tuning") {
		return errors.New("require --suite, --output, positive repeats, and heldout|tuning split")
	}
	if _, e := os.Stat(*output); e == nil {
		return errors.New("output exists; choose a new filename")
	}
	suite, e := benchmark.ReadSuite(*suitePath)
	if e != nil {
		return e
	}
	cases := []benchmark.Case{}
	for _, c := range suite.Cases {
		if c.Split == *split && (c.Reviewed || *unreviewed) {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return errors.New("no eligible cases: review and label the suite first, or use --allow-unreviewed for exploratory results")
	}
	key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	if !*offline && key == "" {
		return errors.New("live benchmark requires TYPESAFE_API_KEY; no live measurements were made")
	}
	client := jev.New(key, strings.TrimSpace(os.Getenv("SHALE_JEV_MODEL")))
	hash, e := hashFile(*suitePath)
	if e != nil {
		return e
	}
	result := runResult{StartedAt: time.Now().UTC(), SourceFingerprint: sourceFingerprint(), Commit: strings.TrimSpace(commit()), Platform: runtime.GOOS + "/" + runtime.GOARCH, Model: client.Model, PromptVersion: jev.PromptVersion, TimeoutMS: jev.Timeout.Milliseconds(), SuiteHash: hash, CorpusFingerprint: suite.CorpusFingerprint, Seed: *seed, Attempts: []benchmark.Attempt{}}
	type job struct {
		c      benchmark.Case
		repeat int
	}
	jobs := []job{}
	for _, c := range cases {
		for i := 0; i < *repeats; i++ {
			jobs = append(jobs, job{c, i})
		}
	}
	rng := rand.New(rand.NewSource(*seed))
	rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	networkSeen := false
	for n, j := range jobs {
		variants := []string{"baseline"}
		if !*offline {
			variants = append(variants, "jev")
			rng.Shuffle(2, func(i, j int) { variants[i], variants[j] = variants[j], variants[i] })
		}
		for _, variant := range variants {
			var ranker pack.Reranker
			rec := &recorder{client: client}
			if variant == "jev" {
				ranker = rec
			}
			start := time.Now()
			p, e := pack.Assemble(context.Background(), &j.c.Candidates, ranker)
			elapsed := float64(time.Since(start).Microseconds()) / 1000
			if e != nil {
				return e
			}
			a := benchmark.Attempt{CaseID: j.c.ID, Split: j.c.Split, Mode: j.c.Mode, Reviewed: j.c.Reviewed, Repeat: j.repeat, Variant: variant, CandidateCount: len(pack.RankingCandidates(&j.c.Candidates)), RetrievalMS: j.c.Candidates.RetrievalMS, AssemblyMS: elapsed, ReconstructedPacketMS: elapsed + j.c.Candidates.RetrievalMS, Citations: p.Citations, UsedTokens: p.Budget.UsedTokens, Reranking: p.Reranking}
			if p.Reranking != nil && p.Reranking.HTTPMS > 0 {
				a.InitialConnection = !networkSeen
				networkSeen = true
			}
			if len(j.c.Required) > 0 {
				accuracy := benchmark.Measure(j.c, p)
				a.Accuracy = &accuracy
			}
			if len(rec.scores) > 0 {
				refs := []string{}
				for r := range rec.scores {
					refs = append(refs, r)
				}
				sort.Slice(refs, func(i, j int) bool {
					if rec.scores[refs[i]].Score == rec.scores[refs[j]].Score {
						return refs[i] < refs[j]
					}
					return rec.scores[refs[i]].Score > rec.scores[refs[j]].Score
				})
				a.DecisionOrder = refs
			}
			result.Attempts = append(result.Attempts, a)
		}
		fmt.Fprintf(os.Stderr, "Completed benchmark task run %d/%d\n", n+1, len(jobs))
	}
	result.Summary = summarize(result.Attempts)
	if e = writeJSON(*output, result); e != nil {
		return e
	}
	summary, _ := json.MarshalIndent(result.Summary, "", "  ")
	fmt.Println(string(summary))
	return nil
}

func band(n int) string {
	if n <= 8 {
		return "1-8"
	}
	if n <= 24 {
		return "9-24"
	}
	return "25+"
}
func summarize(attempts []benchmark.Attempt) any {
	assembly, httpTimes, reconstructed := map[string][]float64{}, map[string][]float64{}, map[string][]float64{}
	byCase := map[string][]benchmark.Attempt{}
	successes, partials, fallbacks, total := 0, 0, 0, 0
	reasons := map[string]int{}
	for _, a := range attempts {
		group := a.Variant + "/" + a.Mode
		keys := []string{group, group + "/candidates=" + band(a.CandidateCount)}
		for _, k := range keys {
			assembly[k] = append(assembly[k], a.AssemblyMS)
			reconstructed[k] = append(reconstructed[k], a.ReconstructedPacketMS)
		}
		if a.Reranking != nil {
			total++
			switch a.Reranking.Status {
			case "applied":
				successes++
			case "partial":
				partials++
			default:
				fallbacks++
			}
			if a.Reranking.Reason != "" {
				reasons[a.Reranking.Reason]++
			}
			if a.Reranking.HTTPMS > 0 {
				httpTimes["all_network_attempts"] = append(httpTimes["all_network_attempts"], a.Reranking.HTTPMS)
				connection := "warm"
				if a.InitialConnection {
					connection = "initial"
				}
				httpTimes[connection] = append(httpTimes[connection], a.Reranking.HTTPMS)
				httpTimes["candidates="+band(a.CandidateCount)] = append(httpTimes["candidates="+band(a.CandidateCount)], a.Reranking.HTTPMS)
			}
			if a.Reranking.Status == "applied" {
				assembly["jev/success_only"] = append(assembly["jev/success_only"], a.AssemblyMS)
				if a.Reranking.HTTPMS > 0 {
					httpTimes["applied_only"] = append(httpTimes["applied_only"], a.Reranking.HTTPMS)
				}
			}
		}
		byCase[a.CaseID] = append(byCase[a.CaseID], a)
	}
	latency := func(m map[string][]float64) map[string]benchmark.Latency {
		out := map[string]benchmark.Latency{}
		for k, v := range m {
			out[k] = benchmark.Latencies(v)
		}
		return out
	}
	taskRows := []map[string]any{}
	differencesByMode := map[string][]float64{}
	criticalByMode := map[string]int{}
	ids := []string{}
	for id := range byCase {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rows := byCase[id]
		b, j := []float64{}, []float64{}
		orders, sets := map[string]bool{}, map[string]bool{}
		baseCritical := map[string]bool{}
		newCritical := map[string]bool{}
		for _, a := range rows {
			if a.Variant == "baseline" && a.Accuracy != nil {
				for _, r := range a.Accuracy.MissingCritical {
					baseCritical[r] = true
				}
			}
		}
		requiredMissing, jevLabeled := 0, 0
		for _, a := range rows {
			if a.Accuracy != nil {
				if a.Variant == "baseline" {
					b = append(b, a.Accuracy.RequiredRecall)
				} else {
					j = append(j, a.Accuracy.RequiredRecall)
					jevLabeled++
					if a.Accuracy.MissingRequired {
						requiredMissing++
					}
					for _, r := range a.Accuracy.MissingCritical {
						if !baseCritical[r] {
							newCritical[r] = true
						}
					}
				}
			}
			if a.Variant == "jev" {
				refs := append([]string{}, a.Citations...)
				sort.Strings(refs)
				sets[strings.Join(refs, "|")] = true
				if len(a.DecisionOrder) > 0 {
					orders[strings.Join(a.DecisionOrder, "|")] = true
				}
			}
		}
		row := map[string]any{"caseId": id, "mode": rows[0].Mode, "reviewed": rows[0].Reviewed, "uniqueJevIncludedSets": len(sets), "uniqueJevDecisionOrders": len(orders), "newCriticalOmissions": len(newCritical), "jevRunsMissingRequired": requiredMissing, "jevLabeledRuns": jevLabeled}
		if len(b) > 0 && len(j) > 0 {
			delta := mean(j) - mean(b)
			row["baselineRecall"] = mean(b)
			row["jevRecall"] = mean(j)
			row["recallDifference"] = delta
			if rows[0].Reviewed && rows[0].Split == "heldout" {
				differencesByMode[rows[0].Mode] = append(differencesByMode[rows[0].Mode], delta)
				criticalByMode[rows[0].Mode] += len(newCritical)
			}
		}
		taskRows = append(taskRows, row)
	}
	primary := map[string]any{}
	for mode, diffs := range differencesByMode {
		lo, hi := benchmark.BootstrapDifference(diffs)
		verdict := "inconclusive"
		if mean(diffs) < 0 || criticalByMode[mode] > 0 {
			verdict = "regressive"
		} else if len(diffs) >= 10 && lo > 0 {
			verdict = "improved_on_this_sample"
		}
		primary[mode] = map[string]any{"reviewedHeldoutTasks": len(diffs), "meanRecallDifference": mean(diffs), "taskBootstrap95CI": []float64{lo, hi}, "newCriticalOmissions": criticalByMode[mode], "verdict": verdict}
	}
	return map[string]any{"assemblyLatency": latency(assembly), "httpLatency": latency(httpTimes), "reconstructedPacketLatency": latency(reconstructed), "jevAttempts": total, "fullyApplied": successes, "partial": partials, "fallbackOrSkipped": fallbacks, "fallbackReasons": reasons, "tasks": taskRows, "primaryAccuracy": primary, "notes": []string{"p95 is descriptive for this sample; repeats are correlated.", "Reconstructed packet time adds one recorded retrieval duration to each measured assembly; it is not a live MCP or agent task latency.", "Initial denotes the first network attempt; later calls reuse the client but may reconnect.", "No reviewed labels means no primary accuracy conclusion. Candidate recall, irrelevant-token fraction and omissions are recorded per attempt."}}
}
func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}
