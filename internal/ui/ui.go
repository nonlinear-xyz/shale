// Package ui holds shale's visual vocabulary: a palette, the semantic tokens
// that name intent on top of it, and the styles every surface draws with.
//
// Two rules keep this from sprawling into a second, accidental design system:
//
//  1. NOTHING OUTSIDE THIS PACKAGE NAMES A COLOR. Callers ask for Theme.Ref or
//     Theme.Warn, never for "#D98E6A". A token is added only when it carries a
//     meaning no existing token carries — not when a surface wants a new shade.
//
//  2. EVERY STYLE COMES FROM A RENDERER BOUND TO A SPECIFIC WRITER. There is no
//     package-level style, because a package-level style resolves against
//     lipgloss's global renderer, which targets os.Stdout — and os.Stdout is the
//     JSON-RPC transport under `shale mcp`. See Plain.
//
// Colors come from the rock. Shale is grey when it is neither oxidized nor
// organic-rich, black when it is full of carbon, red when its iron has rusted,
// green when that iron stayed reduced. Color already encodes state in the
// material; this palette borrows the encoding rather than inventing one.
package ui

import (
	"io"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// shade is one color pinned at all three terminal depths.
//
// The fallbacks are hand-picked rather than computed. Left to termenv's
// nearest-match, this palette degrades badly at 16 colors: these are
// desaturated mineral tones, and the distance metric maps warm greys onto
// bright red. Measured, the automatic result put quartz (a near-white) on
// bright red, put terracotta and iron red on the SAME code — collapsing warn
// and danger into one color — and turned green shale yellow. A semantic system
// where two tokens render identically has stopped being semantic, so each one
// names its own fallback and the test in ui_test.go holds them apart.
type shade struct {
	hex     string
	ansi256 string
	ansi16  string
}

// The palette. Sampled off a shale cobble beach: slate greys, sea-slate blues,
// terracotta, ochre, quartz and iron red. Private on purpose — the exported
// surface is the token set below, so a surface cannot reach past intent and
// grab a hue directly.
var (
	slateDeep  = shade{"#3E4A54", "238", "8"} // wet slate, the darkest structural grey
	slateMid   = shade{"#5A6670", "60", "8"}
	slateLight = shade{"#8B9AA6", "103", "7"} // dry slate in sun
	slateInk   = shade{"#2A3138", "235", "0"} // near-black shale, for text on light ground

	seaSlateLo = shade{"#2E6480", "24", "6"}  // the blue-grey shale of image (b), on light ground
	seaSlateHi = shade{"#6FA3BC", "73", "14"} // the same rock, lit

	ochreLo = shade{"#8A6314", "94", "3"} // lichen yellow, dark enough to read on white
	ochreHi = shade{"#E8C46A", "185", "11"}

	terracottaLo = shade{"#A5542F", "130", "3"} // the salmon cobbles. Yellow at 16
	terracottaHi = shade{"#D98E6A", "173", "3"} // colors, so warn never reads as danger

	ironLo = shade{"#A5432E", "124", "1"} // oxidized iron — redder and more saturated
	ironHi = shade{"#E4573C", "167", "9"} // than terracotta at every depth

	greenLo = shade{"#4A6B3C", "65", "2"} // reduced-iron green shale
	greenHi = shade{"#7E9B6E", "108", "10"}

	quartz = shade{"#EDE7DE", "230", "15"} // the white pebbles
	buff   = shade{"#D8CDBE", "187", "7"}

	borderLo = shade{"#C9BFB2", "251", "7"}
	borderHi = shade{"#3E4A54", "238", "8"}
)

func adaptive(light, dark shade) lipgloss.CompleteAdaptiveColor {
	return lipgloss.CompleteAdaptiveColor{
		Light: lipgloss.CompleteColor{TrueColor: light.hex, ANSI256: light.ansi256, ANSI: light.ansi16},
		Dark:  lipgloss.CompleteColor{TrueColor: dark.hex, ANSI256: dark.ansi256, ANSI: dark.ansi16},
	}
}

// Semantic tokens. Named for what they mean in shale, not for where they sit in
// the palette — "Ref" survives a palette change, "Blue" does not.
var (
	// tokAccent marks the things you copy: refs, commands, the selected row.
	tokAccent = adaptive(seaSlateLo, seaSlateHi)
	// tokHighlight marks the thing that answered your question — a matched term.
	tokHighlight = adaptive(ochreLo, ochreHi)
	// tokEmphasis is the scan line: session titles, repo names.
	tokEmphasis = adaptive(slateInk, quartz)
	// tokMuted is provenance: source, scope, dates, paths.
	tokMuted = adaptive(slateMid, slateLight)
	// tokSubtle is chrome: column heads, rules, counts, key hints.
	tokSubtle = adaptive(slateLight, slateMid)
	// tokSuccess: captured, swept clean.
	tokSuccess = adaptive(greenLo, greenHi)
	// tokWarn: skipped, degraded, a fallback rather than a match.
	tokWarn = adaptive(terracottaLo, terracottaHi)
	// tokDanger: it failed. Reserved for real failure, never for "unusual".
	tokDanger = adaptive(ironLo, ironHi)
	// tokBorder: rules and frames.
	tokBorder = adaptive(borderLo, borderHi)
	// tokDim is the transcript gutter — present, never competing with the body.
	tokDim = adaptive(borderLo, slateDeep)
	// tokParchment backs the buff highlight used for the active tab.
	tokParchment = adaptive(buff, slateDeep)
)

// Theme is the full style set, bound to one output writer.
type Theme struct {
	r *lipgloss.Renderer

	// Text roles.
	Title    lipgloss.Style // session titles, repo names
	Facts    lipgloss.Style // the provenance line under a title
	Body     lipgloss.Style // excerpts and transcript prose
	Ref      lipgloss.Style // chunk:12:3 — the thing you copy
	Match    lipgloss.Style // a matched query term inside an excerpt
	Header   lipgloss.Style // table column heads
	Hint     lipgloss.Style // "run with --all", key hints
	Count    lipgloss.Style // "3 matches for …"
	Success  lipgloss.Style
	Warn     lipgloss.Style
	Danger   lipgloss.Style
	Gutter   lipgloss.Style // transcript line numbers
	Border   lipgloss.Style
	Selected lipgloss.Style // the focused row in an interactive list

	// Segment labels, keyed off sessions.SegmentKind by the render layer.
	LabelUser      lipgloss.Style
	LabelAssistant lipgloss.Style
	LabelTool      lipgloss.Style
	LabelOutput    lipgloss.Style
	LabelError     lipgloss.Style

	// Tabs. Bubbles has no tab component; these are the pieces the browse
	// surface assembles one from.
	TabActive   lipgloss.Style
	TabInactive lipgloss.Style
	TabBar      lipgloss.Style

	// List rows, for the Bubbles delegate. Defined here rather than in the
	// browse package so rule 1 holds: the interactive surface composes styles,
	// it does not name colors.
	ItemTitle         lipgloss.Style
	ItemDesc          lipgloss.Style
	ItemTitleSelected lipgloss.Style
	ItemDescSelected  lipgloss.Style
	ItemTitleDim      lipgloss.Style
	ItemDescDim       lipgloss.Style

	// Interactive chrome.
	Frame       lipgloss.Style // pane outline
	FrameFocus  lipgloss.Style // pane outline when it has focus
	Prompt      lipgloss.Style
	Placeholder lipgloss.Style
	StatusBar   lipgloss.Style
	Key         lipgloss.Style // a key name in a help hint
}

// New builds a theme that renders to w.
//
// Color is decided by termenv from w itself: a pipe, a dumb terminal or
// NO_COLOR all resolve to the ascii profile, and every style below degrades to
// its plain text. That is why `shale search | grep` needs no special case —
// there is nothing to strip, because nothing was ever emitted.
func New(w io.Writer) *Theme {
	r := lipgloss.NewRenderer(w)

	// Pin the background when there is no color to pick, instead of letting
	// lipgloss work it out.
	//
	// Resolving an adaptive color asks the renderer HasDarkBackground(), and
	// lipgloss asks that BEFORE it checks the profile — so even at the ascii
	// profile, where the answer cannot change the output, termenv writes an
	// OSC 11 background query plus a cursor-position report to the terminal and
	// blocks waiting for the reply. Measured under a PTY with NO_COLOR set,
	// that put `\x1b]11;?\x1b\\\x1b[6n` at the head of otherwise plain output
	// and cost a round trip per command. An agent shelling out to shale reads
	// that as content. NO_COLOR must mean zero escape bytes, not "no color plus
	// a handshake".
	if r.ColorProfile() == termenv.Ascii {
		r.SetHasDarkBackground(true)
	}
	return themeFrom(r)
}

// NoColor builds a theme for a writer that IS a terminal but must not be
// styled — an agent shelling out under a PTY, or an explicit --no-color.
func NoColor(w io.Writer) *Theme {
	r := lipgloss.NewRenderer(w)
	r.SetColorProfile(termenv.Ascii)
	r.SetHasDarkBackground(true) // pinned: see New
	return themeFrom(r)
}

// Plain builds a theme that emits no escape sequences at all, whatever the
// environment claims.
//
// Two callers need this. Tests, so golden output does not churn every time a
// shade moves. And `shale mcp`, where stdout carries JSON-RPC frames: resolving
// an adaptive color against a real terminal makes termenv write an OSC
// background-color query and wait for the reply, which on that path would mean
// injecting bytes into the protocol stream and blocking on a response no MCP
// client will ever send.
func Plain() *Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	return themeFrom(r)
}

