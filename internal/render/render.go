// Package render draws the terminal surfaces.
//
// The repo table is the moment the user decides whether to trust this binary, so
// it shows everything discovery found — including what it decided to skip and
// why. A silent skip is indistinguishable from a bug, and "why isn't my repo
// listed" is the first question this tool will ever be asked.
package render

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/pathutil"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
)

// RepoTable prints the discovered repositories, then the skipped ones grouped by
// reason. showAllSkips lifts the per-group inline cap.
func RepoTable(w io.Writer, repos []discover.Repo, showAllSkips bool) {
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

	fmt.Fprintf(w, "\nFound %s.\n\n", CountPhrase(len(selectable), "repository", "repositories"))

	if len(selectable) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "REPO\tCOMMITS\tLAST ACTIVE\tPATH")
		for _, r := range selectable {
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n",
				r.Name(), r.CommitCount, RelativeTime(r.LastCommitAt), pathutil.Shorten(r.Path))
		}
		tw.Flush()
	}

	if len(skipped) == 0 {
		return
	}

	fmt.Fprintf(w, "\nSkipped %d:\n", len(skipped))

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
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
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
			fmt.Fprintf(tw, "  %s\t%s\n", r.Name(), detail)
		}
		if limit < len(group) {
			fmt.Fprintf(tw, "  … and %d more\t%s\n", len(group)-limit, reason)
		}
	}
	tw.Flush()

	if !showAllSkips && len(skipped) > inlineLimit {
		fmt.Fprintln(w, "\nRun with --all to list every skipped repository.")
	}
}

// SearchResults prints ranked passages, each under the session it came from.
//
// Every result leads with its ref. A result you can read but not address is a
// dead end — you have found the passage and still cannot ask for the rest of it.
// The ref is what `shale show` takes, so search and show compose into the thing
// someone actually wants: find the moment, then read around it.
func SearchResults(w io.Writer, query string, hits []store.ChunkHit, infos map[int64]store.SessionInfo) {
	fmt.Fprintf(w, "\n%s for %q.\n\n", CountPhrase(len(hits), "match", "matches"), query)

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
		// Chunk kinds are "transcript" and "error" (sessions.ChunkKind) — NOT the
		// finer-grained segment kinds. A chunk holding a failure is worth flagging
		// in a scan surface: "what went wrong last time" is why people search here.
		if h.Kind == string(sessions.ChunkError) {
			facts = append(facts, "error")
		}

		fmt.Fprintf(w, "  %s\n", title)
		fmt.Fprintf(w, "    %s\n", strings.Join(facts, " · "))
		if h.Excerpt != "" {
			fmt.Fprintf(w, "    %s\n", oneLine(h.Excerpt, 220))
		}
		fmt.Fprintf(w, "    shale show %s\n\n", h.Ref())
	}
}

// Transcript prints a session, optionally clipped to a line range.
//
// lineStart/lineEnd are 1-based and inclusive; 0,0 means the whole thing.
// totalLines is the transcript's true length, so a clipped view can say what it
// is a view OF rather than passing a fragment off as the session.
func Transcript(w io.Writer, info store.SessionInfo, segs []sessions.Segment, lineStart, lineEnd, totalLines int) {
	rec := info.Record
	scope := info.Scope
	if scope == "" {
		scope = "—"
	}

	fmt.Fprintf(w, "\n%s\n", oneLine(rec.Title, 200))
	facts := []string{
		fmt.Sprintf("session:%d", info.Seq),
		info.Source,
		scope,
		shortStamp(info.OccurredAt),
	}
	if rec.Turns > 0 {
		facts = append(facts, CountPhrase(rec.Turns, "turn", "turns"))
	}
	fmt.Fprintf(w, "%s\n", strings.Join(facts, " · "))

	if lineStart > 0 || lineEnd > 0 {
		fmt.Fprintf(w, "showing lines %d–%d of %d — `--full` for the whole session\n", lineStart, lineEnd, totalLines)
	} else {
		fmt.Fprintf(w, "%s\n", CountPhrase(totalLines, "line", "lines"))
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
		fmt.Fprintf(w, "%6d  %-13s %s\n", s.LineNo, segmentLabel(s.Kind), indentBody(s.Text))
	}

	if shown == 0 {
		// The blob is on disk and the range is real, but nothing readable fell in
		// it. Say so; an empty printout otherwise reads as a broken command.
		fmt.Fprintf(w, "  (no readable segments in this range — the lines here carry no prose,\n"+
			"   commands or output. Try --full, or a wider --lines range.)\n")
	}
}

// segmentLabel names a segment kind in a fixed-width column so the transcript
// scans vertically.
func segmentLabel(k sessions.SegmentKind) string {
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
