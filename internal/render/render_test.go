package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
)

// The contract this file exists to defend: shale is a tool people pipe. Adding
// color must not change one byte of what `shale search | grep` sees. Every test
// below renders through ui.Plain() — the theme the ascii profile produces — and
// asserts the output is what it was before styling existed.

const esc = "\x1b"

func TestPlainThemeEmitsNoEscapeSequences(t *testing.T) {
	th := ui.Plain()
	now := time.Now().Add(-3 * time.Hour)

	cases := map[string]func(*bytes.Buffer){
		"RepoTable": func(b *bytes.Buffer) {
			RepoTable(b, th, sampleRepos(&now), false)
		},
		"RepoTable/all": func(b *bytes.Buffer) {
			RepoTable(b, th, sampleRepos(&now), true)
		},
		"SearchResults": func(b *bytes.Buffer) {
			SearchResults(b, th, "worktree isolation", sampleHits(), sampleInfos())
		},
		"Transcript": func(b *bytes.Buffer) {
			Transcript(b, th, sampleInfos()[7], sampleSegments(), 0, 0, 3)
		},
		"Transcript/clipped": func(b *bytes.Buffer) {
			Transcript(b, th, sampleInfos()[7], sampleSegments(), 2, 2, 3)
		},
		"StatusTable": func(b *bytes.Buffer) {
			StatusTable(b, th, [][2]string{{"store", "/home/u/.shale"}, {"sessions", "12"}})
		},
	}

	for name, draw := range cases {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			draw(&b)
			if strings.Contains(b.String(), esc) {
				t.Errorf("plain theme emitted an ANSI escape sequence:\n%q", b.String())
			}
		})
	}
}

// TestColorThemeDoesNotShearColumns is the tabwriter regression. text/tabwriter
// measured cells by counting runes, so a styled cell measured wider than it drew
// and every column to its right shifted. The table here measures display width,
// so the rendered column positions must match the plain ones exactly.
func TestColorThemeDoesNotShearColumns(t *testing.T) {
	now := time.Now().Add(-3 * time.Hour)
	repos := sampleRepos(&now)

	var plain, colored bytes.Buffer
	RepoTable(&plain, ui.Plain(), repos, true)
	RepoTable(&colored, forcedColorTheme(t), repos, true)

	if !strings.Contains(colored.String(), esc) {
		t.Fatal("forced-color theme emitted no escapes; the test is not exercising styling")
	}

	plainLines := strings.Split(strings.TrimRight(plain.String(), "\n"), "\n")
	colorLines := strings.Split(strings.TrimRight(colored.String(), "\n"), "\n")
	if len(plainLines) != len(colorLines) {
		t.Fatalf("line count differs: plain %d, colored %d", len(plainLines), len(colorLines))
	}
	for i := range plainLines {
		// lipgloss.Width ignores escape sequences, so a styled row must occupy
		// exactly the columns its plain twin does.
		if got, want := lipgloss.Width(colorLines[i]), lipgloss.Width(plainLines[i]); got != want {
			t.Errorf("line %d display width %d, want %d\n plain: %q\ncolored: %q",
				i, got, want, plainLines[i], colorLines[i])
		}
	}
}

// TestSearchResultsCarryFollowableRef guards the property the search surface is
// built around: every hit prints the command that reads it.
func TestSearchResultsCarryFollowableRef(t *testing.T) {
	var b bytes.Buffer
	SearchResults(&b, ui.Plain(), "worktree", sampleHits(), sampleInfos())
	out := b.String()

	for _, h := range sampleHits() {
		want := "shale show " + h.Ref()
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
		if _, err := store.ParseRef(h.Ref()); err != nil {
			t.Errorf("printed ref %q does not parse back: %v", h.Ref(), err)
		}
	}
}

