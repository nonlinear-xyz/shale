// Package discover finds git repositories on this machine.
//
// Everything else depends on knowing which directories are repositories, so this
// runs before auth, before policy, before any network call.
//
// Discovery is deliberately LOCAL-ONLY. Walking someone's home directory turns
// up repo names and paths they may never have consented to share; nothing here
// touches the network. The inventory only leaves the machine after an explicit
// prompt, and only the repos the user then selects are ever synced. Discovery is
// local, sync is opt-in.
package discover

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nonlinear-xyz/shale/internal/pathutil"
)

// A directory is a git repo if it contains ".git" — a *directory* for a normal
// clone, or a *file* containing "gitdir: ..." for a worktree or submodule.
// Both count; the worktree case is deduped later via --git-common-dir.
const gitMarker = ".git"

// DefaultMaxDepth is measured in path segments below each root. 6 is deep enough
// for realistic ~/Documents/<org>/<group>/<repo> nesting without walking into
// pathological trees.
const DefaultMaxDepth = 6

// Directories we never descend into. `node_modules` and `Library` are the two
// big time sinks on a Mac; `.Trash` holds deleted work that shouldn't resurface.
var skipDirNames = map[string]bool{
	"node_modules": true,
	"Library":      true,
	".Trash":       true,
	"vendor":       true,
	"Applications": true,
}

// SkipReason explains why a candidate didn't make the selectable list. These are
// shown to the user — a silent skip is indistinguishable from a bug, and "why
// isn't my repo here" is the first support question this tool will get.
type SkipReason string

const (
	SkipHidden    SkipReason = "vendored or cached under a hidden directory"
	SkipWorktree  SkipReason = "worktree of another repository"
	SkipNoRemote  SkipReason = "no git remote — cannot be joined across machines"
	SkipNotGit    SkipReason = "not a usable git repository"
	SkipNoCommits SkipReason = "no commits yet"
)

// Repo is one discovered repository. Path is absolute; Remote is the normalized
// "owner/name" when an origin remote exists.
type Repo struct {
	Path         string     `json:"path"`
	Remote       string     `json:"remote,omitempty"`
	CommitCount  int        `json:"commitCount"`
	LastCommitAt *time.Time `json:"lastCommitAt,omitempty"`
	Branch       string     `json:"branch,omitempty"`

	// commonDir is git's --git-common-dir: identical for a repo and every one of
	// its linked worktrees, which is what makes worktree dedupe possible. Cached
	// here because resolving it costs a subprocess.
	commonDir string

	// Set when the repo was found but cannot be selected. A skipped repo is still
	// reported to the user locally; it is never uploaded.
	Skipped    bool       `json:"-"`
	SkipReason SkipReason `json:"-"`
	// SkipDetail carries the specifics, e.g. which repo a worktree belongs to.
	SkipDetail string `json:"-"`
}

// Selectable reports whether this repo can be synced. Callers must filter on
// this before uploading an inventory.
func (r Repo) Selectable() bool { return !r.Skipped }

// Name is the display label — the remote when we have one, else the directory.
func (r Repo) Name() string {
	if r.Remote != "" {
		return r.Remote
	}
	return filepath.Base(r.Path)
}

// Discover walks roots and returns every git repository found, selectable and
// skipped alike, sorted by commit count descending so the interesting repos are
// at the top of the table.
//
// Errors from individual repos are never fatal: a repo we can't read becomes a
// skip with a reason, not an aborted scan. This binary degrades rather than
// breaks.
func Discover(roots []string, maxDepth int) []Repo {
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}

	// Phase 1: a pure filesystem walk. No git calls, so it stays fast and the
	// prune decision can't depend on information we don't have yet.
	var paths []string
	seen := make(map[string]bool)
	for _, root := range roots {
		abs, err := filepath.Abs(pathutil.ExpandHome(root))
		if err != nil {
			continue
		}
		for _, p := range walkRoot(abs, maxDepth) {
			if !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}

	// Phase 2: inspect in parallel. Each repo costs several git subprocesses and
	// they're fully independent, so this is the difference between ~11s and ~2s on
	// a machine with a couple dozen repos.
	found := inspectAll(paths)

	// Phase 3: fold linked worktrees into the repository they belong to. Must run
	// after inspection because it keys on --git-common-dir.
	dedupeWorktrees(found)

	sortByCommitsDesc(found)
	return found
}