func themeFrom(r *lipgloss.Renderer) *Theme {
	s := r.NewStyle
	return &Theme{
		r: r,

		Title:    s().Foreground(tokEmphasis).Bold(true),
		Facts:    s().Foreground(tokMuted),
		Body:     s(),
		Ref:      s().Foreground(tokAccent),
		Match:    s().Foreground(tokHighlight).Bold(true),
		Header:   s().Foreground(tokSubtle).Bold(true),
		Hint:     s().Foreground(tokSubtle).Italic(true),
		Count:    s().Foreground(tokMuted),
		Success:  s().Foreground(tokSuccess),
		Warn:     s().Foreground(tokWarn),
		Danger:   s().Foreground(tokDanger).Bold(true),
		Gutter:   s().Foreground(tokDim),
		Border:   s().Foreground(tokBorder),
		Selected: s().Foreground(tokAccent).Bold(true),

		// The transcript's own semantics: what you said, what the agent said,
		// what it ran, what came back, and what blew up.
		LabelUser:      s().Foreground(tokAccent),
		LabelAssistant: s().Foreground(tokEmphasis),
		LabelTool:      s().Foreground(tokMuted),
		LabelOutput:    s().Foreground(tokSubtle),
		LabelError:     s().Foreground(tokDanger).Bold(true),

		TabActive: s().Foreground(tokEmphasis).Background(tokParchment).
			Bold(true).Padding(0, 2),
		TabInactive: s().Foreground(tokSubtle).Padding(0, 2),
		TabBar:      s().Foreground(tokBorder),

		// The selected row is marked by an accent bar in the left gutter rather
		// than by a filled background: a full-width highlight fights the excerpt
		// underneath it, and this list exists to be read, not just pointed at.
		ItemTitle: s().Foreground(tokEmphasis).Padding(0, 0, 0, 2),
		ItemDesc:  s().Foreground(tokMuted).Padding(0, 0, 0, 2),
		ItemTitleSelected: s().Foreground(tokAccent).Bold(true).
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(tokAccent).Padding(0, 0, 0, 1),
		ItemDescSelected: s().Foreground(tokMuted).
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(tokAccent).Padding(0, 0, 0, 1),
		ItemTitleDim: s().Foreground(tokSubtle).Padding(0, 0, 0, 2),
		ItemDescDim:  s().Foreground(tokSubtle).Padding(0, 0, 0, 2),

		Frame: s().Border(lipgloss.RoundedBorder()).
			BorderForeground(tokBorder).Padding(0, 1),
		FrameFocus: s().Border(lipgloss.RoundedBorder()).
			BorderForeground(tokAccent).Padding(0, 1),
		Prompt:      s().Foreground(tokAccent),
		Placeholder: s().Foreground(tokSubtle),
		StatusBar:   s().Foreground(tokSubtle),
		Key:         s().Foreground(tokAccent).Bold(true),
	}
}

// Renderer exposes the underlying renderer for the interactive surface, which
// needs it to build Bubbles components against the same output.
func (t *Theme) Renderer() *lipgloss.Renderer { return t.r }

// Rule draws a horizontal divider of the given width.
func (t *Theme) Rule(width int) string {
	if width < 1 {
		return ""
	}
	return t.Border.Render(repeat("─", width))
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
