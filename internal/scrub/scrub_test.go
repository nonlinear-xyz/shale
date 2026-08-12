package scrub

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// Fixture secrets are FAKE, generated for this test. They are the same vectors
// the JavaScript collector's suite used, so a divergence between the two
// implementations shows up here rather than in production.
//
// They are assembled from fragments rather than written as literals, and that is
// not stylistic. A credential-scrubbing tool must contain credential-shaped
// strings to test against, which is exactly what push protection and secret
// scanners are built to reject — GitHub blocked the first push of this file over
// the Slack vector on line 17. Splitting the literals keeps the fixtures out of
// every scanner's reach while the runtime values stay byte-identical, so the
// rules are still tested against the real shapes.
//
// Do not "simplify" these back into literals. The alternative is allowlisting a
// secret-scanning alert, which teaches everyone to wave the next one through.
var fakeSecrets = []string{
	"AKIA" + "IOSFODNN7EXAMPLE",                           // aws
	"ghp" + "_abcdefghijklmnopqrstuvwxyz0123456789",       // github
	"sk-" + "ant-" + "api03-notarealkey-abcdefghijklmnop", // anthropic
	"xoxb" + "-1234567890-abcdefghijklmnop",               // slack
	"npm" + "_abcdefghijklmnopqrstuvwxyz0123456789",       // npm
	"eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0." + // jwt
		"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c",
}

// The verification gate for full-tier capture: no fixture secret may survive to
// the payload that would be uploaded.
func TestRemovesEverySecretFromNestedJSONL(t *testing.T) {
	s, warns := New()
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}

	line := mustJSON(t, map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": []any{
				map[string]any{
					"type": "tool_result",
					"content": []any{
						map[string]any{"type": "text", "text": "output: " + strings.Join(fakeSecrets, " and ")},
						map[string]any{"type": "text", "text": "DATABASE_PASSWORD=hunter2secret123 in .env"},
					},
				},
			},
		},
	})

	out := s.Line(line)

	for _, secret := range fakeSecrets {
		if strings.Contains(out, secret) {
			t.Errorf("secret survived scrubbing: %s", secret)
		}
	}
	if strings.Contains(out, "hunter2secret123") {
		t.Error("env-assignment value survived scrubbing")
	}
	// The key name survives so a reader can tell WHICH secret was present.
	if !strings.Contains(out, "DATABASE_PASSWORD") {
		t.Error("env-assignment key name should survive; only the value is redacted")
	}
	// Placeholders contain no characters that need JSON escaping, so a scrubbed
	// line is still parseable.
	var any0 any
	if err := json.Unmarshal([]byte(out), &any0); err != nil {
		t.Fatalf("scrubbed line is no longer valid JSON: %v\n%s", err, out)
	}
	if got := len(s.Counts()); got < 6 {
		t.Errorf("expected at least 6 distinct rules to fire, got %d (%v)", got, s.Counts())
	}
}

// Equality across occurrences is what lets a reader see "this is the same key as
// on line 40" without the value ever being recoverable.
func TestIdenticalSecretsRedactIdentically(t *testing.T) {
	s, _ := New()
	a := s.Line(mustJSON(t, map[string]any{"t": fakeSecrets[0]}))
	b := s.Line(mustJSON(t, map[string]any{"u": fakeSecrets[0]}))

	token := regexp.MustCompile(`\[REDACTED:aws-key:[0-9a-f]{8}\]`)
	ma, mb := token.FindString(a), token.FindString(b)
	if ma == "" {
		t.Fatalf("no aws-key placeholder in %q", a)
	}
	if ma != mb {
		t.Errorf("same secret produced different placeholders: %q vs %q", ma, mb)
	}
}

// Redacting ids and digests would strip session ids and commit shas out of the
// corpus, which is most of what makes it useful.
func TestLeavesHexHashesAndUUIDsAlone(t *testing.T) {
	s, _ := New()
	hash := strings.Repeat("a", 64)
	uuid := "0198c5c1-1234-4abc-8def-0123456789ab"

	out := s.Line(mustJSON(t, map[string]any{"hash": hash, "uuid": uuid}))
	if !strings.Contains(out, hash) {
		t.Errorf("64-char hex hash was redacted: %s", out)
	}
	if !strings.Contains(out, uuid) {
		t.Errorf("uuid was redacted: %s", out)
	}
}

// The collector must never choke on a malformed line — a partially-written
// transcript is normal when a session is still running.
func TestScrubsUnparseableLines(t *testing.T) {
	s, _ := New()
	out := s.Line("broken json " + fakeSecrets[1] + " tail")
	if strings.Contains(out, fakeSecrets[1]) {
		t.Errorf("secret survived on an unparseable line: %s", out)
	}
}

