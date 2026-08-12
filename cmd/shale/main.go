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

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
	"github.com/nonlinear-xyz/shale/internal/discover"
	"github.com/nonlinear-xyz/shale/internal/render"
)

const usage = `shale — local memory for coding agents

Usage:
  shale repos               list git repositories found on this machine (local only)
  shale version             print the build version

Not built yet (in progress): init, watch, mcp, link, status.

Run "shale <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Ctrl-C cancels in-flight work cleanly. It matters most during setup, where
	// the user may interrupt a selection wait — an expected, non-error path.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_ = ctx

	var err error
	switch cmd {
	case "repos":
		err = cmdRepos(args)
	case "init", "watch", "mcp", "link", "status":
		// Advertised in the roadmap but not built. Say so precisely rather than
		// letting a half-working command imply the loop is closed.
		err = fmt.Errorf(
			"`shale %s` is not built yet — this build discovers repositories; "+
				"local capture and MCP serving land next", cmd)
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
// their machine before anything is uploaded.
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

	repos := discover.Discover(roots, *depth)
	render.RepoTable(os.Stdout, repos, *showAll)
	return nil
}

// resolveRoots turns the --root flag into absolute paths, defaulting to the
// user's home directory.
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
