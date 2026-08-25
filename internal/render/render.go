// Package render draws the terminal surfaces.
//
// The repo table is the moment the user decides whether to trust this binary, so
// it shows everything discovery found — including what it decided to skip and
// why. A silent skip is indistinguishable from a bug, and "why isn't my repo
// listed" is the first question this tool will ever be asked.
//
// Every function here takes a *ui.Theme and names no color of its own. Under a
// pipe the theme resolves to the ascii profile and all of this prints exactly
// the plain text it printed before styling existed.
package render

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/pathutil"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
)

// RepoTable prints the discovered repositories, then the skipped ones grouped by
// reason. showAllSkips lifts the per-group inline cap.
func RepoTable(w io.Writer, t *ui.Theme, repos []discover.Repo, showAllSkips bool) {
	var selectable, skipped []discover.Repo
	for _, r := range repos {
		if r.Selectable() {
			selectable = append(selectable, r)
		} else {
			skipped = append(skipped, r)
		}
	}

	if len(repos) == 0 {
		fmt.Fprintln(w, "No git repositories found.")
		return
	}

	fmt.Fprintf(w, "\nFound %s.\n\n", t.Count.Render(CountPhrase(len(selectable), "repository", "repositories")))

	if len(selectable) > 0 {
		tbl := newTable("REPO", "COMMITS", "LAST ACTIVE", "PATH")
		for _, r := range selectable {
			tbl.row(r.Name(), fmt.Sprint(r.CommitCount), RelativeTime(r.LastCommitAt), pathutil.Shorten(r.Path))
		}
		tbl.write(w, 3, func(col int, cell string) string {
			switch col {
			case 0:
				return t.Title.Render(cell)
			case 3:
				return t.Facts.Render(cell)
			default:
				return t.Body.Render(cell)
			}
		}, func(cell string) string { return t.Header.Render(cell) })
	}

	if len(skipped) == 0 {
		return
	}

	fmt.Fprintf(w, "\nSkipped %s:\n", t.Count.Render(fmt.Sprint(len(skipped))))

	// Group by reason so a home directory full of vendored corpora collapses to
	// one line instead of fifty.
	byReason := map[discover.SkipReason][]discover.Repo{}
	for _, r := range skipped {
		byReason[r.SkipReason] = append(byReason[r.SkipReason], r)
	}
	reasons := make([]discover.SkipReason, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })

	const inlineLimit = 4
	tbl := newTable()
	for _, reason := range reasons {
		group := byReason[reason]
		limit := len(group)
		if !showAllSkips && limit > inlineLimit {
			limit = inlineLimit
		}
		for i := 0; i < limit; i++ {
			r := group[i]
			detail := string(reason)
			if r.SkipDetail != "" {
				detail = fmt.Sprintf("%s (%s)", reason, r.SkipDetail)
			}
			tbl.row("  "+r.Name(), detail)
		}
		if limit < len(group) {
			tbl.row(fmt.Sprintf("  … and %d more", len(group)-limit), string(reason))
		}
	}
	// A skip is not a failure — it is shale declining to look at something, which
	// is the behaviour a cautious user is here to verify. Warn, never Danger.
	tbl.write(w, 2, func(col int, cell string) string {
		if col == 0 {
			return t.Facts.Render(cell)
		}
		return t.Warn.Render(cell)
	}, nil)

	if !showAllSkips && len(skipped) > inlineLimit {
		fmt.Fprintln(w, "\n"+t.Hint.Render("Run with --all to list every skipped repository."))
	}
}

// SearchResults prints ranked passages, each under the session it came from.
//
// Every result leads with its ref. A result you can read but not address is a
// dead end — you have found the passage and still cannot ask for the rest of it.
// The ref is what `shale show` takes, so search and show compose into the thing
// someone actually wants: find the moment, then read around it.
func SearchResults(w io.Writer, t *ui.Theme, query string, hits []store.ChunkHit, infos map[int64]store.SessionInfo) {
	fmt.Fprintf(w, "\n%s for %q.\n\n", t.Count.Render(CountPhrase(len(hits), "match", "matches")), query)

	terms := queryTerms(query)
	for _, h := range hits {
		title := "(untitled session)"
		if info, ok := infos[h.EventSeq]; ok && strings.TrimSpace(info.Record.Title) != "" {
			title = oneLine(info.Record.Title, 96)
		}
		scope := h.Scope
		if scope == "" {
			scope = "—"
		}

		facts := []string{h.Source, scope, shortStamp(h.OccurredAt), fmt.Sprintf("lines %d–%d", h.LineStart, h.LineEnd)}

		fmt.Fprintf(w, "  %s\n", t.Title.Render(title))
		line := t.Facts.Render(strings.Join(facts, " · "))
		// Chunk kinds are "transcript" and "error" (sessions.ChunkKind) — NOT the
		// finer-grained segment kinds. A chunk holding a failure is worth flagging
		// in a scan surface: "what went wrong last time" is why people search here.
		if h.Kind == string(sessions.ChunkError) {
			line += t.Facts.Render(" · ") + t.Danger.Render("error")
		}
		fmt.Fprintf(w, "    %s\n", line)
		if h.Excerpt != "" {
			fmt.Fprintf(w, "    %s\n", highlight(t, oneLine(h.Excerpt, 220), terms))
		}
		fmt.Fprintf(w, "    %s\n\n", t.Ref.Render("shale show "+h.Ref()))
	}
}

