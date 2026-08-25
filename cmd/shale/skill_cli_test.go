package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNativeSkillCLIWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and exercises the real binary")
	}
	bin := filepath.Join(t.TempDir(), "shale")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := t.TempDir()
	library := t.TempDir()
	skillDir := filepath.Join(library, "release-guide")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	core := "---\nname: release-guide\ndescription: Guide safe releases\n---\n\n# Release\n\nRead [details](references/details.md).\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(core), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "details.md"), []byte("notarization-order marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(t.TempDir(), "SKILL.md")
	replacement := strings.Replace(core, "Read [details]", "Use the safer order, then read [details]", 1)
	if err := os.WriteFile(replacementPath, []byte(replacement), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "HOME="+home, "NO_COLOR=1")
		body, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("shale %v: %v\n%s", args, err, body)
		}
		return string(body)
	}

	imported := run("skill", "library", "import", library, "--name", "personal")
	if !strings.Contains(imported, "Imported 1 skill") || !strings.Contains(imported, "skill:personal/release-guide@") {
		t.Fatalf("import output:\n%s", imported)
	}
	searched := run("search", "notarization-order", "--kind", "skill")
	if !strings.Contains(searched, "#references/details.md") {
		t.Fatalf("progressive skill search output:\n%s", searched)
	}
	stableRef := "skill:personal/release-guide"
	proposed := run("skill", "propose", stableRef, "--lesson", "The safer order avoids failures", "--replacement", replacementPath)
	changeRef := regexp.MustCompile(`skill-change:[a-f0-9]+`).FindString(proposed)
	if changeRef == "" {
		t.Fatalf("proposal did not print a change ref:\n%s", proposed)
	}
	accepted := run("skill", "proposal", "accept", changeRef)
	if !strings.Contains(accepted, "accepted") {
		t.Fatalf("accept output:\n%s", accepted)
	}
	applied := run("skill", "apply", changeRef)
	exactRef := regexp.MustCompile(`skill:personal/release-guide@[a-f0-9]{64}`).FindString(applied)
	if exactRef == "" || !strings.Contains(applied, "Created native revision") {
		t.Fatalf("apply output:\n%s", applied)
	}
	target := filepath.Join(t.TempDir(), "agent-skills")
	run("skill", "target", "add", "codex", target)
	installed := run("skill", "install", exactRef, "--target", "codex")
	if !strings.Contains(installed, "Installed "+exactRef) {
		t.Fatalf("install output:\n%s", installed)
	}
	installedCore, err := os.ReadFile(filepath.Join(target, "release-guide", "SKILL.md"))
	if err != nil || !strings.Contains(string(installedCore), "safer order") {
		t.Fatalf("installed core = %q err=%v", installedCore, err)
	}
}
