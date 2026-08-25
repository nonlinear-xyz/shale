// Package mcp serves the local corpus to agents over the Model Context Protocol.
//
// Transport is stdio JSON-RPC 2.0: the agent launches this process and speaks
// line-delimited JSON over stdin/stdout. That means STDOUT IS THE PROTOCOL —
// anything else written there corrupts the stream and the client disconnects with
// a parse error that looks like a crash. Every diagnostic in this package goes to
// stderr, deliberately.
//
// The protocol is implemented by hand rather than pulled from a dependency. The
// surface is small, and owning it keeps the module dependency-free apart from the
// SQLite driver.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
	"github.com/nonlinear-xyz/shale/internal/pack"
	skillops "github.com/nonlinear-xyz/shale/internal/skills"
	"github.com/nonlinear-xyz/shale/internal/store"
)

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2024-11-05"

// Stdio MCP frames are one JSON value per line. Artifact bodies are capped at
// 64 KiB, so a 1 MiB frame leaves ample envelope room while preventing an agent
// from making the server allocate an unbounded request before validation runs.
const maxFrameBytes = 4 << 20

// Server answers MCP requests from the local store. Mutating tools are narrow by
// design: direct writes are only for an explicit user request, while inferred
// knowledge can only enter the pending proposal queue.
type Server struct {
	DB  *store.DB
	Log io.Writer // stderr; never stdout
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve runs the JSON-RPC loop until in is exhausted or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	enc := json.NewEncoder(out)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil // cancelled is a clean shutdown, not a failure
		}
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			return fmt.Errorf("decode: %w", err)
		}

		// A request with no id is a NOTIFICATION and must receive no response.
		// Replying to one is a protocol violation that some clients treat as fatal.
		isNotification := len(req.ID) == 0 || string(req.ID) == "null"

		result, rpcErr := s.dispatch(ctx, req)
		if isNotification {
			continue
		}

		resp := response{JSONRPC: "2.0", ID: req.ID}
		if rpcErr != nil {
			resp.Error = rpcErr
		} else {
			resp.Result = result
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("encode: %w", err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("decode frame (maximum %d bytes): %w", maxFrameBytes, err)
	}
	return nil
}

