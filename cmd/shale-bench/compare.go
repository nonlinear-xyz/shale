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

// A frozen ranking lets both policies use precisely the same model decisions.
// The error is replayed as well: an invalid call cannot become an ungated success.
type frozenRanking struct {
	scores map[string]pack.Relevance
	report pack.RankingReport
	err    error
}

func (f frozenRanking) Rank(context.Context, string, []pack.Evidence) (map[string]pack.Relevance, pack.RankingReport, error) {
	return f.scores, f.report, f.err
}

type policyResult struct {
	Policy                        string              `json:"policy"`
	ReplayMS                      float64             `json:"replayMs"`
	EstimatedStandaloneAssemblyMS float64             `json:"estimatedStandaloneAssemblyMs"`
	Citations                     []string            `json:"citations"`
	UsedTokens                    int                 `json:"usedTokens"`
	Accuracy                      *benchmark.Accuracy `json:"accuracy,omitempty"`
	Status                        string              `json:"status"`
	Reason                        string              `json:"reason,omitempty"`
	Sections                      map[string]string   `json:"sections,omitempty"`
}
type comparisonTrial struct {
	CaseID                string                    `json:"caseId"`
	Mode                  string                    `json:"mode"`
	Split                 string                    `json:"split"`
	Reviewed              bool                      `json:"reviewed"`
	Repeat                int                       `json:"repeat"`
	CandidateCount        int                       `json:"candidateCount"`
	InitialNetworkAttempt bool                      `json:"initialNetworkAttempt"`
	SharedScoringMS       float64                   `json:"sharedScoringMs"`
	Call                  pack.RankingReport        `json:"call"`
	Scores                map[string]pack.Relevance `json:"scores,omitempty"`
	Results               []policyResult            `json:"results"`
}

func compareCase(ctx context.Context, c benchmark.Case, ranker pack.Reranker, rng *rand.Rand, repeat int) (comparisonTrial, error) {
	trial := comparisonTrial{CaseID: c.ID, Mode: c.Mode, Split: c.Split, Reviewed: c.Reviewed, Repeat: repeat, CandidateCount: len(pack.RankingCandidates(&c.Candidates))}
	if err := ctx.Err(); err != nil {
		return trial, err
	}
	start := time.Now()
	scores, report, err := ranker.Rank(ctx, c.Candidates.Packet.Task, pack.RankingCandidates(&c.Candidates))
	if ctx.Err() != nil {
		return trial, ctx.Err()
	}
	trial.SharedScoringMS = float64(time.Since(start).Nanoseconds()) / 1e6
	trial.Call, trial.Scores = report, scores
	frozen := frozenRanking{scores, report, err}
	policies := []string{"baseline", "jev_gated", "jev_ungated"}
	rng.Shuffle(len(policies), func(i, j int) { policies[i], policies[j] = policies[j], policies[i] })
	for _, policy := range policies {
		start = time.Now()
		var p *pack.Packet
		switch policy {
		case "baseline":
			p, err = pack.Assemble(ctx, &c.Candidates, nil)
		case "jev_gated":
			p, err = pack.Assemble(ctx, &c.Candidates, frozen)
		case "jev_ungated":
			p, err = pack.AssembleUngated(ctx, &c.Candidates, frozen)
		}
		if err != nil {
			return trial, err
		}
		elapsed := float64(time.Since(start).Nanoseconds()) / 1e6
		r := policyResult{Policy: policy, ReplayMS: elapsed, EstimatedStandaloneAssemblyMS: elapsed, Citations: p.Citations, UsedTokens: p.Budget.UsedTokens, Status: "local"}
		if policy != "baseline" {
			r.EstimatedStandaloneAssemblyMS += trial.SharedScoringMS
		}
		if p.Reranking != nil {
			r.Status = p.Reranking.Status
			r.Reason = p.Reranking.Reason
			r.Sections = p.Reranking.Sections
		}
		if len(c.Required) > 0 {
			a := benchmark.Measure(c, p)
			r.Accuracy = &a
		}
		trial.Results = append(trial.Results, r)
	}
	return trial, nil
}

func compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	path := fs.String("suite", "", "private labeled suite")
	output := fs.String("output", "", "new result JSON")
	split := fs.String("split", "tuning", "tuning or heldout")
	repeats := fs.Int("repeats", 5, "independent scoring calls per case")
	seed := fs.Int64("seed", 42, "randomization seed")
	exploratory := fs.Bool("allow-unreviewed", false, "exploratory only; no reviewed accuracy claim")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *output == "" || *repeats < 1 || (*split != "tuning" && *split != "heldout") {
		return errors.New("require --suite, --output, positive repeats, and tuning|heldout split")
	}
	if _, err := os.Stat(*output); err == nil {
		return errors.New("output exists; choose a new path")
	}
	suite, err := benchmark.ReadSuite(*path)
	if err != nil {
		return err
	}
	cases := []benchmark.Case{}
	for _, c := range suite.Cases {
		if c.Split == *split && (c.Reviewed || *exploratory) {
			if len(pack.RankingCandidates(&c.Candidates)) == 0 {
				return fmt.Errorf("%s has no scoring candidates", c.ID)
			}
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return errors.New("no eligible cases: review task/labels first, or use --allow-unreviewed for exploratory runs")
	}
	key := strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY"))
	if key == "" {
		return errors.New("live comparison requires TYPESAFE_API_KEY")
	}
	hash, err := hashFile(*path)
	if err != nil {
		return err
	}
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
	client := jev.New(key, strings.TrimSpace(os.Getenv("SHALE_JEV_MODEL")))
	trials := []comparisonTrial{}
	networkSeen := false
	started := time.Now().UTC()
	for i, j := range jobs {
		trial, err := compareCase(context.Background(), j.c, client, rng, j.repeat)
		if err != nil {
			return err
		}
		if trial.Call.HTTPMS > 0 {
			trial.InitialNetworkAttempt = !networkSeen
			networkSeen = true
		}
		trials = append(trials, trial)
		fmt.Fprintf(os.Stderr, "Completed shared-score comparison %d/%d\n", i+1, len(jobs))
	}
	summary := comparisonSummary(trials)
	result := map[string]any{"startedAt": started, "commit": strings.TrimSpace(commit()), "sourceFingerprint": sourceFingerprint(), "suiteHash": hash, "corpusFingerprint": suite.CorpusFingerprint, "configuredModel": client.Model, "promptVersion": jev.PromptVersion, "timeoutMs": jev.Timeout.Milliseconds(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "seed": *seed, "trials": trials, "summary": summary}
	if err = writeJSON(*output, result); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(b))
	return nil
}

