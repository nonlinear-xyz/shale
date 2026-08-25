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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mattn/go-isatty"
	"github.com/nonlinear-xyz/shale/internal/browse"
	"github.com/nonlinear-xyz/shale/internal/buildinfo"
	"github.com/nonlinear-xyz/shale/internal/config"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/mcp"
	"github.com/nonlinear-xyz/shale/internal/render"
	"github.com/nonlinear-xyz/shale/internal/scrub"
	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
	"github.com/nonlinear-xyz/shale/internal/ui"
	"github.com/nonlinear-xyz/shale/internal/watch"
)

const usage = `shale — local memory for coding agents

Usage:
  shale                     open the browser (same as "shale browse")
  shale repos               list git repositories found on this machine (local only)
  shale watch               capture settled agent sessions into the local store
  shale search <query>      search the local corpus
  shale show <ref>          print the passage, session, or durable artifact a ref names
  shale remember <text>     save an explicit durable memory
  shale memories            list approved memories
  shale proposals           review pending agent memory proposals
  shale accept <ref>        approve a pending memory proposal
  shale reject <ref>        reject and destroy a pending proposal
  shale supersede <ref>     replace an approved memory with a new version
  shale forget <ref>        retract native state from recall
  shale purge <ref> --yes   irreversibly destroy every stored body version
  shale checkpoints         list task handoffs saved by agents
  shale runbook <command>   create, register, revise, or list runbooks
  shale refresh             index Claude/Codex memory and instruction files
  shale browse              search, read and audit interactively
  shale status              what has been captured
  shale mcp                 serve context to agents over stdio MCP
  shale version             print the build version

Not built yet (in progress): link.

Everything above is local. Nothing leaves this machine.

Colour follows the terminal: piped, redirected, or NO_COLOR set, every command
above prints plain text. Pass --no-color to force it — useful when an agent
shells out to shale through a PTY, where shale cannot tell it is not a person.

Run "shale <command> -h" for command flags.
`

func main() {
	// Bare `shale` opens the browser — but only with a terminal to draw on.
	//
	// The guard is the whole point. Bare `shale` already had a meaning: print
	// usage, exit 2. Scripts and CI depend on that, and an alternate-screen TUI
	// launched into a pipe either hangs waiting on a keyboard that is not there
	// or floods the redirect with cursor escapes. Both stdin and stdout must be
	// terminals, because the program reads one and draws to the other.
	cmd, args := "browse", []string(nil)
	if len(os.Args) >= 2 {
		cmd, args = os.Args[1], stripGlobalFlags(os.Args[2:])
	} else if !interactive() {
		fmt.Print(usage)
		os.Exit(2)
	}

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
	case "show":
		err = cmdShow(ctx, args)
	case "remember":
		err = cmdRemember(ctx, args)
	case "memories":
		err = cmdMemories(ctx, args)
	case "proposals":
		err = cmdProposals(ctx, args)
	case "accept":
		err = cmdAccept(ctx, args)
	case "reject":
		err = cmdReject(ctx, args)
	case "supersede":
		err = cmdSupersede(ctx, args)
	case "forget":
		err = cmdForget(ctx, args)
	case "purge":
		err = cmdPurge(ctx, args)
	case "checkpoints":
		err = cmdCheckpoints(ctx, args)
	case "runbook":
		err = cmdRunbook(ctx, args)
	case "runbooks":
		err = cmdRunbook(ctx, append([]string{"list"}, args...))
	case "refresh":
		err = cmdRefresh(ctx, args)
	case "browse":
		err = cmdBrowse(ctx, args)
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
	render.RepoTable(os.Stdout, theme(), discover.Discover(roots, *depth), *showAll)
	return nil
}