func (s *Server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{
				"name":    buildinfo.Name,
				"version": buildinfo.Version,
			},
		}, nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": toolDefinitions()}, nil

	case "tools/call":
		return s.callTool(ctx, req.Params)

	default:
		return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name": "context_for_task",
			// The description IS the routing logic — it is the only thing that decides
			// whether an agent calls this at all. It states when to call, what comes
			// back, and how to read the provenance.
			"description": "Call this at the START of any nontrivial task. Returns a bounded packet with " +
				"the latest exact-task checkpoint, approved memories, relevant runbooks, things that went " +
				"wrong before, and supporting transcript passages. Pending proposals are excluded. Every " +
				"item carries a resolvable ref plus source, scope, authority, and freshness provenance. The packet " +
				"reports how it retrieved (exact match, loosened match, or recency fallback) and what it " +
				"had to leave out — treat a recency_fallback packet with much less confidence.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task": map[string]any{
						"type":        "string",
						"minLength":   3,
						"description": "What you are about to do, in a sentence or two",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": `Repository full name in "owner/name" format, if the task is repo-scoped`,
					},
					"taskKey": map[string]any{
						"type":        "string",
						"description": "Stable task or issue key; includes the latest matching checkpoint",
					},
					"sinceDays": map[string]any{
						"type": "integer", "minimum": 1, "maximum": 365,
						"description": "Evidence window in days (default 30)",
					},
					"maxTokens": map[string]any{
						"type": "integer", "minimum": 500, "maximum": 24000,
						"description": "Token budget for the packet (default 8000)",
					},
				},
				"required": []string{"task"},
			},
		},
		{
			"name": "search_evidence",
			"description": "Search this machine's captured agent sessions by keyword. Lexical search — " +
				"exact identifiers, file names, error messages and commands work best; a paraphrased " +
				"question will not. Supports AND, OR and \"quoted phrases\". Returns ranked passages " +
				"with provenance. Use this for a targeted dig; use context_for_task to start work.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type": "string", "minLength": 2,
						"description": "Search terms; supports AND, OR and quoted phrases",
					},
					"repo": map[string]any{
						"type":        "string",
						"description": `Filter to one repository ("owner/name")`,
					},
					"kind": map[string]any{
						"type": "string", "enum": []string{"transcript", "error"},
						"description": `Filter to a passage kind; "error" finds only things that failed`,
					},
					"sinceDays": map[string]any{
						"type": "integer", "minimum": 1, "maximum": 365,
						"description": "Only search sessions from the last N days (default: all history)",
					},
					"limit": map[string]any{
						"type": "integer", "minimum": 1, "maximum": 50,
						"description": "Max results (default 10)",
					},
				},
				"required": []string{"query"},
			},
		},
		memoryToolDefinition("remember_explicit", "Write a durable memory only when the user explicitly asks you to remember it. The memory is active immediately. Never use this for an inference; use propose_memory instead."),
		memoryToolDefinition("propose_memory", "Propose inferred durable knowledge for human review. Proposals are pending and are never recalled into task context until the user accepts them with `shale accept <ref>`."),
		{
			"name":        "save_checkpoint",
			"description": "Save a structured handoff for a stable task key. Use at a meaningful stopping point so a later agent can resume from the goal, decisions, open loops, and next actions. Checkpoints are active immediately and automatically chain to the prior checkpoint for that task.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"taskKey":            map[string]any{"type": "string", "minLength": 1, "description": "Stable issue or task key"},
					"repo":               map[string]any{"type": "string", "description": `Repository full name in "owner/name" format, when known`},
					"title":              map[string]any{"type": "string", "description": "Short checkpoint title"},
					"goal":               map[string]any{"type": "string", "description": "What the task is trying to accomplish"},
					"summary":            map[string]any{"type": "string", "description": "Current state and progress"},
					"decisions":          stringArraySchema("Decisions already made and why"),
					"artifacts":          stringArraySchema("Files, branches, PRs, commands, or other outputs"),
					"openLoops":          stringArraySchema("Unresolved questions or risks"),
					"nextActions":        stringArraySchema("Concrete next steps in order"),
					"evidenceRefs":       stringArraySchema("Shale refs supporting the handoff"),
					"previousCheckpoint": map[string]any{"type": "string", "description": "Optional checkpoint ref to chain explicitly; defaults to the latest for this task"},
				},
				"required": []string{"taskKey", "goal", "summary"},
			},
		},
		{
			"name":        "read_ref",
			"description": "Resolve a Shale citation and provenance. Skill refs return the exact core SKILL.md plus compact refs for bundled files; skill file refs ending in #path return only that file. Also supports memories, checkpoints, runbooks, instructions, chunks, sessions, and skill changes.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ref": map[string]any{"type": "string", "minLength": 1, "description": "A Shale ref copied from a packet or command"},
				},
				"required": []string{"ref"},
			},
		},
		{
			"name":        "search_skills",
			"description": "Search the local skill catalog without loading whole skill packages. Returns routing metadata, an excerpt from the relevant exact file, and portable versioned file refs. Read only the files needed with read_ref. SQLite is discovery only; exact files remain authoritative.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query":   map[string]any{"type": "string", "minLength": 1, "description": "Capability, procedure, identifier, or error to find"},
					"library": map[string]any{"type": "string", "description": "Optional portable library key"},
					"status":  map[string]any{"type": "string", "enum": []string{"active", "draft"}, "description": "Default: active"},
					"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 50},
				},
				"required": []string{"query"},
			},
		},
		{
			"name":        "propose_skill_change",
			"description": "Record a newly learned lesson against an exact skill revision for human review. Optionally include one complete replacement SKILL.md. This tool cannot accept, apply, install, commit, push, or mutate the source library; use the human CLI review queue for those actions.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"skillRef":     map[string]any{"type": "string", "minLength": 1, "description": "Stable or exact skill ref"},
					"lesson":       map[string]any{"type": "string", "minLength": 1, "description": "What was learned and should persist"},
					"rationale":    map[string]any{"type": "string", "description": "Why the skill should change"},
					"replacement":  map[string]any{"type": "string", "description": "Optional complete replacement SKILL.md; only this file may be proposed in v1"},
					"evidenceRefs": stringArraySchema("Shale refs supporting the lesson"),
				},
				"required": []string{"skillRef", "lesson"},
			},
		},
	}
}

