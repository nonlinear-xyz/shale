# Jev context evaluation

The integration improves selection within the candidates Shale already retrieves.
It does not add semantic candidate retrieval, summaries, recursive skills, or
agent orchestration. Checkpoints retain their original ordering.

Enable the MCP server with both environment variables:

```sh
export SHALE_CONTEXT_RERANKER=jev
export TYPESAFE_API_KEY='your-key'
shale mcp
```

Configure these in the launching harness environment when it does not inherit
your shell. A key alone does not enable Jev. `SHALE_JEV_MODEL` overrides the pinned
`jev-1.13.0` default. Removing `SHALE_CONTEXT_RERANKER` restores local-only behavior.

When enabled, the task and scrubbed candidate titles/excerpts are sent to
`https://api.typesafe.ai/v1/systemone`. Explicit source paths, refs, repository
metadata, and task keys are omitted. Paths or sensitive business information may
still occur within task/content text: scrubbing removes recognized secrets, not
all private information. This is an opt-in hosted-data path, not local inference.
The client never persists provider requests or responses.

One request scores each unique candidate on a four-level relevance rubric. It
sorts within each section, preserving ties and lexical scores. Any confidence
below 0.6 leaves that section in local order. This threshold is provisional and
must be evaluated on the tuning split, not adjusted against held-out results.
The model never approves memories, changes scope/authority, or deletes evidence.

A two-second HTTP deadline and no retries bound provider waiting. Failures restore
local ordering. Packet `reranking` diagnostics distinguish applied, partial,
fallback, and skipped states; `relevance` carries valid per-item scores and
confidence. Existing retrieval labels describe the original local search.

## Prepare a real-data benchmark

Build the runner and create an isolated snapshot outside the repository:

```sh
CGO_ENABLED=0 go build -o /tmp/shale-bench ./cmd/shale-bench
python3 scripts/prepare_jev_benchmark.py --output /private/tmp/shale-jev-benchmark
/tmp/shale-bench prepare --dir /private/tmp/shale-jev-benchmark
/tmp/shale-bench inspect --suite /private/tmp/shale-jev-benchmark/suite.json
```

Use a new output directory for each preparation. The Python SQLite backup API
includes committed WAL contents. Required blob directories are copied; the live
store is read-only. Only the private snapshot is migrated and its search
projections changed. The original snapshot fingerprint remains in the manifest.

Task drafts come from the first user-message excerpt in real sessions. They may
need editing when a message depends on unstated preceding context. The preparer
selects up to 10 tuning and 30 held-out tasks, dividing whole repositories between
splits and sampling repositories round-robin. Cases with fewer than two eligible
candidates are listed in `skipped.json`; report the remaining sample size.

Historical cases retrieve only completed sessions in the 30 days preceding the
task and exclude every capture of the target session. Future evidence is removed
**before** retrieval. Historical native-artifact states are not reconstructed:
this mode is explicitly `historical_transcript_only`. It cannot establish
memory/runbook ranking accuracy. To test real harness memories, prepare a **separate** snapshot:

```sh
python3 scripts/prepare_jev_benchmark.py --output /private/tmp/shale-jev-present-day
/tmp/shale-bench prepare --dir /private/tmp/shale-jev-present-day --mode present_day
```

This snapshots existing Claude/Codex memory files into the isolated store and
retrieves present-day context for the sampled tasks, still excluding the target
session's transcript versions. These cases are labeled `present_day`: memories
and evidence may have been created after the original task. Never pool these
results with historical cases. `memory-refresh.json` records refresh counts.
Source memory files and the live Shale index remain untouched.

`suite.json` contains the task, exact refs, timestamps/provenance, and full local
candidate text, all before packing or provider excerpting. It is private and must
not be committed. Review each task and assign refs to:

- `required`: context needed to solve the task, including known refs missed by retrieval.
- `critical`: the subset of required context whose omission is a serious regression.
- `helpful`: useful context that is not required.
- `irrelevant`: distractors or unrelated context.

Record why in `labelNotes`. Labels must be based on task evidence, not the Jev
ranking or the baseline rank. Set `taskReviewed` and `reviewed` to true only after
human review. Reviewed cases require a nonempty required set and labels for all
candidates; zero-context tasks are exploratory in this version. The frozen full
candidate text is reviewable directly; all original chunk rows also remain in
`benchmark_chunks` in the private snapshot for investigating missed refs.

## Run the experiment

With the key set in this terminal:

```sh
/tmp/shale-bench run --suite /private/tmp/shale-jev-benchmark/suite.json \
  --output /private/tmp/shale-jev-benchmark/heldout-results.json
/tmp/shale-bench batch --suite /private/tmp/shale-jev-benchmark/suite.json \
  --output /private/tmp/shale-jev-benchmark/batch-results.json
```

