// Package artifacts discovers and snapshots durable context that lives outside
// shale: harness-generated memories, repository instruction files, and
// registered runbooks. Source files remain canonical and are never modified.
package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nonlinear-xyz/shale/internal/sessions"
	"github.com/nonlinear-xyz/shale/internal/store"
)

type Options struct {
	HomeDir    string
	CodexHome  string
	ClaudeHome string
	CurrentDir string
}

type Result struct {
	Scanned   int
	Indexed   int
	Unchanged int
	Removed   int
	Skipped   []Skip
	Errors    []error
}

type Skip struct {
	Path   string
	Reason string
}

type sourceSpec struct {
	store.ArtifactSource
	Root string
}

// RegisterRunbook marks a file inside a Git worktree as a canonical runbook
// source. The file remains authoritative; Refresh snapshots new revisions and
// never writes back to it.
func RegisterRunbook(ctx context.Context, db *store.DB, path, repoOverride string) (store.ArtifactSource, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return store.ArtifactSource{}, fmt.Errorf("resolve runbook path: %w", err)
	}
	abs = filepath.Clean(abs)
	if !regularOrSymlink(abs) {
		return store.ArtifactSource{}, fmt.Errorf("runbook source is not a regular file: %s", abs)
	}
	physicalDir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return store.ArtifactSource{}, fmt.Errorf("resolve runbook directory: %w", err)
	}
	// Normalize parent aliases such as macOS /var -> /private/var while retaining
	// the final path component, which may itself be a symlink that refresh must
	// re-evaluate on every pass.
	abs = filepath.Join(physicalDir, filepath.Base(abs))
	root, detectedRepo := gitRootAndRepo(filepath.Dir(abs))
	if root == "" {
		return store.ArtifactSource{}, errors.New("registered runbooks must live inside a Git worktree")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return store.ArtifactSource{}, fmt.Errorf("resolve repository root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return store.ArtifactSource{}, fmt.Errorf("resolve runbook source: %w", err)
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return store.ArtifactSource{}, errors.New("runbook symlink escapes its Git worktree")
	}
	repo := strings.TrimSpace(repoOverride)
	if repo == "" {
		repo = detectedRepo
	}
	if repo == "" {
		repo = root
	}
	source := store.ArtifactSource{
		Path: abs, ArtifactID: deterministicID(string(store.ArtifactRunbook), abs),
		Kind: store.ArtifactRunbook, ScopeKind: store.ScopeRepo,
		ScopeKey: repo, Repo: repo, Source: "manual", Origin: "file",
	}
	existing, err := db.ArtifactSources(ctx)
	if err != nil {
		return store.ArtifactSource{}, fmt.Errorf("inspect existing source registration: %w", err)
	}
	for _, old := range existing {
		if old.Path != abs || old.ArtifactID == source.ArtifactID {
			continue
		}
		if a, err := db.Artifact(ctx, old.ArtifactID); err == nil && a.Status == store.ArtifactActive {
			if _, err := db.RetractArtifact(ctx, old.ArtifactID, "human"); err != nil {
				return store.ArtifactSource{}, fmt.Errorf("retract previous source identity: %w", err)
			}
		} else if err != nil && !errors.Is(err, store.ErrArtifactNotFound) {
			return store.ArtifactSource{}, fmt.Errorf("inspect previous source identity: %w", err)
		}
		break
	}
	if err := db.RegisterArtifactSource(ctx, source); err != nil {
		return store.ArtifactSource{}, fmt.Errorf("register runbook: %w", err)
	}
	return source, nil
}

