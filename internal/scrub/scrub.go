// Package scrub removes secrets from transcript text before anything is hashed,
// stored, or uploaded.
//
// The rule set below was derived from real agent transcripts and is the most
// expensive knowledge in this repository to re-derive. Treat additions as
// additive: removing or loosening a rule silently widens what leaves a machine.
//
// # Ordering is load-bearing
//
// Rules run specific → generic, then a high-entropy catch-all runs last. A
// specific rule that fires first both produces a more useful redaction label and
// prevents the entropy rule from claiming the match under a vaguer name.
//
// # Scrub before hash, always
//
// Callers must scrub, then hash the scrubbed bytes. A hub recomputes the digest
// over what it receives, so scrubbing has to precede hashing by construction —
// reversing them produces a hash mismatch on every upload.
//
// # Why this operates on raw text, not parsed JSON
//
// The JavaScript collector this replaces parsed each JSONL line, walked the
// object, and scrubbed only string values — then re-serialized. That is not
// portable to Go: encoding/json sorts map keys alphabetically while JavaScript
// preserves insertion order, so a Go port that round-tripped through a map would
// rewrite every line and change the content hash of every session.
//
// Operating on the raw line instead preserves the original bytes exactly except
// where a secret was found. That is strictly better: hashes are stable, unchanged
// prefixes of a grown session stay byte-identical, and there is no canonicalizing
// step to disagree about. The tradeoff the JSON walk bought — never scrubbing an
// object *key* — costs nothing here, because every rule requires either a
// distinctive prefix or 32+ characters, and JSONL field names are short
// identifiers that cannot match.
package scrub

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// EntropyThreshold is Shannon entropy in bits per character, measured over the
// candidate's own character frequencies. 4.2 separates real high-entropy secrets
// from long ordinary identifiers.
const EntropyThreshold = 4.2

// entropyCandidate is the shape of a token worth entropy-testing: base64/base64url
// alphabet, at least 32 characters.
var entropyCandidate = regexp.MustCompile(`\b[A-Za-z0-9+/_-]{32,}\b`)

// hexOnly matches ids and digests — UUIDs, sha hashes, ULIDs in hex form. These
// are identifiers, not secrets, and they are common enough in transcripts that
// redacting them would make the corpus unreadable.
var hexOnly = regexp.MustCompile(`^[0-9a-fA-F-]+$`)

// Rule is one named redaction pattern.
type Rule struct {
	Name  string
	Regex *regexp.Regexp
	// KeepPrefixGroup, when non-zero, is the submatch index preserved verbatim in
	// the replacement. Only env-assignment uses it: the key name and separator
	// survive so a reader can still see WHICH secret was present, while the value
	// is replaced.
	KeepPrefixGroup int
}

