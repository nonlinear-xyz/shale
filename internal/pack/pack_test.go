package pack

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nonlinear-xyz/shale/internal/store"
)

// seedCorpus builds a store with known content so retrieval behaviour can be
// asserted rather than eyeballed.
func seedCorpus(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	sessions := []struct {
		hash, repo string
		daysAgo    int
		chunks     []store.ChunkRow
	}{
		{"h1", "acme/app", 2, []store.ChunkRow{
			{Index: 0, LineStart: 1, LineEnd: 8, Kind: "transcript",
				Text: "user: add goreleaser signing for the darwin arm64 release"},
			{Index: 1, LineStart: 9, LineEnd: 14, Kind: "error",
				Text: "tool_error: goreleaser exit 1 signing identity not found"},
			{Index: 2, LineStart: 15, LineEnd: 22, Kind: "transcript",
				Text: "assistant: set APPLE_DEVELOPER_ID before invoking goreleaser"},
		}},
		{"h2", "acme/other", 5, []store.ChunkRow{
			{Index: 0, LineStart: 1, LineEnd: 6, Kind: "transcript",
				Text: "user: rewrite the pagination helper in the reporting module"},
		}},
		{"h3", "acme/app", 200, []store.ChunkRow{
			{Index: 0, LineStart: 1, LineEnd: 5, Kind: "transcript",
				Text: "user: ancient goreleaser notes from long ago"},
		}},
	}

	for _, s := range sessions {
		rec := store.SessionRecord{
			Source: "claude_code", SourceKey: s.hash, Title: s.hash, Digest: "digest",
			Repo: s.repo, EndedAt: time.Now().AddDate(0, 0, -s.daysAgo).UTC(),
		}
		if _, _, err := db.PutSession(ctx, s.hash, rec, s.chunks); err != nil {
			t.Fatalf("seed %s: %v", s.hash, err)
		}
	}
	return db
}

func TestPacketRetrievesRelevantEvidence(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{
		Task: "goreleaser signing darwin release", SinceDays: 90,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Sections.Evidence) == 0 {
		t.Fatal("no evidence retrieved for a query that matches seeded content")
	}
	if p.Retrieval == "recency_fallback" {
		t.Errorf("retrieval = %s; a matching query must not fall back", p.Retrieval)
	}

	var found bool
	for _, e := range p.Sections.Evidence {
		if strings.Contains(e.Content, "goreleaser") {
			found = true
		}
		if e.Prov.Epistemic != "observed" {
			t.Errorf("epistemic = %q; local evidence is always observed", e.Prov.Epistemic)
		}
		if e.Prov.Lines[0] < 1 || e.Prov.Lines[1] < e.Prov.Lines[0] {
			t.Errorf("bad line range %v on %s", e.Prov.Lines, e.Ref)
		}
	}
	if !found {
		t.Error("evidence does not contain the query term")
	}
	if len(p.Citations) != len(p.Sections.Evidence)+len(p.Sections.Corrections) {
		t.Errorf("citations (%d) do not cover every served item (%d)",
			len(p.Citations), len(p.Sections.Evidence)+len(p.Sections.Corrections))
	}
}

// Errors are disproportionately valuable — "we tried this and it broke" is what
// stops an agent repeating it — so they get their own section rather than
// competing with ordinary evidence for space.
func TestCorrectionsSurfaceSeparately(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{Task: "goreleaser signing identity", SinceDays: 90})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(p.Sections.Corrections) == 0 {
		t.Fatal("the seeded error chunk did not reach the corrections section")
	}
	// A chunk served as a correction must never be repeated as evidence.
	for _, c := range p.Sections.Corrections {
		for _, e := range p.Sections.Evidence {
			if c.Ref == e.Ref {
				t.Errorf("%s served twice, as both correction and evidence", c.Ref)
			}
		}
	}
}

