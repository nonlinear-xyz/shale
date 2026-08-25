package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/render"
	skillops "github.com/nonlinear-xyz/shale/internal/skills"
	"github.com/nonlinear-xyz/shale/internal/store"
)

const skillUsage = `usage: shale skill <command>

Library:
  shale skill library import <directory> --name <key>
  shale skill library register <git-repo> [--root skills] [--name <key>]
  shale skill library list
  shale skill refresh [--library <key>]

Skills:
  shale skill list [--library <key>] [--status active|draft|retracted|all]
  shale skill show <skill-ref|skill-file-ref>
  shale skill create --library <key> --name <name> --description <text> --file <path|->
  shale skill activate <draft-ref> --description <text>

Learning queue:
  shale skill propose <skill-ref> --lesson <text> [--replacement <path|->]
  shale skill proposals [--status pending|accepted|materialized|applied|rejected|stale|all]
  shale skill proposal show|accept|reject <skill-change-ref>
  shale skill apply <skill-change-ref> [--replacement <path|->]

Installation:
  shale skill target add <name> <absolute-directory>
  shale skill target list
  shale skill install <exact-skill-ref> --target <name>
`

func cmdSkill(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(skillUsage)
	}
	switch args[0] {
	case "library":
		return cmdSkillLibrary(ctx, args[1:])
	case "refresh":
		return cmdSkillRefresh(ctx, args[1:])
	case "list":
		return cmdSkillList(ctx, args[1:])
	case "show":
		return cmdSkillShow(ctx, args[1:])
	case "create":
		return cmdSkillCreate(ctx, args[1:])
	case "activate":
		return cmdSkillActivate(ctx, args[1:])
	case "propose":
		return cmdSkillPropose(ctx, args[1:])
	case "proposals":
		return cmdSkillProposals(ctx, args[1:])
	case "proposal":
		return cmdSkillProposal(ctx, args[1:])
	case "apply":
		return cmdSkillApply(ctx, args[1:])
	case "target":
		return cmdSkillTarget(ctx, args[1:])
	case "install":
		return cmdSkillInstall(ctx, args[1:])
	case "-h", "--help", "help":
		fmt.Print(skillUsage)
		return nil
	default:
		return fmt.Errorf("unknown skill command %q\n\n%s", args[0], skillUsage)
	}
}

func cmdSkillLibrary(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: shale skill library <import|register|list>")
	}
	switch args[0] {
	case "import":
		fs := flag.NewFlagSet("skill library import", flag.ExitOnError)
		name := fs.String("name", "", "portable library key")
		if err := fs.Parse(reorderFlagsFirst(args[1:], map[string]bool{"name": true})); err != nil {
			return err
		}
		if len(fs.Args()) != 1 || strings.TrimSpace(*name) == "" {
			return errors.New("usage: shale skill library import <directory> --name <key>")
		}
		db, err := openStore()
		if err != nil {
			return err
		}
		defer db.Close()
		result, err := skillops.ImportLibrary(ctx, db, fs.Args()[0], *name)
		if err != nil {
			return err
		}
		fmt.Printf("Imported %d skill%s into %s. The original directory was not modified.\n", len(result.Skills), plural(len(result.Skills)), result.Library.Key)
		for _, warning := range result.Warnings {
			fmt.Println(theme().Warn.Render("warning: " + warning))
		}
		render.Skills(os.Stdout, theme(), result.Skills)
		return nil
	case "register":
		fs := flag.NewFlagSet("skill library register", flag.ExitOnError)
		root := fs.String("root", ".", "skill root relative to the Git repository")
		name := fs.String("name", "", "portable library key (defaults to normalized origin remote)")
		valued := map[string]bool{"root": true, "name": true}
		if err := fs.Parse(reorderFlagsFirst(args[1:], valued)); err != nil {
			return err
		}
		if len(fs.Args()) != 1 {
			return errors.New("usage: shale skill library register <git-repo> [--root skills] [--name key]")
		}
		db, err := openStore()
		if err != nil {
			return err
		}
		defer db.Close()
		result, err := skillops.RegisterGitLibrary(ctx, db, fs.Args()[0], *root, *name)
		if err != nil {
			return err
		}
		fmt.Printf("Registered clean Git library %s at %s and indexed %d skill%s.\n", result.Library.Key, result.Library.Head, len(result.Skills), plural(len(result.Skills)))
		for _, warning := range result.Warnings {
			fmt.Println(theme().Warn.Render("warning: " + warning))
		}
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New("usage: shale skill library list")
		}
		db, err := openStore()
		if err != nil {
			return err
		}
		defer db.Close()
		items, err := db.ListSkillLibraries(ctx)
		if err != nil {
			return err
		}
		render.SkillLibraries(os.Stdout, theme(), items)
		return nil
	default:
		return fmt.Errorf("unknown skill library command %q", args[0])
	}
}

