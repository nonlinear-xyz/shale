package discover

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// NormalizeRemote is the single most correctness-critical pure function in the
// binary: its output IS the repo's identity, and a wrong answer silently merges
// two codebases in the graph (or splits one across machines) with no error
// anywhere. Every case below except the plain https/ssh pair was found on a real
// machine during the first run.
func TestNormalizeRemote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"https", "https://github.com/nonlinear-xyz/observatory.git", "nonlinear-xyz/observatory"},
		{"https no suffix", "https://github.com/nonlinear-xyz/observatory", "nonlinear-xyz/observatory"},
		{"scp with user", "git@github.com:nonlinear-xyz/observatory.git", "nonlinear-xyz/observatory"},
		{"ssh scheme", "ssh://git@github.com/nonlinear-xyz/observatory.git", "nonlinear-xyz/observatory"},

		// The regression: per-account SSH config aliases produce remotes with no
		// user@ at all. A parser keyed on "@" leaves the alias glued to the owner
		// and yields "nishu-nonlinear:ZampaAI/zampaAI-frontend".
		{"ssh config alias", "nishu-nonlinear:ZampaAI/zampaAI-frontend.git", "ZampaAI/zampaAI-frontend"},
		{"ssh config alias no suffix", "myalias:owner/name", "owner/name"},

		{"https with port", "https://git.example.com:8443/owner/name.git", "owner/name"},
		{"trailing slash", "https://github.com/owner/name/", "owner/name"},
		{"whitespace", "  git@github.com:owner/name.git\n", "owner/name"},

		// GitLab subgroups nest arbitrarily deep and the FULL namespace is the
		// identity. Truncating to the trailing pair would make these two the same
		// repository — and since the capture policy is keyed by this string,
		// ticking one would authorize uploads from the other.
		{"gitlab subgroup", "git@gitlab.com:group/sub/name.git", "group/sub/name"},
		{"gitlab deep subgroup", "https://gitlab.com/a/b/c/app.git", "a/b/c/app"},
		{"distinct top groups stay distinct", "git@gitlab.com:group-a/team/app.git", "group-a/team/app"},

		// Declining beats guessing — a bad identity is worse than none.
		{"empty", "", ""},
		{"single segment", "https://github.com/name", ""},
		{"bare word", "origin", ""},

		// Filesystem remotes name a location, not a shared identity. Reducing them
		// to their last two segments would make "/Users/a/src/app" and
		// "/home/b/src/app" the same repository on two different machines.
		{"absolute local path", "/Users/me/src/app", ""},
		{"relative local path", "../sibling/app", ""},
		{"file scheme", "file:///srv/git/app.git", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeRemote(tc.in); got != tc.want {
				t.Errorf("NormalizeRemote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A parent directory that happens to contain a stray `git init` must not hide the
// real repositories inside it. This is not hypothetical: ~/Documents/bootstrapper
// on the author's machine is an empty repo wrapping five real projects, and an
// earlier prune-on-hit walk made all five invisible.
func TestDiscoverSeesThroughEmptyParentRepo(t *testing.T) {
	requireGitOrSkip(t)
	root := t.TempDir()

	parent := filepath.Join(root, "container")
	mustMkdir(t, parent)
	mustGit(t, parent, "init", "-q")
	// No commits, no remote — exactly the accidental-init shape.

	child := filepath.Join(parent, "real-project")
	mustMkdir(t, child)
	initRepoWithCommit(t, child, "git@github.com:acme/real-project.git")

	repos := Discover([]string{root}, 6)

	var found *Repo
	for i := range repos {
		if repos[i].Path == child {
			found = &repos[i]
		}
	}
	if found == nil {
		t.Fatalf("nested repo %q was not discovered; walk stopped at the empty parent", child)
	}
	if !found.Selectable() {
		t.Errorf("nested repo should be selectable, got skip %q", found.SkipReason)
	}
	if found.Remote != "acme/real-project" {
		t.Errorf("remote = %q, want acme/real-project", found.Remote)
	}
	if found.CommitCount != 1 {
		t.Errorf("commitCount = %d, want 1", found.CommitCount)
	}

	// The empty parent is still reported, so the user can see why it isn't listed.
	for _, r := range repos {
		if r.Path == parent {
			if r.Selectable() {
				t.Error("empty parent repo should not be selectable")
			}
			if r.SkipReason != SkipNoCommits {
				t.Errorf("parent skip reason = %q, want %q", r.SkipReason, SkipNoCommits)
			}
			return
		}
	}
	t.Error("empty parent repo was not reported at all")
}

// A repo and its linked worktrees share one --git-common-dir and must collapse to
// a single selectable entry — otherwise a developer using worktrees reports the
// same commits two or three times.
func TestDiscoverDedupesWorktrees(t *testing.T) {
	requireGitOrSkip(t)
	root := t.TempDir()

	main := filepath.Join(root, "project")
	mustMkdir(t, main)
	initRepoWithCommit(t, main, "git@github.com:acme/project.git")

	tree := filepath.Join(root, "project-feature")
	mustGit(t, main, "worktree", "add", "-q", "-b", "feature", tree)

	repos := Discover([]string{root}, 6)

	var selectable []Repo
	for _, r := range repos {
		if r.Selectable() {
			selectable = append(selectable, r)
		}
	}
	if len(selectable) != 1 {
		for _, r := range selectable {
			t.Logf("selectable: %s", r.Path)
		}
		t.Fatalf("got %d selectable repos, want 1 (worktree should fold into its main checkout)", len(selectable))
	}
	// The main checkout wins, not whichever the walk reached first — chosen
	// structurally via --git-common-dir so the result is scan-order independent.
	if selectable[0].Path != main {
		t.Errorf("canonical repo = %q, want the main checkout %q", selectable[0].Path, main)
	}

	for _, r := range repos {
		if r.Path == tree && r.SkipReason != SkipWorktree {
			t.Errorf("worktree skip reason = %q, want %q", r.SkipReason, SkipWorktree)
		}
	}
}

// Hidden directories hold tool caches and vendored corpora. Without this rule a
// real machine surfaced ~/.local/share/ruby-advisory-db — 1,875 commits of code
// the user never wrote — which would dominate their own history.
func TestDiscoverSkipsHiddenAndVendoredDirs(t *testing.T) {
	requireGitOrSkip(t)
	root := t.TempDir()

	for _, dir := range []string{".cache/vendored", "node_modules/dep", "visible"} {
		p := filepath.Join(root, dir)
		mustMkdir(t, p)
		initRepoWithCommit(t, p, "git@github.com:acme/"+filepath.Base(dir)+".git")
	}

	repos := Discover([]string{root}, 6)
	for _, r := range repos {
		if filepath.Base(r.Path) != "visible" {
			t.Errorf("discovered %q, which should have been skipped by directory name", r.Path)
		}
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want only the visible one", len(repos))
	}
}

// A repo with no remote is reported but never selectable: repo identity for
// cross-machine and cross-person joins IS the remote, so syncing one would create
// a row nothing could ever join to.
func TestDiscoverRejectsRemotelessRepo(t *testing.T) {
	requireGitOrSkip(t)
	root := t.TempDir()

	local := filepath.Join(root, "local-only")
	mustMkdir(t, local)
	initRepoWithCommit(t, local, "")

	repos := Discover([]string{root}, 6)
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}
	if repos[0].Selectable() {
		t.Error("remoteless repo must not be selectable")
	}
	if repos[0].SkipReason != SkipNoRemote {
		t.Errorf("skip reason = %q, want %q", repos[0].SkipReason, SkipNoRemote)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func requireGitOrSkip(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null", // ignore the developer's own git config
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

func initRepoWithCommit(t *testing.T, dir, remote string) {
	t.Helper()
	mustGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "initial")
	if remote != "" {
		mustGit(t, dir, "remote", "add", "origin", remote)
	}
}
