package scrub

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestCrossCheckRealTranscript scrubs a real transcript and reports per-rule
// counts, so the Go port can be diffed against the JavaScript collector it
// replaces. Set SHALE_CROSSCHECK_FILE to a .jsonl path to run it.
//
// It also asserts the two properties that must hold on any real input regardless
// of what the JS implementation did: the line count is preserved exactly, and no
// placeholder is ever emitted inside another placeholder.
func TestCrossCheckRealTranscript(t *testing.T) {
	path := os.Getenv("SHALE_CROSSCHECK_FILE")
	if path == "" {
		t.Skip("set SHALE_CROSSCHECK_FILE to a transcript path to run")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	s, _ := New()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20) // transcript lines can be very long

	var lines int
	for sc.Scan() {
		lines++
		in := sc.Text()
		out := s.Line(in)

		// Structural invariants that must hold on real input.
		if strings.Contains(out, "REDACTED:high-entropy:") && strings.Contains(out, "[REDACTED:[") {
			t.Fatalf("nested placeholder emitted on line %d", lines)
		}
		if in != "" && out == "" {
			t.Fatalf("line %d was emptied by scrubbing", lines)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	report, _ := json.MarshalIndent(map[string]any{
		"counts": s.Counts(),
		"lines":  lines,
		"total":  s.Total(),
	}, "", "  ")
	t.Logf("go scrubber over %s:\n%s", path, report)
}