// cmdBrowse is the interactive surface over the same index the other commands
// read. It is additive: nothing above changes behaviour because this exists.
func cmdBrowse(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("browse", flag.ExitOnError)
	rootsFlag := fs.String("root", "", "comma-separated roots for the Repos tab (default: home directory)")
	depth := fs.Int("depth", discover.DefaultMaxDepth, "maximum directory depth below each root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	roots, err := resolveRoots(*rootsFlag)
	if err != nil {
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

	return browse.Run(ctx, db, theme(), dir, roots, *depth)
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

	db, cleanup, err := openWatchStore(*dryRun)
	if err != nil {
		return err
	}
	defer cleanup()
	defer db.Close()

	sc, warnings := scrub.New()
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "warning: %v\n", w)
	}

	machineLabel := "dry-run"
	if !*dryRun {
		machine, err := config.LoadOrCreateMachine()
		if err != nil {
			return err
		}
		machineLabel = machine.Label
	}

	res, err := watch.Sweep(ctx, db, sc, watch.Options{
		Sources: srcs,
		DryRun:  *dryRun,
		Rescan:  *rescan,
		Machine: machineLabel,
	})
	if err != nil {
		return err
	}

	th := theme()
	verb := "captured"
	if *dryRun {
		verb = "would capture"
	}
	fmt.Printf("scanned %d, %s %s\n", res.Scanned, verb, th.Success.Render(fmt.Sprint(res.Captured)))
	if res.Backfilled > 0 {
		fmt.Printf("backfilled chunk index for %d already-captured session%s\n",
			res.Backfilled, plural(res.Backfilled))
	}

	// Redactions are reported in Warn, not Success. A secret found in a
	// transcript is a thing that happened, not a job well done.
	if redactions := sc.Total(); redactions > 0 {
		fmt.Println(th.Warn.Render(fmt.Sprintf("redacted %d secret%s: %s",
			redactions, plural(redactions), formatCounts(sc.Counts()))))
	}

	if *verbose || *dryRun {
		for _, s := range res.Skipped {
			fmt.Printf("  skip %s — %s\n", th.Facts.Render(shortName(s.Path)), th.Warn.Render(s.Reason))
		}
	} else if n := len(res.Skipped); n > 0 {
		fmt.Println(th.Hint.Render(fmt.Sprintf("skipped %d (run with --verbose for reasons)", n)))
	}

	for _, e := range res.Errors {
		fmt.Fprintf(os.Stderr, "error: %v\n", e)
	}
	if len(res.Errors) > 0 {
		// A failed file holds the watermark, so the next sweep re-offers it. Exiting
		// non-zero is what makes that visible to a scheduler instead of silent.
		return fmt.Errorf("%d file%s failed", len(res.Errors), plural(len(res.Errors)))
	}
	if !*dryRun {
		if err := refreshArtifacts(ctx, db, *verbose); err != nil {
			return err
		}
	}
	return nil
}

// cmdSearch queries the local corpus. Lexical, local, no model involved.
//
// This searches CHUNKS — the transcript bodies — not the per-session digests.
// The distinction is the whole point. There is one digest per session and dozens
// of chunks, so the digest index sees a few percent of what was captured; a
// phrase that is plainly in a transcript is simply absent from it. Searching
// digests here meant the CLI and the MCP server disagreed about what "the
// corpus" contains, and the CLI's answer was a confident, silent "no matches"
// for text shale was holding the whole time. Both surfaces read the same index
// now.
func cmdSearch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	limit := fs.Int("limit", 10, "maximum results")
	repo := fs.String("repo", "", `filter to one repository ("owner/name")`)
	kind := fs.String("kind", "", `transcript, error, memory, checkpoint, runbook, or instruction`)
	taskKey := fs.String("task", "", "filter durable state to a task key")
	sinceDays := fs.Int("since", 0, "only search sessions from the last N days")
	// Go's flag package stops parsing at the first positional argument, so
	// `search worktree --limit 3` would put "--limit 3" INTO the query and search
	// for it. Nobody types flags before a search term, so reorder first.
	valued := map[string]bool{"limit": true, "repo": true, "kind": true, "task": true, "since": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
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

	kindValue := strings.TrimSpace(*kind)
	artifactKind := store.ArtifactKind(kindValue)
	switch artifactKind {
	case store.ArtifactMemory, store.ArtifactCheckpoint, store.ArtifactRunbook, store.ArtifactInstruction:
		hits, err := db.SearchArtifacts(ctx, store.ArtifactSearch{
			Query: query, Kind: artifactKind, Repo: *repo, TaskKey: *taskKey, Limit: *limit,
		})
		if err != nil {
			return err
		}
		if len(hits) == 0 {
			fmt.Printf("No %s matches for %q.\n", artifactKind, query)
			return nil
		}
		render.ArtifactSearchResults(os.Stdout, theme(), query, hits)
		return nil
	case "", "transcript", "error":
	default:
		return fmt.Errorf("unknown --kind %q (want transcript, error, memory, checkpoint, runbook, or instruction)", *kind)
	}
	chunkKind := kindValue
	if chunkKind == "transcript" {
		// "transcript" means the ordinary corpus, not an exact chunks_fts
		// subtype. An empty kind is the store's all-transcript-passages query.
		chunkKind = ""
	}

	hits, err := db.SearchChunks(ctx, query, *repo, chunkKind, *sinceDays, *limit)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Printf("No matches for %q.\n\nSearch is lexical — exact identifiers, file names and error\nmessages work best. Try fewer words, or OR between them.\n", query)
		return nil
	}

	// One lookup for every session the hits landed in, so each result can be
	// shown under the session it came from rather than as a floating excerpt.
	seqs := make([]int64, 0, len(hits))
	for _, h := range hits {
		seqs = append(seqs, h.EventSeq)
	}
	sessions, err := db.Sessions(ctx, seqs)
	if err != nil {
		return err
	}

	render.SearchResults(os.Stdout, theme(), query, hits, sessions)
	return nil
}