// ArtifactSearchResults prints durable-state matches with their exact versioned
// refs so a result can be resolved even after the artifact changes.
func ArtifactSearchResults(w io.Writer, t *ui.Theme, query string, hits []store.ArtifactHit) {
	fmt.Fprintf(w, "\n%s for %q.\n\n", t.Count.Render(CountPhrase(len(hits), "match", "matches")), query)
	terms := queryTerms(query)
	for _, h := range hits {
		facts := []string{string(h.Kind), string(h.Status), artifactScope(h.Artifact), h.Source, shortStamp(h.UpdatedAt)}
		fmt.Fprintf(w, "  %s\n", t.Title.Render(oneLine(h.Title, 120)))
		fmt.Fprintf(w, "    %s\n", t.Facts.Render(strings.Join(facts, " · ")))
		if h.Excerpt != "" {
			fmt.Fprintf(w, "    %s\n", highlight(t, oneLine(h.Excerpt, 220), terms))
		}
		fmt.Fprintf(w, "    %s\n\n", t.Ref.Render("shale show "+h.VersionedRef()))
	}
}

// ArtifactList prints current durable artifacts. Lifecycle commands operate on
// the stable ref; the exact version remains visible for citations and audits.
func ArtifactList(w io.Writer, t *ui.Theme, noun string, items []store.Artifact) {
	if len(items) == 0 {
		fmt.Fprintf(w, "No %s.\n", noun)
		return
	}
	fmt.Fprintf(w, "\n%s.\n\n", t.Count.Render(CountPhrase(len(items), artifactSingular(noun), noun)))
	for _, a := range items {
		facts := []string{string(a.Status), artifactScope(a), a.Source, shortStamp(a.UpdatedAt)}
		fmt.Fprintf(w, "  %s\n", t.Title.Render(oneLine(a.Title, 120)))
		fmt.Fprintf(w, "    %s\n", t.Facts.Render(strings.Join(facts, " · ")))
		fmt.Fprintf(w, "    %s", t.Ref.Render(a.Ref()))
		if a.EventSeq > 0 {
			fmt.Fprintf(w, " %s", t.Facts.Render(fmt.Sprintf("(version @%d)", a.EventSeq)))
		}
		fmt.Fprint(w, "\n\n")
	}
}

func artifactSingular(noun string) string {
	switch noun {
	case "memories":
		return "memory"
	default:
		return strings.TrimSuffix(noun, "s")
	}
}

// ArtifactDetail prints one current or historical artifact version.
func ArtifactDetail(w io.Writer, t *ui.Theme, a store.Artifact) {
	fmt.Fprintf(w, "\n%s\n", t.Title.Render(oneLine(a.Title, 200)))
	facts := []string{a.VersionedRef(), string(a.Status), artifactScope(a), a.Source, a.Authority, shortStamp(a.UpdatedAt)}
	if a.Origin != "" {
		facts = append(facts, a.Origin)
	}
	fmt.Fprintf(w, "%s\n", t.Facts.Render(strings.Join(facts, " · ")))
	if a.SourcePointer != "" {
		fmt.Fprintf(w, "%s\n", t.Facts.Render(a.SourcePointer))
	}
	if a.Content.Trigger != "" {
		fmt.Fprintf(w, "%s\n", t.Facts.Render("recall when: "+a.Content.Trigger))
	}
	if len(a.Content.EvidenceRefs) > 0 && a.Kind != store.ArtifactCheckpoint {
		fmt.Fprintf(w, "%s\n", t.Facts.Render("evidence: "+strings.Join(a.Content.EvidenceRefs, ", ")))
	}
	fmt.Fprintln(w)
	if !a.ContentPresent {
		fmt.Fprintln(w, t.Hint.Render("(content is unavailable or has been purged)"))
		return
	}
	body := a.Content.RenderText(a.Kind)
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(w, t.Hint.Render("(empty content)"))
		return
	}
	fmt.Fprintln(w, body)
}