// Rules are applied in this order. Every pattern is RE2-compatible — no
// backreferences, no lookaround — which also means none of them can backtrack
// catastrophically on adversarial input.
var Rules = []Rule{
	{Name: "aws-key", Regex: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[A-Z0-9]{16}\b`)},
	{Name: "github-token", Regex: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_\w{22}_\w{59})\b`)},
	{Name: "model-api-key", Regex: regexp.MustCompile(`\bsk-(?:ant-)?[\w-]{20,}\b`)},
	{Name: "slack-token", Regex: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{Name: "npm-token", Regex: regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}\b`)},
	{Name: "vercel-blob-token", Regex: regexp.MustCompile(`\bvercel_blob_rw_[A-Za-z0-9_]{20,}\b`)},
	{Name: "jwt", Regex: regexp.MustCompile(`\beyJ[\w-]{10,}\.[\w-]{10,}\.[\w-]{10,}\b`)},
	// (?s) so the block can span the newlines a PEM body always contains — and in
	// JSONL those arrive as the two characters \ and n, which [\s\S] also covers.
	{Name: "private-key", Regex: regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)},
	{
		// KEY=value assignments where the key name smells secret — redact the value,
		// keep the name. Seeing that DATABASE_PASSWORD was present is useful; seeing
		// its value is the leak.
		Name:            "env-assignment",
		Regex:           regexp.MustCompile(`\b((?:[A-Z0-9_]*(?:SECRET|TOKEN|PASSW(?:OR)?D|API[_-]?KEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY)[A-Z0-9_]*)\s*[=:]\s*["']?)([^\s"']{8,})`),
		KeepPrefixGroup: 1,
	},
}

// Scrubber applies the rule set and accumulates per-rule hit counts across every
// line it processes. Counts are reported to the hub as an audit trail: an
// operator can see that eleven github-tokens were caught without ever seeing one.
//
// A Scrubber is not safe for concurrent use; give each worker its own.
type Scrubber struct {
	rules  []Rule
	counts map[string]int
}

// New returns a Scrubber with the built-in rules plus any extras.
//
// Extra patterns run AFTER all built-ins and before the entropy pass, matching
// the ordering contract. An extra whose regex does not compile is skipped with a
// warning rather than being fatal: a typo in user configuration must not stop
// capture, because the alternative is a machine that silently stops syncing.
func New(extra ...Rule) (*Scrubber, []error) {
	s := &Scrubber{
		rules:  append([]Rule(nil), Rules...),
		counts: map[string]int{},
	}
	var warnings []error
	for _, e := range extra {
		if e.Regex == nil {
			warnings = append(warnings, fmt.Errorf("scrub pattern %q has no regex — skipped", e.Name))
			continue
		}
		if e.Name == "" {
			e.Name = "custom"
		}
		s.rules = append(s.rules, e)
	}
	return s, warnings
}

// NewFromPatterns compiles user-supplied pattern strings, returning warnings for
// the ones that do not compile rather than failing.
func NewFromPatterns(patterns map[string]string) (*Scrubber, []error) {
	var extra []Rule
	var warnings []error
	// Sort for deterministic rule order — map iteration order would otherwise make
	// the output depend on run-to-run hash seeding.
	names := make([]string, 0, len(patterns))
	for name := range patterns {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		re, err := regexp.Compile(patterns[name])
		if err != nil {
			warnings = append(warnings, fmt.Errorf("invalid scrub pattern %q — skipped: %w", name, err))
			continue
		}
		extra = append(extra, Rule{Name: name, Regex: re})
	}
	s, more := New(extra...)
	return s, append(warnings, more...)
}

// String scrubs one string and returns the result.
func (s *Scrubber) String(in string) string {
	if in == "" {
		return in
	}
	out := in
	for _, r := range s.rules {
		out = s.applyRule(out, r)
	}
	return s.applyEntropy(out)
}

// Line scrubs a single JSONL line. Blank lines pass through untouched so the line
// structure of the transcript is preserved exactly.
func (s *Scrubber) Line(line string) string {
	if strings.TrimSpace(line) == "" {
		return line
	}
	return s.String(line)
}

// Counts returns per-rule hit counts accumulated so far. The caller ships this as
// capture metadata; it never contains any part of a matched value.
func (s *Scrubber) Counts() map[string]int {
	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// Total is the number of redactions made across every rule.
func (s *Scrubber) Total() int {
	n := 0
	for _, v := range s.counts {
		n += v
	}
	return n
}

func (s *Scrubber) applyRule(in string, r Rule) string {
	locs := r.Regex.FindAllStringSubmatchIndex(in, -1)
	if len(locs) == 0 {
		return in
	}

	var b strings.Builder
	b.Grow(len(in))
	last := 0
	for _, loc := range locs {
		start, end := loc[0], loc[1]
		whole := in[start:end]
		b.WriteString(in[last:start])

		// The placeholder hashes the WHOLE match even when a prefix is preserved.
		// That is what makes the same secret redact to the same token everywhere in
		// a transcript — a reader can tell "this is the same key as on line 40"
		// without the value ever being recoverable.
		if g := r.KeepPrefixGroup; g > 0 && 2*g+1 < len(loc) && loc[2*g] >= 0 {
			b.WriteString(in[loc[2*g]:loc[2*g+1]])
		}
		b.WriteString(placeholder(r.Name, whole))
		s.counts[r.Name]++
		last = end
	}
	b.WriteString(in[last:])
	return b.String()
}

// applyEntropy is the catch-all for credentials no named rule anticipated. It
// runs last so a specific rule always wins the label.
func (s *Scrubber) applyEntropy(in string) string {
	return replaceAllFunc(in, entropyCandidate, func(match string) string {
		// Never double-redact a placeholder a named rule already emitted — its hash
		// suffix is exactly the kind of long token this rule looks for.
		if strings.Contains(match, "REDACTED") {
			return match
		}
		// Hex-only and UUID-shaped strings are ids and digests, not secrets.
		// Redacting them would strip session ids and commit shas out of the corpus,
		// which is most of what makes it useful.
		if hexOnly.MatchString(match) {
			return match
		}
		if shannonEntropy(match) < EntropyThreshold {
			return match
		}
		s.counts["high-entropy"]++
		return placeholder("high-entropy", match)
	})
}

// placeholder renders [REDACTED:<rule>:<first 8 hex of sha256(match)>].
func placeholder(rule, match string) string {
	return "[REDACTED:" + rule + ":" + sha8(match) + "]"
}

func sha8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// shannonEntropy is bits per character over the string's own character
// frequencies.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]int{}
	n := 0
	for _, r := range s {
		freq[r]++
		n++
	}
	h := 0.0
	for _, c := range freq {
		p := float64(c) / float64(n)
		h -= p * math.Log2(p)
	}
	return h
}

// replaceAllFunc is regexp.ReplaceAllStringFunc, spelled out so the entropy pass
// and the rule pass share one traversal shape.
func replaceAllFunc(in string, re *regexp.Regexp, fn func(string) string) string {
	return re.ReplaceAllStringFunc(in, fn)
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