func comparisonSummary(trials []comparisonTrial) any {
	httpTimes := map[string][]float64{}
	scoring := []float64{}
	byPolicy := map[string][]policyResult{}
	byTask := map[string][]comparisonTrial{}
	requests, inputTokens, outputTokens := 0, 0, 0
	for _, t := range trials {
		scoring = append(scoring, t.SharedScoringMS)
		if t.Call.HTTPMS > 0 {
			requests++
			httpTimes["all_network_attempts"] = append(httpTimes["all_network_attempts"], t.Call.HTTPMS)
			k := "warm"
			if t.InitialNetworkAttempt {
				k = "initial"
			}
			httpTimes[k] = append(httpTimes[k], t.Call.HTTPMS)
			httpTimes["candidates="+band(t.CandidateCount)] = append(httpTimes["candidates="+band(t.CandidateCount)], t.Call.HTTPMS)
		}
		inputTokens += t.Call.InputTokens
		outputTokens += t.Call.OutputTokens
		for _, r := range t.Results {
			byPolicy[r.Policy] = append(byPolicy[r.Policy], r)
		}
		key := t.Split + "/" + t.Mode + "/" + t.CaseID
		byTask[key] = append(byTask[key], t)
	}
	policySummaries := map[string]any{}
	for policy, rows := range byPolicy {
		statuses := map[string]int{}
		reasons := map[string]int{}
		replay, estimated := []float64{}, []float64{}
		for _, r := range rows {
			statuses[r.Status]++
			if r.Reason != "" {
				reasons[r.Reason]++
			}
			replay = append(replay, r.ReplayMS)
			estimated = append(estimated, r.EstimatedStandaloneAssemblyMS)
		}
		policySummaries[policy] = map[string]any{"outcomes": statuses, "reasons": reasons, "localReplayLatency": benchmark.Latencies(replay), "estimatedStandaloneAssemblyLatency": benchmark.Latencies(estimated)}
	}
	keys := []string{}
	for k := range byTask {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	taskRows := []map[string]any{}
	deltas := map[string][]float64{}
	criticalLosses := map[string]int{}
	for _, key := range keys {
		trials := byTask[key]
		first := trials[0]
		results := map[string][]policyResult{}
		for _, t := range trials {
			for _, r := range t.Results {
				results[r.Policy] = append(results[r.Policy], r)
			}
		}
		metrics := map[string]any{}
		for policy, rows := range results {
			recall, badTokens, pool := []float64{}, []float64{}, []float64{}
			sets := map[string]bool{}
			missing := 0
			for _, r := range rows {
				refs := append([]string{}, r.Citations...)
				sort.Strings(refs)
				sets[strings.Join(refs, "|")] = true
				if r.Accuracy != nil {
					recall = append(recall, r.Accuracy.RequiredRecall)
					pool = append(pool, r.Accuracy.CandidateRecall)
					badTokens = append(badTokens, r.Accuracy.IrrelevantTokenFraction)
					if r.Accuracy.MissingRequired {
						missing++
					}
				}
			}
			m := map[string]any{"uniqueIncludedSets": len(sets), "runsMissingRequired": missing, "labeledRuns": len(recall)}
			if len(recall) > 0 {
				m["meanRequiredRecall"] = mean(recall)
				m["meanCandidateRecall"] = mean(pool)
				m["meanIrrelevantTokenFraction"] = mean(badTokens)
			}
			metrics[policy] = m
		}
		comparisons := map[string]any{}
		for _, pair := range [][2]string{{"jev_gated", "baseline"}, {"jev_ungated", "baseline"}, {"jev_ungated", "jev_gated"}} {
			pairName := pair[0] + "_vs_" + pair[1]
			diffs := []float64{}
			newCritical := map[string]bool{}
			for _, t := range trials {
				r := map[string]policyResult{}
				for _, v := range t.Results {
					r[v.Policy] = v
				}
				a, b := r[pair[0]].Accuracy, r[pair[1]].Accuracy
				if a == nil || b == nil {
					continue
				}
				diffs = append(diffs, a.RequiredRecall-b.RequiredRecall)
				already := map[string]bool{}
				for _, ref := range b.MissingCritical {
					already[ref] = true
				}
				for _, ref := range a.MissingCritical {
					if !already[ref] {
						newCritical[ref] = true
					}
				}
			}
			if len(diffs) > 0 {
				comparisons[pairName] = map[string]any{"meanRecallDifference": mean(diffs), "newCriticalOmissions": len(newCritical)}
				if first.Reviewed {
					group := first.Split + "/" + first.Mode + "/" + pairName
					deltas[group] = append(deltas[group], mean(diffs))
					criticalLosses[group] += len(newCritical)
				}
			}
		}
		taskRows = append(taskRows, map[string]any{"caseId": first.CaseID, "split": first.Split, "mode": first.Mode, "reviewed": first.Reviewed, "policies": metrics, "comparisons": comparisons})
	}
	reviewed := map[string]any{}
	for group, d := range deltas {
		lo, hi := benchmark.BootstrapDifference(d)
		interpretation := "tuning_only"
		if strings.HasPrefix(group, "heldout/") {
			interpretation = "inconclusive"
			if mean(d) < 0 || criticalLosses[group] > 0 {
				interpretation = "regressive"
			} else if len(d) >= 10 && lo > 0 {
				interpretation = "improved_on_this_sample"
			}
		}
		reviewed[group] = map[string]any{"tasks": len(d), "meanRecallDifference": mean(d), "taskBootstrap95CI": []float64{lo, hi}, "newCriticalOmissions": criticalLosses[group], "interpretation": interpretation}
	}
	latency := map[string]benchmark.Latency{}
	for k, v := range httpTimes {
		latency[k] = benchmark.Latencies(v)
	}
	return map[string]any{"scoringCalls": len(trials), "networkAttempts": requests, "inputTokens": inputTokens, "outputTokens": outputTokens, "sharedScoringLatency": benchmark.Latencies(scoring), "httpLatency": latency, "policies": policySummaries, "tasks": taskRows, "reviewedAccuracy": reviewed, "notes": []string{"One scoring call per case/repeat. Both Jev policies replay identical scores, confidence, and errors; network latency and usage are counted once.", "Replay latency excludes the provider call. Standalone assembly estimates add that same measured call time to each policy; these are not independent live timing measurements.", "Low confidence is bypassed only in jev_ungated; malformed or failed calls fall back under both policies.", "Tuning results do not validate a policy. Lock the policy before using untouched held-out cases; small-sample p95 is descriptive."}}
}