func Refresh(ctx context.Context, db *store.DB, opts Options) Result {
	opts = normalizeOptions(opts)
	specs := map[string]sourceSpec{}
	add := func(spec sourceSpec) {
		if spec.Path == "" {
			return
		}
		spec.Path = filepath.Clean(spec.Path)
		if existing, ok := specs[spec.Path]; ok {
			// An explicitly registered runbook is more intentional than automatic
			// instruction discovery and keeps its stable artifact identity.
			if existing.Kind == store.ArtifactRunbook {
				return
			}
		}
		specs[spec.Path] = spec
	}

	registered, err := db.ArtifactSources(ctx)
	if err != nil {
		return Result{Errors: []error{fmt.Errorf("list registered sources: %w", err)}}
	}
	for _, source := range registered {
		// Git-backed runbooks are explicitly registered and remain watched even
		// while missing. Harness memory/instruction sources are rediscovered from
		// current configuration on every pass; blindly re-adding them here would
		// keep a now-shadowed AGENTS.md or an obsolete auto-memory directory alive.
		if source.Kind == store.ArtifactRunbook && source.Origin == "file" {
			add(sourceSpec{ArtifactSource: source, Root: sourceRoot(source)})
		}
	}

	collectCodex(opts, add)
	collectClaude(opts, db, ctx, add)
	collectRepoInstructions(opts, db, ctx, add)

	result := Result{}
	seen := map[string]bool{}
	for _, spec := range sortedSpecs(specs) {
		result.Scanned++
		body, hash, reason, err := readSafeSource(spec.Path, spec.Root)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("%s: %w", spec.Path, err))
			continue
		}
		if reason != "" {
			result.Skipped = append(result.Skipped, Skip{Path: spec.Path, Reason: reason})
			continue
		}
		seen[spec.Path] = true
		if spec.ArtifactID == "" {
			spec.ArtifactID = deterministicID(string(spec.Kind), spec.Path)
		}
		if spec.ScopeKind == "" {
			spec.ScopeKind = store.ScopeUser
			spec.ScopeKey = "local"
		}
		if spec.Source == "" {
			spec.Source = "manual"
		}
		if spec.Origin == "" {
			spec.Origin = "file"
		}
		var occurredAt time.Time
		if info, err := os.Stat(spec.Path); err == nil {
			occurredAt = info.ModTime()
		}

		eventKind := store.KindExternalIndexed
		if spec.Kind == store.ArtifactRunbook {
			eventKind = store.KindRunbookRefreshed
			if _, err := db.Artifact(ctx, spec.ArtifactID); errors.Is(err, store.ErrArtifactNotFound) {
				eventKind = store.KindRunbookRegistered
			}
		}
		artifact, inserted, err := db.PutArtifact(ctx, store.ArtifactInput{
			ID: spec.ArtifactID, Kind: spec.Kind, Status: store.ArtifactActive,
			ScopeKind: spec.ScopeKind, ScopeKey: spec.ScopeKey, Repo: spec.Repo,
			Title: filepath.Base(spec.Path), Origin: spec.Origin,
			Authority: externalAuthority(spec.Kind), Source: spec.Source,
			SourcePointer: spec.Path, Actor: "system", EventKind: eventKind,
			OccurredAt: occurredAt,
			Content:    store.ArtifactContent{Text: body},
		})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("index %s: %w", spec.Path, err))
			continue
		}
		spec.LastHash = artifact.ContentHash
		if err := db.RegisterArtifactSource(ctx, spec.ArtifactSource); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("register %s: %w", spec.Path, err))
			continue
		}
		if err := db.TouchArtifactSource(ctx, spec.Path, hash); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("touch %s: %w", spec.Path, err))
			continue
		}
		if inserted {
			result.Indexed++
		} else {
			result.Unchanged++
		}
	}

	// A missing registered source is a real state transition. Permission errors
	// are not: keep the last good snapshot rather than interpreting an unreadable
	// disk as the user deleting knowledge.
	for _, source := range registered {
		if seen[source.Path] {
			continue
		}
		_, configured := specs[source.Path]
		_, statErr := os.Stat(source.Path)
		if configured {
			if !os.IsNotExist(statErr) {
				continue
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			// The source is not currently discoverable, but an access error is not
			// evidence that its knowledge ceased to exist.
			continue
		}
		a, err := db.Artifact(ctx, source.ArtifactID)
		if err != nil && !errors.Is(err, store.ErrArtifactNotFound) {
			result.Errors = append(result.Errors, fmt.Errorf("inspect missing source %s: %w", source.Path, err))
			continue
		}
		if errors.Is(err, store.ErrArtifactNotFound) || a.Status != store.ArtifactActive {
			if !configured {
				_ = db.RemoveArtifactSource(ctx, source.Path)
			}
			continue
		}
		if _, err := db.RetractArtifact(ctx, source.ArtifactID, "system"); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("remove %s: %w", source.Path, err))
			continue
		}
		result.Removed++
		if !configured {
			if err := db.RemoveArtifactSource(ctx, source.Path); err != nil {
				result.Errors = append(result.Errors, fmt.Errorf("forget source %s: %w", source.Path, err))
			}
		}
	}
	return result
}