// cmdShow resolves a ref to the passage or session it names.
//
// Refs were mintable and not resolvable: packets and search results cited
// "chunk:12:3" and nothing in the binary could turn that back into text, so
// following a citation meant querying SQLite by hand and gunzipping a blob.
// A citation you cannot follow is decoration.
func cmdShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("show", flag.ExitOnError)
	full := fs.Bool("full", false, "print the entire session transcript")
	lines := fs.String("lines", "", "print an explicit line range, e.g. 120,180")
	context_ := fs.Int("context", 1, "chunks of surrounding context to include either side")
	valued := map[string]bool{"lines": true, "context": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	arg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if arg == "" {
		return errors.New(`usage: shale show <ref>   (artifact, chunk:<seq>:<index>, session:<seq> or <seq>)`)
	}

	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if artifactRef, parseErr := store.ParseArtifactRef(arg); parseErr == nil {
		if *lines != "" {
			return errors.New("--lines only applies to transcript refs")
		}
		a, err := db.ResolveArtifactRef(ctx, artifactRef)
		if err != nil {
			return err
		}
		render.ArtifactDetail(os.Stdout, theme(), a)
		return nil
	}
	ref, err := store.ParseRef(arg)
	if err != nil {
		return err
	}

	info, err := db.Session(ctx, ref.EventSeq)
	if err != nil {
		return err
	}

	lineStart, lineEnd, err := showRange(ctx, db, ref, *full, *lines, *context_)
	if err != nil {
		return err
	}

	blob, err := db.ReadBlob(info.ContentHash)
	if err != nil {
		return fmt.Errorf("%w\n\nThe event is in the log but its transcript is not on disk. "+
			"Blobs are content-addressed under ~/.shale/blobs", err)
	}

	segs := sessions.Segments(sessions.Source(info.Source), blob)
	render.Transcript(os.Stdout, theme(), info, segs, lineStart, lineEnd, len(blob))
	return nil
}

// showRange decides which lines of the transcript to print.
//
// Precedence is explicit over implied: --lines beats --full beats the chunk the
// ref names. A whole-session ref with no flags shows everything, because "show
// me session 12" has no narrower reading.
func showRange(ctx context.Context, db *store.DB, ref store.Ref, full bool, lines string, ctxChunks int) (int, int, error) {
	if lines != "" {
		return parseLineRange(lines)
	}
	if full || !ref.HasChunk {
		return 0, 0, nil // 0,0 means unbounded
	}

	hit, err := db.Chunk(ctx, ref.EventSeq, ref.ChunkIndex)
	if err != nil {
		return 0, 0, err
	}
	start, end := hit.LineStart, hit.LineEnd

	// Widen through the neighbouring chunks rather than by a fixed line count.
	// Chunk boundaries fall between turns, so this keeps whole exchanges intact
	// instead of slicing one in half at an arbitrary offset.
	for i := 1; i <= ctxChunks; i++ {
		if before, err := db.Chunk(ctx, ref.EventSeq, ref.ChunkIndex-i); err == nil && before.LineStart < start {
			start = before.LineStart
		}
		if after, err := db.Chunk(ctx, ref.EventSeq, ref.ChunkIndex+i); err == nil && after.LineEnd > end {
			end = after.LineEnd
		}
	}
	return start, end, nil
}