func memoryToolDefinition(name, description string) map[string]any {
	return map[string]any{
		"name": name, "description": description,
		"inputSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":         map[string]any{"type": "string", "minLength": 1, "description": "The durable fact, preference, or decision"},
				"title":        map[string]any{"type": "string", "description": "Short label; derived from text when omitted"},
				"trigger":      map[string]any{"type": "string", "description": "When this memory is useful"},
				"scope":        map[string]any{"type": "string", "enum": []string{"user", "repo", "task"}, "description": "Recall boundary; inferred from taskKey/repo when omitted"},
				"repo":         map[string]any{"type": "string", "description": `Repository full name in "owner/name" format; required for repo scope`},
				"taskKey":      map[string]any{"type": "string", "description": "Stable task key; required for task scope"},
				"evidenceRefs": stringArraySchema("Shale refs supporting this memory"),
			},
			"required": []string{"text"},
		},
	}
}

func stringArraySchema(description string) map[string]any {
	return map[string]any{
		"type": "array", "maxItems": 100,
		"items": map[string]any{"type": "string"}, "description": description,
	}
}

type toolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var call toolCall
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid params: " + err.Error()}
	}

	switch call.Name {
	case "context_for_task":
		return s.contextForTask(ctx, call.Arguments)
	case "search_evidence":
		return s.searchEvidence(ctx, call.Arguments)
	case "remember_explicit":
		return s.writeMemory(ctx, call.Arguments, false)
	case "propose_memory":
		return s.writeMemory(ctx, call.Arguments, true)
	case "save_checkpoint":
		return s.saveCheckpoint(ctx, call.Arguments)
	case "read_ref":
		return s.readRef(ctx, call.Arguments)
	case "search_skills":
		return s.searchSkills(ctx, call.Arguments)
	case "propose_skill_change":
		return s.proposeSkillChange(ctx, call.Arguments)
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
	}
}

func (s *Server) contextForTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Task    string `json:"task"`
		Repo    string `json:"repo"`
		TaskKey string `json:"taskKey"`
		// Note the boundary rename: the wire calls this maxTokens, the packet
		// builder calls it TokenBudget. Preserved from Observatory's contract so an
		// agent wired to a hub can be pointed here unchanged.
		MaxTokens int `json:"maxTokens"`
		SinceDays int `json:"sinceDays"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if err := validateSizedText("task", args.Task, true, 8<<10); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("repo", args.Repo, false, 4096); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("taskKey", args.TaskKey, false, 1024); err != nil {
		return toolError(err.Error()), nil
	}
	if args.SinceDays < 0 || args.SinceDays > 365 {
		return toolError("sinceDays must be between 1 and 365 when provided"), nil
	}

	p, err := pack.Build(ctx, s.DB, pack.Input{
		Task:        args.Task,
		TaskKey:     strings.TrimSpace(args.TaskKey),
		Repo:        strings.TrimSpace(args.Repo),
		SinceDays:   args.SinceDays,
		TokenBudget: args.MaxTokens,
	})
	if err != nil {
		s.logf("context_for_task: %v", err)
		// Errors are returned as tool CONTENT, not as JSON-RPC errors. A protocol
		// error can abort the agent's turn; a content error lets it read the
		// message and carry on without the packet.
		return textResult(`{"error":"packet assembly failed"}`), nil
	}
	return jsonResult(p), nil
}

func (s *Server) searchEvidence(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Query     string `json:"query"`
		Repo      string `json:"repo"`
		Kind      string `json:"kind"`
		SinceDays int    `json:"sinceDays"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if err := validateSizedText("query", args.Query, true, 8<<10); err != nil {
		return toolError(err.Error()), nil
	}
	if args.Kind != "" && args.Kind != "transcript" && args.Kind != "error" {
		return toolError("kind must be transcript or error"), nil
	}
	if args.SinceDays < 0 || args.SinceDays > 365 {
		return toolError("sinceDays must be between 1 and 365 when provided"), nil
	}
	if args.Limit <= 0 {
		args.Limit = 10
	} else if args.Limit > 50 {
		args.Limit = 50
	}

	hits, err := s.DB.SearchChunks(ctx, args.Query, strings.TrimSpace(args.Repo), args.Kind, args.SinceDays, args.Limit)
	if err != nil {
		s.logf("search_evidence: %v", err)
		return textResult(`{"error":"search failed"}`), nil
	}

	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		results = append(results, map[string]any{
			"ref":     h.Ref(),
			"excerpt": h.Excerpt,
			"score":   h.Score,
			"provenance": map[string]any{
				"source":     h.Source,
				"repo":       h.Scope,
				"occurredAt": h.OccurredAt,
				"epistemic":  "observed",
				"kind":       h.Kind,
				"lines":      []int{h.LineStart, h.LineEnd},
			},
		})
	}
	return jsonResult(map[string]any{"query": args.Query, "results": results}), nil
}