func normalizeOptions(opts Options) Options {
	if opts.HomeDir == "" {
		opts.HomeDir, _ = os.UserHomeDir()
	}
	if opts.CodexHome == "" {
		opts.CodexHome = os.Getenv("CODEX_HOME")
		if opts.CodexHome == "" {
			opts.CodexHome = filepath.Join(opts.HomeDir, ".codex")
		}
	}
	if opts.ClaudeHome == "" {
		opts.ClaudeHome = filepath.Join(opts.HomeDir, ".claude")
	}
	if opts.CurrentDir == "" {
		opts.CurrentDir, _ = os.Getwd()
	}
	return opts
}

func collectCodex(opts Options, add func(sourceSpec)) {
	memoryRoot := filepath.Join(opts.CodexHome, "memories")
	walkTextFiles(memoryRoot, func(path string) {
		add(sourceSpec{ArtifactSource: store.ArtifactSource{
			Path: path, Kind: store.ArtifactMemory,
			ScopeKind: store.ScopeUser, ScopeKey: "local",
			Source: "codex", Origin: "codex_memory",
		}, Root: memoryRoot})
	})
	for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
		path := filepath.Join(opts.CodexHome, name)
		if nonEmptyRegularOrSymlink(path) {
			add(sourceSpec{ArtifactSource: store.ArtifactSource{
				Path: path, Kind: store.ArtifactInstruction,
				ScopeKind: store.ScopeUser, ScopeKey: "local",
				Source: "codex", Origin: "codex_instruction",
			}, Root: opts.CodexHome})
			break // Codex loads only the first non-empty global instruction file.
		}
	}
}

func collectClaude(opts Options, db *store.DB, ctx context.Context, add func(sourceSpec)) {
	memoryRoots := []string{filepath.Join(opts.ClaudeHome, "projects")}
	settingsPath := filepath.Join(opts.ClaudeHome, "settings.json")
	if raw, err := os.ReadFile(settingsPath); err == nil {
		var settings struct {
			AutoMemoryDirectory string `json:"autoMemoryDirectory"`
		}
		if json.Unmarshal(raw, &settings) == nil && settings.AutoMemoryDirectory != "" {
			memoryRoots = append(memoryRoots, expandHome(settings.AutoMemoryDirectory, opts.HomeDir))
		}
	}

	cwdRepos := recentCWDRepos(ctx, db)
	defaultRoot := memoryRoots[0]
	entries, _ := os.ReadDir(defaultRoot)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		projectDir := filepath.Join(defaultRoot, entry.Name())
		memoryDir := filepath.Join(projectDir, "memory")
		repo, scopeKey := claudeProjectScope(projectDir, cwdRepos)
		if repo == "" {
			// An unmapped project memory must not become global user state. Keep it
			// isolated until a captured session lets us associate the Claude project
			// directory with a repository.
			repo = "claude-project:" + entry.Name()
			scopeKey = repo
		}
		walkTextFiles(memoryDir, func(path string) {
			add(sourceSpec{ArtifactSource: store.ArtifactSource{
				Path: path, Kind: store.ArtifactMemory,
				ScopeKind: store.ScopeRepo, ScopeKey: scopeKey, Repo: repo,
				Source: "claude_code", Origin: "claude_memory",
			}, Root: memoryDir})
		})
	}
	for _, root := range memoryRoots[1:] {
		walkTextFiles(root, func(path string) {
			add(sourceSpec{ArtifactSource: store.ArtifactSource{
				Path: path, Kind: store.ArtifactMemory,
				ScopeKind: store.ScopeUser, ScopeKey: "local",
				Source: "claude_code", Origin: "claude_memory",
			}, Root: root})
		})
	}
	path := filepath.Join(opts.ClaudeHome, "CLAUDE.md")
	if regularOrSymlink(path) {
		add(sourceSpec{ArtifactSource: store.ArtifactSource{
			Path: path, Kind: store.ArtifactInstruction,
			ScopeKind: store.ScopeUser, ScopeKey: "local",
			Source: "claude_code", Origin: "claude_instruction",
		}, Root: opts.ClaudeHome})
	}
}