// walkRoot does a bounded directory walk and returns every repo root it finds.
//
// It deliberately does NOT prune on hit. An earlier version stopped descending at
// the first `.git`, which silently swallowed real work: an empty git repo that
// contains five actual projects made all five vanish from the inventory. Nested
// repos and submodules are normal; the walk has to see through a repo boundary.
// Descending is cheap because `.git` itself is hidden (so it's skipped by the
// hidden-directory rule) and the depth cap bounds the rest.
func walkRoot(root string, maxDepth int) []string {
	type entry struct {
		path  string
		depth int
	}
	var repos []string
	queue := []entry{{root, 0}}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		if isRepoRoot(cur.path) {
			repos = append(repos, cur.path)
		}

		if cur.depth >= maxDepth {
			continue
		}

		entries, err := os.ReadDir(cur.path)
		if err != nil {
			continue // unreadable dir (permissions) — not our problem, move on
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if skipDirNames[name] {
				continue
			}
			// Hidden directories hold tool caches and vendored corpora, not the
			// user's work. Measured on a real machine, this single rule excludes
			// ~/.local/share/ruby-advisory-db (1,875 commits of a vendored
			// dependency) and ~/.codex/.tmp/marketplaces/* — both of which would
			// otherwise swamp the corpus with code the user never wrote.
			if strings.HasPrefix(name, ".") {
				continue
			}
			// Don't follow symlinks: they make the walk cyclic and usually point at
			// a repo we'll reach through its real path anyway.
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			queue = append(queue, entry{filepath.Join(cur.path, name), cur.depth + 1})
		}
	}
	return repos
}

// isRepoRoot reports whether path contains a .git entry of either form.
func isRepoRoot(path string) bool {
	_, err := os.Lstat(filepath.Join(path, gitMarker))
	return err == nil
}

// inspectAll runs inspect across paths concurrently, preserving input order so
// the result is deterministic regardless of scheduling.
func inspectAll(paths []string) []Repo {
	out := make([]Repo, len(paths))
	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, p := range paths {
		wg.Add(1)
		go func(i int, p string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = inspect(p)
		}(i, p)
	}
	wg.Wait()
	return out
}

// dedupeWorktrees folds linked worktrees into the repository they belong to. A
// worktree shares its parent's --git-common-dir, so repos are grouped by that and
// exactly one is kept selectable.
//
// The canonical one is chosen structurally, not by scan order: for a main
// worktree, --git-common-dir resolves to <path>/.git. That makes the result
// stable no matter which directory the walk reached first. If no candidate looks
// canonical (e.g. the main checkout is outside the scanned roots), the one with
// the most commits wins so the inventory still reports the fullest history.
func dedupeWorktrees(repos []Repo) {
	groups := make(map[string][]int)
	for i := range repos {
		if repos[i].Skipped || repos[i].commonDir == "" {
			continue
		}
		groups[repos[i].commonDir] = append(groups[repos[i].commonDir], i)
	}

	for _, idxs := range groups {
		if len(idxs) < 2 {
			continue
		}
		canonical := idxs[0]
		for _, i := range idxs {
			if repos[i].isMainWorktree() {
				canonical = i
				break
			}
			if repos[i].CommitCount > repos[canonical].CommitCount {
				canonical = i
			}
		}
		for _, i := range idxs {
			if i == canonical {
				continue
			}
			repos[i].Skipped = true
			repos[i].SkipReason = SkipWorktree
			// The canonical repo's path, not its name: a worktree usually shares its
			// parent's remote, so showing the name renders as the useless
			// "acme/thing is a worktree of acme/thing".
			repos[i].SkipDetail = pathutil.Shorten(repos[canonical].Path)
		}
	}
}

// isMainWorktree reports whether this path is the repository's main checkout
// rather than a linked worktree: git resolves --git-common-dir to <path>/.git for
// the main one and to the *parent's* .git for every linked worktree.
func (r Repo) isMainWorktree() bool {
	if r.commonDir == "" {
		return false
	}
	expected := filepath.Join(r.Path, gitMarker)
	if resolved, err := filepath.EvalSymlinks(expected); err == nil {
		expected = resolved
	}
	return r.commonDir == expected
}