func (s *Server) searchSkills(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Query   string `json:"query"`
		Library string `json:"library"`
		Status  string `json:"status"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if err := validateSizedText("query", args.Query, true, 8<<10); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("library", args.Library, false, 240); err != nil {
		return toolError(err.Error()), nil
	}
	status := store.SkillStatus(strings.TrimSpace(args.Status))
	if status == "" {
		status = store.SkillActive
	}
	if status != store.SkillActive && status != store.SkillDraft {
		return toolError("status must be active or draft"), nil
	}
	if args.Limit <= 0 {
		args.Limit = 10
	} else if args.Limit > 50 {
		args.Limit = 50
	}
	hits, err := s.DB.SearchSkills(ctx, args.Query, strings.TrimSpace(args.Library), status, args.Limit)
	if err != nil {
		s.logf("search_skills: %v", err)
		return toolError("skill search failed"), nil
	}
	results := make([]map[string]any, 0, len(hits))
	for _, hit := range hits {
		exact := store.SkillRef{LibraryKey: hit.LibraryKey, Name: hit.Name, TreeHash: hit.TreeHash}
		detail, err := s.DB.ResolveSkillRef(ctx, exact)
		if err != nil {
			s.logf("search_skills resolve %s: %v", exact.String(), err)
			continue
		}
		files := skillFileSummaries(detail, 25)
		results = append(results, map[string]any{
			"ref": exact.String(), "name": hit.Name, "description": hit.Description,
			"status": hit.Status, "relevantFileRef": hit.FileRef(),
			"relevantPath": hit.FilePath, "excerpt": hit.Excerpt,
			"availableFiles": files, "fileCount": len(detail.Files),
			"filesTruncated": len(files) < len(detail.Files),
		})
	}
	return jsonResult(map[string]any{
		"query": args.Query, "results": results,
		"note": "Search excerpts are discovery hints. Read the exact SKILL.md and only needed file refs before following a procedure.",
	}), nil
}

func (s *Server) proposeSkillChange(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		SkillRef     string   `json:"skillRef"`
		Lesson       string   `json:"lesson"`
		Rationale    string   `json:"rationale"`
		Replacement  string   `json:"replacement"`
		EvidenceRefs []string `json:"evidenceRefs"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if err := validateSizedText("skillRef", args.SkillRef, true, 512); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("lesson", args.Lesson, true, store.ArtifactContentMax); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("rationale", args.Rationale, false, store.ArtifactContentMax); err != nil {
		return toolError(err.Error()), nil
	}
	if len(args.Replacement) > 1<<20 {
		return toolError("replacement exceeds 1 MiB"), nil
	}
	if len(args.EvidenceRefs) > 100 {
		return toolError("evidenceRefs has more than 100 items"), nil
	}
	ref, err := store.ParseSkillRef(args.SkillRef)
	if err != nil {
		return toolError("skillRef is invalid"), nil
	}
	change, warnings, err := skillops.ProposeChange(ctx, s.DB, skillops.ProposalInput{
		Ref: ref, Lesson: args.Lesson, Rationale: args.Rationale,
		EvidenceRefs: args.EvidenceRefs, Replacement: []byte(args.Replacement),
		Source: "mcp", Actor: "agent",
	})
	if err != nil {
		s.logf("propose_skill_change: %v", err)
		return toolError(err.Error()), nil
	}
	result := skillChangeSummary(change)
	result["warnings"] = warnings
	result["approvalCommand"] = "shale skill proposal accept " + change.Ref()
	result["message"] = "Proposal saved for human review. No source, installed skill, Git branch, or agent behavior was changed."
	return jsonResult(result), nil
}

