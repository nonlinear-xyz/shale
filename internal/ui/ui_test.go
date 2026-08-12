package ui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

var profiles = []struct {
	name string
	prof termenv.Profile
}{
	{"TrueColor", termenv.TrueColor},
	{"ANSI256", termenv.ANSI256},
	{"ANSI16", termenv.ANSI},
}

// themeAt builds a theme pinned to one profile and background, so degradation
// is testable without a terminal.
func themeAt(p termenv.Profile, dark bool) *Theme {
	r := lipgloss.NewRenderer(&bytes.Buffer{})
	r.SetColorProfile(p)
	r.SetHasDarkBackground(dark)
	return themeFrom(r)
}

// TestSemanticTokensStayDistinct is the reason the palette pins its own
// fallbacks. Left to nearest-match conversion, terracotta and iron red both
// landed on bright red at 16 colors, so a skipped repository and a failed
// capture rendered identically — the two states a user most needs to tell
// apart. Any future palette edit that recreates a collision fails here.
func TestSemanticTokensStayDistinct(t *testing.T) {
	// Pairs that carry different meanings and must never render the same.
	pairs := [][2]string{
		{"Warn", "Danger"}, // "shale declined to look" vs "it broke"
		{"Success", "Danger"},
		{"Success", "Warn"},
		{"Accent", "Highlight"}, // "the ref" vs "why it matched"
		{"Accent", "Danger"},
		{"Title", "Danger"}, // a session title is not an error
		{"Title", "Facts"},  // the scan line vs its provenance
		{"Facts", "Gutter"},
	}

	for _, p := range profiles {
		for _, dark := range []bool{true, false} {
			mode := "dark"
			if !dark {
				mode = "light"
			}
			th := themeAt(p.prof, dark)
			styles := map[string]lipgloss.Style{
				"Warn": th.Warn, "Danger": th.Danger, "Success": th.Success,
				"Accent": th.Ref, "Highlight": th.Match, "Title": th.Title,
				"Facts": th.Facts, "Gutter": th.Gutter,
			}
			for _, pair := range pairs {
				a := sequenceOf(styles[pair[0]])
				b := sequenceOf(styles[pair[1]])
				if a == b {
					t.Errorf("%s/%s: %s and %s both render as %q — the distinction is lost",
						p.name, mode, pair[0], pair[1], a)
				}
			}
		}
	}
}

// TestEveryProfileEmitsColor catches a token that silently resolves to nothing
// at some depth, which would read as "this surface has no styling" rather than
// as a bug.
func TestEveryProfileEmitsColor(t *testing.T) {
	for _, p := range profiles {
		th := themeAt(p.prof, true)
		for name, st := range map[string]lipgloss.Style{
			"Title": th.Title, "Facts": th.Facts, "Ref": th.Ref, "Match": th.Match,
			"Success": th.Success, "Warn": th.Warn, "Danger": th.Danger,
			"Header": th.Header, "Gutter": th.Gutter, "Border": th.Border,
		} {
			if out := st.Render("x"); !strings.Contains(out, "\x1b[") {
				t.Errorf("%s: %s rendered without any escape sequence: %q", p.name, name, out)
			}
		}
	}
}

// TestPlainEmitsNothing is the piping guarantee at its source. Everything in
// render depends on this holding.
func TestPlainEmitsNothing(t *testing.T) {
	th := Plain()
	for name, st := range map[string]lipgloss.Style{
		"Title": th.Title, "Danger": th.Danger, "Ref": th.Ref,
		"Match": th.Match, "TabActive": th.TabActive, "Frame": th.Frame,
	} {
		if out := st.Render("x"); strings.Contains(out, "\x1b") {
			t.Errorf("%s emitted an escape sequence under the plain theme: %q", name, out)
		}
	}
	if r := th.Rule(4); r != "────" {
		t.Errorf("Rule(4) = %q, want an unstyled rule", r)
	}
}

// TestRuleWidth: a negative or zero width is a layout edge (a terminal one
// column wide, a pane with no room), not a panic.
func TestRuleWidth(t *testing.T) {
	th := Plain()
	for _, w := range []int{-5, 0} {
		if got := th.Rule(w); got != "" {
			t.Errorf("Rule(%d) = %q, want empty", w, got)
		}
	}
	if got := lipgloss.Width(th.Rule(10)); got != 10 {
		t.Errorf("Rule(10) width = %d, want 10", got)
	}
}

// sequenceOf extracts just the escape prefix a style emits, which is its
// identity for comparison purposes.
func sequenceOf(st lipgloss.Style) string {
	out := st.Render("\x00")
	if i := strings.Index(out, "\x00"); i >= 0 {
		return out[:i]
	}
	return out
}
