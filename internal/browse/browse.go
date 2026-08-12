// Package browse is shale's interactive surface.
//
// It exists because `search` and `show` are the same act performed twice: you
// run a search, read four lines of excerpt, copy a ref, run `show`, discover it
// was the wrong passage, and start again. Every round trip re-pays the cost of
// deciding what to type. This collapses that loop into one screen — query, hits,
// and the transcript around the hit, live.
//
// It does NOT replace those commands. `shale search` stays a print-and-exit
// surface so it can be piped, grepped and scripted; this is the surface for when
// you do not yet know what you are looking for. Both read the same index.
package browse

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/filepicker"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
)

type tab int

const (
	tabSearch tab = iota
	tabSessions
	tabRepos
	tabStatus
	numTabs
)

var tabNames = [numTabs]string{"Search", "Sessions", "Repos", "Status"}

// focus tracks which half of a two-pane tab takes navigation keys.
type focus int

const (
	focusList focus = iota
	focusPreview
	focusPicker
)

// Model is the whole browser. One model rather than four, because the preview
// pane is shared: a session selected on the Sessions tab and a passage found on
// the Search tab open into the same reader.
type Model struct {
	ctx      context.Context
	db       *store.DB
	th       *ui.Theme
	storeDir string
	roots    []string
	depth    int

	tab   tab
	focus focus
	w, h  int

	// Search tab.
	input  textinput.Model
	hits   list.Model
	serial int // query serial; see searchDoneMsg
	inFlight

	// Sessions tab.
	sessions list.Model

	// Repos tab.
	repos    list.Model
	picker   filepicker.Model
	scanning bool

	// Status tab.
	stats store.Stats

	// Shared reader. The segments are held rather than the rendered text so the
	// reader can re-wrap on resize without re-reading the blob.
	preview     viewport.Model
	previewRef  string
	previewSegs []sessions.Segment
	previewLine int // the transcript line the current ref points at, 0 for none
	previewErr  error

	spin spinner.Model
	err  error
	msg  string
}

// inFlight is embedded rather than a bare bool so the searching flag cannot be
// confused with the other booleans on the model.
type inFlight struct{ searching bool }

// New builds the browser. Nothing is loaded yet — Init issues the first fetches
// so the program draws immediately instead of blocking on SQLite.
func New(ctx context.Context, db *store.DB, th *ui.Theme, storeDir string, roots []string, depth int) Model {
	in := textinput.New()
	in.Placeholder = "search your corpus — exact identifiers, file names, error messages"
	in.Prompt = "  search  "
	in.PromptStyle = th.Prompt
	in.PlaceholderStyle = th.Placeholder
	in.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = th.Ref

	pick := filepicker.New()
	pick.DirAllowed = true
	pick.FileAllowed = false
	pick.ShowHidden = false
	if len(roots) > 0 {
		pick.CurrentDirectory = roots[0]
	}

	return Model{
		ctx:      ctx,
		db:       db,
		th:       th,
		storeDir: storeDir,
		roots:    roots,
		depth:    depth,
		input:    in,
		spin:     sp,
		picker:   pick,
		hits:     newList(th, "passages"),
		sessions: newList(th, "sessions"),
		repos:    newList(th, "repositories"),
		preview:  viewport.New(0, 0),
	}
}