func skillFileSummaries(detail store.SkillDetail, limit int) []map[string]any {
	if limit <= 0 || limit > len(detail.Files) {
		limit = len(detail.Files)
	}
	base := store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash}
	out := make([]map[string]any, 0, limit)
	for _, file := range detail.Files[:limit] {
		out = append(out, map[string]any{
			"path": file.Path, "size": file.Size,
			"ref": store.SkillFileRef{SkillRef: base, Path: file.Path}.String(),
		})
	}
	return out
}

type memoryArgs struct {
	Text         string   `json:"text"`
	Title        string   `json:"title"`
	Trigger      string   `json:"trigger"`
	Scope        string   `json:"scope"`
	Repo         string   `json:"repo"`
	TaskKey      string   `json:"taskKey"`
	EvidenceRefs []string `json:"evidenceRefs"`
}

func (s *Server) writeMemory(ctx context.Context, raw json.RawMessage, pending bool) (any, *rpcError) {
	var args memoryArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if err := validateText("text", args.Text, true); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("title", args.Title, false, 1024); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("trigger", args.Trigger, false, 16<<10); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateEvidenceRefs(args.EvidenceRefs); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("repo", args.Repo, false, 4096); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("taskKey", args.TaskKey, false, 1024); err != nil {
		return toolError(err.Error()), nil
	}
	scopeKind, scopeKey, repo, err := resolveScope(args.Scope, args.Repo, args.TaskKey)
	if err != nil {
		return toolError(err.Error()), nil
	}
	status, authority, eventKind := store.ArtifactActive, "asserted", store.KindMemoryAsserted
	if pending {
		status, authority, eventKind = store.ArtifactPending, "proposed", store.KindMemoryProposed
	}
	contentTaskKey := ""
	if scopeKind == store.ScopeTask {
		contentTaskKey = strings.TrimSpace(args.TaskKey)
	}
	a, _, err := s.DB.PutArtifact(ctx, store.ArtifactInput{
		Kind: store.ArtifactMemory, Status: status,
		ScopeKind: scopeKind, ScopeKey: scopeKey, Repo: repo,
		Title: strings.TrimSpace(args.Title), Origin: "native", Authority: authority,
		Source: "mcp", Actor: "agent", EventKind: eventKind,
		Content: store.ArtifactContent{
			Text: strings.TrimSpace(args.Text), Trigger: strings.TrimSpace(args.Trigger),
			TaskKey: contentTaskKey, EvidenceRefs: args.EvidenceRefs,
		},
	})
	if err != nil {
		s.logf("write memory: %v", err)
		return toolError("memory write failed"), nil
	}
	result := artifactSummary(a)
	if pending {
		result["approvalCommand"] = "shale accept " + a.Ref()
		result["message"] = "Proposal saved but excluded from recall until accepted."
	} else {
		result["message"] = "Memory saved and active."
	}
	return jsonResult(result), nil
}

