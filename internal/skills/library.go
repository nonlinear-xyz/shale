package skills

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/store"
)

type LibraryResult struct {
	Library  store.SkillLibrary
	Skills   []store.Skill
	Warnings []string
}

type RefreshResult struct {
	Libraries int
	Scanned   int
	Indexed   int
	Unchanged int
	Retracted int
	Applied   int
	Skipped   []RefreshSkip
	Errors    []error
}

type RefreshSkip struct {
	Library string
	Reason  string
}

func ImportLibrary(ctx context.Context, db *store.DB, directory, key string) (LibraryResult, error) {
	key = strings.TrimSpace(key)
	if !store.ValidLibraryKey(key) {
		return LibraryResult{}, fmt.Errorf("invalid library key %q", key)
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return LibraryResult{}, err
	}
	snapshots, err := discoverSnapshots(root, true)
	if err != nil {
		return LibraryResult{}, err
	}
	lib, _, err := db.RegisterSkillLibrary(ctx, store.SkillLibraryInput{
		Key: key, Kind: store.SkillLibraryNative, Actor: "human",
	})
	if err != nil {
		return LibraryResult{}, err
	}
	result := LibraryResult{Library: lib}
	for _, snapshot := range snapshots {
		skill, _, _, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
			LibraryID: lib.ID, Name: snapshot.Name, Status: snapshot.Status,
			Description: snapshot.Description, Actor: "human", Files: snapshot.Files,
		})
		if err != nil {
			return result, fmt.Errorf("import %s: %w", snapshot.Name, err)
		}
		result.Skills = append(result.Skills, skill)
		for _, warning := range snapshot.Warnings {
			result.Warnings = append(result.Warnings, snapshot.Name+": "+warning)
		}
	}
	return result, nil
}

func RegisterGitLibrary(ctx context.Context, db *store.DB, repository, skillsRoot, key string) (LibraryResult, error) {
	repo, err := gitOutput(ctx, repository, "rev-parse", "--show-toplevel")
	if err != nil {
		return LibraryResult{}, errors.New("skill library source is not a Git worktree")
	}
	repo, err = filepath.EvalSymlinks(strings.TrimSpace(repo))
	if err != nil {
		return LibraryResult{}, err
	}
	if IsPluginCachePath(repo) {
		return LibraryResult{}, errors.New("plugin cache copies are read-only deployment artifacts; import a snapshot or register their canonical Git repository")
	}
	if strings.TrimSpace(skillsRoot) == "" {
		skillsRoot = "."
	}
	skillsRoot = filepath.ToSlash(filepath.Clean(skillsRoot))
	if filepath.IsAbs(skillsRoot) || skillsRoot == ".." || strings.HasPrefix(skillsRoot, "../") {
		return LibraryResult{}, errors.New("--root must stay inside the Git repository")
	}
	rootPath := filepath.Join(repo, filepath.FromSlash(skillsRoot))
	if err := ensureWithin(repo, rootPath); err != nil {
		return LibraryResult{}, err
	}
	if err := requireCleanGitRoot(ctx, repo, skillsRoot); err != nil {
		return LibraryResult{}, err
	}
	head, err := gitOutput(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return LibraryResult{}, fmt.Errorf("read Git HEAD: %w", err)
	}
	head = strings.TrimSpace(head)
	remote, _ := gitOutput(ctx, repo, "remote", "get-url", "origin")
	remote = strings.TrimSpace(remote)
	if strings.TrimSpace(key) == "" {
		key = NormalizeGitRemote(remote)
		if key == "" {
			return LibraryResult{}, errors.New("--name is required when the repository has no portable origin remote")
		}
	}
	if !store.ValidLibraryKey(key) {
		return LibraryResult{}, fmt.Errorf("invalid library key %q", key)
	}
	snapshots, err := discoverSnapshots(rootPath, false)
	if err != nil {
		return LibraryResult{}, err
	}
	if err := requireTrackedSnapshots(ctx, repo, skillsRoot, snapshots); err != nil {
		return LibraryResult{}, err
	}
	lib, _, err := db.RegisterSkillLibrary(ctx, store.SkillLibraryInput{
		Key: key, Kind: store.SkillLibraryGit, SourcePath: repo,
		SkillsRoot: skillsRoot, Remote: NormalizeRemoteURL(remote), Head: head, Actor: "human",
	})
	if err != nil {
		return LibraryResult{}, err
	}
	return indexSnapshots(ctx, db, lib, snapshots, head, "human")
}

