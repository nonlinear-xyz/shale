package browse

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/nonlinear-xyz/shale/internal/render"
)

// Chrome the layout must account for. A frame costs two columns of border plus
// two of padding, and two rows of border.
const (
	frameW = 4
	frameH = 2

	// Below this width the two panes stop being readable side by side and the
	// reader takes the whole screen instead, reached with Enter.
	splitMinWidth = 96
)

// layout resizes every component to the current terminal. Called on every
// WindowSizeMsg — components are sized imperatively in Bubble Tea, so anything
// not resized here silently keeps drawing at its old size.
func (m *Model) layout() {
	bodyH := m.h - m.chromeHeight()
	if bodyH < 3 {
		bodyH = 3
	}

	m.input.Width = m.w - 14

	listW, previewW := m.splitWidths()
	m.hits.SetSize(listW-frameW, bodyH-frameH)
	m.sessions.SetSize(listW-frameW, bodyH-frameH)
	m.repos.SetSize(listW-frameW, bodyH-frameH)

	if !m.split() {
		// Stacked: whichever pane is showing gets the full width.
		previewW = m.w
	}
	m.preview.Width = previewW - frameW
	m.preview.Height = bodyH - frameH
	m.picker.SetHeight(bodyH - frameH)

	// The reader is width-sensitive: its content is wrapped text, so a resize
	// changes what it says, not just how much of it fits.
	m.renderPreview(false)
}

// split reports whether the list and reader are shown side by side.
func (m Model) split() bool { return m.w >= splitMinWidth }

func (m Model) splitWidths() (list, preview int) {
	if !m.split() {
		return m.w, m.w
	}
	list = m.w * 45 / 100
	if list < 34 {
		list = 34
	}
	return list, m.w - list
}

// chromeHeight is the number of rows the body does NOT get.
func (m Model) chromeHeight() int {
	h := 3 // tab bar, rule, status bar
	if m.tab == tabSearch {
		h++ // the query input
	}
	return h
}

func (m Model) View() string {
	if m.w == 0 {
		return "" // pre-first-resize; drawing now would flash a broken frame
	}

	var b strings.Builder
	b.WriteString(m.tabBar())
	b.WriteString("\n")
	b.WriteString(m.th.Rule(m.w))
	b.WriteString("\n")

	if m.tab == tabSearch {
		b.WriteString(m.input.View())
		b.WriteString("\n")
	}

	b.WriteString(m.body())
	b.WriteString("\n")
	b.WriteString(m.statusBar())
	return b.String()
}

// tabBar draws the tabs. Bubbles has no tab component, so this is Lipgloss
// padding plus the model's own enum — which is all a tab bar has ever been.
func (m Model) tabBar() string {
	cells := make([]string, 0, numTabs)
	for i, name := range tabNames {
		if tab(i) == m.tab {
			cells = append(cells, m.th.TabActive.Render(name))
			continue
		}
		cells = append(cells, m.th.TabInactive.Render(name))
	}
	bar := lipgloss.JoinHorizontal(lipgloss.Top, cells...)
	brand := m.th.Ref.Render(" shale ")
	gap := m.w - lipgloss.Width(bar) - lipgloss.Width(brand)
	if gap < 0 {
		gap = 0
	}
	return bar + strings.Repeat(" ", gap) + brand
}