// newList builds a list with shale's chrome rather than the Bubbles default:
// no title bar, no status bar, no help line, because the browser draws its own
// and two competing sets of chrome is how a TUI starts looking accidental.
func newList(th *ui.Theme, noun string) list.Model {
	d := list.NewDefaultDelegate()
	d.Styles.NormalTitle = th.ItemTitle
	d.Styles.NormalDesc = th.ItemDesc
	d.Styles.SelectedTitle = th.ItemTitleSelected
	d.Styles.SelectedDesc = th.ItemDescSelected
	d.Styles.DimmedTitle = th.ItemTitleDim
	d.Styles.DimmedDesc = th.ItemDescDim
	d.Styles.FilterMatch = th.Match

	l := list.New(nil, d, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetStatusBarItemName(noun, noun)
	l.Styles.NoItems = th.Hint.PaddingLeft(2)
	return l
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textinput.Blink,
		m.spin.Tick,
		sessionsCmd(m.ctx, m.db),
		statsCmd(m.ctx, m.db),
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case searchDoneMsg:
		// Drop answers to questions the user has already moved past.
		if msg.serial != m.serial {
			return m, nil
		}
		m.searching = false
		m.err = msg.err
		items := make([]hitItem, 0, len(msg.hits))
		for _, h := range msg.hits {
			title := "(untitled session)"
			if info, ok := msg.infos[h.EventSeq]; ok && strings.TrimSpace(info.Record.Title) != "" {
				title = oneLine(info.Record.Title, 100)
			}
			items = append(items, hitItem{hit: h, title: title})
		}
		cmd := m.hits.SetItems(toItems(items))
		return m, tea.Batch(cmd, m.previewSelected())

	case sessionsMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		items := make([]sessionItem, 0, len(msg.items))
		for _, s := range msg.items {
			items = append(items, sessionItem{info: s})
		}
		return m, m.sessions.SetItems(toItems(items))

	case reposMsg:
		m.scanning = false
		m.err = msg.err
		items := make([]repoItem, 0, len(msg.repos))
		for _, r := range msg.repos {
			items = append(items, repoItem{repo: r})
		}
		// Selectable repos first: the skipped ones are here to be auditable, not
		// to be the first thing you scroll through.
		sortRepoItems(items)
		return m, m.repos.SetItems(toItems(items))

	case statsMsg:
		m.err = msg.err
		m.stats = msg.stats
		return m, nil

	case transcriptMsg:
		m.previewRef, m.previewErr = msg.ref, msg.err
		m.previewSegs, m.previewLine = msg.segs, msg.focusLine
		m.renderPreview(true)
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}

	return m.routeToFocused(msg)
}

// handleKey is the whole key contract, in precedence order: global keys, then
// the picker when it is open, then the focused pane.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// The list's filter takes over the keyboard entirely while it is being
	// typed into — including "tab" and "q" — so it wins ahead of everything.
	if l := m.activeList(); l != nil && l.SettingFilter() {
		return m.routeToFocused(msg)
	}

	switch msg.String() {
	case "ctrl+c":
		return m, tea.Quit

	case "tab":
		m.tab = (m.tab + 1) % numTabs
		return m, m.onTabChange()
	case "shift+tab":
		m.tab = (m.tab + numTabs - 1) % numTabs
		return m, m.onTabChange()

	case "esc":
		switch {
		case m.focus == focusPicker:
			m.focus = focusList
			return m, nil
		case m.focus == focusPreview:
			m.focus = focusList
			return m, nil
		case m.tab == tabSearch && m.input.Value() != "":
			m.input.SetValue("")
			return m, m.runSearch()
		}
		return m, tea.Quit

	case "enter":
		if m.focus == focusList && m.tab != tabRepos && m.tab != tabStatus {
			m.focus = focusPreview
			return m, m.previewSelected()
		}

	case "q":
		// Only a quit key where it cannot be a character someone meant to type.
		if m.tab != tabSearch {
			return m, tea.Quit
		}
	}

	if m.focus == focusPicker {
		return m.updatePicker(msg)
	}

	// Repos-tab actions. Guarded to that tab so they stay out of the way of the
	// search input on every other screen.
	if m.tab == tabRepos && m.focus == focusList {
		switch msg.String() {
		case "r":
			m.scanning = true
			return m, tea.Batch(reposCmd(m.roots, m.depth), m.spin.Tick)
		case "a":
			m.focus = focusPicker
			return m, m.picker.Init()
		}
	}

	return m.routeToFocused(msg)
}