func cmdSkillRefresh(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill refresh", flag.ExitOnError)
	library := fs.String("library", "", "refresh one library key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	result := skillops.RefreshLibraries(ctx, db, strings.TrimSpace(*library))
	fmt.Printf("refreshed skill libraries: scanned %d, indexed %d, unchanged %d, retracted %d, applied proposals %d\n",
		result.Scanned, result.Indexed, result.Unchanged, result.Retracted, result.Applied)
	for _, skip := range result.Skipped {
		fmt.Printf("  skip %s — %s\n", skip.Library, skip.Reason)
	}
	for _, item := range result.Errors {
		fmt.Fprintf(os.Stderr, "error: skill refresh: %v\n", item)
	}
	if len(result.Errors) > 0 {
		return fmt.Errorf("%d skill librar%s failed", len(result.Errors), map[bool]string{true: "y", false: "ies"}[len(result.Errors) == 1])
	}
	return nil
}

func cmdSkillList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill list", flag.ExitOnError)
	library := fs.String("library", "", "filter to a library key")
	status := fs.String("status", "active", "active, draft, retracted, or all")
	limit := fs.Int("limit", 200, "maximum results")
	valued := map[string]bool{"library": true, "status": true, "limit": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	parsedStatus, err := parseSkillStatus(*status)
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	items, err := db.ListSkills(ctx, store.SkillFilter{LibraryKey: strings.TrimSpace(*library), Status: parsedStatus, Limit: *limit})
	if err != nil {
		return err
	}
	render.Skills(os.Stdout, theme(), items)
	return nil
}

func cmdSkillShow(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: shale skill show <skill-ref|skill-file-ref>")
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	if ref, err := store.ParseSkillFileRef(args[0]); err == nil {
		body, err := db.ReadSkillFile(ctx, ref.SkillRef, ref.Path)
		if err != nil {
			return err
		}
		render.SkillFile(os.Stdout, theme(), ref, body)
		return nil
	}
	ref, err := store.ParseSkillRef(args[0])
	if err != nil {
		return err
	}
	detail, err := db.ResolveSkillRef(ctx, ref)
	if err != nil {
		return err
	}
	body, err := db.ReadSkillFile(ctx, store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash}, "SKILL.md")
	if err != nil {
		return err
	}
	render.SkillDetail(os.Stdout, theme(), detail, body)
	return nil
}

func cmdSkillCreate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill create", flag.ExitOnError)
	library := fs.String("library", "", "Shale-native library key")
	name := fs.String("name", "", "lowercase hyphen-case skill name")
	description := fs.String("description", "", "routing description and trigger context")
	file := fs.String("file", "", "SKILL.md or instruction body, or - for stdin")
	valued := map[string]bool{"library": true, "name": true, "description": true, "file": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	if *library == "" || *name == "" || *description == "" || *file == "" || len(fs.Args()) != 0 {
		return errors.New("usage: shale skill create --library <key> --name <name> --description <text> --file <path|->")
	}
	body, err := readSkillBytes(*file)
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	detail, warnings, err := skillops.CreateSkill(ctx, db, skillops.CreateInput{LibraryKey: *library, Name: *name, Description: *description, Body: body})
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Println(theme().Warn.Render("warning: " + warning))
	}
	core, _ := db.ReadSkillFile(ctx, store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash}, "SKILL.md")
	render.SkillDetail(os.Stdout, theme(), detail, core)
	return nil
}

