package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	for _, want := range []string{
		"context_for_task", "search_evidence", "remember_explicit",
		"propose_memory", "save_checkpoint", "read_ref", "search_skills",
		"propose_skill_change",
	} {
		if !names[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

func TestSkillToolsUseProgressiveDisclosureAndProposalOnlyMutation(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()
	lib, _, err := s.DB.RegisterSkillLibrary(ctx, store.SkillLibraryInput{
		Key: "nonlinear-xyz/factory-kit", Kind: store.SkillLibraryNative,
	})
	if err != nil {
		t.Fatal(err)
	}
	core := []byte("---\nname: release-guide\ndescription: Guide safe releases\n---\n\n# Release\n\nRead [details](references/details.md).\n")
	reference := []byte("# Details\n\nThe violet-notarization-marker belongs here.\n")
	skill, _, _, err := s.DB.PutSkillRevision(ctx, store.SkillRevisionInput{
		LibraryID: lib.ID, Name: "release-guide", Status: store.SkillActive,
		Description: "Guide safe releases", Files: []store.SkillFileInput{
			{Path: "SKILL.md", Content: core, Mode: 0o644},
			{Path: "references/details.md", Content: reference, Mode: 0o644},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	searched := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_skills","arguments":{"query":"violet-notarization-marker"}}}`,
	)
	searchPayload := toolPayload(t, searched[0])
	results := searchPayload["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("skill search results = %d", len(results))
	}
	hit := results[0].(map[string]any)
	fileRef := hit["relevantFileRef"].(string)
	if !strings.Contains(fileRef, "@"+skill.TreeHash+"#references/details.md") {
		t.Fatalf("search did not return exact relevant file ref: %v", hit)
	}
	if _, loadedWholePackage := hit["content"]; loadedWholePackage {
		t.Fatalf("search loaded authoritative content instead of metadata: %v", hit)
	}

	readFile := run(t, s, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_ref","arguments":{"ref":%q}}}`,
		fileRef,
	))
	filePayload := toolPayload(t, readFile[0])
	if filePayload["kind"] != "skill-file" || !strings.Contains(filePayload["content"].(string), "violet-notarization-marker") {
		t.Fatalf("read exact skill file = %v", filePayload)
	}

	exactRef := skill.VersionedRef()
	readCore := run(t, s, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_ref","arguments":{"ref":%q}}}`,
		exactRef,
	))
	corePayload := toolPayload(t, readCore[0])
	if corePayload["kind"] != "skill" || !strings.Contains(corePayload["content"].(string), "Read [details]") {
		t.Fatalf("read skill core = %v", corePayload)
	}
	if len(corePayload["availableFiles"].([]any)) != 2 {
		t.Fatalf("available files = %v", corePayload["availableFiles"])
	}
	packetResponse := run(t, s,
		`{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"context_for_task","arguments":{"task":"violet-notarization-marker"}}}`,
	)
	packet := toolPayload(t, packetResponse[0])
	if _, present := packet["skills"]; present {
		t.Fatalf("task packet duplicated harness-owned skills: %v", packet)
	}
	if _, present := packet["sections"].(map[string]any)["skills"]; present {
		t.Fatalf("task packet sections included skills: %v", packet["sections"])
	}

	replacement := "---\nname: release-guide\ndescription: Guide safe releases\n---\n\n# Release\n\nUse the safer order and read [details](references/details.md).\n"
	proposed := run(t, s, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"propose_skill_change","arguments":{"skillRef":%q,"lesson":"The safer order avoids a failure","replacement":%q}}}`,
		exactRef, replacement,
	))
	proposal := toolPayload(t, proposed[0])
	if proposal["status"] != string(store.SkillChangePending) || !strings.Contains(proposal["approvalCommand"].(string), "shale skill proposal accept") {
		t.Fatalf("skill proposal boundary = %v", proposal)
	}
	current, err := s.DB.ResolveSkillRef(ctx, store.SkillRef{LibraryKey: lib.Key, Name: skill.Name})
	if err != nil || current.TreeHash != skill.TreeHash {
		t.Fatalf("MCP proposal changed active skill: %+v err=%v", current, err)
	}

	listed := run(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	tools := listed[0]["result"].(map[string]any)["tools"].([]any)
	for _, raw := range tools {
		name := raw.(map[string]any)["name"].(string)
		if name == "accept_skill_change" || name == "apply_skill_change" || name == "install_skill" {
			t.Fatalf("MCP exposed forbidden human transition %q", name)
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
	for _, k := range []string{"checkpoints", "memories", "runbooks", "corrections", "evidence"} {
		if sections[k] == nil {
			t.Errorf("section %q is null; must be an empty array", k)
		}
	}
	if p["citations"] == nil {
		t.Error("citations is null; must be an empty array")
	}
}

func TestMemoryWritesRespectApprovalBoundaryAndReadRef(t *testing.T) {
	s := newServer(t)
	proposed := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"propose_memory","arguments":{"text":"Release signing needs the purple inferred key","repo":"acme/app"}}}`,
	)
	proposal := toolPayload(t, proposed[0])
	if proposal["status"] != string(store.ArtifactPending) {
		t.Fatalf("proposal status = %v, want pending", proposal["status"])
	}
	if !strings.Contains(proposal["approvalCommand"].(string), "shale accept memory:") {
		t.Errorf("proposal has no actionable approval command: %v", proposal)
	}

	direct := run(t, s,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"remember_explicit","arguments":{"text":"Release signing uses the green explicit key","repo":"acme/app","evidenceRefs":[]}}}`,
	)
	memory := toolPayload(t, direct[0])
	if memory["status"] != string(store.ArtifactActive) {
		t.Fatalf("direct memory status = %v, want active", memory["status"])
	}

	packetResponse := run(t, s,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"context_for_task","arguments":{"task":"release signing key","repo":"acme/app"}}}`,
	)
	packet := toolPayload(t, packetResponse[0])
	memories := packet["sections"].(map[string]any)["memories"].([]any)
	if len(memories) != 1 {
		t.Fatalf("packet memories = %d, want only the active memory", len(memories))
	}
	content := memories[0].(map[string]any)["content"].(string)
	if !strings.Contains(content, "green explicit") || strings.Contains(content, "purple inferred") {
		t.Errorf("approval boundary failed; served content %q", content)
	}

	ref := memory["versionedRef"].(string)
	read := run(t, s, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_ref","arguments":{"ref":%q}}}`,
		ref,
	))
	resolved := toolPayload(t, read[0])
	if resolved["versionedRef"] != ref || resolved["contentPresent"] != true {
		t.Errorf("read_ref did not resolve exact memory version: %v", resolved)
	}
}

