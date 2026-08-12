// Package sessions reads agent transcripts off disk.
//
// Each harness writes JSONL somewhere under the home directory in its own shape.
// This package is the only place that knows those shapes; everything downstream
// works with the normalized Session type.
//
// Adding a harness means adding a reader here and nothing else. That is the whole
// bet of being harness-agnostic: the formats are undocumented and change without
// notice, so the cost of neutrality is paid in this one file rather than smeared
// across the codebase.
package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Source identifies which harness produced a transcript.
type Source string

const (
	SourceClaudeCode Source = "claude_code"
	SourceCodex      Source = "codex"
)

// AllSources is the full set, used by sweeps that were not narrowed with a flag.
// A per-source cursor means a narrowed run must never advance the others.
var AllSources = []Source{SourceClaudeCode, SourceCodex}

// Session is one transcript, normalized across harnesses.
type Session struct {
	Source Source
	// SourceKey is the harness's own session id — the dedupe key. For Claude it is
	// the file's UUID basename; for Codex it is session_meta.id, falling back to
	// the basename when a transcript has no meta record.
	SourceKey string
	Path      string
	ModTime   time.Time
	SizeBytes int64

	CWD       string
	Repo      string // normalized owner/name, resolved from the cwd's git remote
	Branch    string
	Project   string
	StartedAt time.Time
	EndedAt   time.Time

	// FirstUserMessage is the session title. Harness-injected envelopes are
	// excluded — they are instructions to the agent, never the user's ask.
	FirstUserMessage string

	Turns        int
	UsageByModel map[string]Usage
}

// Usage is per-model token accounting. Clients report raw counts and never price
// them: rates change, and a binary on someone's laptop would carry a stale table
// forever.
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

// Title renders the session's display name, bounded for storage.
func (s Session) Title() string {
	msg := strings.TrimSpace(s.FirstUserMessage)
	if msg == "" {
		msg = "(no user message)"
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	t := msg
	if s.Project != "" {
		t = s.Project + ": " + msg
	}
	if len(t) > 500 {
		t = t[:500]
	}
	return t
}

// ── discovery ────────────────────────────────────────────────────────────────

// ClaudeDir is where Claude Code keeps per-project session transcripts.
func ClaudeDir() string { return filepath.Join(home(), ".claude", "projects") }

// CodexDir is where Codex keeps sessions, nested by YYYY/MM/DD.
func CodexDir() string { return filepath.Join(home(), ".codex", "sessions") }

// Discover returns transcript paths for one source, newest-modified last is NOT
// guaranteed — callers that advance a watermark must sort or clamp themselves,
// because readdir returns directory order, not mtime order.
func Discover(src Source) ([]string, error) {
	switch src {
	case SourceClaudeCode:
		return listClaude(ClaudeDir())
	case SourceCodex:
		return listCodex(CodexDir())
	default:
		return nil, nil
	}
}

// listClaude walks exactly two levels: ~/.claude/projects/<project>/<uuid>.jsonl.
// It is deliberately not recursive — Claude Code does not nest deeper, and a
// recursive walk here would pick up unrelated JSONL a user happened to store.
func listClaude(root string) ([]string, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // harness not installed — not an error
		}
		return nil, err
	}
	var out []string
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // unreadable project dir — skip, don't fail the sweep
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out, nil
}

// listCodex walks the whole tree: Codex nests sessions under YYYY/MM/DD.
func listCodex(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree — skip it, keep walking
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return out, err
	}
	return out, nil
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// ── parsing ──────────────────────────────────────────────────────────────────

// Read parses a transcript into a Session plus its raw lines.
//
// The raw lines are returned because the caller scrubs and hashes them; this
// package never decides what leaves the machine.
func Read(path string, src Source) (Session, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Session{}, nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Session{}, nil, err
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")

	s := Session{
		Source:       src,
		Path:         path,
		ModTime:      info.ModTime(),
		SizeBytes:    info.Size(),
		SourceKey:    strings.TrimSuffix(filepath.Base(path), ".jsonl"),
		UsageByModel: map[string]Usage{},
	}

	switch src {
	case SourceClaudeCode:
		parseClaude(&s, lines)
	case SourceCodex:
		parseCodex(&s, lines)
	}

	if s.Project == "" {
		if s.CWD != "" {
			s.Project = filepath.Base(s.CWD)
		} else {
			s.Project = filepath.Base(filepath.Dir(path))
		}
	}
	return s, lines, nil
}

// claudeRecord is the subset of Claude Code's JSONL this reader consumes.
// Unknown fields are ignored: the harness adds them without warning, and a strict
// decoder would turn a harmless addition into a capture outage.
type claudeRecord struct {
	Type      string          `json:"type"`
	CWD       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`
	IsMeta    bool            `json:"isMeta"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *claudeUsage    `json:"usage"`
}

type claudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func parseClaude(s *Session, lines []string) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec claudeRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue // a partially-written line is normal on a live session
		}

		// First non-empty wins: the harness repeats these on every record, and the
		// earliest is the one that describes where the session actually started.
		if s.CWD == "" && rec.CWD != "" {
			s.CWD = rec.CWD
		}
		if s.Branch == "" && rec.GitBranch != "" {
			s.Branch = rec.GitBranch
		}

		// Timestamps are taken across ALL record types, not just assistant turns —
		// a session's span includes the tool calls and system records between them.
		if ts, err := time.Parse(time.RFC3339, rec.Timestamp); err == nil {
			if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
				s.StartedAt = ts
			}
			if ts.After(s.EndedAt) {
				s.EndedAt = ts
			}
		}

		if len(rec.Message) == 0 {
			continue
		}
		var msg claudeMessage
		if json.Unmarshal(rec.Message, &msg) != nil {
			continue
		}

		if rec.Type == "user" && msg.Role == "user" && !rec.IsMeta {
			if s.FirstUserMessage == "" {
				if text := firstText(msg.Content); text != "" && !strings.HasPrefix(text, "<") {
					s.FirstUserMessage = text
				}
			}
		}

		if rec.Type == "assistant" && msg.Usage != nil {
			s.Turns++
			model := msg.Model
			if model == "" {
				model = "unknown"
			}
			u := s.UsageByModel[model]
			u.Input += msg.Usage.InputTokens
			u.Output += msg.Usage.OutputTokens
			u.CacheRead += msg.Usage.CacheReadInputTokens
			u.CacheWrite += msg.Usage.CacheCreationInputTokens
			s.UsageByModel[model] = u
		}
	}
}

// codexEnvelope is Codex's outer record: {type, timestamp, payload}.
type codexEnvelope struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	// session_meta
	ID  string `json:"id"`
	CWD string `json:"cwd"`
	Git *struct {
		Branch string `json:"branch"`
	} `json:"git"`

	// response_item / turn_context
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Model   string          `json:"model"`

	// event_msg (token_count)
	Info *struct {
		TotalTokenUsage struct {
			InputTokens       int `json:"input_tokens"`
			CachedInputTokens int `json:"cached_input_tokens"`
			OutputTokens      int `json:"output_tokens"`
		} `json:"total_token_usage"`
	} `json:"info"`
}

func parseCodex(s *Session, lines []string) {
	// Codex reports CUMULATIVE totals on every token_count event, and its
	// input_tokens is inclusive of cached. Deltas are attributed to whichever
	// model turn_context last named, so a session that switches models mid-way
	// prices correctly rather than assigning everything to the final one.
	var prev struct{ input, cached, output int }
	model := "unknown"

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal([]byte(line), &env) != nil {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, env.Timestamp); err == nil {
			if s.StartedAt.IsZero() || ts.Before(s.StartedAt) {
				s.StartedAt = ts
			}
			if ts.After(s.EndedAt) {
				s.EndedAt = ts
			}
		}
		if len(env.Payload) == 0 {
			continue
		}
		var p codexPayload
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}

		switch env.Type {
		case "session_meta":
			if p.ID != "" {
				s.SourceKey = p.ID
			}
			if p.CWD != "" {
				s.CWD = p.CWD
			}
			if p.Git != nil && p.Git.Branch != "" {
				s.Branch = p.Git.Branch
			}

		case "turn_context":
			if p.Model != "" {
				model = p.Model
			}

		case "response_item":
			if p.Type == "message" && p.Role == "user" && s.FirstUserMessage == "" {
				if text := firstText(p.Content); text != "" && !IsCodexEnvelope(text) {
					s.FirstUserMessage = text
				}
			}

		case "event_msg":
			if p.Type != "token_count" || p.Info == nil {
				continue
			}
			t := p.Info.TotalTokenUsage
			// max(0, …) guards against totals resetting, which happens after a
			// compaction. Without it a reset would subtract into a negative delta
			// and corrupt the running total.
			cachedDelta := maxInt(0, t.CachedInputTokens-prev.cached)
			u := s.UsageByModel[model]
			u.Input += maxInt(0, t.InputTokens-prev.input-cachedDelta)
			u.CacheRead += cachedDelta
			u.Output += maxInt(0, t.OutputTokens-prev.output)
			s.UsageByModel[model] = u

			prev.input, prev.cached, prev.output = t.InputTokens, t.CachedInputTokens, t.OutputTokens
			s.Turns++
		}
	}
}

// IsCodexEnvelope reports whether a user-role message is really a harness
// injection. Codex prepends instruction and environment blocks as user messages —
// "<user_instructions>…", "<environment_context>…", "# AGENTS.md instructions for
// <path>…". None of them are the user's ask, and taking one as the session title
// makes every Codex session in the corpus look identical.
func IsCodexEnvelope(text string) bool {
	return strings.HasPrefix(text, "<") || strings.HasPrefix(text, "# AGENTS.md")
}

// firstText pulls readable text out of a content field that is either a bare
// string or an array of typed blocks.
func firstText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(content, &str) == nil {
		return strings.TrimSpace(str)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				return strings.TrimSpace(b.Text)
			}
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