`run` defaults to the held-out split and five repetitions per case. Use
`--split tuning` during prompt/threshold development. Each run randomizes task
repetitions and baseline/Jev order with a recorded seed. It reuses one HTTP client.
All failures stay in operational latency measurements, with separate successful
application and initial/warmed-network summaries. Later requests may reconnect;
“warm” describes client reuse rather than a guaranteed persistent connection.

For a dry local baseline, add `--baseline-only`. For latency exploration before
label review, add `--allow-unreviewed`; those cases never contribute to primary
accuracy claims. No live benchmark runs in ordinary CI. Output files are created
exclusively to avoid overwriting labels or earlier measurements.

Report accuracy and latency together:

- Required-reference recall and candidate-pool recall separate retrieval failure
  from packing/reranking failure. These measure reference inclusion, not whether a
  truncated excerpt retains every required fact.
- Missing-required cases and newly missing critical refs expose regressions.
- Irrelevant-token and labeled-token fractions measure content-budget quality.
- Unique included sets and score-based decision orders expose repeat instability.
- HTTP and assembly p50/p95 show measured latency, grouped by candidate count.
  “Reconstructed packet latency” adds the recorded retrieval time to each measured
  assembly. It is an estimate, not measured live MCP or agent task latency.
- Request bytes, provider token usage, fallback reasons, model/prompt versions,
  suite hash, corpus/source fingerprints, platform, and Git commit make comparisons auditable.

The report bootstraps paired recall differences by task, never by repetition.
An improvement verdict requires at least ten reviewed held-out tasks, a positive
95% interval, and no new critical omission. Otherwise report uncertainty or
regression. p95 from a small sample is descriptive. These results do not establish
end-to-end task speed; that requires running an agent on paired tasks.

The separate batching experiment repeats ten pairs of one eight-question request
versus eight sequential single-question requests over identical state. It reports
all-attempt and complete-only latency, usage, and answer differences. This tests
batching, not parallel singleton requests or local keyword retrieval.

## Build verification

```sh
CGO_ENABLED=0 go test ./...
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=1 go test -race ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
CGO_ENABLED=0 go test ./internal/pack ./internal/jev -run '^$' -bench . -benchmem
```

The Go benchmarks measure local overhead only. Client tests use controlled
transports to verify batching, deadline enforcement, and failure handling; they
cannot establish real Jev latency or accuracy.

## Compare the confidence gate using shared scores

`compare` evaluates **baseline**, **jev_gated** (the current 0.6 section veto), and
**jev_ungated**. It defaults to the tuning split and makes one provider call per
case/repetition, then replays the same scores, confidence values, and errors into
both policies. Production `context_for_task` still uses the original gate.
Malformed or failed provider responses fall back in both policies.

First review the task and source-based labels, without looking at Jev scores:

```sh
python3 scripts/render_jev_review.py --suite /path/suite-draft.json \
  --output /path/review.html
open /path/review.html
```

The offline sheet shows draft judgments and supporting excerpts when the suite
has `labelEvidence` entries. Edit the classifications and critical flags, check
the task and case review boxes, then **Download reviewed suite**. Nothing is
uploaded or written back to the original file. Changing a label clears that
case's reviewed status. These controls record your judgment; they are not an
independent check that a label is correct.

With `TYPESAFE_API_KEY` set:

```sh
CGO_ENABLED=0 go build -o /tmp/shale-bench ./cmd/shale-bench
/tmp/shale-bench compare --suite /path/jev-tuning-reviewed.json \
  --output /path/comparison.json --repeats 5
```

The comparison records actual scoring/network time once and distinguishes local
policy replay time from estimated standalone assembly time (shared scoring plus
replay). Each policy reports required-reference recall, irrelevant-token fraction,
critical omissions, and inclusion consistency; the three paired contrasts are
reported independently. Reviewed tuning results remain `tuning_only`. Choose a
policy before running `--split heldout`; do not repurpose held-out cases for tuning.

The initial private review artifact contains six **authored memory probes** over
an eight-memory project-scoped pool. These are real source memories with drafted
tasks and labels; they are not historical user requests or lexical-retrieval
results. Their `curated_memory_probe` mode keeps them separate from production
retrieval evaluation. They diagnose policy behavior under a fixed token budget;
validate the eventual choice on independently labeled, untouched retrieval cases.

## View results in HTML

```sh
python3 scripts/render_jev_report.py --results /path/comparison.json \
  --suite /path/jev-tuning-reviewed.json --output /path/comparison.html
open /path/comparison.html
```

The report supports both the original `run` JSON and the three-policy `compare`
JSON. It displays actual provider p50/p95, policy outcomes, reviewed accuracy,
and per-task citations added or lost against the baseline. The optional suite
provides task text and source titles. Its **Load benchmark JSON** button lets you
view another run without rebuilding; unknown task refs remain visible by ID.
The **Review tuning labels** link expects `review.html` in the same directory.

Both artifacts are self-contained and make no network requests. Keep generated
HTML, private suites, and results outside the repository. Missing measurements
are displayed as not run or unreviewed, rather than as zero accuracy.
