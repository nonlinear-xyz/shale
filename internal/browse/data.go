package browse

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/render"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
)

// ── messages ─────────────────────────────────────────────────────────────────

// searchDoneMsg carries a result set back with the query serial it was issued
// under. Keystrokes fire queries faster than SQLite answers them, so an older
// query can land after a newer one; the serial is what lets Update drop the
// stale answer instead of showing results for a prefix the user has moved past.
type searchDoneMsg struct {
	serial int
	hits   []store.ChunkHit
	infos  map[int64]store.SessionInfo
	err    error
}

type sessionsMsg struct {
	items []store.SessionInfo
	err   error
}

// transcriptMsg carries the SEGMENTS rather than rendered text. The reader has
// to re-wrap on every resize, and a pre-rendered string cannot be re-wrapped —
// only re-fetched, which would mean hitting the store on every drag of the
// window edge.
type transcriptMsg struct {
	ref       string
	segs      []sessions.Segment
	focusLine int
	err       error
}

type reposMsg struct {
	repos []discover.Repo
	err   error
}

type statsMsg struct {
	stats store.Stats
	err   error
}

// ── commands ─────────────────────────────────────────────────────────────────

func searchCmd(ctx context.Context, db *store.DB, query string, serial int) tea.Cmd {
	return func() tea.Msg {
		if strings.TrimSpace(query) == "" {
			return searchDoneMsg{serial: serial}
		}
		hits, err := db.SearchChunks(ctx, query, "", "", 0, 60)
		if err != nil {
			// A malformed FTS query (an unbalanced quote mid-typing, say) is the
			// normal case here, not a fault — report it in the pane and keep going.
			return searchDoneMsg{serial: serial, err: err}
		}
		seqs := make([]int64, 0, len(hits))
		for _, h := range hits {
			seqs = append(seqs, h.EventSeq)
		}
		infos, err := db.Sessions(ctx, seqs)
		return searchDoneMsg{serial: serial, hits: hits, infos: infos, err: err}
	}
}

func sessionsCmd(ctx context.Context, db *store.DB) tea.Cmd {
	return func() tea.Msg {
		items, err := db.RecentSessions(ctx, "", 200)
		return sessionsMsg{items: items, err: err}
	}
}

func statsCmd(ctx context.Context, db *store.DB) tea.Cmd {
	return func() tea.Msg {
		st, err := db.Stats(ctx)
		return statsMsg{stats: st, err: err}
	}
}

// reposCmd rescans the filesystem. Slow by nature — it shells out to git once
// per candidate directory — so it runs off the event loop behind a spinner.
func reposCmd(roots []string, depth int) tea.Cmd {
	return func() tea.Msg {
		if len(roots) == 0 {
			return reposMsg{}
		}
		return reposMsg{repos: discover.Discover(roots, depth)}
	}
}

// transcriptCmd loads and renders the passage a ref names.
//
// It renders the WHOLE session and reports the ref's line window separately,
// so the preview can be scrolled out of the matched passage into its context.
// A preview clipped to the chunk would answer "what matched" and refuse the
// obvious next question, "what happened around it".
func transcriptCmd(ctx context.Context, db *store.DB, ref store.Ref, focusLine int) tea.Cmd {
	return func() tea.Msg {
		label := ref.String()
		info, err := db.Session(ctx, ref.EventSeq)
		if err != nil {
			return transcriptMsg{ref: label, err: err}
		}
		blob, err := db.ReadBlob(info.ContentHash)
		if err != nil {
			return transcriptMsg{ref: label, err: fmt.Errorf(
				"the event is in the log but its transcript is not on disk: %w", err)}
		}
		return transcriptMsg{
			ref:       label,
			segs:      sessions.Segments(sessions.Source(info.Source), blob),
			focusLine: focusLine,
		}
	}
}

// Column geometry for the reader: "▸ " marker, a 5-wide line-number gutter, and
// a 9-wide segment label, each followed by a space.
const readerGutter = 2 + 5 + 1 + 9 + 1

