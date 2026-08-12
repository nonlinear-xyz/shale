package sessions

import (
	"encoding/json"
	"strings"
)

// SegmentKind labels what a piece of transcript text is. It drives both search
// filtering and the packet's corrections section, which is built from errors.
type SegmentKind string

const (
	SegUser      SegmentKind = "user"
	SegAssistant SegmentKind = "assistant"
	SegToolUse   SegmentKind = "tool_use"
	SegToolOut   SegmentKind = "tool_output"
	SegToolError SegmentKind = "tool_error"
)

// Segment is one piece of readable text pulled out of a transcript, tagged with
// the line it came from.
//
// LineNo is 1-based and refers to the SCRUBBED transcript stored in the blob, so
// a search hit can point back at exact bytes on disk. That is what makes a
// citation checkable rather than a claim.
type Segment struct {
	LineNo int
	Kind   SegmentKind
	Text   string
}

// Segments extracts readable text from a transcript.
//
// It deliberately does NOT return the raw JSONL. Indexing raw records would fill
// the search index with JSON syntax — `"type":"tool_use"`, `"role":"assistant"`,
// field names repeated on every line — and those tokens appear in every session,
// so they carry no signal while inflating the index and polluting bm25 scoring.
// What a person or an agent searches for is the prose and the commands, so that
// is what gets indexed.
func Segments(src Source, lines []string) []Segment {
	switch src {
	case SourceClaudeCode:
		return segmentsClaude(lines)
	case SourceCodex:
		return segmentsCodex(lines)
	}
	return nil
}

func segmentsClaude(lines []string) []Segment {
	var out []Segment
	for i, line := range lines {
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
		lineNo := i + 1

		// Content is either a bare string or an array of typed blocks.
		var str string
		if json.Unmarshal(msg.Content, &str) == nil {
			if t := strings.TrimSpace(str); t != "" && !strings.HasPrefix(t, "<") {
				out = append(out, Segment{lineNo, kindForRole(rec.Type), t})
			}
			continue
		}

		var blocks []struct {
			Type    string          `json:"type"`
			Text    string          `json:"text"`
			Name    string          `json:"name"`
			Input   json.RawMessage `json:"input"`
			IsError bool            `json:"is_error"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			continue
		}

		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					out = append(out, Segment{lineNo, kindForRole(rec.Type), t})
				}
			case "tool_use":
				// The tool's INPUT is the interesting part — the command that ran, the
				// file that was read, the pattern that was searched. That is what
				// someone looks for when asking "did we ever try X".
				if t := toolInputText(b.Name, b.Input); t != "" {
					out = append(out, Segment{lineNo, SegToolUse, t})
				}
			case "tool_result":
				kind := SegToolOut
				if b.IsError {
					kind = SegToolError
				}
				if t := blockText(b.Content); t != "" {
					out = append(out, Segment{lineNo, kind, t})
				}
			}
		}
	}
	return out
}

func segmentsCodex(lines []string) []Segment {
	var out []Segment
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var env codexEnvelope
		if json.Unmarshal([]byte(line), &env) != nil || env.Type != "response_item" {
			continue
		}
		var p struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			continue
		}
		lineNo := i + 1

		switch p.Type {
		case "message":
			text := allText(p.Content)
			if text == "" {
				continue
			}
			if p.Role == "user" {
				if IsCodexEnvelope(text) {
					continue
				}
				out = append(out, Segment{lineNo, SegUser, text})
			} else {
				out = append(out, Segment{lineNo, SegAssistant, text})
			}

		case "function_call", "custom_tool_call":
			if t := strings.TrimSpace(p.Name + " " + p.Arguments); strings.TrimSpace(t) != "" {
				out = append(out, Segment{lineNo, SegToolUse, t})
			}

		case "function_call_output", "custom_tool_call_output":
			body, exit := codexOutput(p.Output)
			if body == "" {
				continue
			}
			kind := SegToolOut
			if exit != 0 {
				kind = SegToolError
			}
			out = append(out, Segment{lineNo, kind, body})
		}
	}
	return out
}

func kindForRole(recType string) SegmentKind {
	if recType == "assistant" {
		return SegAssistant
	}
	return SegUser
}

// toolInputText renders a tool call as searchable text: the tool name plus the
// string-valued fields of its input. Nested objects are skipped — they are
// structure, not prose, and flattening them reintroduces the JSON noise this
// package exists to avoid.
func toolInputText(name string, input json.RawMessage) string {
	parts := []string{name}
	if len(input) > 0 {
		var fields map[string]any
		if json.Unmarshal(input, &fields) == nil {
			for _, k := range sortedFieldNames(fields) {
				if s, ok := fields[k].(string); ok && strings.TrimSpace(s) != "" {
					parts = append(parts, s)
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// sortedFieldNames keeps tool-input rendering deterministic. Map iteration order
// is randomized in Go, and without this the same tool call would produce
// different chunk text — and therefore a different content hash — on every run.
func sortedFieldNames(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// blockText renders a tool_result body, which is either a string or blocks.
func blockText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return allText(raw)
}

// allText concatenates every text block, unlike firstText which stops at the
// first. Chunking wants the whole message; titling wants only the opening.
func allText(raw json.RawMessage) string {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if t := strings.TrimSpace(b.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// codexOutput unwraps Codex's doubly-encoded tool output and returns the body
// plus its exit code.
func codexOutput(raw json.RawMessage) (body string, exitCode int) {
	if len(raw) == 0 {
		return "", 0
	}
	target := raw
	var inner string
	if json.Unmarshal(raw, &inner) == nil {
		target = json.RawMessage(inner)
	}
	var out struct {
		Output   string `json:"output"`
		Metadata *struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal(target, &out) != nil {
		// Not the documented envelope — treat the raw string as the body rather
		// than dropping output we could have indexed.
		return strings.TrimSpace(string(target)), 0
	}
	if out.Metadata != nil && out.Metadata.ExitCode != nil {
		exitCode = *out.Metadata.ExitCode
	}
	return strings.TrimSpace(out.Output), exitCode
}
