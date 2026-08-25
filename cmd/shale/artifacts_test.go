package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNativeMemoryCLIWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and exercises the real binary")
	}
	bin := filepath.Join(t.TempDir(), "shale")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := t.TempDir()
	run := func(wantSuccess bool, args ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HOME="+home, "NO_COLOR=1")
		out, err := cmd.Output()
		stderr := ""
		if exit, ok := err.(*exec.ExitError); ok {
			stderr = string(exit.Stderr)
		}
		if wantSuccess && err != nil {
			t.Fatalf("shale %v: %v\nstdout: %s\nstderr: %s", args, err, out, stderr)
		}
		if !wantSuccess && err == nil {
			t.Fatalf("shale %v unexpectedly succeeded\n%s", args, out)
		}
		return string(out), stderr
	}

	created, _ := run(true, "remember", "Use pnpm for release builds", "--repo", "acme/app", "--title", "Package manager")
	match := regexp.MustCompile(`memory:[a-f0-9]+`).FindString(created)
	if match == "" {
		t.Fatalf("created memory did not print a stable ref:\n%s", created)
	}
	oldVersion := regexp.MustCompile(`memory:[a-f0-9]+@[0-9]+`).FindString(created)

	listed, _ := run(true, "memories", "--repo", "acme/app")
	if !strings.Contains(listed, match) || !strings.Contains(listed, "Package manager") {
		t.Fatalf("memory list omitted created item:\n%s", listed)
	}
	searched, _ := run(true, "search", "pnpm", "--kind", "memory", "--repo", "acme/app")
	if !strings.Contains(searched, "Use pnpm") || !strings.Contains(searched, oldVersion) {
		t.Fatalf("artifact search omitted body or exact ref:\n%s", searched)
	}

	updated, _ := run(true, "supersede", match, "Use bun for release builds")
	if !strings.Contains(updated, "Use bun") {
		t.Fatalf("supersede did not print replacement:\n%s", updated)
	}
	old, _ := run(true, "show", oldVersion)
	if !strings.Contains(old, "Use pnpm") {
		t.Fatalf("historical ref did not retain its exact body:\n%s", old)
	}
	current, _ := run(true, "show", match)
	if !strings.Contains(current, "Use bun") {
		t.Fatalf("stable ref did not resolve current body:\n%s", current)
	}

	run(true, "forget", match)
	forgottenSearch, _ := run(true, "search", "bun", "--kind", "memory", "--repo", "acme/app")
	if !strings.Contains(forgottenSearch, "No memory matches") {
		t.Fatalf("retracted memory remained searchable:\n%s", forgottenSearch)
	}
	_, refused := run(false, "purge", match)
	if !strings.Contains(refused, "pass --yes") {
		t.Fatalf("non-interactive purge did not require --yes:\n%s", refused)
	}
	purged, _ := run(true, "purge", match, "--yes")
	if !strings.Contains(purged, "Purged "+match) {
		t.Fatalf("purge did not report its target:\n%s", purged)
	}
	afterPurge, _ := run(true, "show", oldVersion)
	if !strings.Contains(afterPurge, "content is unavailable or has been purged") {
		t.Fatalf("purged historical body remained readable:\n%s", afterPurge)
	}
}

func TestCLIScopeInferenceAndValidation(t *testing.T) {
	kind, key, repo, err := cliScope("", "acme/app", "REL-42")
	if err != nil || string(kind) != "task" || key != "REL-42" || repo != "acme/app" {
		t.Fatalf("task inference = %s %q %q err=%v", kind, key, repo, err)
	}
	if _, _, _, err := cliScope("repo", "", ""); err == nil {
		t.Fatal("repo scope without --repo was accepted")
	}
}

func TestDryRunStoreDoesNotCreatePersistentState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	db, cleanup, err := openWatchStore(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Join(home, ".shale")); !os.IsNotExist(err) {
		t.Fatalf("dry-run created persistent state: %v", err)
	}
}
