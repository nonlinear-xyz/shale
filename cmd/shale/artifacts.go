package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	artifactindex "github.com/nonlinear-xyz/shale/internal/artifacts"
	"github.com/nonlinear-xyz/shale/internal/render"
	"github.com/nonlinear-xyz/shale/internal/store"
)

func cmdRemember(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("remember", flag.ExitOnError)
	title := fs.String("title", "", "short memory title")
	trigger := fs.String("trigger", "", "when this memory should be recalled")
	scope := fs.String("scope", "", "user, repo, or task (inferred from --task/--repo when omitted)")
	repo := fs.String("repo", "", `repository scope (usually "owner/name")`)
	taskKey := fs.String("task", "", "stable task key for task scope")
	file := fs.String("file", "", "read memory text from a file, or - for stdin")
	evidence := fs.String("evidence", "", "comma-separated Shale refs supporting this memory")
	valued := map[string]bool{"title": true, "trigger": true, "scope": true, "repo": true, "task": true, "file": true, "evidence": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	body, err := readArtifactInput(*file, fs.Args())
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("usage: shale remember <text> [--scope user|repo|task] [--repo owner/name] [--task key]")
	}
	scopeKind, scopeKey, repoValue, err := cliScope(*scope, *repo, *taskKey)
	if err != nil {
		return err
	}
	refs, err := parseEvidenceRefs(*evidence)
	if err != nil {
		return err
	}
	contentTaskKey := ""
	if scopeKind == store.ScopeTask {
		contentTaskKey = strings.TrimSpace(*taskKey)
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	a, _, err := db.PutArtifact(ctx, store.ArtifactInput{
		Kind: store.ArtifactMemory, Status: store.ArtifactActive,
		ScopeKind: scopeKind, ScopeKey: scopeKey, Repo: repoValue,
		Title: *title, Origin: "native", Authority: "asserted",
		Source: "cli", Actor: "human", EventKind: store.KindMemoryAsserted,
		Content: store.ArtifactContent{
			Text: body, Trigger: strings.TrimSpace(*trigger), TaskKey: contentTaskKey,
			EvidenceRefs: refs,
		},
	})
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdMemories(ctx context.Context, args []string) error {
	return cmdArtifactList(ctx, "memories", store.ArtifactMemory, store.ArtifactActive, args, true)
}

func cmdProposals(ctx context.Context, args []string) error {
	return cmdArtifactList(ctx, "proposals", store.ArtifactMemory, store.ArtifactPending, args, false)
}

func cmdCheckpoints(ctx context.Context, args []string) error {
	return cmdArtifactList(ctx, "checkpoints", store.ArtifactCheckpoint, store.ArtifactActive, args, false)
}

func cmdArtifactList(ctx context.Context, noun string, kind store.ArtifactKind, defaultStatus store.ArtifactStatus, args []string, statusFlag bool) error {
	fs := flag.NewFlagSet(noun, flag.ExitOnError)
	repo := fs.String("repo", "", "filter to a repository")
	scope := fs.String("scope", "", "filter to user, repo, or task scope")
	taskKey := fs.String("task", "", "filter to a task key")
	limit := fs.Int("limit", 100, "maximum results")
	var status *string
	if statusFlag {
		status = fs.String("status", string(defaultStatus), "active, pending, retracted, rejected, purged, or all")
	}
	valued := map[string]bool{"repo": true, "scope": true, "task": true, "limit": true, "status": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	filter := store.ArtifactFilter{Kind: kind, Status: defaultStatus, Repo: strings.TrimSpace(*repo), Limit: *limit}
	if status != nil {
		parsed, err := parseArtifactStatus(*status)
		if err != nil {
			return err
		}
		filter.Status = parsed
	}
	if *scope != "" {
		filter.ScopeKind = store.ScopeKind(strings.TrimSpace(*scope))
		switch filter.ScopeKind {
		case store.ScopeUser, store.ScopeRepo, store.ScopeTask:
		default:
			return errors.New("--scope must be user, repo, or task")
		}
	}
	if *taskKey != "" {
		filter.ScopeKind, filter.ScopeKey = store.ScopeTask, strings.TrimSpace(*taskKey)
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	items, err := db.ListArtifacts(ctx, filter)
	if err != nil {
		return err
	}
	render.ArtifactList(os.Stdout, theme(), noun, items)
	return nil
}

func cmdAccept(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("accept", flag.ExitOnError)
	file := fs.String("file", "", "replace the proposal text from a file, or - for stdin")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"file": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: shale accept memory:<id> [--file path|-]")
	}
	ref, err := mutableArtifactRef(fs.Args()[0], store.ArtifactMemory)
	if err != nil {
		return err
	}
	var replacement string
	if *file != "" {
		body, err := readArtifactInput(*file, nil)
		if err != nil {
			return err
		}
		replacement = body
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	var edited *store.ArtifactContent
	if replacement != "" {
		current, err := db.Artifact(ctx, ref.ID)
		if err != nil {
			return err
		}
		content := current.Content
		content.Text = replacement
		edited = &content
	}
	a, err := db.AcceptMemory(ctx, ref.ID, edited, "human")
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdReject(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: shale reject memory:<id>")
	}
	ref, err := mutableArtifactRef(args[0], store.ArtifactMemory)
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	a, err := db.RejectMemory(ctx, ref.ID, "human")
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdSupersede(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("supersede", flag.ExitOnError)
	title := fs.String("title", "", "replacement title (keeps the current title when omitted)")
	file := fs.String("file", "", "read replacement text from a file, or - for stdin")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"title": true, "file": true})); err != nil {
		return err
	}
	if len(fs.Args()) < 1 {
		return errors.New("usage: shale supersede memory:<id> <replacement text> [--file path|-]")
	}
	ref, err := mutableArtifactRef(fs.Args()[0], store.ArtifactMemory)
	if err != nil {
		return err
	}
	body, err := readArtifactInput(*file, fs.Args()[1:])
	if err != nil {
		return err
	}
	if body == "" {
		return errors.New("replacement memory text is required")
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	current, err := db.Artifact(ctx, ref.ID)
	if err != nil {
		return err
	}
	content := current.Content
	content.Text = body
	a, err := db.SupersedeMemory(ctx, ref.ID, content, *title, "human")
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdForget(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: shale forget <memory|checkpoint|runbook>:<id>")
	}
	ref, err := mutableArtifactRef(args[0], "")
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := requireNativeArtifact(ctx, db, ref); err != nil {
		return err
	}
	a, err := db.RetractArtifact(ctx, ref.ID, "human")
	if err != nil {
		return err
	}
	th := theme()
	render.ArtifactDetail(os.Stdout, th, a)
	fmt.Println(th.Hint.Render("Retracted from recall. Use `shale purge " + a.Ref() + " --yes` to destroy stored versions."))
	return nil
}

func cmdPurge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	yes := fs.Bool("yes", false, "confirm irreversible deletion without prompting")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: shale purge <artifact-ref> --yes")
	}
	ref, err := mutableArtifactRef(fs.Args()[0], "")
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	a, err := requirePurgeableArtifact(ctx, db, ref)
	if err != nil {
		return err
	}
	confirmed, err := confirmPurge(a, *yes)
	if err != nil {
		return err
	}
	if !confirmed {
		fmt.Println("Purge cancelled.")
		return nil
	}
	a, err = db.PurgeArtifact(ctx, ref.ID, "human")
	if err != nil {
		return err
	}
	if a.Origin != "native" && a.SourcePointer != "" {
		if err := db.RemoveArtifactSource(ctx, a.SourcePointer); err != nil {
			return fmt.Errorf("content was purged, but its source registration could not be removed: %w", err)
		}
	}
	fmt.Printf("Purged %s. Stored content and every Shale-managed version were destroyed; the metadata tombstone remains.\n", a.Ref())
	return nil
}

func cmdRunbook(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: shale runbook <create|register|revise|list> ...")
	}
	switch args[0] {
	case "create":
		return cmdRunbookCreate(ctx, args[1:])
	case "register":
		return cmdRunbookRegister(ctx, args[1:])
	case "revise":
		return cmdRunbookRevise(ctx, args[1:])
	case "list":
		return cmdArtifactList(ctx, "runbooks", store.ArtifactRunbook, store.ArtifactActive, args[1:], false)
	default:
		return fmt.Errorf("unknown runbook command %q (want create, register, revise, or list)", args[0])
	}
}

func cmdRunbookCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runbook create", flag.ExitOnError)
	file := fs.String("file", "", "runbook Markdown file, or - for stdin (required)")
	title := fs.String("title", "", "short runbook title")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"file": true, "title": true})); err != nil {
		return err
	}
	if *file == "" || len(fs.Args()) != 0 {
		return errors.New("usage: shale runbook create --file path|- [--title title]")
	}
	body, err := readArtifactInput(*file, nil)
	if err != nil {
		return err
	}
	name := strings.TrimSpace(*title)
	if name == "" && *file != "-" {
		name = filepath.Base(*file)
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	a, _, err := db.PutArtifact(ctx, store.ArtifactInput{
		Kind: store.ArtifactRunbook, Status: store.ArtifactActive,
		ScopeKind: store.ScopeUser, ScopeKey: "local", Title: name,
		Origin: "native", Authority: "asserted", Source: "cli", Actor: "human",
		EventKind: store.KindRunbookCreated, Content: store.ArtifactContent{Text: body},
	})
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdRunbookRegister(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runbook register", flag.ExitOnError)
	repo := fs.String("repo", "", `repository scope override (default: origin "owner/name", then worktree path)`)
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"repo": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: shale runbook register <path> [--repo owner/name]")
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	source, err := artifactindex.RegisterRunbook(ctx, db, fs.Args()[0], *repo)
	if err != nil {
		return err
	}
	if err := refreshArtifacts(ctx, db, false); err != nil {
		return err
	}
	a, err := db.Artifact(ctx, source.ArtifactID)
	if err != nil {
		return fmt.Errorf("registered source was not indexed: %w", err)
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdRunbookRevise(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("runbook revise", flag.ExitOnError)
	file := fs.String("file", "", "replacement Markdown file, or - for stdin (required)")
	title := fs.String("title", "", "replacement title (keeps current title when omitted)")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"file": true, "title": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 || *file == "" {
		return errors.New("usage: shale runbook revise runbook:<id> --file path|- [--title title]")
	}
	ref, err := mutableArtifactRef(fs.Args()[0], store.ArtifactRunbook)
	if err != nil {
		return err
	}
	body, err := readArtifactInput(*file, nil)
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	current, err := requireNativeArtifact(ctx, db, ref)
	if err != nil {
		return err
	}
	if current.Kind != store.ArtifactRunbook || current.Status != store.ArtifactActive {
		return fmt.Errorf("%s is not an active native runbook", current.Ref())
	}
	name := strings.TrimSpace(*title)
	if name == "" {
		name = current.Title
	}
	a, _, err := db.PutArtifact(ctx, store.ArtifactInput{
		ID: current.ID, Kind: current.Kind, Status: store.ArtifactActive,
		ScopeKind: current.ScopeKind, ScopeKey: current.ScopeKey, Repo: current.Repo,
		Title: name, Origin: current.Origin, Authority: "asserted", Source: current.Source,
		Actor: "human", EventKind: store.KindRunbookRevised,
		Content: store.ArtifactContent{Text: body},
	})
	if err != nil {
		return err
	}
	render.ArtifactDetail(os.Stdout, theme(), a)
	return nil
}

func cmdRefresh(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("refresh", flag.ExitOnError)
	verbose := fs.Bool("verbose", false, "print every skipped source")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	return refreshArtifacts(ctx, db, *verbose)
}

// refreshArtifacts is shared by `refresh`, `watch`, and runbook registration.
// It indexes snapshots only; canonical Claude, Codex, and Git files are never
// modified.
func refreshArtifacts(ctx context.Context, db *store.DB, verbose bool) error {
	result := artifactindex.Refresh(ctx, db, artifactindex.Options{})
	fmt.Printf("refreshed durable sources: scanned %d, indexed %d, unchanged %d, retracted %d\n",
		result.Scanned, result.Indexed, result.Unchanged, result.Removed)
	if verbose {
		for _, skip := range result.Skipped {
			fmt.Printf("  skip %s — %s\n", skip.Path, skip.Reason)
		}
	} else if len(result.Skipped) > 0 {
		fmt.Printf("skipped %d source%s (run with --verbose for reasons)\n", len(result.Skipped), plural(len(result.Skipped)))
	}
	for _, err := range result.Errors {
		fmt.Fprintf(os.Stderr, "error: refresh: %v\n", err)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%d durable source%s failed", len(result.Errors), plural(len(result.Errors)))
	}
	return nil
}

func readArtifactInput(file string, words []string) (string, error) {
	if file != "" && len(words) > 0 {
		return "", errors.New("provide text or --file, not both")
	}
	if file == "" {
		return strings.TrimSpace(strings.Join(words, " ")), nil
	}
	var reader io.Reader
	var closeFile io.Closer
	if file == "-" {
		reader = os.Stdin
	} else {
		f, err := os.Open(file)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", file, err)
		}
		reader, closeFile = f, f
	}
	if closeFile != nil {
		defer closeFile.Close()
	}
	const maxInput = store.ArtifactContentMax - 4096
	raw, err := io.ReadAll(io.LimitReader(reader, maxInput+1))
	if err != nil {
		return "", err
	}
	if len(raw) > maxInput {
		return "", fmt.Errorf("artifact input exceeds %d bytes", maxInput)
	}
	body := strings.TrimSpace(string(raw))
	if body == "" {
		return "", errors.New("artifact content is empty")
	}
	return body, nil
}

func cliScope(scope, repo, taskKey string) (store.ScopeKind, string, string, error) {
	scope, repo, taskKey = strings.TrimSpace(scope), strings.TrimSpace(repo), strings.TrimSpace(taskKey)
	if scope == "" {
		switch {
		case taskKey != "":
			scope = string(store.ScopeTask)
		case repo != "":
			scope = string(store.ScopeRepo)
		default:
			scope = string(store.ScopeUser)
		}
	}
	switch store.ScopeKind(scope) {
	case store.ScopeUser:
		return store.ScopeUser, "local", "", nil
	case store.ScopeRepo:
		if repo == "" {
			return "", "", "", errors.New("repo scope requires --repo")
		}
		return store.ScopeRepo, repo, repo, nil
	case store.ScopeTask:
		if taskKey == "" {
			return "", "", "", errors.New("task scope requires --task")
		}
		return store.ScopeTask, taskKey, repo, nil
	default:
		return "", "", "", errors.New("--scope must be user, repo, or task")
	}
}

func parseEvidenceRefs(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var refs []string
	for _, value := range strings.Split(value, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, err := store.ParseArtifactRef(value); err != nil {
			if _, transcriptErr := store.ParseRef(value); transcriptErr != nil {
				return nil, fmt.Errorf("invalid evidence ref %q", value)
			}
		}
		refs = append(refs, value)
	}
	return refs, nil
}

func parseArtifactStatus(value string) (store.ArtifactStatus, error) {
	switch status := store.ArtifactStatus(strings.TrimSpace(value)); status {
	case "", "all":
		return "", nil
	case store.ArtifactActive, store.ArtifactPending, store.ArtifactRetracted, store.ArtifactRejected, store.ArtifactPurged:
		return status, nil
	default:
		return "", fmt.Errorf("unknown status %q", value)
	}
}

func mutableArtifactRef(value string, want store.ArtifactKind) (store.ArtifactRef, error) {
	ref, err := store.ParseArtifactRef(value)
	if err != nil {
		return ref, err
	}
	if ref.EventSeq != 0 {
		return ref, errors.New("lifecycle commands require a stable ref without @version")
	}
	if want != "" && ref.Kind != want {
		return ref, fmt.Errorf("expected a %s ref, got %s", want, ref.Kind)
	}
	return ref, nil
}

func requireNativeArtifact(ctx context.Context, db *store.DB, ref store.ArtifactRef) (store.Artifact, error) {
	a, err := db.Artifact(ctx, ref.ID)
	if err != nil {
		return a, err
	}
	if a.Kind != ref.Kind {
		return store.Artifact{}, store.ErrArtifactNotFound
	}
	if a.Origin != "native" {
		message := fmt.Sprintf("%s is managed by %s", a.Ref(), a.Source)
		if a.SourcePointer != "" {
			message += "; edit or remove its canonical source at " + a.SourcePointer + " and run `shale refresh`"
		}
		return a, errors.New(message)
	}
	return a, nil
}

func requirePurgeableArtifact(ctx context.Context, db *store.DB, ref store.ArtifactRef) (store.Artifact, error) {
	a, err := db.Artifact(ctx, ref.ID)
	if err != nil {
		return a, err
	}
	if a.Kind != ref.Kind {
		return store.Artifact{}, store.ErrArtifactNotFound
	}
	if a.Origin == "native" {
		return a, nil
	}
	if a.SourcePointer == "" {
		return a, fmt.Errorf("%s is externally managed and has no removable canonical source", a.Ref())
	}
	if _, err := os.Stat(a.SourcePointer); err == nil {
		return a, fmt.Errorf("%s is managed by %s; remove its canonical source at %s and run `shale refresh` before purging the snapshot", a.Ref(), a.Source, a.SourcePointer)
	} else if !os.IsNotExist(err) {
		return a, fmt.Errorf("cannot verify canonical source %s: %w", a.SourcePointer, err)
	}
	return a, nil
}

func confirmPurge(a store.Artifact, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if !interactive() {
		return false, errors.New("purge is irreversible; pass --yes in non-interactive use")
	}
	fmt.Printf("Permanently destroy every stored version of %s (%q)? [y/N] ", a.Ref(), a.Title)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
