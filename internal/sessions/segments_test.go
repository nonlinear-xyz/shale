package sessions

import (
	"strings"
	"testing"
)

// A minimal but realistic Claude Code transcript. Line numbers matter: a segment
// carries the line it came from, which is what lets a citation point at bytes in
// the stored blob rather than at a claim.
var claudeLines = []string{
	`{"type":"user","cwd":"/src/app","gitBranch":"main","timestamp":"2026-08-01T10:00:00Z","message":{"role":"user","content":"add goreleaser signing for darwin arm64"}}`,
	`{"type":"assistant","timestamp":"2026-08-01T10:00:05Z","message":{"role":"assistant","model":"claude-opus-5","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":5},"content":[{"type":"text","text":"I will edit the release workflow."},{"type":"tool_use","name":"Bash","input":{"command":"goreleaser release --snapshot","description":"dry run"}}]}}`,
	`{"type":"user","timestamp":"2026-08-01T10:00:09Z","message":{"role":"user","content":[{"type":"tool_result","is_error":true,"content":"exit 1: signing identity not found"}]}}`,
	`{"type":"assistant","timestamp":"2026-08-01T10:00:20Z","message":{"role":"assistant","model":"claude-opus-5","usage":{"input_tokens":200,"output_tokens":80},"content":[{"type":"text","text":"You need APPLE_DEVELOPER_ID set."}]}}`,
	`{"type":"user","isMeta":true,"timestamp":"2026-08-01T10:00:21Z","message":{"role":"user","content":"<system-reminder>ignore me</system-reminder>"}}`,
}

func TestSegmentsClaudeExtractsEachKind(t *testing.T) {
	segs := Segments(SourceClaudeCode, claudeLines)

	var kinds []SegmentKind
	for _, s := range segs {
		kinds = append(kinds, s.Kind)
	}
	for _, want := range []SegmentKind{SegUser, SegAssistant, SegToolUse, SegToolError} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no %s segment extracted; got %v", want, kinds)
		}
	}

	// The tool INPUT is the searchable part — the command that ran is what someone
	// looks for when asking "did we ever try this".
	var toolText string
	for _, s := range segs {
		if s.Kind == SegToolUse {
			toolText = s.Text
		}
	}
	if !strings.Contains(toolText, "goreleaser release --snapshot") {
		t.Errorf("tool_use segment lost the command: %q", toolText)
	}

	// Line numbers are 1-based and must point at the record they came from.
	if segs[0].LineNo != 1 {
		t.Errorf("first segment LineNo = %d, want 1", segs[0].LineNo)
	}

	// Harness-injected turns are not the user speaking.
	for _, s := range segs {
		if strings.Contains(s.Text, "ignore me") {
			t.Error("isMeta record was indexed as user content")
		}
	}
}

// Raw JSONL must never reach the index: field names appear on every line of every
// session, so they carry no signal while inflating the index and flattening bm25.
func TestSegmentsNeverEmitRawJSON(t *testing.T) {
	for _, s := range Segments(SourceClaudeCode, claudeLines) {
		for _, noise := range []string{`"type":`, `"role":`, `"usage":`, `cache_read_input_tokens`} {
			if strings.Contains(s.Text, noise) {
				t.Errorf("segment leaked JSON syntax %q: %s", noise, s.Text)
			}
		}
	}
}

var codexLines = []string{
	`{"type":"session_meta","timestamp":"2026-08-02T09:00:00Z","payload":{"id":"sess-1","cwd":"/src/app","git":{"branch":"main"}}}`,
	`{"type":"response_item","timestamp":"2026-08-02T09:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"text","text":"<user_instructions>be terse</user_instructions>"}]}}`,
	`{"type":"response_item","timestamp":"2026-08-02T09:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"text","text":"wire up the notarization step"}]}}`,
	`{"type":"response_item","timestamp":"2026-08-02T09:00:04Z","payload":{"type":"function_call_output","output":"{\"output\":\"xcrun: error: unable to notarize\",\"metadata\":{\"exit_code\":1}}"}}`,
	`{"type":"response_item","timestamp":"2026-08-02T09:00:09Z","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"Store credentials with notarytool."}]}}`,
}

