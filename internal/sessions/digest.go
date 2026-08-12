package sessions

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ContentMax bounds a digest. 64 KB is large enough to hold a long session's
// signal and small enough that a corpus of them stays searchable and cheap to
// store.
const ContentMax = 65536

const (
	maxToolErrors    = 20
	toolErrorMaxLen  = 500
	truncationMarker = "\n…[truncated]"
)

// Digest is the searchable extract of a transcript: what the user asked, what
// went wrong, and how it ended.
//
// This is what the local index stores and what a digest-tier upload sends. The
// full transcript stays on disk (or, at full tier, ships separately) — the digest
// exists so search works without reading every session end to end.
type Digest struct {
	Text             string
	FirstUserMessage string
	UserMessages     int
	ToolErrors       int
}

// BuildDigest extracts the digest for a session. It returns ok=false when the
// transcript contains no user messages at all — a session nobody spoke in has
// nothing worth indexing, and storing it would dilute every search.
func BuildDigest(s Session, lines []string) (Digest, bool) {
	var userMessages, toolErrors []string
	var lastAssistant string

	switch s.Source {
	case SourceClaudeCode:
		userMessages, toolErrors, lastAssistant = digestClaude(lines)
	case SourceCodex:
		userMessages, toolErrors, lastAssistant = digestCodex(lines)
	}

	if len(userMessages) == 0 {
		return Digest{}, false
	}

	var parts []string
	parts = append(parts,
		fmt.Sprintf("## User messages (%d)", len(userMessages)),
		strings.Join(userMessages, "\n\n---\n\n"),
	)
	if len(toolErrors) > 0 {
		capped := toolErrors
		if len(capped) > maxToolErrors {
			capped = capped[:maxToolErrors]
		}
		parts = append(parts,
			fmt.Sprintf("\n## Tool errors (%d)", len(toolErrors)),
			strings.Join(capped, "\n---\n"),
		)
	}
	if lastAssistant != "" {
		parts = append(parts, "\n## Final assistant message", lastAssistant)
	}

	return Digest{
		Text:             Truncate(strings.Join(parts, "\n\n")),
		FirstUserMessage: userMessages[0],
		UserMessages:     len(userMessages),
		ToolErrors:       len(toolErrors),
	}, true
}

// Truncate bounds text to ContentMax, marking that it was cut. A silently
// truncated digest reads as a complete one, which is the failure this avoids.
func Truncate(text string) string {
	if len(text) <= ContentMax {
		return text
	}
	return text[:ContentMax-len(truncationMarker)] + truncationMarker
}

func digestClaude(lines []string) (users, errs []string, lastAssistant string) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec claudeRecord
		if json.Unmarshal([]byte(line), &rec) != nil || len(rec.Message) == 0 {
			continue
		}
		var msg claudeMessage
		if json.Unmarshal(rec.Message, &msg) != nil {
			continue
		}

		switch rec.Type {
		case "user":
			if msg.Role != "user" {
				continue
			}
			// Tool results ride user-role turns. Errors among them are the most
			// valuable signal in a transcript — they are what the next session
			// should not repeat.
			for _, e := range toolErrorsFrom(msg.Content) {
				errs = append(errs, e)
			}
			// isMeta marks harness-injected turns; a leading "<" marks an envelope.
			// Neither is something the user said.
			if rec.IsMeta {
				continue
			}
			if text := firstText(msg.Content); text != "" && !strings.HasPrefix(text, "<") {
				users = append(users, text)
			}
		case "assistant":
			if text := firstText(msg.Content); text != "" {
				lastAssistant = text
			}
		}
	}
	return users, errs, lastAssistant
}

func digestCodex(lines []string) (users, errs []string, lastAssistant string) {
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal([]byte(line), &env) != nil || env.Type != "response_item" {
			continue
		}
		var p struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Output  json.RawMessage `json:"output"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}

		switch p.Type {
		case "message":
			text := firstText(p.Content)
			if text == "" {
				continue
			}
			switch p.Role {
			case "user":
				if !IsCodexEnvelope(text) {
					users = append(users, text)
				}
			case "assistant":
				lastAssistant = text
			}

		case "function_call_output", "custom_tool_call_output":
			if e := codexToolError(p.Output); e != "" {
				errs = append(errs, e)
			}
		}
	}
	return users, errs, lastAssistant
}

// codexToolError pulls a failure out of a tool result. Codex commonly nests a
// JSON *string* here: {"output": "...", "metadata": {"exit_code": N}}. A non-zero
// exit code is the error signal; output that isn't in that shape yields nothing
// rather than a guess.
func codexToolError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// The value is usually a JSON-encoded string containing more JSON.
	var inner string
	body := raw
	if json.Unmarshal(raw, &inner) == nil {
		body = json.RawMessage(inner)
	}
	var out struct {
		Output   string `json:"output"`
		Metadata *struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &out) != nil {
		return ""
	}
	if out.Metadata == nil || out.Metadata.ExitCode == nil || *out.Metadata.ExitCode == 0 {
		return ""
	}
	return clip(out.Output)
}

// toolErrorsFrom extracts is_error tool_result blocks from a Claude content array.
func toolErrorsFrom(content json.RawMessage) []string {
	if len(content) == 0 {
		return nil
	}
	var blocks []struct {
		Type    string          `json:"type"`
		IsError bool            `json:"is_error"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var out []string
	for _, b := range blocks {
		if b.Type != "tool_result" || !b.IsError {
			continue
		}
		var s string
		if json.Unmarshal(b.Content, &s) == nil {
			out = append(out, clip(s))
			continue
		}
		if text := firstText(b.Content); text != "" {
			out = append(out, clip(text))
		}
	}
	return out
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > toolErrorMaxLen {
		return s[:toolErrorMaxLen]
	}
	return s
}