func RefreshLibraries(ctx context.Context, db *store.DB, onlyKey string) RefreshResult {
	result := RefreshResult{}
	libraries, err := db.ListSkillLibraries(ctx)
	if err != nil {
		result.Errors = append(result.Errors, err)
		return result
	}
	for _, lib := range libraries {
		if onlyKey != "" && lib.Key != onlyKey {
			continue
		}
		result.Libraries++
		if lib.Kind == store.SkillLibraryNative {
			result.Skipped = append(result.Skipped, RefreshSkip{Library: lib.Key, Reason: "native revisions are already canonical in Shale"})
			continue
		}
		if err := requireCleanGitRoot(ctx, lib.SourcePath, lib.SkillsRoot); err != nil {
			result.Skipped = append(result.Skipped, RefreshSkip{Library: lib.Key, Reason: err.Error() + "; retained the last clean snapshot"})
			continue
		}
		head, err := gitOutput(ctx, lib.SourcePath, "rev-parse", "HEAD")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: read Git HEAD: %w", lib.Key, err))
			continue
		}
		head = strings.TrimSpace(head)
		root := filepath.Join(lib.SourcePath, filepath.FromSlash(lib.SkillsRoot))
		snapshots, err := discoverSnapshots(root, false)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", lib.Key, err))
			continue
		}
		if err := requireTrackedSnapshots(ctx, lib.SourcePath, lib.SkillsRoot, snapshots); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", lib.Key, err))
			continue
		}
		result.Scanned += len(snapshots)
		var names []string
		failed := false
		for _, snapshot := range snapshots {
			skill, _, inserted, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
				LibraryID: lib.ID, Name: snapshot.Name, Status: snapshot.Status,
				Description: snapshot.Description, SourceHead: head, Actor: "human", Files: snapshot.Files,
			})
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s/%s: %w", lib.Key, snapshot.Name, err))
				failed = true
				break
			}
			names = append(names, snapshot.Name)
			if inserted {
				result.Indexed++
			} else {
				result.Unchanged++
			}
			observed, err := db.ObserveMaterializedSkillChanges(ctx, lib.ID, skill.Name, skill.TreeHash)
			if err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("%s/%s observe applied proposal: %w", lib.Key, snapshot.Name, err))
				failed = true
				break
			}
			result.Applied += observed
		}
		if failed {
			continue
		}
		retracted, err := db.RetractMissingSkills(ctx, lib.ID, names, "human")
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s retract missing skills: %w", lib.Key, err))
			continue
		}
		result.Retracted += retracted
		if err := db.UpdateSkillLibraryHead(ctx, lib.ID, head); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s update head: %w", lib.Key, err))
		}
	}
	if onlyKey != "" && result.Libraries == 0 {
		result.Errors = append(result.Errors, fmt.Errorf("skill library %q not found", onlyKey))
	}
	return result
}

func indexSnapshots(ctx context.Context, db *store.DB, lib store.SkillLibrary, snapshots []Snapshot, head, actor string) (LibraryResult, error) {
	result := LibraryResult{Library: lib}
	for _, snapshot := range snapshots {
		skill, _, _, err := db.PutSkillRevision(ctx, store.SkillRevisionInput{
			LibraryID: lib.ID, Name: snapshot.Name, Status: snapshot.Status,
			Description: snapshot.Description, SourceHead: head, Actor: actor, Files: snapshot.Files,
		})
		if err != nil {
			return result, err
		}
		result.Skills = append(result.Skills, skill)
		for _, warning := range snapshot.Warnings {
			result.Warnings = append(result.Warnings, snapshot.Name+": "+warning)
		}
	}
	return result, nil
}