func collectRepoInstructions(opts Options, db *store.DB, ctx context.Context, add func(sourceSpec)) {
	cwdRepos := recentCWDRepos(ctx, db)
	if root, repo := gitRootAndRepo(opts.CurrentDir); root != "" {
		cwdRepos[opts.CurrentDir] = repoInfo{Root: root, Repo: repo}
	}
	seenDirs := map[string]bool{}
	seenRules := map[string]bool{}
	for cwd, info := range cwdRepos {
		if info.Root == "" {
			continue
		}
		repo := info.Repo
		if repo == "" {
			repo = info.Root
		}
		for _, dir := range pathFromRoot(info.Root, cwd) {
			if seenDirs[dir] {
				continue
			}
			seenDirs[dir] = true
			// Codex selects one instruction file per directory, override first.
			for _, name := range []string{"AGENTS.override.md", "AGENTS.md"} {
				path := filepath.Join(dir, name)
				if !nonEmptyRegularOrSymlink(path) {
					continue
				}
				add(sourceSpec{ArtifactSource: store.ArtifactSource{
					Path: path, Kind: store.ArtifactInstruction,
					ScopeKind: store.ScopeRepo, ScopeKey: repo, Repo: repo,
					Source: "codex", Origin: "repo_instruction",
				}, Root: info.Root})
				break
			}
			for _, relative := range []string{"CLAUDE.md", "CLAUDE.local.md", filepath.Join(".claude", "CLAUDE.md")} {
				path := filepath.Join(dir, relative)
				if !regularOrSymlink(path) {
					continue
				}
				add(sourceSpec{ArtifactSource: store.ArtifactSource{
					Path: path, Kind: store.ArtifactInstruction,
					ScopeKind: store.ScopeRepo, ScopeKey: repo, Repo: repo,
					Source: "claude_code", Origin: "repo_instruction",
				}, Root: info.Root})
			}
		}
		if seenRules[info.Root] {
			continue
		}
		seenRules[info.Root] = true
		rulesRoot := filepath.Join(info.Root, ".claude", "rules")
		walkTextFiles(rulesRoot, func(path string) {
			add(sourceSpec{ArtifactSource: store.ArtifactSource{
				Path: path, Kind: store.ArtifactInstruction,
				ScopeKind: store.ScopeRepo, ScopeKey: repo, Repo: repo,
				Source: "claude_code", Origin: "repo_instruction",
			}, Root: info.Root})
		})
	}
}

type repoInfo struct {
	Root string
	Repo string
}

func recentCWDRepos(ctx context.Context, db *store.DB) map[string]repoInfo {
	out := map[string]repoInfo{}
	items, err := db.RecentSessions(ctx, "", 1000)
	if err != nil {
		return out
	}
	for _, item := range items {
		cwd := item.Record.CWD
		if cwd == "" {
			continue
		}
		root, detectedRepo := gitRootAndRepo(cwd)
		repo := item.Record.Repo
		if repo == "" {
			repo = detectedRepo
		}
		out[cwd] = repoInfo{Root: root, Repo: repo}
	}
	return out
}