func TestCheckpointsChainAndLatestTaskCheckpointIsServed(t *testing.T) {
	s := newServer(t)
	first := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save_checkpoint","arguments":{"taskKey":"REL-42","repo":"acme/app","goal":"Ship release","summary":"Signing is done"}}}`,
	)
	firstPayload := toolPayload(t, first[0])
	second := run(t, s,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"save_checkpoint","arguments":{"taskKey":"REL-42","repo":"acme/app","goal":"Ship release","summary":"Notarization is done","nextActions":["Publish"]}}}`,
	)
	secondPayload := toolPayload(t, second[0])
	if secondPayload["previousCheckpoint"] != firstPayload["versionedRef"] {
		t.Errorf("checkpoint chain = %v, want %v", secondPayload["previousCheckpoint"], firstPayload["versionedRef"])
	}

	response := run(t, s,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"context_for_task","arguments":{"task":"continue the release","taskKey":"REL-42","repo":"acme/app"}}}`,
	)
	packet := toolPayload(t, response[0])
	checkpoints := packet["sections"].(map[string]any)["checkpoints"].([]any)
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoints = %d, want latest one", len(checkpoints))
	}
	got := checkpoints[0].(map[string]any)
	if got["ref"] != secondPayload["versionedRef"] || !strings.Contains(got["content"].(string), "Notarization is done") {
		t.Errorf("latest checkpoint not served: %v", got)
	}
}

func TestMutationValidationErrorsAreToolContent(t *testing.T) {
	s := newServer(t)
	got := run(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember_explicit","arguments":{"text":"remember this","scope":"repo"}}}`,
	)
	if _, isErr := got[0]["error"]; isErr {
		t.Fatal("scope validation produced a JSON-RPC error")
	}
	payload := toolPayload(t, got[0])
	if !strings.Contains(payload["error"].(string), "requires repo") {
		t.Errorf("unexpected validation payload: %v", payload)
	}
}

func toolPayload(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %v", response)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool payload is not JSON: %v\n%s", err, text)
	}
	return payload
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

func TestOversizedProtocolFrameIsRejectedBeforeDispatch(t *testing.T) {
	s := newServer(t)
	var out bytes.Buffer
	err := s.Serve(context.Background(), strings.NewReader(strings.Repeat("x", maxFrameBytes+1)+"\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized frame error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversized frame emitted protocol output: %q", out.String())
	}
}