// inspect gathers the facts we report per repo. Each git call is independent and
// best-effort — a failure downgrades the repo to a skip rather than aborting
// discovery.
func inspect(path string) Repo {
	r := Repo{Path: path}

	r.commonDir = gitCommonDir(path)
	if r.commonDir == "" {
		r.Skipped = true
		r.SkipReason = SkipNotGit
		return r
	}

	count, err := strconv.Atoi(Git(path, "rev-list", "--count", "HEAD"))
	if err != nil || count == 0 {
		r.Skipped = true
		r.SkipReason = SkipNoCommits
		return r
	}
	r.CommitCount = count

	r.Branch = Git(path, "rev-parse", "--abbrev-ref", "HEAD")

	if ts := Git(path, "log", "-1", "--format=%cI"); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			r.LastCommitAt = &t
		}
	}

	r.Remote = NormalizeRemote(Git(path, "remote", "get-url", "origin"))
	if r.Remote == "" {
		// Repo identity for cross-machine and cross-person joins IS the remote. A
		// local-only repo can be shown, but syncing it would create a repo row
		// nothing else could ever join to.
		r.Skipped = true
		r.SkipReason = SkipNoRemote
	}
	return r
}

// gitCommonDir returns the absolute --git-common-dir, which is identical for a
// repo and all of its worktrees. Empty when path isn't a usable repository.
func gitCommonDir(path string) string {
	out := Git(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if out == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(out); err == nil {
		return resolved
	}
	return out
}

// Git runs a git command in dir and returns trimmed stdout, or "" on any error.
// Swallowing errors is deliberate: every caller here treats failure as absence.
//
// Exported because the CLI's git-availability preflight uses it too.
func Git(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Never prompt for credentials or open an editor during discovery.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// NormalizeRemote reduces a remote URL to "owner/name". Handles every form git
// remotes actually take:
//
//	https://github.com/owner/name.git
//	ssh://git@github.com/owner/name.git
//	git@github.com:owner/name.git
//	myalias:owner/name.git          ← SSH config Host alias, no user@
//
// That fourth form is not exotic: anyone using per-account SSH aliases (a common
// way to juggle two GitHub identities) has remotes with no `user@` at all, and a
// parser that keys on `@` leaves the alias glued to the owner — producing repo
// identities like "myalias:Org/name" that can never join across machines.
//
// Anything unrecognized returns "" — a wrong repo identity silently merges two
// codebases in the graph, so guessing is worse than declining.
func NormalizeRemote(url string) string {
	if url == "" {
		return ""
	}
	s := strings.TrimSuffix(strings.TrimSpace(url), ".git")

	// A remote that names a location on some filesystem carries no shared identity
	// — "/Users/me/src/app" and "file:///srv/git/app" would reduce to "src/app" and
	// "srv/app", so two unrelated clones on two machines collide on one repo.
	// Declining is the only safe answer.
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "file://") {
		return ""
	}

	if i := strings.Index(s, "://"); i >= 0 {
		// scheme://[user@]host/path
		s = s[i+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		slash := strings.Index(s, "/")
		if slash < 0 {
			return ""
		}
		s = s[slash+1:] // drop host
	} else if colon := strings.Index(s, ":"); colon >= 0 && !strings.HasPrefix(s, "/") {
		// scp-like [user@]host:path — the host segment may be an SSH config alias
		// with no user@ at all, so split on the colon, not the at-sign.
		s = s[colon+1:]
	}

	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return ""
	}
	// Keep the COMPLETE namespace after the host, not just the trailing pair.
	//
	// GitLab subgroups nest arbitrarily deep, and truncating to the last two
	// segments collapses "group-a/team/project" and "group-b/team/project" into one
	// identity. That is not a cosmetic loss: the picker and the capture policy are
	// keyed by this string, so ticking one repository would authorize uploads from
	// the other, and graph sync would merge two codebases' history.
	for _, p := range parts {
		if p == "" {
			return "" // a doubled slash means we misparsed; decline rather than guess
		}
	}
	return strings.Join(parts, "/")
}

func sortByCommitsDesc(rs []Repo) {
	// Selectable repos first, then by commit count. Insertion sort: n is the number
	// of repos on one machine (tens), and this keeps the file dependency free.
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && less(rs[j], rs[j-1]); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func less(a, b Repo) bool {
	if a.Skipped != b.Skipped {
		return !a.Skipped
	}
	if a.CommitCount != b.CommitCount {
		return a.CommitCount > b.CommitCount
	}
	return a.Path < b.Path
}
