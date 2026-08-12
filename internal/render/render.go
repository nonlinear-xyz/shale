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
	"text/tabwriter"
	"time"

	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/pathutil"
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