func artifactScope(a store.Artifact) string {
	switch a.ScopeKind {
	case store.ScopeUser:
		return "user"
	case store.ScopeRepo:
		return "repo:" + a.Repo
	case store.ScopeTask:
		if a.Repo != "" {
			return "task:" + a.ScopeKey + " · " + a.Repo
		}
		return "task:" + a.ScopeKey
	default:
		return string(a.ScopeKind)
	}
}

// Transcript prints a session, optionally clipped to a line range.
//
// lineStart/lineEnd are 1-based and inclusive; 0,0 means the whole thing.
// totalLines is the transcript's true length, so a clipped view can say what it
// is a view OF rather than passing a fragment off as the session.
func Transcript(w io.Writer, t *ui.Theme, info store.SessionInfo, segs []sessions.Segment, lineStart, lineEnd, totalLines int) {
	rec := info.Record
	scope := info.Scope
	if scope == "" {
		scope = "—"
	}

	fmt.Fprintf(w, "\n%s\n", t.Title.Render(oneLine(rec.Title, 200)))
	facts := []string{
		fmt.Sprintf("session:%d", info.Seq),
		info.Source,
		scope,
		shortStamp(info.OccurredAt),
	}
	if rec.Turns > 0 {
		facts = append(facts, CountPhrase(rec.Turns, "turn", "turns"))
	}
	fmt.Fprintf(w, "%s\n", t.Facts.Render(strings.Join(facts, " · ")))

	if lineStart > 0 || lineEnd > 0 {
		fmt.Fprintf(w, "%s\n", t.Hint.Render(
			fmt.Sprintf("showing lines %d–%d of %d — `--full` for the whole session", lineStart, lineEnd, totalLines)))
	} else {
		fmt.Fprintf(w, "%s\n", t.Facts.Render(CountPhrase(totalLines, "line", "lines")))
	}
	fmt.Fprintln(w)

	shown := 0
	for _, s := range segs {
		if lineStart > 0 && s.LineNo < lineStart {
			continue
		}
		if lineEnd > 0 && s.LineNo > lineEnd {
			continue
		}
		shown++
		fmt.Fprintf(w, "%s  %s %s\n",
			t.Gutter.Render(fmt.Sprintf("%6d", s.LineNo)),
			SegmentLabelStyle(t, s.Kind).Render(fmt.Sprintf("%-13s", SegmentLabel(s.Kind))),
			indentBody(s.Text))
	}

	if shown == 0 {
		// The blob is on disk and the range is real, but nothing readable fell in
		// it. Say so; an empty printout otherwise reads as a broken command.
		fmt.Fprintf(w, "%s\n", t.Hint.Render(
			"  (no readable segments in this range — the lines here carry no prose,\n"+
				"   commands or output. Try --full, or a wider --lines range.)"))
	}
}

// SegmentLabel names a segment kind in a fixed-width column so the transcript
// scans vertically.
func SegmentLabel(k sessions.SegmentKind) string {
	switch k {
	case sessions.SegUser:
		return "user"
	case sessions.SegAssistant:
		return "assistant"
	case sessions.SegToolUse:
		return "tool"
	case sessions.SegToolOut:
		return "output"
	case sessions.SegToolError:
		return "ERROR"
	default:
		return string(k)
	}
}

// SegmentLabelStyle colors a segment label by what the segment IS — who spoke,
// what ran, what came back, what failed. This is the transcript's own semantics,
// so it gets its own tokens rather than reusing the search surface's.
func SegmentLabelStyle(t *ui.Theme, k sessions.SegmentKind) lipgloss.Style {
	switch k {
	case sessions.SegUser:
		return t.LabelUser
	case sessions.SegAssistant:
		return t.LabelAssistant
	case sessions.SegToolUse:
		return t.LabelTool
	case sessions.SegToolOut:
		return t.LabelOutput
	case sessions.SegToolError:
		return t.LabelError
	default:
		return t.Facts
	}
}

// StatusTable prints the store summary.
func StatusTable(w io.Writer, t *ui.Theme, rows [][2]string) {
	tbl := newTable()
	for _, r := range rows {
		tbl.row(r[0], r[1])
	}
	tbl.write(w, 2, func(col int, cell string) string {
		if col == 0 {
			return t.Facts.Render(cell)
		}
		return t.Body.Render(cell)
	}, nil)
}