type checkpointArgs struct {
	TaskKey            string   `json:"taskKey"`
	Repo               string   `json:"repo"`
	Title              string   `json:"title"`
	Goal               string   `json:"goal"`
	Summary            string   `json:"summary"`
	Decisions          []string `json:"decisions"`
	Artifacts          []string `json:"artifacts"`
	OpenLoops          []string `json:"openLoops"`
	NextActions        []string `json:"nextActions"`
	EvidenceRefs       []string `json:"evidenceRefs"`
	PreviousCheckpoint string   `json:"previousCheckpoint"`
}

func (s *Server) saveCheckpoint(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args checkpointArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	for _, field := range []struct {
		name, value string
		required    bool
	}{
		{"taskKey", args.TaskKey, true}, {"title", args.Title, false},
		{"goal", args.Goal, true}, {"summary", args.Summary, true},
	} {
		if err := validateText(field.name, field.value, field.required); err != nil {
			return toolError(err.Error()), nil
		}
	}
	if err := validateSizedText("taskKey", args.TaskKey, true, 1024); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("title", args.Title, false, 1024); err != nil {
		return toolError(err.Error()), nil
	}
	if err := validateSizedText("repo", args.Repo, false, 4096); err != nil {
		return toolError(err.Error()), nil
	}
	for name, values := range map[string][]string{
		"decisions": args.Decisions, "artifacts": args.Artifacts,
		"openLoops": args.OpenLoops, "nextActions": args.NextActions,
	} {
		if err := validateStringList(name, values); err != nil {
			return toolError(err.Error()), nil
		}
	}
	if err := validateEvidenceRefs(args.EvidenceRefs); err != nil {
		return toolError(err.Error()), nil
	}
	previous := strings.TrimSpace(args.PreviousCheckpoint)
	if previous != "" {
		ref, err := store.ParseArtifactRef(previous)
		if err != nil || ref.Kind != store.ArtifactCheckpoint {
			return toolError("previousCheckpoint must be a checkpoint ref"), nil
		}
		prior, err := s.DB.ResolveArtifactRef(ctx, ref)
		if err != nil || !prior.ContentPresent {
			return toolError("previousCheckpoint could not be resolved"), nil
		}
		if prior.Content.TaskKey != strings.TrimSpace(args.TaskKey) {
			return toolError("previousCheckpoint belongs to a different task"), nil
		}
		if args.Repo != "" && prior.Repo != "" && prior.Repo != strings.TrimSpace(args.Repo) {
			return toolError("previousCheckpoint belongs to a different repository"), nil
		}
		previous = prior.VersionedRef()
	} else if prior, err := s.DB.LatestCheckpoint(ctx, strings.TrimSpace(args.Repo), strings.TrimSpace(args.TaskKey)); err == nil {
		previous = prior.VersionedRef()
	} else if !errors.Is(err, store.ErrArtifactNotFound) {
		s.logf("find prior checkpoint: %v", err)
		return toolError("checkpoint lookup failed"), nil
	}
	a, _, err := s.DB.PutArtifact(ctx, store.ArtifactInput{
		Kind: store.ArtifactCheckpoint, Status: store.ArtifactActive,
		ScopeKind: store.ScopeTask, ScopeKey: strings.TrimSpace(args.TaskKey), Repo: strings.TrimSpace(args.Repo),
		Title: strings.TrimSpace(args.Title), Origin: "native", Authority: "asserted",
		Source: "mcp", Actor: "agent", EventKind: store.KindCheckpointSaved,
		Content: store.ArtifactContent{
			TaskKey: strings.TrimSpace(args.TaskKey), Goal: strings.TrimSpace(args.Goal),
			Summary: strings.TrimSpace(args.Summary), Decisions: args.Decisions,
			Artifacts: args.Artifacts, OpenLoops: args.OpenLoops, NextActions: args.NextActions,
			EvidenceRefs: args.EvidenceRefs, PreviousCheckpoint: previous,
		},
	})
	if err != nil {
		s.logf("save checkpoint: %v", err)
		return toolError("checkpoint write failed"), nil
	}
	result := artifactSummary(a)
	result["previousCheckpoint"] = previous
	result["message"] = "Checkpoint saved."
	return jsonResult(result), nil
}

