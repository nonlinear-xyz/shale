package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nonlinear-xyz/shale/internal/store"
)

func newServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{DB: db, Log: io.Discard}
}

// run feeds frames through the server and returns the decoded responses.
func run(t *testing.T, s *Server, frames ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(frames, "\n")+"\n"), &out); err != nil {
		t.Fatalf("serve: %v", err)
	}
	var got []map[string]any
	dec := json.NewDecoder(&out)
	for {
		var m map[string]any
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode response: %v", err)
		}
		got = append(got, m)
	}
	return got
}

// A JSON-RPC notification has no id and MUST receive no response. Replying to one
// is a protocol violation that some clients treat as fatal, and it desynchronizes
// every subsequent id.
func TestNotificationsGetNoResponse(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2 (notifications must be silent)", len(got))
	}
	if got[0]["id"].(float64) != 1 || got[1]["id"].(float64) != 2 {
		t.Errorf("response ids out of order: %v, %v", got[0]["id"], got[1]["id"])
	}
}

func TestInitializeAndToolsList(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	)
	if len(got) != 2 {
		t.Fatalf("got %d responses, want 2", len(got))
	}

	init := got[0]["result"].(map[string]any)
	if init["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %v", init["protocolVersion"], protocolVersion)
	}

	tools := got[1]["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, raw := range tools {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"context_for_task", "search_evidence"} {
		if !names[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

// Tool failures must come back as CONTENT, not as JSON-RPC errors. A protocol
// error can abort the agent's turn; a content error lets it read the message and
// carry on without the packet.
func TestToolErrorsAreContentNotProtocolErrors(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_for_task","arguments":{"task":""}}}`,
	)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	if _, isErr := got[0]["error"]; isErr {
		t.Fatal("empty task produced a JSON-RPC error; it must be tool content")
	}
	text := got[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "error") {
		t.Errorf("expected an error payload in content, got %q", text)
	}
}

// An unknown method is one of the few things that IS a protocol error — the
// client asked for something that does not exist.
func TestUnknownMethodIsAProtocolError(t *testing.T) {
	s := newServer(t)
	got := run(t, s, `{"jsonrpc":"2.0","id":1,"method":"does/not/exist"}`)
	if len(got) != 1 {
		t.Fatalf("got %d responses, want 1", len(got))
	}
	e, ok := got[0]["error"].(map[string]any)
	if !ok {
		t.Fatal("unknown method did not produce a JSON-RPC error")
	}
	if e["code"].(float64) != -32601 {
		t.Errorf("code = %v, want -32601 (method not found)", e["code"])
	}
}

// An empty corpus must return a well-formed packet, not an error. A new install
// has nothing captured, and an agent's first call must not look like a crash.
func TestEmptyCorpusReturnsAWellFormedPacket(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"context_for_task","arguments":{"task":"add a signing step to the release pipeline"}}}`,
	)
	text := got[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)

	var p map[string]any
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("packet is not valid JSON: %v\n%s", err, text)
	}
	if p["retrieval"] != "recency_fallback" {
		t.Errorf("retrieval = %v, want recency_fallback on an empty corpus", p["retrieval"])
	}
	// The sections must be present and empty, never null: an agent that indexes
	// into a null section crashes on what should be an ordinary empty result.
	sections := p["sections"].(map[string]any)
	for _, k := range []string{"corrections", "evidence"} {
		if sections[k] == nil {
			t.Errorf("section %q is null; must be an empty array", k)
		}
	}
	if p["citations"] == nil {
		t.Error("citations is null; must be an empty array")
	}
}

// The response payload is a JSON string inside a text block, not MCP structured
// output. Changing that shape breaks every agent already reading one.
func TestResponseShapeIsSingleTextBlock(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_evidence","arguments":{"query":"anything"}}}`,
	)
	result := got[0]["result"].(map[string]any)
	if _, has := result["structuredContent"]; has {
		t.Error("result carries structuredContent; the contract is a single text block")
	}
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content has %d blocks, want exactly 1", len(content))
	}
	if content[0].(map[string]any)["type"] != "text" {
		t.Errorf("block type = %v, want text", content[0].(map[string]any)["type"])
	}
}
