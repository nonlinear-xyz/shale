package browse

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
)

// Bubble Tea models are testable without a terminal: Update is a pure function
// from (model, msg) to (model, cmd), and View is a pure function to a string.
// These exercise the parts that break silently in a TUI — layout arithmetic at
// hostile sizes, and key routing, where a key claimed by the wrong component
// looks like a dead keyboard rather than a crash.

func newTestModel(t *testing.T) Model {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, _, err = db.PutSession(context.Background(), "aaa", store.SessionRecord{
		Source: "claude_code", SourceKey: "aaa", Title: "fixing worktree isolation",
		Digest: "the worktree is isolated", Repo: "acme/shale", EndedAt: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	return New(context.Background(), db, ui.Plain(), t.TempDir(), []string{t.TempDir()}, 3)
}

func size(m Model, w, h int) Model {
	next, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return next.(Model)
}

func key(m Model, k string) Model {
	var msg tea.KeyMsg
	switch k {
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		msg = tea.KeyMsg{Type: tea.KeyShiftTab}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEscape}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
	next, _ := m.Update(msg)
	return next.(Model)
}

// TestViewSurvivesHostileSizes: a TUI that panics on a small terminal is a TUI
// that panics in a split pane, which is where people actually run them.
func TestViewSurvivesHostileSizes(t *testing.T) {
	sizes := [][2]int{
		{0, 0},    // before the first resize
		{1, 1},    // absurd but reachable while dragging a divider
		{20, 4},   // narrower than the tab bar
		{80, 24},  // classic, below the split threshold
		{96, 30},  // exactly at the split threshold
		{200, 60}, // wide
		{40, 3},   // shorter than the chrome
	}
	for _, s := range sizes {
		for tabIdx := tab(0); tabIdx < numTabs; tabIdx++ {
			m := newTestModel(t)
			m.tab = tabIdx
			m = size(m, s[0], s[1])
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("View panicked at %dx%d on tab %s: %v",
							s[0], s[1], tabNames[tabIdx], r)
					}
				}()
				_ = m.View()
			}()
		}
	}
}

// TestViewNeverExceedsTerminalWidth: a line wider than the terminal wraps, and
// a wrapped line pushes the whole layout down by a row on every redraw.
func TestViewNeverExceedsTerminalWidth(t *testing.T) {
	for _, w := range []int{60, 80, 96, 120, 200} {
		m := size(newTestModel(t), w, 30)
		for tabIdx := tab(0); tabIdx < numTabs; tabIdx++ {
			m.tab = tabIdx
			for i, line := range strings.Split(m.View(), "\n") {
				if got := len([]rune(line)); got > w {
					t.Errorf("tab %s at width %d: line %d is %d cells wide\n%q",
						tabNames[tabIdx], w, i, got, line)
				}
			}
		}
	}
}

// TestTabCyclingWraps in both directions — an off-by-one here strands a tab
// that can only be reached one way round.
func TestTabCyclingWraps(t *testing.T) {
	m := size(newTestModel(t), 120, 30)
	if m.tab != tabSearch {
		t.Fatalf("opens on %v, want Search", m.tab)
	}
	for i := 0; i < int(numTabs); i++ {
		m = key(m, "tab")
	}
	if m.tab != tabSearch {
		t.Errorf("after a full cycle forward, on %v; want back at Search", m.tab)
	}
	m = key(m, "shift+tab")
	if m.tab != numTabs-1 {
		t.Errorf("shift+tab from Search went to %v, want the last tab", m.tab)
	}
}

// TestSearchTabAcceptsTypedLetters is the key-routing regression that matters
// most: "q" quits on every other tab, and must NOT quit while someone is
// typing a query containing it.
func TestSearchTabAcceptsTypedLetters(t *testing.T) {
	m := size(newTestModel(t), 120, 30)
	for _, r := range "query" {
		m = key(m, string(r))
	}
	if got := m.input.Value(); got != "query" {
		t.Errorf("input = %q, want %q — a letter was eaten by a command binding", got, "query")
	}
}

// TestEscapeBacksOutBeforeQuitting: esc unwinds one level at a time. Quitting
// straight out of a preview would discard the query that got you there.
func TestEscapeBacksOutBeforeQuitting(t *testing.T) {
	m := size(newTestModel(t), 120, 30)
	for _, r := range "worktree" {
		m = key(m, string(r))
	}
	m.focus = focusPreview

	m = key(m, "esc")
	if m.focus != focusList {
		t.Fatalf("first esc left focus at %v, want the list", m.focus)
	}
	m = key(m, "esc")
	if m.input.Value() != "" {
		t.Fatalf("second esc left the query as %q, want it cleared", m.input.Value())
	}
}

// TestStaleSearchResultsAreDropped: results arrive out of order because SQLite
// answers at its own pace while the user keeps typing. An answer to an
// abandoned prefix must not overwrite the current one.
func TestStaleSearchResultsAreDropped(t *testing.T) {
	m := size(newTestModel(t), 120, 30)
	m.serial = 7

	stale := searchDoneMsg{
		serial: 3,
		hits:   []store.ChunkHit{{EventSeq: 1, ChunkIndex: 0, Excerpt: "stale"}},
		infos:  map[int64]store.SessionInfo{},
	}
	next, _ := m.Update(stale)
	if n := len(next.(Model).hits.Items()); n != 0 {
		t.Errorf("a stale result set landed: %d items shown", n)
	}

	fresh := stale
	fresh.serial = 7
	next, _ = m.Update(fresh)
	if n := len(next.(Model).hits.Items()); n != 1 {
		t.Errorf("the current result set was dropped: %d items shown, want 1", n)
	}
}

// TestReposSortSelectableFirst — the skipped entries exist to be auditable, not
// to be the first thing you scroll past.
func TestReposSortSelectableFirst(t *testing.T) {
	items := []repoItem{
		{repo: repoFixture("skipped-a", true)},
		{repo: repoFixture("live-a", false)},
		{repo: repoFixture("skipped-b", true)},
		{repo: repoFixture("live-b", false)},
	}
	sortRepoItems(items)

	seenSkip := false
	for _, it := range items {
		if !it.repo.Selectable() {
			seenSkip = true
			continue
		}
		if seenSkip {
			t.Fatalf("selectable repo %q sorted after a skipped one", it.repo.Name())
		}
	}
}

func repoFixture(name string, skipped bool) discover.Repo {
	return discover.Repo{
		Path: "/tmp/" + name, Remote: name, CommitCount: 3,
		Skipped: skipped, SkipReason: discover.SkipReason("no commits"),
	}
}