func (s *Server) readRef(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	value := strings.TrimSpace(args.Ref)
	if value == "" {
		return toolError("ref is required"), nil
	}
	if ref, err := store.ParseSkillFileRef(value); err == nil {
		body, err := s.DB.ReadSkillFile(ctx, ref.SkillRef, ref.Path)
		if err != nil {
			return toolError("skill file ref not found"), nil
		}
		return jsonResult(map[string]any{
			"ref": ref.String(), "kind": "skill-file", "path": ref.Path,
			"content": string(body),
			"provenance": map[string]any{
				"library": ref.LibraryKey, "skill": ref.Name, "treeHash": ref.TreeHash,
			},
		}), nil
	}
	if ref, err := store.ParseSkillRef(value); err == nil {
		detail, err := s.DB.ResolveSkillRef(ctx, ref)
		if err != nil {
			return toolError("skill ref not found"), nil
		}
		exact := store.SkillRef{LibraryKey: detail.LibraryKey, Name: detail.Name, TreeHash: detail.TreeHash}
		core, err := s.DB.ReadSkillFile(ctx, exact, "SKILL.md")
		if err != nil {
			return toolError("skill core file is unavailable"), nil
		}
		return jsonResult(map[string]any{
			"ref": detail.Ref(), "versionedRef": exact.String(), "kind": "skill",
			"name": detail.Name, "description": detail.Description,
			"status": detail.Status, "content": string(core),
			"availableFiles": skillFileSummaries(detail, 25), "fileCount": len(detail.Files),
			"filesTruncated": len(detail.Files) > 25,
			"provenance": map[string]any{
				"library": detail.LibraryKey, "treeHash": detail.TreeHash,
				"sourceHead": detail.Revision.SourceHead,
			},
		}), nil
	}
	if ref, err := store.ParseSkillChangeRef(value); err == nil {
		change, err := s.DB.SkillChange(ctx, ref.ID)
		if err != nil {
			return toolError("skill change ref not found"), nil
		}
		return jsonResult(skillChangeSummary(change)), nil
	}
	if ref, err := store.ParseArtifactRef(value); err == nil {
		a, err := s.DB.ResolveArtifactRef(ctx, ref)
		if err != nil {
			if !errors.Is(err, store.ErrArtifactNotFound) {
				s.logf("read_ref %s: %v", value, err)
			}
			return toolError("artifact ref not found"), nil
		}
		result := artifactSummary(a)
		result["contentPresent"] = a.ContentPresent
		if a.ContentPresent {
			result["content"] = a.Content
			result["rendered"] = a.Content.RenderText(a.Kind)
		}
		return jsonResult(result), nil
	}
	ref, err := store.ParseRef(value)
	if err != nil {
		return toolError("invalid Shale ref"), nil
	}
	if ref.HasChunk {
		hit, err := s.DB.Chunk(ctx, ref.EventSeq, ref.ChunkIndex)
		if err != nil {
			return toolError("chunk ref not found"), nil
		}
		return jsonResult(map[string]any{
			"ref": hit.Ref(), "kind": "chunk", "content": hit.Body,
			"provenance": map[string]any{
				"source": hit.Source, "repo": hit.Scope, "occurredAt": hit.OccurredAt,
				"lines": []int{hit.LineStart, hit.LineEnd}, "chunkKind": hit.Kind,
			},
		}), nil
	}
	info, err := s.DB.Session(ctx, ref.EventSeq)
	if err != nil {
		return toolError("session ref not found"), nil
	}
	return jsonResult(map[string]any{
		"ref": fmt.Sprintf("session:%d", info.Seq), "kind": "session",
		"source": info.Source, "repo": info.Scope, "occurredAt": info.OccurredAt,
		"record": info.Record,
	}), nil
}

