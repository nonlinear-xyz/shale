package pack

import "strings"

// DistillQuery reduces a task description to a few distinctive search terms.
//
// This exists because a task is a sentence and FTS is a term matcher. "Fix the
// worktree cleanup so stale directories are removed after a merge" ANDed together
// matches nothing at all, and ORed together matches everything — so the sentence
// has to become a handful of terms before it touches the index.
//
// Terms are sorted longest-first as a cheap rarity proxy. Long tokens in developer
// prose are identifiers, file names and error codes; short ones are grammar. That
// ordering is what makes the first few terms the discriminating ones, which
// matters because only the first `max` survive.
func DistillQuery(task string, max int) []string {
	seen := map[string]bool{}
	var terms []string

	for _, raw := range strings.FieldsFunc(strings.ToLower(task), func(r rune) bool {
		// Keep the characters that hold identifiers together: a path, a
		// dotted.name and a kebab-case-flag must survive as single terms.
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return false
		case r == '_', r == '.', r == '/', r == '-':
			return false
		default:
			return true
		}
	}) {
		t := strings.Trim(raw, "./-")
		if len(t) < 3 || stopwords[t] || seen[t] {
			continue
		}
		seen[t] = true
		terms = append(terms, t)
	}

	// Longest first — rarity proxy, see above.
	for i := 1; i < len(terms); i++ {
		for j := i; j > 0 && len(terms[j]) > len(terms[j-1]); j-- {
			terms[j], terms[j-1] = terms[j-1], terms[j]
		}
	}
	if len(terms) > max {
		terms = terms[:max]
	}
	return terms
}

// stopwords includes the imperatives that open almost every task description
// ("fix the…", "add a…", "help me…"). They are perfectly ordinary English and
// carry zero retrieval signal, so leaving them in would make every task match
// every session.
var stopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "that": true, "this": true,
	"from": true, "into": true, "onto": true, "over": true, "under": true, "then": true,
	"when": true, "where": true, "what": true, "which": true, "while": true, "about": true,
	"there": true, "here": true, "have": true, "has": true, "had": true, "was": true,
	"were": true, "are": true, "not": true, "but": true, "can": true, "will": true,
	"would": true, "should": true, "could": true, "must": true, "may": true,
	"fix": true, "make": true, "add": true, "help": true, "need": true, "want": true,
	"use": true, "using": true, "get": true, "set": true, "put": true, "run": true,
	"try": true, "let": true, "our": true, "out": true, "all": true, "any": true,
	"some": true, "more": true, "most": true, "just": true, "also": true, "than": true,
	"them": true, "they": true, "you": true, "your": true, "its": true, "his": true,
	"her": true, "him": true, "she": true, "who": true, "how": true, "why": true,
	"now": true, "new": true, "old": true, "one": true, "two": true, "way": true,
	"see": true, "look": true, "know": true, "think": true, "like": true, "well": true,
	"good": true, "back": true, "down": true, "off": true, "only": true, "very": true,
	"much": true, "many": true, "such": true, "same": true, "each": true, "both": true,
	"does": true, "did": true, "done": true, "doing": true, "been": true, "being": true,
}