// Codex prepends instruction and environment blocks as user messages. Taking one
// as the user's ask makes every Codex session in the corpus look identical.
func TestSegmentsCodexFiltersEnvelopesAndDetectsErrors(t *testing.T) {
	segs := Segments(SourceCodex, codexLines)

	for _, s := range segs {
		if strings.Contains(s.Text, "be terse") {
			t.Error("harness envelope was indexed as a user message")
		}
	}

	var sawAsk, sawErr bool
	for _, s := range segs {
		if s.Kind == SegUser && strings.Contains(s.Text, "notarization") {
			sawAsk = true
		}
		if s.Kind == SegToolError && strings.Contains(s.Text, "unable to notarize") {
			sawErr = true
		}
	}
	if !sawAsk {
		t.Error("real user ask was not extracted")
	}
	if !sawErr {
		t.Error("non-zero exit code was not classified as a tool error")
	}
}

func TestChunksCarryLineRangesAndMarkErrors(t *testing.T) {
	chunks := Chunks(Segments(SourceClaudeCode, claudeLines))
	if len(chunks) == 0 {
		t.Fatal("no chunks produced")
	}

	var sawError bool
	for i, c := range chunks {
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
		if c.LineStart < 1 || c.LineEnd < c.LineStart {
			t.Errorf("chunk %d has an invalid line range %d-%d", i, c.LineStart, c.LineEnd)
		}
		if c.LineEnd > len(claudeLines) {
			t.Errorf("chunk %d ends past the transcript: line %d of %d", i, c.LineEnd, len(claudeLines))
		}
		if c.Kind == ChunkError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("no chunk was marked as an error; the corrections section would be empty")
	}
}

// One oversized tool result must not dominate the index or swamp bm25 scoring for
// every other chunk in the session.
func TestOversizedSegmentIsClipped(t *testing.T) {
	huge := strings.Repeat("x", SegmentMaxChars*3)
	chunks := Chunks([]Segment{{LineNo: 1, Kind: SegToolOut, Text: huge}})
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if len(chunks[0].Text) > SegmentMaxChars+200 {
		t.Errorf("segment not clipped: %d chars", len(chunks[0].Text))
	}
	if !strings.Contains(chunks[0].Text, "truncated") {
		t.Error("clipped segment does not say it was truncated")
	}
}

// Chunking accumulates whole segments rather than cutting at a fixed character
// count: half a thought scores poorly and reads worse.
func TestChunksDoNotSplitSegments(t *testing.T) {
	segs := []Segment{
		{LineNo: 1, Kind: SegUser, Text: strings.Repeat("alpha ", 200)},
		{LineNo: 2, Kind: SegUser, Text: strings.Repeat("beta ", 200)},
		{LineNo: 3, Kind: SegUser, Text: strings.Repeat("gamma ", 200)},
	}
	for _, c := range Chunks(segs) {
		for _, word := range []string{"alpha", "beta", "gamma"} {
			if n := strings.Count(c.Text, word); n > 0 && n < 200 {
				t.Errorf("segment %q was split across chunks (%d occurrences)", word, n)
			}
		}
	}
}

// Tool-input rendering must be deterministic. Go randomizes map iteration, and
// without sorting the same tool call would produce different chunk text — and so
// a different content hash — on every run.
func TestToolInputRenderingIsDeterministic(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/a/b.go","old_string":"foo","new_string":"bar","zzz":"last"}}]}}`
	first := Segments(SourceClaudeCode, []string{line})[0].Text
	for i := 0; i < 30; i++ {
		if got := Segments(SourceClaudeCode, []string{line})[0].Text; got != first {
			t.Fatalf("tool input rendering is not stable:\n%q\n%q", first, got)
		}
	}
}