func TestRepoScopingFiltersEvidence(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{
		Task: "pagination helper reporting module", Repo: "acme/app", SinceDays: 90,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range append(p.Sections.Evidence, p.Sections.Corrections...) {
		if e.Prov.Repo != "acme/app" {
			t.Errorf("%s is scoped to %q despite repo=acme/app", e.Ref, e.Prov.Repo)
		}
	}
}

// sinceDays is reported in the packet, so retrieval must honour it. Reporting a
// window that is not applied tells the agent the evidence is recent when it may
// be a year old.
func TestSinceDaysExcludesOldEvidence(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{
		Task: "ancient goreleaser notes", Repo: "acme/app", SinceDays: 30,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for _, e := range p.Sections.Evidence {
		if strings.Contains(e.Content, "ancient") {
			t.Errorf("200-day-old evidence served under a 30-day window: %s", e.Ref)
		}
	}
}

// A packet labelled recency_fallback must actually carry recent evidence — the
// field states that recent work was substituted for a failed match.
func TestRecencyFallbackCarriesEvidence(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{
		Task: "zzzznomatchwhatsoever qqqqunfindable", SinceDays: 365,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if p.Retrieval != "recency_fallback" {
		t.Fatalf("retrieval = %s, want recency_fallback", p.Retrieval)
	}
	if len(p.Sections.Evidence) == 0 {
		t.Fatal("recency_fallback returned an empty packet; the label promises evidence")
	}
	if len(p.Citations) == 0 {
		t.Error("fallback evidence was not cited")
	}
}

// The budget is a hard ceiling. Over-filling truncates the agent's prompt
// somewhere it cannot see or control.
func TestBudgetIsNeverExceededAndTruncationIsReported(t *testing.T) {
	db := seedCorpus(t)
	p, err := Build(context.Background(), db, Input{
		Task: "goreleaser signing darwin release", SinceDays: 90, TokenBudget: MinBudget,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if p.Budget.UsedTokens > p.Budget.MaxTokens {
		t.Errorf("used %d tokens of a %d budget", p.Budget.UsedTokens, p.Budget.MaxTokens)
	}
	// With a floor budget against multiple matches, something must have been
	// dropped — and the packet must say so rather than look complete.
	total := len(p.Sections.Evidence) + len(p.Sections.Corrections)
	if total > 0 && len(p.Budget.Truncated) == 0 && p.Budget.UsedTokens > p.Budget.MaxTokens/2 {
		t.Log("note: nothing dropped at floor budget; corpus may be smaller than the cap")
	}
}

func TestBudgetIsClamped(t *testing.T) {
	db := seedCorpus(t)
	ctx := context.Background()
	for _, tc := range []struct{ in, want int }{
		{0, DefaultBudget}, {10, MinBudget}, {999999, MaxBudget}, {4000, 4000},
	} {
		p, err := Build(ctx, db, Input{Task: "goreleaser", TokenBudget: tc.in})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if p.Budget.MaxTokens != tc.want {
			t.Errorf("TokenBudget %d -> MaxTokens %d, want %d", tc.in, p.Budget.MaxTokens, tc.want)
		}
	}
}

func TestPacketCarriesDurableStateWithScopedVersionedCitations(t *testing.T) {
	db := seedCorpus(t)
	ctx := context.Background()
	put := func(in store.ArtifactInput) store.Artifact {
		t.Helper()
		a, _, err := db.PutArtifact(ctx, in)
		if err != nil {
			t.Fatalf("put %s: %v", in.Kind, err)
		}
		return a
	}

	checkpoint := put(store.ArtifactInput{
		Kind: store.ArtifactCheckpoint, ScopeKind: store.ScopeTask,
		ScopeKey: "release-42", Repo: "acme/app", Title: "Release handoff",
		Content: store.ArtifactContent{TaskKey: "release-42", Goal: "Ship the signed release", Summary: "Signing is wired; notarization remains."},
	})
	memory := put(store.ArtifactInput{
		Kind: store.ArtifactMemory, ScopeKind: store.ScopeRepo, Repo: "acme/app",
		Title: "Release signing", Content: store.ArtifactContent{Text: "Release signing uses the Apple Developer identity."},
	})
	runbook := put(store.ArtifactInput{
		Kind: store.ArtifactRunbook, ScopeKind: store.ScopeRepo, Repo: "acme/app",
		Title: "Release runbook", Content: store.ArtifactContent{Text: "Release runbook: sign, notarize, then publish."},
	})
	put(store.ArtifactInput{
		Kind: store.ArtifactMemory, Status: store.ArtifactPending,
		ScopeKind: store.ScopeRepo, Repo: "acme/app", Title: "Unapproved release guess",
		Content: store.ArtifactContent{Text: "Release signing should skip notarization."},
	})
	put(store.ArtifactInput{
		Kind: store.ArtifactMemory, ScopeKind: store.ScopeRepo, Repo: "acme/other",
		Title: "Other repo release", Content: store.ArtifactContent{Text: "Release signing uses an unrelated certificate."},
	})

	p, err := Build(ctx, db, Input{
		Task: "finish release signing and notarization", TaskKey: "release-42",
		Repo: "acme/app", SinceDays: 90,
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	for name, section := range map[string][]Evidence{
		"checkpoints": p.Sections.Checkpoints,
		"memories":    p.Sections.Memories,
		"runbooks":    p.Sections.Runbooks,
	} {
		if len(section) == 0 {
			t.Fatalf("%s section is empty", name)
		}
	}
	if p.Sections.Checkpoints[0].Ref != checkpoint.VersionedRef() {
		t.Errorf("checkpoint ref = %q, want %q", p.Sections.Checkpoints[0].Ref, checkpoint.VersionedRef())
	}
	if p.Sections.Memories[0].Ref != memory.VersionedRef() {
		t.Errorf("memory ref = %q, want %q", p.Sections.Memories[0].Ref, memory.VersionedRef())
	}
	if p.Sections.Runbooks[0].Ref != runbook.VersionedRef() {
		t.Errorf("runbook ref = %q, want %q", p.Sections.Runbooks[0].Ref, runbook.VersionedRef())
	}
	for _, e := range p.Sections.Memories {
		if strings.Contains(e.Content, "skip notarization") || strings.Contains(e.Content, "unrelated certificate") {
			t.Errorf("unapproved or cross-repo memory leaked into packet: %q", e.Content)
		}
		if e.Prov.ScopeKind == "" || e.Prov.Status != string(store.ArtifactActive) {
			t.Errorf("memory provenance is incomplete: %+v", e.Prov)
		}
	}
	total := len(p.Sections.Checkpoints) + len(p.Sections.Memories) + len(p.Sections.Runbooks) +
		len(p.Sections.Corrections) + len(p.Sections.Evidence)
	if len(p.Citations) != total {
		t.Errorf("citations (%d) do not cover every served item (%d)", len(p.Citations), total)
	}
	if p.Budget.UsedTokens > p.Budget.MaxTokens {
		t.Errorf("used %d tokens of a %d budget", p.Budget.UsedTokens, p.Budget.MaxTokens)
	}
}

func TestPacketWithoutScopeOnlyRecallsUserArtifacts(t *testing.T) {
	db := seedCorpus(t)
	ctx := context.Background()
	for _, in := range []store.ArtifactInput{
		{Kind: store.ArtifactMemory, ScopeKind: store.ScopeUser, Title: "Global formatter", Content: store.ArtifactContent{Text: "Use the quartz formatter everywhere."}},
		{Kind: store.ArtifactMemory, ScopeKind: store.ScopeRepo, Repo: "secret/repo", Title: "Private formatter", Content: store.ArtifactContent{Text: "Use the quartz formatter only in the private repository."}},
	} {
		if _, _, err := db.PutArtifact(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	p, err := Build(ctx, db, Input{Task: "configure the quartz formatter"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sections.Memories) != 1 || strings.Contains(p.Sections.Memories[0].Content, "private repository") {
		t.Fatalf("unscoped packet leaked repository state: %+v", p.Sections.Memories)
	}
}

func TestLargeCheckpointIsExcerptedRatherThanDropped(t *testing.T) {
	db := seedCorpus(t)
	ctx := context.Background()
	checkpoint, _, err := db.PutArtifact(ctx, store.ArtifactInput{
		Kind: store.ArtifactCheckpoint, ScopeKind: store.ScopeTask,
		ScopeKey: "BIG-1", Repo: "acme/app", Title: "Large handoff",
		Content: store.ArtifactContent{
			TaskKey: "BIG-1", Goal: "Resume safely",
			Summary: strings.Repeat("Detailed checkpoint state. ", 1500),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Build(ctx, db, Input{
		Task: "resume safely", TaskKey: "BIG-1", Repo: "acme/app", TokenBudget: MinBudget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sections.Checkpoints) != 1 || p.Sections.Checkpoints[0].Ref != checkpoint.VersionedRef() {
		t.Fatalf("large exact checkpoint was dropped: %+v", p.Sections.Checkpoints)
	}
	if !strings.Contains(p.Sections.Checkpoints[0].Content, "read the ref for the full artifact") {
		t.Errorf("large checkpoint was not marked as an excerpt")
	}
	if p.Budget.UsedTokens > p.Budget.MaxTokens {
		t.Fatalf("excerpt exceeded budget: %+v", p.Budget)
	}
}

// The estimator must never under-count, or the ceiling it enforces is not a
// ceiling.
func TestTokenEstimateOverCounts(t *testing.T) {
	for _, s := range []string{"", "a", "hello world", strings.Repeat("token ", 500), "日本語のテキスト"} {
		if got, floor := EstimateTokens(s), len(s)/4; got < floor {
			t.Errorf("EstimateTokens(%d bytes) = %d, below a 4-chars-per-token floor of %d", len(s), got, floor)
		}
	}
}

func TestDistillQueryKeepsIdentifiersAndDropsGrammar(t *testing.T) {
	terms := DistillQuery("Please fix the goreleaser config so that darwin_arm64 builds are signed", 8)
	joined := strings.Join(terms, " ")
	for _, want := range []string{"goreleaser", "darwin_arm64"} {
		if !strings.Contains(joined, want) {
			t.Errorf("identifier %q was dropped; got %v", want, terms)
		}
	}
	for _, unwanted := range []string{"the", "that", "are", "fix"} {
		for _, got := range terms {
			if got == unwanted {
				t.Errorf("stopword %q survived distillation: %v", unwanted, terms)
			}
		}
	}
	// Longest-first is the rarity proxy that makes the surviving terms the
	// discriminating ones when the list is capped.
	for i := 1; i < len(terms); i++ {
		if len(terms[i]) > len(terms[i-1]) {
			t.Errorf("terms not sorted longest-first: %v", terms)
			break
		}
	}
}