// TestHighlightPreservesText: styling a match must not add, drop or reorder a
// single character — an excerpt is evidence, and evidence that got rewritten on
// the way to the screen is not evidence.
func TestHighlightPreservesText(t *testing.T) {
	th := ui.Plain()
	cases := []struct {
		text  string
		terms []string
	}{
		{"the worktree is isolated", []string{"worktree"}},
		{"WORKTREE and worktree and WorkTree", []string{"worktree"}},
		{"naïve — em dash, ümlaut, 日本語", []string{"dash"}},
		{"no terms at all", nil},
		{"overlapping worktreeworktree", []string{"worktree", "work"}},
		{"", []string{"x"}},
	}
	for _, c := range cases {
		if got := highlight(th, c.text, c.terms); got != c.text {
			t.Errorf("highlight(%q, %v) = %q, want the text unchanged", c.text, c.terms, got)
		}
	}
}

// FTS operators are syntax, not terms, and single characters are dropped too:
// highlighting every "a" in an excerpt is noise, not evidence.
func TestQueryTermsDropsOperators(t *testing.T) {
	got := queryTerms(`"exact phrase" OR worktree AND a b*`)
	want := []string{"exact", "phrase", "worktree"}
	if len(got) != len(want) {
		t.Fatalf("queryTerms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("queryTerms = %v, want %v", got, want)
		}
	}
}

func TestCountPhrase(t *testing.T) {
	if got := CountPhrase(1, "repository", "repositories"); got != "1 repository" {
		t.Errorf("got %q", got)
	}
	if got := CountPhrase(0, "match", "matches"); got != "0 matches" {
		t.Errorf("got %q", got)
	}
}

// ── fixtures ─────────────────────────────────────────────────────────────────

// forcedColorTheme builds a theme that emits escapes regardless of environment,
// so the shear test exercises styling even in CI where stdout is a pipe.
func forcedColorTheme(t *testing.T) *ui.Theme {
	t.Helper()
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	return ui.New(&bytes.Buffer{})
}

func sampleRepos(now *time.Time) []discover.Repo {
	return []discover.Repo{
		{Path: "/home/u/src/shale", Remote: "nonlinear-xyz/shale", CommitCount: 142, LastCommitAt: now},
		{Path: "/home/u/src/a-much-longer-repository-name", CommitCount: 7, LastCommitAt: now},
		{Path: "/home/u/vendor/corpus", Skipped: true,
			SkipReason: discover.SkipReason("no commits")},
		{Path: "/home/u/src/shale-wt", Skipped: true,
			SkipReason: discover.SkipReason("worktree"), SkipDetail: "of nonlinear-xyz/shale"},
	}
}

func sampleHits() []store.ChunkHit {
	return []store.ChunkHit{
		{EventSeq: 7, ChunkIndex: 3, LineStart: 120, LineEnd: 180, Kind: "transcript",
			Source: "claude", Scope: "nonlinear-xyz/shale", OccurredAt: "2026-08-09T11:00:00Z",
			Excerpt: "the worktree is isolated from the main checkout"},
		{EventSeq: 9, ChunkIndex: 0, LineStart: 1, LineEnd: 60, Kind: string(sessions.ChunkError),
			Source: "codex", Scope: "", OccurredAt: "2026-08-01T09:30:00Z",
			Excerpt: "fatal: worktree already exists"},
	}
}

func sampleInfos() map[int64]store.SessionInfo {
	return map[int64]store.SessionInfo{
		7: {Seq: 7, Source: "claude", Scope: "nonlinear-xyz/shale", OccurredAt: "2026-08-09T11:00:00Z",
			Record: store.SessionRecord{Title: "fixing worktree isolation", Turns: 14}},
		9: {Seq: 9, Source: "codex", OccurredAt: "2026-08-01T09:30:00Z",
			Record: store.SessionRecord{Title: "", Turns: 3}},
	}
}

func sampleSegments() []sessions.Segment {
	return []sessions.Segment{
		{LineNo: 1, Kind: sessions.SegUser, Text: "add a worktree"},
		{LineNo: 2, Kind: sessions.SegToolUse, Text: "git worktree add ../wt"},
		{LineNo: 3, Kind: sessions.SegToolError, Text: "fatal: already exists"},
	}
}