func discoverSnapshots(root string, allowLoose bool) ([]Snapshot, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("skill library root must be a real directory")
	}
	if _, err := os.Lstat(filepath.Join(root, "SKILL.md")); err == nil {
		snapshot, err := SnapshotDir(root, filepath.Base(root))
		if err != nil {
			return nil, err
		}
		return []Snapshot{snapshot}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seen := map[string]bool{}
	var snapshots []Snapshot
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.IsDir() {
			if _, err := os.Lstat(filepath.Join(path, "SKILL.md")); err != nil {
				continue
			}
			snapshot, err := SnapshotDir(path, entry.Name())
			if err != nil {
				return nil, fmt.Errorf("%s: %w", entry.Name(), err)
			}
			if seen[snapshot.Name] {
				return nil, fmt.Errorf("duplicate skill name %q", snapshot.Name)
			}
			seen[snapshot.Name] = true
			snapshots = append(snapshots, snapshot)
			continue
		}
		if !allowLoose || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		base := strings.ToLower(entry.Name())
		if base == "readme.md" || base == "changelog.md" || base == "license.md" {
			continue
		}
		snapshot, err := LooseDraft(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if seen[snapshot.Name] {
			return nil, fmt.Errorf("duplicate skill name %q", snapshot.Name)
		}
		seen[snapshot.Name] = true
		snapshots = append(snapshots, snapshot)
	}
	if len(snapshots) == 0 {
		return nil, errors.New("no skill directories or loose Markdown drafts found")
	}
	return snapshots, nil
}

func requireCleanGitRoot(ctx context.Context, repo, root string) error {
	out, err := gitOutput(ctx, repo, "status", "--porcelain", "--untracked-files=all", "--", root)
	if err != nil {
		return fmt.Errorf("inspect Git source: %w", err)
	}
	if strings.TrimSpace(out) != "" {
		return errors.New("Git skill source is dirty")
	}
	return nil
}

func requireTrackedSnapshots(ctx context.Context, repo, root string, snapshots []Snapshot) error {
	out, err := gitOutput(ctx, repo, "ls-files", "--cached", "-z", "--", root)
	if err != nil {
		return fmt.Errorf("list committed skill files: %w", err)
	}
	tracked := map[string]bool{}
	for _, path := range strings.Split(out, "\x00") {
		if path = filepath.ToSlash(path); path != "" {
			tracked[path] = true
		}
	}
	for _, snapshot := range snapshots {
		for _, file := range snapshot.Files {
			absolute := filepath.Join(snapshot.SourcePath, filepath.FromSlash(file.Path))
			rel, err := filepath.Rel(repo, absolute)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if !tracked[rel] {
				return fmt.Errorf("skill file %s is not committed; Git libraries require fully tracked package trees", rel)
			}
		}
	}
	return nil
}

func gitOutput(ctx context.Context, directory string, args ...string) (string, error) {
	all := append([]string{"-C", directory}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	body, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = err.Error()
		}
		return "", errors.New(message)
	}
	return string(body), nil
}

func NormalizeGitRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, "file:") {
		return ""
	}
	path, host := "", ""
	if strings.HasPrefix(remote, "git@") && strings.Contains(remote, ":") {
		parts := strings.SplitN(remote, ":", 2)
		host = strings.TrimPrefix(parts[0], "git@")
		path = parts[1]
	} else if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		host, path = parsed.Hostname(), strings.TrimPrefix(parsed.Path, "/")
	} else {
		return ""
	}
	path = strings.TrimSuffix(path, ".git")
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	if host != "github.com" && host != "gitlab.com" && host != "bitbucket.org" {
		path = host + "/" + path
	}
	path = strings.ToLower(path)
	if !store.ValidLibraryKey(path) {
		return ""
	}
	return path
}

func NormalizeRemoteURL(remote string) string {
	// Remotes are portable provenance, but credentials embedded in a URL are not.
	remote = strings.TrimSpace(remote)
	if remote == "" || filepath.IsAbs(remote) || strings.HasPrefix(strings.ToLower(remote), "file:") {
		return ""
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		parsed.User = nil
		parsed.RawQuery, parsed.Fragment = "", ""
		return parsed.String()
	}
	return remote
}

func ensureWithin(root, target string) error {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes its configured root")
	}
	return nil
}

func IsPluginCachePath(path string) bool {
	normalized := filepath.ToSlash(filepath.Clean(path))
	return strings.Contains(normalized, "/.codex/plugins/cache/") ||
		strings.Contains(normalized, "/.claude/plugins/cache/") ||
		strings.HasSuffix(normalized, "/.codex/plugins/cache") ||
		strings.HasSuffix(normalized, "/.claude/plugins/cache")
}