func cmdSkillActivate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill activate", flag.ExitOnError)
	description := fs.String("description", "", "confirmed routing description")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"description": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 || *description == "" {
		return errors.New("usage: shale skill activate <draft-ref> --description <text>")
	}
	ref, err := store.ParseSkillRef(fs.Args()[0])
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	detail, warnings, err := skillops.ActivateDraft(ctx, db, ref, *description)
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Println(theme().Warn.Render("warning: " + warning))
	}
	core, _ := db.ReadSkillFile(ctx, store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash}, "SKILL.md")
	render.SkillDetail(os.Stdout, theme(), detail, core)
	return nil
}

func cmdSkillPropose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill propose", flag.ExitOnError)
	lesson := fs.String("lesson", "", "what was learned and should persist")
	rationale := fs.String("rationale", "", "why the skill should change")
	replacement := fs.String("replacement", "", "complete replacement SKILL.md, or - for stdin")
	evidence := fs.String("evidence", "", "comma-separated Shale refs")
	valued := map[string]bool{"lesson": true, "rationale": true, "replacement": true, "evidence": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	if len(fs.Args()) != 1 || strings.TrimSpace(*lesson) == "" {
		return errors.New("usage: shale skill propose <skill-ref> --lesson <text> [--replacement path|-]")
	}
	ref, err := store.ParseSkillRef(fs.Args()[0])
	if err != nil {
		return err
	}
	var body []byte
	if *replacement != "" {
		body, err = readSkillBytes(*replacement)
		if err != nil {
			return err
		}
	}
	refs := splitComma(*evidence)
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	change, warnings, err := skillops.ProposeChange(ctx, db, skillops.ProposalInput{
		Ref: ref, Lesson: *lesson, Rationale: *rationale, EvidenceRefs: refs,
		Replacement: body, Source: "cli", Actor: "human",
	})
	if err != nil {
		return err
	}
	for _, warning := range warnings {
		fmt.Println(theme().Warn.Render("warning: " + warning))
	}
	render.SkillChangeDetail(os.Stdout, theme(), change)
	fmt.Println(theme().Hint.Render("Review with `shale skill proposal show " + change.Ref() + "`, then accept explicitly."))
	return nil
}

func cmdSkillProposals(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill proposals", flag.ExitOnError)
	status := fs.String("status", "pending", "pending, accepted, materialized, applied, rejected, stale, or all")
	library := fs.String("library", "", "filter to a library key")
	valued := map[string]bool{"status": true, "library": true}
	if err := fs.Parse(reorderFlagsFirst(args, valued)); err != nil {
		return err
	}
	parsed, err := parseSkillChangeStatus(*status)
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	items, err := db.ListSkillChanges(ctx, store.SkillChangeFilter{Status: parsed, LibraryKey: strings.TrimSpace(*library)})
	if err != nil {
		return err
	}
	render.SkillChanges(os.Stdout, theme(), items)
	return nil
}

func cmdSkillProposal(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return errors.New("usage: shale skill proposal show|accept|reject <skill-change-ref>")
	}
	ref, err := store.ParseSkillChangeRef(args[1])
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	var change store.SkillChange
	switch args[0] {
	case "show":
		change, err = db.SkillChange(ctx, ref.ID)
	case "accept":
		change, err = db.AcceptSkillChange(ctx, ref.ID, "human")
	case "reject":
		change, err = db.RejectSkillChange(ctx, ref.ID, "human")
	default:
		return fmt.Errorf("unknown skill proposal command %q", args[0])
	}
	if err != nil {
		return err
	}
	render.SkillChangeDetail(os.Stdout, theme(), change)
	if args[0] == "accept" && change.ReplacementBlobHash == "" {
		fmt.Println(theme().Hint.Render("Lesson accepted. Behavior is unchanged until a replacement is attached and applied."))
	}
	if args[0] == "reject" {
		fmt.Println(theme().Hint.Render("Proposal content and replacement bytes were destroyed; its audit tombstone remains."))
	}
	return nil
}

func cmdSkillApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill apply", flag.ExitOnError)
	replacement := fs.String("replacement", "", "complete replacement SKILL.md, or - for stdin")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"replacement": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 {
		return errors.New("usage: shale skill apply <skill-change-ref> [--replacement path|-]")
	}
	ref, err := store.ParseSkillChangeRef(fs.Args()[0])
	if err != nil {
		return err
	}
	var body []byte
	if *replacement != "" {
		body, err = readSkillBytes(*replacement)
		if err != nil {
			return err
		}
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := skillops.ApplyChange(ctx, db, ref.ID, body)
	if err != nil {
		return err
	}
	render.SkillChangeDetail(os.Stdout, theme(), result.Change)
	if result.Skill != nil {
		fmt.Printf("\nCreated native revision %s. Install it explicitly to activate it in an agent harness.\n", result.Skill.VersionedRef())
	}
	if result.Worktree != "" {
		fmt.Printf("\nMaterialized the reviewed one-file edit in %s on branch %s. No commit, push, or PR was created.\n", result.Worktree, result.Branch)
		for _, command := range result.Guidance {
			fmt.Printf("Detected validation guidance (not run): %s\n", command)
		}
	}
	return nil
}

func cmdSkillTarget(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: shale skill target add|list ...")
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	switch args[0] {
	case "add":
		if len(args) != 3 {
			return errors.New("usage: shale skill target add <name> <absolute-directory>")
		}
		target, err := skillops.AddTarget(ctx, db, args[1], args[2])
		if err != nil {
			return err
		}
		fmt.Printf("Added skill target %s at %s.\n", target.Name, target.Path)
		return nil
	case "list":
		if len(args) != 1 {
			return errors.New("usage: shale skill target list")
		}
		items, err := db.ListSkillTargets(ctx)
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No skill targets.")
			return nil
		}
		for _, item := range items {
			fmt.Printf("%s\t%s\n", item.Name, item.Path)
		}
		return nil
	default:
		return fmt.Errorf("unknown skill target command %q", args[0])
	}
}

func cmdSkillInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("skill install", flag.ExitOnError)
	target := fs.String("target", "", "named install target")
	if err := fs.Parse(reorderFlagsFirst(args, map[string]bool{"target": true})); err != nil {
		return err
	}
	if len(fs.Args()) != 1 || *target == "" {
		return errors.New("usage: shale skill install <exact-skill-ref> --target <name>")
	}
	ref, err := store.ParseSkillRef(fs.Args()[0])
	if err != nil {
		return err
	}
	db, err := openStore()
	if err != nil {
		return err
	}
	defer db.Close()
	result, err := skillops.Install(ctx, db, ref, *target)
	if err != nil {
		return err
	}
	fmt.Printf("Installed %s at %s.\n", ref.String(), result.Installation.InstalledPath)
	if result.PreviousTree != "" && result.PreviousTree != result.Installation.TreeHash {
		fmt.Printf("Replaced managed revision %s; reinstall that exact ref to roll back.\n", result.PreviousTree)
	}
	for _, warning := range result.Warnings {
		fmt.Println(theme().Warn.Render("warning: " + warning))
	}
	return nil
}

func readSkillBytes(path string) ([]byte, error) {
	var reader io.Reader
	var closer io.Closer
	if path == "-" {
		reader = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		reader, closer = file, file
	}
	if closer != nil {
		defer closer.Close()
	}
	body, err := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > 1<<20 {
		return nil, errors.New("skill input must be between 1 byte and 1 MiB")
	}
	return body, nil
}

func splitComma(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func parseSkillStatus(value string) (store.SkillStatus, error) {
	switch store.SkillStatus(strings.TrimSpace(value)) {
	case store.SkillActive:
		return store.SkillActive, nil
	case store.SkillDraft:
		return store.SkillDraft, nil
	case store.SkillRetracted:
		return store.SkillRetracted, nil
	case "", "all":
		return "", nil
	default:
		return "", errors.New("--status must be active, draft, retracted, or all")
	}
}

func parseSkillChangeStatus(value string) (store.SkillChangeStatus, error) {
	switch store.SkillChangeStatus(strings.TrimSpace(value)) {
	case store.SkillChangePending, store.SkillChangeAccepted, store.SkillChangeMaterialized,
		store.SkillChangeApplied, store.SkillChangeRejected, store.SkillChangeStale:
		return store.SkillChangeStatus(strings.TrimSpace(value)), nil
	case "", "all":
		return "", nil
	default:
		return "", errors.New("--status must be pending, accepted, materialized, applied, rejected, stale, or all")
	}
}