func artifactSummary(a store.Artifact) map[string]any {
	return map[string]any{
		"ref": a.Ref(), "versionedRef": a.VersionedRef(), "kind": a.Kind,
		"status": a.Status, "title": a.Title, "scope": a.ScopeKind,
		"scopeKey": a.ScopeKey, "repo": a.Repo, "origin": a.Origin,
		"authority": a.Authority, "source": a.Source, "sourcePointer": a.SourcePointer,
		"createdAt": a.CreatedAt, "updatedAt": a.UpdatedAt,
	}
}

func skillChangeSummary(c store.SkillChange) map[string]any {
	return map[string]any{
		"ref": c.Ref(), "kind": "skill-change", "status": c.Status,
		"skillRef": c.SkillRef(), "library": c.LibraryKey, "skill": c.SkillName,
		"baseTreeHash": c.BaseTreeHash, "baseSourceHead": c.BaseSourceHead,
		"resultTreeHash": c.ResultTreeHash,
		"lesson":         c.Lesson, "rationale": c.Rationale, "evidenceRefs": c.EvidenceRefs,
		"hasReplacement": c.ReplacementBlobHash != "",
		"createdAt":      c.CreatedAt, "updatedAt": c.UpdatedAt,
	}
}

const maxToolStringBytes = store.ArtifactContentMax - 4096

func validateText(name, value string, required bool) error {
	return validateSizedText(name, value, required, maxToolStringBytes)
}

func validateSizedText(name, value string, required bool, maxBytes int) error {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s is too large", name)
	}
	return nil
}

func validateStringList(name string, values []string) error {
	if len(values) > 100 {
		return fmt.Errorf("%s has more than 100 items", name)
	}
	for _, value := range values {
		if err := validateText(name+" item", value, true); err != nil {
			return err
		}
	}
	return nil
}

func validateEvidenceRefs(refs []string) error {
	if err := validateStringList("evidenceRefs", refs); err != nil {
		return err
	}
	for _, value := range refs {
		if _, err := store.ParseArtifactRef(value); err == nil {
			continue
		}
		if _, err := store.ParseSkillRef(value); err == nil {
			continue
		}
		if _, err := store.ParseSkillFileRef(value); err == nil {
			continue
		}
		if _, err := store.ParseSkillChangeRef(value); err == nil {
			continue
		}
		if _, err := store.ParseRef(value); err != nil {
			return fmt.Errorf("invalid evidence ref %q", value)
		}
	}
	return nil
}

func resolveScope(scope, repo, taskKey string) (store.ScopeKind, string, string, error) {
	scope, repo, taskKey = strings.TrimSpace(scope), strings.TrimSpace(repo), strings.TrimSpace(taskKey)
	if scope == "" {
		switch {
		case taskKey != "":
			scope = string(store.ScopeTask)
		case repo != "":
			scope = string(store.ScopeRepo)
		default:
			scope = string(store.ScopeUser)
		}
	}
	switch store.ScopeKind(scope) {
	case store.ScopeUser:
		return store.ScopeUser, "local", "", nil
	case store.ScopeRepo:
		if repo == "" {
			return "", "", "", fmt.Errorf("repo scope requires repo")
		}
		return store.ScopeRepo, repo, repo, nil
	case store.ScopeTask:
		if taskKey == "" {
			return "", "", "", fmt.Errorf("task scope requires taskKey")
		}
		return store.ScopeTask, taskKey, repo, nil
	default:
		return "", "", "", fmt.Errorf("scope must be user, repo, or task")
	}
}

func toolError(message string) map[string]any {
	return jsonResult(map[string]string{"error": message})
}

// jsonResult marshals v into the single-text-block shape MCP clients expect.
//
// The payload is a JSON string inside a text block, NOT structured output. That
// is the shape Observatory's server returns, and changing it would break every
// agent already reading one.
func jsonResult(v any) map[string]any {
	body, err := json.Marshal(v)
	if err != nil {
		return textResult(`{"error":"encode failed"}`)
	}
	return textResult(string(body))
}

func textResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

// logf writes diagnostics to stderr. Never stdout — that is the protocol stream.
func (s *Server) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, "[shale mcp] "+format+"\n", args...)
}
