package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPStdoutIsPureProtocol runs the real binary, because the property it
// defends cannot be observed from inside the process.
//
// Bubble Tea v1 calls lipgloss.HasDarkBackground() from a package init() —
// deliberately, as a workaround it documents removing in v2 — which writes an
// OSC 11 background-color query to os.Stdout and waits for the terminal to
// answer. That runs before main(), so no amount of care inside a command can
// prevent it; only the absence of a terminal does.
//
// Under `shale mcp` stdout is the JSON-RPC transport. In every real deployment
// it is a pipe, so the query never fires — but that is a property of the
// deployment, not of the code, and adding a TUI to this binary is what made it
// load-bearing. This test holds it: styling forced on, the transport stays
// pure, and every frame parses.
func TestMCPStdoutIsPureProtocol(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped under -short")
	}

	bin := filepath.Join(t.TempDir(), "shale")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_evidence","arguments":{"query":"worktree"}}}`,
	}, "\n") + "\n"

	cmd := exec.Command(bin, "mcp")
	cmd.Stdin = strings.NewReader(requests)
	// Force every signal that would turn styling ON, so a regression cannot
	// hide behind this test's own environment being colorless.
	cmd.Env = append(os.Environ(),
		"HOME="+t.TempDir(), // a fresh empty store, not the developer's
		"CLICOLOR_FORCE=1",
		"COLORTERM=truecolor",
		"TERM=xterm-256color",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	if i := bytes.IndexByte(stdout.Bytes(), 0x1b); i >= 0 {
		t.Fatalf("escape byte at offset %d on the JSON-RPC transport:\n%q",
			i, stdout.String()[max(0, i-40):min(stdout.Len(), i+40)])
	}

	frames := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("frame %d does not parse as JSON: %v\n%q", frames, err, line)
		}
		if msg.JSONRPC != "2.0" {
			t.Errorf("frame %d has jsonrpc=%q, want 2.0", frames, msg.JSONRPC)
		}
		frames++
	}
	if frames != 3 {
		t.Errorf("got %d frames, want 3 (one per request)", frames)
	}
}