func claudeProjectScope(projectDir string, known map[string]repoInfo) (repo, scopeKey string) {
	entries, _ := os.ReadDir(projectDir)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sess, _, err := sessions.Read(filepath.Join(projectDir, entry.Name()), sessions.SourceClaudeCode)
		if err != nil || sess.CWD == "" {
			continue
		}
		if info, ok := known[sess.CWD]; ok {
			if info.Repo != "" {
				return info.Repo, info.Repo
			}
			return info.Root, info.Root
		}
		root, detectedRepo := gitRootAndRepo(sess.CWD)
		if detectedRepo != "" {
			return detectedRepo, detectedRepo
		}
		if root != "" {
			return root, root
		}
	}
	return "", ""
}

func readSafeSource(path, root string) (body, hash, reason string, err error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", "source root is missing", nil
		}
		return "", "", "", err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", "source is missing", nil
		}
		return "", "", "", err
	}
	rel, err := filepath.Rel(realRoot, realPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", "symlink escapes its trusted root", nil
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", "not a regular file", nil
	}
	if info.Size() > store.ArtifactContentMax-1024 {
		return "", "", fmt.Sprintf("file is %d bytes; maximum is %d", info.Size(), store.ArtifactContentMax-1024), nil
	}
	raw, err := os.ReadFile(realPath)
	if err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256(raw)
	return string(raw), hex.EncodeToString(sum[:]), "", nil
}

func walkTextFiles(root string, fn func(string)) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type().IsRegular() || entry.Type()&os.ModeSymlink != 0 {
			switch strings.ToLower(filepath.Ext(path)) {
			case ".md", ".markdown", ".txt":
				fn(path)
			}
		}
		return nil
	})
}

func regularOrSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && (info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0)
}

func nonEmptyRegularOrSymlink(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

func sourceRoot(source store.ArtifactSource) string {
	if source.Origin == "file" {
		if root, _ := gitRootAndRepo(filepath.Dir(source.Path)); root != "" {
			return root
		}
	}
	return filepath.Dir(source.Path)
}

func gitRootAndRepo(cwd string) (string, string) {
	if cwd == "" {
		return "", ""
	}
	rootBytes, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", ""
	}
	root := strings.TrimSpace(string(rootBytes))
	remoteBytes, _ := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	return root, normalizeRemote(strings.TrimSpace(string(remoteBytes)))
}

func normalizeRemote(remote string) string {
	remote = strings.TrimSpace(strings.TrimSuffix(remote, ".git"))
	if remote == "" {
		return ""
	}
	if strings.HasPrefix(remote, "git@") {
		if colon := strings.IndexByte(remote, ':'); colon >= 0 {
			return strings.TrimPrefix(remote[colon+1:], "/")
		}
	}
	if parsed, err := url.Parse(remote); err == nil && parsed.Host != "" {
		return strings.TrimPrefix(parsed.Path, "/")
	}
	return ""
}

func pathFromRoot(root, cwd string) []string {
	if physical, err := filepath.EvalSymlinks(root); err == nil {
		root = physical
	}
	if physical, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = physical
	}
	root = filepath.Clean(root)
	cwd = filepath.Clean(cwd)
	rel, err := filepath.Rel(root, cwd)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return []string{root}
	}
	out := []string{root}
	cur := root
	if rel == "." {
		return out
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		out = append(out, cur)
	}
	return out
}

func externalAuthority(kind store.ArtifactKind) string {
	if kind == store.ArtifactMemory {
		return "external_generated"
	}
	return "external_asserted"
}

func deterministicID(kind, path string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + filepath.Clean(path)))
	return hex.EncodeToString(sum[:16])
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return path
}

func sortedSpecs(in map[string]sourceSpec) []sourceSpec {
	paths := make([]string, 0, len(in))
	for path := range in {
		paths = append(paths, path)
	}
	for i := 1; i < len(paths); i++ {
		for j := i; j > 0 && paths[j] < paths[j-1]; j-- {
			paths[j], paths[j-1] = paths[j-1], paths[j]
		}
	}
	out := make([]sourceSpec, 0, len(paths))
	for _, path := range paths {
		out = append(out, in[path])
	}
	return out
}