func (m Model) body() string {
	bodyH := m.h - m.chromeHeight()
	if bodyH < 3 {
		bodyH = 3
	}

	if m.tab == tabStatus {
		return m.frame(m.statusPane(), m.w, bodyH, false)
	}

	listW, previewW := m.splitWidths()

	// The reader replaces the list entirely on a narrow terminal.
	if !m.split() {
		if m.focus == focusPreview {
			return m.frame(m.preview.View(), m.w, bodyH, true)
		}
		if m.focus == focusPicker {
			return m.frame(m.picker.View(), m.w, bodyH, true)
		}
		return m.frame(m.listView(), m.w, bodyH, true)
	}

	left := m.frame(m.listView(), listW, bodyH, m.focus == focusList)

	var right string
	switch {
	case m.focus == focusPicker:
		right = m.frame(m.picker.View(), previewW, bodyH, true)
	case m.tab == tabRepos:
		right = m.frame(m.reposHelp(), previewW, bodyH, false)
	default:
		right = m.frame(m.preview.View(), previewW, bodyH, m.focus == focusPreview)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
}

func (m Model) listView() string {
	switch m.tab {
	case tabSearch:
		if strings.TrimSpace(m.input.Value()) == "" {
			return m.th.Hint.Render(
				"Type to search.\n\n" +
					"Search is lexical — exact identifiers, file names and error\n" +
					"messages work best. Quoted \"phrases\" and OR both work.")
		}
		if m.searching && len(m.hits.Items()) == 0 {
			return m.spin.View() + m.th.Hint.Render(" searching…")
		}
		if m.err != nil {
			return m.th.Danger.Render(m.err.Error())
		}
		return m.hits.View()
	case tabSessions:
		return m.sessions.View()
	case tabRepos:
		if m.scanning {
			return m.spin.View() + m.th.Hint.Render(" scanning for git repositories…")
		}
		if len(m.repos.Items()) == 0 {
			return m.th.Hint.Render("No repositories scanned yet. Press r to scan, a to add a root.")
		}
		return m.repos.View()
	}
	return ""
}

// reposHelp fills the right pane on the Repos tab, which has no reader.
//
// It states what the tab is FOR: the repos list is the trust surface, the place
// a cautious user checks what shale can see before letting it capture anything.
func (m Model) reposHelp() string {
	var b strings.Builder
	b.WriteString(m.th.Title.Render("What shale can see") + "\n\n")
	b.WriteString(m.th.Body.Render(
		"Every git repository found under your scan roots, including\nthe ones discovery decided to skip and why.\n\n"))
	b.WriteString(m.th.Facts.Render("Scanning is read-only and makes no network calls.\n\n"))

	b.WriteString(m.th.Header.Render("ROOTS") + "\n")
	if len(m.roots) == 0 {
		b.WriteString(m.th.Hint.Render("  none — press a to add one") + "\n")
	}
	for _, r := range m.roots {
		b.WriteString(m.th.Facts.Render("  "+r) + "\n")
	}

	if it, ok := m.repos.SelectedItem().(repoItem); ok {
		b.WriteString("\n" + m.th.Header.Render("SELECTED") + "\n")
		b.WriteString(m.th.Title.Render("  "+it.repo.Name()) + "\n")
		b.WriteString(m.th.Facts.Render("  "+it.repo.Path) + "\n")
		if !it.repo.Selectable() {
			b.WriteString(m.th.Warn.Render("  skipped — "+string(it.repo.SkipReason)) + "\n")
		} else {
			b.WriteString(m.th.Success.Render(fmt.Sprintf("  %s · last active %s",
				render.CountPhrase(it.repo.CommitCount, "commit", "commits"),
				render.RelativeTime(it.repo.LastCommitAt))) + "\n")
		}
	}
	return b.String()
}

func (m Model) statusPane() string {
	var b strings.Builder
	b.WriteString(m.th.Title.Render("Store") + "\n\n")

	rows := [][2]string{
		{"store", m.storeDir},
		{"sessions", fmt.Sprint(m.stats.Sessions)},
		{"repositories", fmt.Sprint(m.stats.Repos)},
		{"indexed chunks", fmt.Sprint(m.stats.Chunks)},
	}
	if m.stats.OldestAt != "" {
		rows = append(rows, [2]string{"span", m.stats.OldestAt + " → " + m.stats.NewestAt})
	}
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("  %s  %s\n",
			m.th.Facts.Render(fmt.Sprintf("%-15s", r[0])),
			m.th.Body.Render(r[1])))
	}

	if m.stats.Sessions == 0 {
		b.WriteString("\n" + m.th.Warn.Render("Nothing captured yet. Run `shale watch`.") + "\n")
		b.WriteString(m.th.Hint.Render("Sessions must be idle for 30 minutes before they are swept.") + "\n")
	}
	return b.String()
}

// frame wraps a pane in a border that brightens when it has focus, so "where do
// my arrow keys go" is answerable without pressing one.
func (m Model) frame(content string, width, height int, focused bool) string {
	style := m.th.Frame
	if focused {
		style = m.th.FrameFocus
	}
	return style.Width(width - 2).Height(height - frameH).MaxHeight(height).Render(content)
}

func (m Model) statusBar() string {
	var left string
	switch {
	case m.err != nil:
		left = m.th.Danger.Render(m.err.Error())
	case m.msg != "":
		left = m.th.Success.Render(m.msg)
	case m.previewRef != "" && m.tab != tabRepos && m.tab != tabStatus:
		// The ref is the export from this surface: it is what you paste into
		// `shale show`, a commit message, or an agent prompt. Keep it on screen.
		left = m.th.Ref.Render("shale show " + m.previewRef)
	}

	right := m.th.StatusBar.Render(strings.Join(m.keyHints(), "  "))
	gap := m.w - lipgloss.Width(left) - lipgloss.Width(right) - 1
	if gap < 1 {
		gap = 1
		right = ""
	}
	return " " + left + strings.Repeat(" ", gap) + right
}

func (m Model) keyHints() []string {
	key := func(k, what string) string { return m.th.Key.Render(k) + " " + what }

	hints := []string{key("tab", "pane")}
	switch {
	case m.focus == focusPicker:
		return []string{key("↑↓", "move"), key("→", "open"), key("enter", "add root"), key("esc", "cancel")}
	case m.focus == focusPreview:
		hints = append(hints, key("↑↓", "scroll"), key("esc", "back"))
	case m.tab == tabSearch:
		hints = append(hints, key("↑↓", "results"), key("enter", "read"), key("esc", "clear"))
	case m.tab == tabSessions:
		hints = append(hints, key("↑↓", "move"), key("enter", "read"), key("/", "filter"))
	case m.tab == tabRepos:
		hints = append(hints, key("r", "rescan"), key("a", "add root"), key("/", "filter"))
	}
	return append(hints, key("ctrl+c", "quit"))
}