// routeToFocused hands a message to whichever component owns the keyboard.
func (m Model) routeToFocused(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	if m.focus == focusPicker {
		return m.updatePicker(msg)
	}

	if m.focus == focusPreview {
		var cmd tea.Cmd
		m.preview, cmd = m.preview.Update(msg)
		return m, cmd
	}

	switch m.tab {
	case tabSearch:
		before := m.input.Value()
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
		if m.input.Value() != before {
			cmds = append(cmds, m.runSearch())
		}

		selBefore := m.hits.Index()
		m.hits, cmd = m.hits.Update(msg)
		cmds = append(cmds, cmd)
		// Moving the selection reloads the reader, so the transcript tracks the
		// cursor without needing Enter. Enter is for committing focus TO it.
		if m.hits.Index() != selBefore {
			cmds = append(cmds, m.previewSelected())
		}

	case tabSessions:
		selBefore := m.sessions.Index()
		var cmd tea.Cmd
		m.sessions, cmd = m.sessions.Update(msg)
		cmds = append(cmds, cmd)
		if m.sessions.Index() != selBefore {
			cmds = append(cmds, m.previewSelected())
		}

	case tabRepos:
		var cmd tea.Cmd
		m.repos, cmd = m.repos.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m Model) updatePicker(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	m.picker, cmd = m.picker.Update(msg)
	if ok, path := m.picker.DidSelectFile(msg); ok {
		// A root is added for this session only. Persisting scan roots is a
		// config decision, and a browser is the wrong place to silently make one.
		m.roots = appendUnique(m.roots, path)
		m.focus = focusList
		m.scanning = true
		m.msg = "added scan root " + path
		return m, tea.Batch(reposCmd(m.roots, m.depth), m.spin.Tick)
	}
	return m, cmd
}

// onTabChange loads whatever the newly-shown tab needs and nothing it does not.
func (m *Model) onTabChange() tea.Cmd {
	m.focus = focusList
	m.err = nil
	switch m.tab {
	case tabSearch:
		m.input.Focus()
		return m.previewSelected()
	case tabRepos:
		m.input.Blur()
		// Discovery is a filesystem sweep; run it when the tab is first opened,
		// not at startup, so `shale browse` does not stat a home directory to
		// show a search box.
		if len(m.repos.Items()) == 0 && !m.scanning {
			m.scanning = true
			return tea.Batch(reposCmd(m.roots, m.depth), m.spin.Tick)
		}
	default:
		m.input.Blur()
		if m.tab == tabSessions {
			return m.previewSelected()
		}
	}
	return nil
}

func (m *Model) runSearch() tea.Cmd {
	m.serial++
	m.searching = true
	m.err = nil
	return tea.Batch(searchCmd(m.ctx, m.db, m.input.Value(), m.serial), m.spin.Tick)
}

// previewSelected loads the reader for whatever the active list has selected.
func (m Model) previewSelected() tea.Cmd {
	switch m.tab {
	case tabSearch:
		if it, ok := m.hits.SelectedItem().(hitItem); ok {
			return transcriptCmd(m.ctx, m.db, it.ref(), it.hit.LineStart)
		}
	case tabSessions:
		if it, ok := m.sessions.SelectedItem().(sessionItem); ok {
			return transcriptCmd(m.ctx, m.db, it.ref(), 0)
		}
	}
	return nil
}

// renderPreview lays the held segments out at the reader's current width.
//
// rescroll is true when the content changed (a new ref) and false on a resize:
// re-wrapping under someone who has scrolled halfway down a transcript must not
// throw them back to the matched line they already read past.
func (m *Model) renderPreview(rescroll bool) {
	if m.previewErr != nil {
		m.preview.SetContent(m.th.Danger.Render(m.previewErr.Error()))
		m.preview.GotoTop()
		return
	}
	if len(m.previewSegs) == 0 {
		m.preview.SetContent("")
		return
	}

	body, focusRow := renderTranscript(m.th, m.previewSegs, m.previewLine, m.preview.Width)
	m.preview.SetContent(body)
	if !rescroll {
		return
	}
	// Open ON the passage the ref names, with a few rows of lead-in above it so
	// it arrives in context rather than pinned to the top edge.
	const lead = 3
	if focusRow > lead {
		m.preview.SetYOffset(focusRow - lead)
		return
	}
	m.preview.GotoTop()
}

func (m Model) activeList() *list.Model {
	switch m.tab {
	case tabSearch:
		return &m.hits
	case tabSessions:
		return &m.sessions
	case tabRepos:
		return &m.repos
	}
	return nil
}

func appendUnique(in []string, v string) []string {
	for _, s := range in {
		if s == v {
			return in
		}
	}
	return append(in, v)
}

func sortRepoItems(items []repoItem) {
	// Insertion sort: this list is short and already nearly ordered, and it
	// avoids pulling sort.Slice's reflection in for a dozen rows.
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			a, b := items[j-1], items[j]
			if a.repo.Selectable() || !b.repo.Selectable() {
				break
			}
			items[j-1], items[j] = b, a
		}
	}
}

// Run starts the browser on the alternate screen.
func Run(ctx context.Context, db *store.DB, th *ui.Theme, storeDir string, roots []string, depth int) error {
	m := New(ctx, db, th, storeDir, roots, depth)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	_, err := p.Run()
	// A ctrl-C inside the program surfaces as a context cancellation from
	// tea.WithContext, which is a clean exit here rather than a failure.
	if err != nil && ctx.Err() != nil {
		return nil
	}
	return err
}