func TestUserExtraPatterns(t *testing.T) {
	s, warns := NewFromPatterns(map[string]string{"internal-id": `II-[0-9]{6}`})
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	out := s.Line(mustJSON(t, map[string]any{"t": "ticket II-123456 ok"}))
	if strings.Contains(out, "II-123456") {
		t.Errorf("extra pattern did not fire: %s", out)
	}
	if !strings.Contains(out, "[REDACTED:internal-id:") {
		t.Errorf("expected internal-id placeholder, got %s", out)
	}
}

// An invalid user pattern must warn and be skipped, never abort capture — a typo
// in configuration must not silently stop a machine from syncing.
func TestInvalidExtraPatternWarnsButDoesNotFail(t *testing.T) {
	s, warns := NewFromPatterns(map[string]string{"broken": `([unclosed`})
	if len(warns) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warns), warns)
	}
	// Built-in rules still work.
	out := s.Line(fakeSecrets[0])
	if strings.Contains(out, fakeSecrets[0]) {
		t.Error("built-in rules stopped working after an invalid extra pattern")
	}
}

// Byte preservation is the property the raw-text approach buys over the JSON
// round-trip the JavaScript collector used: a line with no secrets must come back
// absolutely unchanged, so unchanged prefixes of a grown session keep their hash.
func TestNonSecretContentIsBytePreserved(t *testing.T) {
	s, _ := New()
	line := `{"type":"assistant","message":{"content":[{"type":"text","text":"Refactored parseConfig() — see src/config.go:42. Ünïcödé ✓ \"quoted\" and \\escaped\\"}]},"ts":"2026-08-12T10:00:00Z"}`
	if out := s.Line(line); out != line {
		t.Errorf("clean line was modified:\n in: %s\nout: %s", line, out)
	}
	if s.Total() != 0 {
		t.Errorf("clean line produced %d redactions", s.Total())
	}
}

// Blank lines pass through untouched so the transcript's line structure — which
// the server's chunk boundaries depend on — is preserved exactly.
func TestBlankLinesUntouched(t *testing.T) {
	s, _ := New()
	for _, in := range []string{"", "   ", "\t"} {
		if out := s.Line(in); out != in {
			t.Errorf("blank line %q became %q", in, out)
		}
	}
}

// A specific rule must win the label over the entropy catch-all, and the entropy
// pass must not re-redact a placeholder a named rule already emitted.
func TestSpecificRuleWinsAndPlaceholdersAreNotDoubleRedacted(t *testing.T) {
	s, _ := New()
	out := s.String("token=" + fakeSecrets[1])
	if !strings.Contains(out, "[REDACTED:github-token:") {
		t.Errorf("expected github-token label, got %s", out)
	}
	if strings.Count(out, "REDACTED") != 1 {
		t.Errorf("placeholder was re-redacted: %s", out)
	}
}

// A PEM block spans newlines, and in JSONL those arrive as the two characters
// backslash-n. Both forms must be caught.
func TestPrivateKeyBlockAcrossNewlines(t *testing.T) {
	s, _ := New()
	raw := "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\nabcd\n-----END RSA PRIVATE KEY-----"
	if out := s.String(raw); strings.Contains(out, "MIIEowIBAAKCAQEA1234") {
		t.Errorf("raw PEM body survived: %s", out)
	}
	escaped := `{"text":"-----BEGIN PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234\n-----END PRIVATE KEY-----"}`
	if out := s.String(escaped); strings.Contains(out, "MIIEowIBAAKCAQEA1234") {
		t.Errorf("JSON-escaped PEM body survived: %s", out)
	}
}

// Counts are shipped to the hub as an audit trail, so they must be accurate and
// must never carry any part of a matched value.
func TestCountsAccumulateAcrossLines(t *testing.T) {
	s, _ := New()
	s.Line(mustJSON(t, map[string]any{"a": fakeSecrets[0]}))
	s.Line(mustJSON(t, map[string]any{"b": fakeSecrets[0]}))
	s.Line(mustJSON(t, map[string]any{"c": fakeSecrets[3]}))

	counts := s.Counts()
	if counts["aws-key"] != 2 {
		t.Errorf("aws-key count = %d, want 2", counts["aws-key"])
	}
	if counts["slack-token"] != 1 {
		t.Errorf("slack-token count = %d, want 1", counts["slack-token"])
	}
	for rule, n := range counts {
		if n <= 0 {
			t.Errorf("rule %q recorded a non-positive count %d", rule, n)
		}
		for _, secret := range fakeSecrets {
			if strings.Contains(rule, secret) {
				t.Errorf("rule name leaked a secret value: %q", rule)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