func parseLineRange(s string) (int, int, error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("bad --lines %q: want start,end", s)
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad --lines %q: %v", s, err)
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, fmt.Errorf("bad --lines %q: %v", s, err)
	}
	if start > end {
		return 0, 0, fmt.Errorf("bad --lines %q: start is after end", s)
	}
	return start, end, nil
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

	th := theme()
	rows := [][2]string{
		{"store", dir},
		{"sessions", fmt.Sprint(st.Sessions)},
		{"repositories", fmt.Sprint(st.Repos)},
		{"indexed chunks", fmt.Sprint(st.Chunks)},
		{"active memories", fmt.Sprint(st.Memories)},
		{"pending proposals", fmt.Sprint(st.Proposals)},
		{"task checkpoints", fmt.Sprint(st.Checkpoints)},
		{"active runbooks", fmt.Sprint(st.Runbooks)},
		{"indexed instructions", fmt.Sprint(st.Instructions)},
		{"watched sources", fmt.Sprint(st.Sources)},
	}
	if st.OldestAt != "" {
		rows = append(rows, [2]string{"span", st.OldestAt + " → " + st.NewestAt})
	}
	render.StatusTable(os.Stdout, th, rows)

	if st.Sessions == 0 {
		message := "No agent sessions captured yet. Run `shale watch`."
		if st.Memories+st.Proposals+st.Checkpoints+st.Runbooks+st.Instructions == 0 {
			message = "Nothing captured yet. Run `shale watch` or `shale remember`."
		}
		fmt.Println("\n" + th.Warn.Render(message))
		fmt.Println(th.Hint.Render("Sessions must be idle for 30 minutes before they are swept."))
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// theme builds the style set against stdout, which is what makes color
// automatic and piping safe: termenv reads the profile off the writer, so a
// redirect, a dumb terminal or NO_COLOR all resolve to plain text with no
// special case at any call site.
//
// Never call this from cmdMCP. Stdout there is the JSON-RPC transport, and
// resolving an adaptive color against a real terminal makes termenv write an
// OSC background-color query into it and block waiting for a reply. Use
// ui.Plain() if that path ever needs styling for stderr diagnostics.
func theme() *ui.Theme {
	if noColor {
		return ui.NoColor(os.Stdout)
	}
	return ui.New(os.Stdout)
}

// noColor is set by the global --no-color flag. It is global rather than a
// per-command flag because it is a property of the caller, not of the command:
// an agent shelling out wants every shale invocation plain, and should not have
// to know which subcommands happen to print styled output.
var noColor bool

// stripGlobalFlags pulls --no-color out of the argument list before the
// per-command FlagSet sees it, so it can be passed to any subcommand without
// each one having to declare it.
func stripGlobalFlags(args []string) []string {
	out := args[:0:0]
	for _, a := range args {
		switch a {
		case "--no-color", "-no-color":
			noColor = true
		default:
			out = append(out, a)
		}
	}
	return out
}

// interactive reports whether there is a real terminal on both ends. Bubble Tea
// needs stdin to read keys and stdout to draw; either one being a pipe means
// this invocation is part of a script, not a session with a person.
func interactive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

func openStore() (*store.DB, error) {
	dir, err := config.StateDir()
	if err != nil {
		return nil, err
	}
	return store.Open(dir)
}

func openWatchStore(dryRun bool) (*store.DB, func(), error) {
	if !dryRun {
		db, err := openStore()
		return db, func() {}, err
	}
	dir, err := config.StateDirPath()
	if err != nil {
		return nil, func() {}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "shale.db")); err == nil {
		db, err := store.OpenReadOnly(dir)
		return db, func() {}, err
	} else if !os.IsNotExist(err) {
		return nil, func() {}, err
	}
	tmp, err := os.MkdirTemp("", "shale-dry-run-")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tmp) }
	db, err := store.Open(tmp)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return db, cleanup, nil
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