// ── tables ───────────────────────────────────────────────────────────────────

// table replaces text/tabwriter, which cannot be used once cells carry color:
// tabwriter measures a cell by counting its runes, and an ANSI escape sequence
// is made of runes, so every styled cell measures wider than it draws and the
// columns shear. This measures the PLAIN text with lipgloss.Width (which also
// gets CJK and emoji right, another thing tabwriter does not), then styles.
type table struct {
	head []string
	rows [][]string
}

func newTable(head ...string) *table { return &table{head: head} }

func (tb *table) row(cells ...string) { tb.rows = append(tb.rows, cells) }

// write renders the table with `gap` spaces between columns. styleCell styles a
// body cell by column index; styleHead styles the header row, or is nil when
// there is no header.
func (tb *table) write(w io.Writer, gap int, styleCell func(col int, cell string) string, styleHead func(string) string) {
	n := len(tb.head)
	for _, r := range tb.rows {
		if len(r) > n {
			n = len(r)
		}
	}
	if n == 0 {
		return
	}

	widths := make([]int, n)
	measure := func(cells []string) {
		for i, c := range cells {
			if wdt := lipgloss.Width(c); wdt > widths[i] {
				widths[i] = wdt
			}
		}
	}
	measure(tb.head)
	for _, r := range tb.rows {
		measure(r)
	}

	pad := strings.Repeat(" ", gap)
	emit := func(cells []string, style func(col int, cell string) string) {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString(pad)
			}
			b.WriteString(style(i, c))
			// Pad AFTER styling, using the plain width — trailing spaces inside a
			// styled span would carry the background color on themes that set one.
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-lipgloss.Width(c)))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}

	if len(tb.head) > 0 && styleHead != nil {
		emit(tb.head, func(_ int, c string) string { return styleHead(c) })
	}
	for _, r := range tb.rows {
		emit(r, styleCell)
	}
}

// ── text helpers ─────────────────────────────────────────────────────────────

// queryTerms pulls the searchable words out of an FTS query so the excerpt can
// show WHY it matched. Quotes and the OR operator are syntax, not terms.
func queryTerms(q string) []string {
	q = strings.ReplaceAll(q, `"`, " ")
	var out []string
	for _, f := range strings.Fields(q) {
		if strings.EqualFold(f, "or") || strings.EqualFold(f, "and") || strings.EqualFold(f, "not") {
			continue
		}
		if f = strings.Trim(f, "*"); len(f) > 1 {
			out = append(out, strings.ToLower(f))
		}
	}
	return out
}

// highlight wraps each occurrence of a term in the Match style. Case-insensitive
// on the haystack, and it walks the string once so overlapping terms cannot
// double-wrap and emit nested escape sequences.
func highlight(t *ui.Theme, s string, terms []string) string {
	if len(terms) == 0 {
		return t.Body.Render(s)
	}
	lower := strings.ToLower(s)
	var b strings.Builder
	for i := 0; i < len(s); {
		matched := 0
		for _, term := range terms {
			if strings.HasPrefix(lower[i:], term) && len(term) > matched {
				matched = len(term)
			}
		}
		if matched > 0 {
			b.WriteString(t.Match.Render(s[i : i+matched]))
			i += matched
			continue
		}
		// Advance one rune, not one byte, so multi-byte text survives intact.
		r := []rune(s[i:])[0]
		size := len(string(r))
		b.WriteString(t.Body.Render(s[i : i+size]))
		i += size
	}
	return b.String()
}

// indentBody aligns a multi-line segment under the column its first line starts
// in, so a wrapped tool output does not collide with the line-number gutter.
func indentBody(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+strings.Repeat(" ", 21))
}

// oneLine flattens text to a single line and clips it. Search output is a scan
// surface: a result that spills over ten lines buries the ones below it.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + " …"
}

// shortStamp trims an RFC3339 timestamp to the date. Search results are scanned
// for "how old is this", which the time of day does not help answer.
func shortStamp(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// CountPhrase renders "1 repository" / "3 repositories" so callers never emit
// the "1 repositories" that makes a tool look unfinished.
func CountPhrase(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// RelativeTime is coarse on purpose: this column exists to answer "is this repo
// alive?", not to report a timestamp.
func RelativeTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d months ago", int(d.Hours()/24/30))
	default:
		return fmt.Sprintf("%d years ago", int(d.Hours()/24/365))
	}
}
