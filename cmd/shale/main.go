// Command shale is the local memory layer for coding agents.
//
// It captures agent sessions from whatever harness wrote them, keeps them in a
// local append-only store, and serves them back to any agent over MCP. It never
// calls an LLM locally, never parses source code, and is read-only on the
// filesystem outside its own state directory (~/.shale).
//
// Interpretation — distillation, entity resolution, cross-machine joins — happens
// on a hub, where it can be rewritten without shipping a new binary to anyone.
//
// Zero runtime dependencies beyond `git`, which it detects rather than installs.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
	"github.com/nonlinear-xyz/shale/internal/config"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/mcp"
	"github.com/nonlinear-xyz/shale/internal/render"
	"github.com/nonlinear-xyz/shale/internal/scrub"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/watch"
)

const usage = `shale — local memory for coding agents

Usage:
  shale repos               list git repositories found on this machine (local only)
  shale watch               capture settled agent sessions into the local store
  shale search <query>      search the local corpus
  shale status              what has been captured
  shale mcp                 serve context to agents over stdio MCP
  shale version             print the build version

Not built yet (in progress): link.

Everything above is local. Nothing leaves this machine.

Run "shale <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Ctrl-C cancels in-flight work cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "repos":
		err = cmdRepos(args)
	case "watch":
		err = cmdWatch(ctx, args)
	case "search":
		err = cmdSearch(ctx, args)
	case "status":
		err = cmdStatus(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "link":
		err = fmt.Errorf(
			"`shale link` is not built yet — this build is local-only; " +
				"hub replication for cross-machine joins lands next")
	case "version", "-v", "--version":
		fmt.Printf("shale %s (%s)\n", buildinfo.Version, buildinfo.PlatformLabel())
		return
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "\ninterrupted.")
		os.Exit(130)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// cmdRepos is the local inventory. It makes NO network calls of any kind — this
// is the command a cautious user runs first to see exactly what shale can see on
// their machine before anything is captured.
func cmdRepos(args []string) error {
	fs := flag.NewFlagSet("repos", flag.ExitOnError)
	rootsFlag := fs.String("root", "", "comma-separated roots to scan (default: home directory)")
	depth := fs.Int("depth", discover.DefaultMaxDepth, "maximum directory depth below each root")
	showAll := fs.Bool("all", false, "include skipped repositories in the table")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireGit(); err != nil {
		return err
	}
	roots, err := resolveRoots(*rootsFlag)
	if err != nil {
		return err
	}
	render.RepoTable(os.Stdout, discover.Discover(roots, *depth), *showAll)
	return nil
}

// cmdWatch sweeps settled transcripts into the local store.
func cmdWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report what would be captured, touching nothing")
	sourceFlag := fs.String("source", "all", "claude, codex, or all")
	rescan := fs.Bool("rescan", false, "re-offer every settled session, ignoring the cursor (use after scrub rules change)")
	verbose := fs.Bool("verbose", false, "print skip reasons")
	if err := fs.Parse(args); err != nil {
		return err
	}

	srcs, err := parseSources(*sourceFlag)
	if err != nil {
		return err
	}

	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	sc, warnings := scrub.New()
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %v\n", w)
	}

	machine, err := config.LoadOrCreateMachine()
	if err != nil {
		return err
	}

	res, err := watch.Sweep(ctx, db, sc, watch.Options{
		Sources: srcs,
		DryRun:  *dryRun,
		Rescan:  *rescan,
		Machine: machine.Label,
	})
	if err != nil {
		return err
	}

	verb := "captured"
	if *dryRun {
		verb = "would capture"
	}
	fmt.Printf("scanned %d, %s %d\n", res.Scanned, verb, res.Captured)
	if res.Backfilled > 0 {
		fmt.Printf("backfilled chunk index for %d already-captured session%s\n",
			res.Backfilled, plural(res.Backfilled))
	}

	if redactions := sc.Total(); redactions > 0 {
		fmt.Printf("redacted %d secret%s: %s\n", redactions, plural(redactions), formatCounts(sc.Counts()))
	}

	if *verbose || *dryRun {
		for _, s := range res.Skipped {
			fmt.Printf("  skip %s — %s\n", shortName(s.Path), s.Reason)
		}
	} else if n := len(res.Skipped); n > 0 {
		fmt.Printf("skipped %d (run with --verbose for reasons)\n", n)
	}

	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "error: %v\n", e)
	}
	if len(res.Errors) > 0 {
		// A failed file holds the watermark, so the next sweep re-offers it. Exiting
		// non-zero is what makes that visible to a scheduler instead of silent.
		return fmt.Errorf("%d file%s failed", len(res.Errors), plural(len(res.Errors)))
	}
	return nil
}

// cmdSearch queries the local corpus. Lexical, local, no model involved.
func cmdSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	limit := fs.Int("limit", 10, "maximum results")
	// Go's flag package stops parsing at the first positional argument, so
	// `search worktree --limit 3` would put "--limit 3" INTO the query and search
	// for it. Nobody types flags before a search term, so reorder first.
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"limit": true})); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("usage: shale search <query>  (supports OR and \"quoted phrases\")")
	}

	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	hits, err := db.Search(ctx, query, *limit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Printf("No matches for %q.\n\nSearch is lexical — exact identifiers, file names and error\nmessages work best. Try fewer words, or OR between them.\n", query)
		return nil
	}

	fmt.Printf("\n%s for %q.\n\n", render.CountPhrase(len(hits), "match", "matches"), query)
	for _, h := range hits {
		scope := h.Scope
		if scope == "" {
			scope = "—"
		}
		fmt.Printf("  %s\n", h.Title)
		fmt.Printf("    %s · %s · %s\n", h.Source, scope, h.OccurredAt)
		if h.Excerpt != "" {
			fmt.Printf("    %s\n", strings.ReplaceAll(strings.TrimSpace(h.Excerpt), "\n", " "))
		}
		fmt.Println()
	}
	return nil
}

// cmdMCP serves the local corpus to an agent over stdio.
//
// STDOUT IS THE PROTOCOL. Nothing may be printed there but JSON-RPC frames, so
// this command is silent on success and every diagnostic goes to stderr. A stray
// fmt.Println here surfaces to the user as an unexplained MCP disconnect.
func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	srv := &mcp.Server{DB: db, Log: os.Stderr}
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	dir, err := config.StateDir()
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()

	st, err := db.Stats(ctx)
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "store\t%s\n", dir)
	fmt.Fprintf(tw, "sessions\t%d\n", st.Sessions)
	fmt.Fprintf(tw, "repositories\t%d\n", st.Repos)
	fmt.Fprintf(tw, "indexed chunks\t%d\n", st.Chunks)
	if st.OldestAt != "" {
		fmt.Fprintf(tw, "span\t%s → %s\n", st.OldestAt, st.NewestAt)
	}
	tw.Flush()

	if st.Sessions == 0 {
		fmt.Println("\nNothing captured yet. Run `shale watch`.")
		fmt.Println("Sessions must be idle for 30 minutes before they are swept.")
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func openStore() (*store.DB, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	return store.Open(dir)
}

func parseSources(flagValue string) ([]sessions.Source, error) {
	switch strings.ToLower(strings.TrimSpace(flagValue)) {
	case "", "all":
		return sessions.AllSources, nil
	case "claude", "claude_code":
		return []sessions.Source{sessions.SourceClaudeCode}, nil
	case "codex":
		return []sessions.Source{sessions.SourceCodex}, nil
	default:
		return nil, fmt.Errorf("unknown --source %q (want claude, codex, or all)", flagValue)
	}
}

// reorderFlagsFirst moves flag tokens ahead of positional ones so a command with
// a free-text argument accepts flags in either position.
//
// valueFlags names the flags that consume a following token (`--limit 3`); the
// `--limit=3` form carries its own value and needs no lookahead. A bare "--"
// ends flag parsing, and everything after it is positional even if it looks like
// a flag — which is how someone searches for a literal string starting with a
// dash.
func reorderFlagsFirst(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if idx := strings.IndexByte(name, '='); idx >= 0 {
			continue // --limit=3 carries its own value
		}
		if valueFlags[name] && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func resolveRoots(flagValue string) ([]string, error) {
	if strings.TrimSpace(flagValue) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory: %w", err)
		}
		return []string{home}, nil
	}
	var roots []string
	for _, r := range strings.Split(flagValue, ",") {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("--root was given but contained no paths")
	}
	return roots, nil
}

// requireGit fails with an actionable message rather than installing anything.
// Installing software is the highest-privilege action a background agent could
// take, and this one deliberately never does it.
func requireGit() error {
	if discover.Git("", "--version") == "" {
		return fmt.Errorf("git not found on PATH — install git and re-run (this tool will not install it for you)")
	}
	return nil
}

func formatCounts(counts map[string]int) string {
	var parts []string
	for _, name := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s×%d", name, counts[name]))
	}
	return strings.Join(parts, ", ")
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func shortName(path string) string {
	parts := strings.Split(path, string(os.PathSeparator))
	if len(parts) < 2 {
		return path
	}
	return strings.Join(parts[len(parts)-2:], string(os.PathSeparator))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
