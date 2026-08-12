// Package mcp serves the local corpus to agents over the Model Context Protocol.
//
// Transport is stdio JSON-RPC 2.0: the agent launches this process and speaks
// line-delimited JSON over stdin/stdout. That means STDOUT IS THE PROTOCOL —
// anything else written there corrupts the stream and the client disconnects with
// a parse error that looks like a crash. Every diagnostic in this package goes to
// stderr, deliberately.
//
// The protocol is implemented by hand rather than pulled from a dependency. It is
// three methods and a response envelope; owning it costs less than tracking
// someone else's release cycle, and keeps the module dependency-free apart from
// the SQLite driver.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/nonlinear-xyz/shale/internal/buildinfo"
	"github.com/nonlinear-xyz/shale/internal/pack"
	"github.com/nonlinear-xyz/shale/internal/store"
)

// protocolVersion is the MCP revision this server implements.
const protocolVersion = "2024-11-05"

// Server answers MCP requests from the local store. It is read-only: there is no
// tool here that mutates anything, which is what makes it safe to hand to an
// agent without a confirmation step.
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
	dec := json.NewDecoder(bufio.NewReaderSize(in, 1<<20))
	enc := json.NewEncoder(out)

	for {
		if err := ctx.Err(); err != nil {
			return nil // cancelled is a clean shutdown, not a failure
		}

		var req request
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil // client closed the pipe — normal exit
			}
			// A malformed frame is unrecoverable on a stream: we cannot know where
			// the next one starts. Report and stop rather than emitting garbage.
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
			"description": "Call this at the START of any nontrivial task. Returns a bounded packet of " +
				"prior work from this machine's captured agent sessions: passages relevant to the task, " +
				"and things that went wrong when similar work was attempted before. Every item carries " +
				"provenance (source, repo, line range, freshness) so claims can be checked. The packet " +
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
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool: " + call.Name}
	}
}

func (s *Server) contextForTask(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var args struct {
		Task string `json:"task"`
		Repo string `json:"repo"`
		// Note the boundary rename: the wire calls this maxTokens, the packet
		// builder calls it TokenBudget. Preserved from Observatory's contract so an
		// agent wired to a hub can be pointed here unchanged.
		MaxTokens int `json:"maxTokens"`
		SinceDays int `json:"sinceDays"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid arguments: " + err.Error()}
	}
	if strings.TrimSpace(args.Task) == "" {
		return textResult(`{"error":"task is required"}`), nil
	}

	p, err := pack.Build(ctx, s.DB, pack.Input{
		Task:        args.Task,
		Repo:        args.Repo,
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
	if strings.TrimSpace(args.Query) == "" {
		return textResult(`{"error":"query is required"}`), nil
	}
	if args.Limit <= 0 {
		args.Limit = 10
	}

	hits, err := s.DB.SearchChunks(ctx, args.Query, args.Repo, args.Kind, args.SinceDays, args.Limit)
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