// renderTranscript lays the segments out at a given width and reports which
// display row the focus line landed on, so the caller can scroll to it.
//
// Wrapping happens here rather than being left to the viewport, which clips
// instead. A reader that silently cuts the right-hand half of a command is
// worse than no reader — you cannot tell a truncated line from a short one.
func renderTranscript(th *ui.Theme, segs []sessions.Segment, focusLine, width int) (string, int) {
	body := width - readerGutter
	if body < 20 {
		body = 20
	}
	pad := strings.Repeat(" ", readerGutter)

	var b strings.Builder
	row, focusRow := 0, 0
	for _, s := range segs {
		marker := "  "
		if focusLine > 0 && s.LineNo == focusLine {
			marker = th.Ref.Render("▸ ")
			focusRow = row
		}
		text := wrap(strings.TrimRight(s.Text, "\n"), body)
		lines := strings.Split(text, "\n")
		for i, ln := range lines {
			if i == 0 {
				fmt.Fprintf(&b, "%s%s %s %s\n",
					marker,
					th.Gutter.Render(fmt.Sprintf("%5d", s.LineNo)),
					render.SegmentLabelStyle(th, s.Kind).Render(fmt.Sprintf("%-9s", render.SegmentLabel(s.Kind))),
					ln)
				row++
				continue
			}
			// Continuation lines sit under the body column, not under the gutter,
			// so a wrapped tool output stays one visually contiguous block.
			fmt.Fprintf(&b, "%s%s\n", pad, ln)
			row++
		}
	}
	return b.String(), focusRow
}

// wrap soft-wraps to width. lipgloss breaks on word boundaries and hard-breaks
// anything longer than the pane — a base64 blob or a 300-character path, both
// of which are common in a transcript.
func wrap(s string, width int) string {
	return lipgloss.NewStyle().Width(width).Render(s)
}

// ── list items ───────────────────────────────────────────────────────────────

// hitItem is one search result.
type hitItem struct {
	hit   store.ChunkHit
	title string
}

func (i hitItem) Title() string { return i.title }

func (i hitItem) Description() string {
	scope := i.hit.Scope
	if scope == "" {
		scope = "—"
	}
	facts := []string{i.hit.Source, scope, shortStamp(i.hit.OccurredAt), i.hit.Ref()}
	if i.hit.Kind == string(sessions.ChunkError) {
		facts = append(facts, "error")
	}
	return strings.Join(facts, " · ")
}

// FilterValue includes the body so the list's own `/` filter narrows within a
// result set — a second, cheaper pass over what FTS already returned.
func (i hitItem) FilterValue() string { return i.title + " " + i.hit.Scope + " " + i.hit.Excerpt }

func (i hitItem) ref() store.Ref {
	return store.Ref{EventSeq: i.hit.EventSeq, ChunkIndex: i.hit.ChunkIndex, HasChunk: true}
}

// sessionItem is one captured session.
type sessionItem struct{ info store.SessionInfo }

func (i sessionItem) Title() string {
	if t := strings.TrimSpace(i.info.Record.Title); t != "" {
		return oneLine(t, 100)
	}
	return "(untitled session)"
}

func (i sessionItem) Description() string {
	scope := i.info.Scope
	if scope == "" {
		scope = "—"
	}
	facts := []string{i.info.Source, scope, shortStamp(i.info.OccurredAt)}
	if i.info.Record.Turns > 0 {
		facts = append(facts, render.CountPhrase(i.info.Record.Turns, "turn", "turns"))
	}
	facts = append(facts, fmt.Sprintf("session:%d", i.info.Seq))
	return strings.Join(facts, " · ")
}

func (i sessionItem) FilterValue() string {
	return i.info.Record.Title + " " + i.info.Scope + " " + i.info.Record.Digest
}

func (i sessionItem) ref() store.Ref {
	return store.Ref{EventSeq: i.info.Seq}
}

// repoItem is one discovered repository, selectable or skipped.
type repoItem struct{ repo discover.Repo }

func (i repoItem) Title() string { return i.repo.Name() }

func (i repoItem) Description() string {
	if !i.repo.Selectable() {
		reason := string(i.repo.SkipReason)
		if i.repo.SkipDetail != "" {
			reason = fmt.Sprintf("%s (%s)", reason, i.repo.SkipDetail)
		}
		return "skipped — " + reason
	}
	return fmt.Sprintf("%s · %s · %s",
		render.CountPhrase(i.repo.CommitCount, "commit", "commits"),
		render.RelativeTime(i.repo.LastCommitAt),
		i.repo.Path)
}

func (i repoItem) FilterValue() string { return i.repo.Name() + " " + i.repo.Path }

// toItems adapts a typed slice to the list's interface.
func toItems[T list.Item](in []T) []list.Item {
	out := make([]list.Item, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func shortStamp(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + " …"
}
