package tui

import (
	"image/color"

	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

type colorRole uint8

const (
	roleNormal colorRole = iota
	roleStrong
	roleQuiet
	roleLive
	roleLink
	roleCursor
	roleControl
	roleAccent
	roleSuccess
	roleWarning
	roleError
	roleSyntaxKeyword
	roleSyntaxLiteral
	roleSyntaxComment
	roleCount
)

const (
	resetForeground = "\x1b[39m"
	minTextContrast = 4.5
	resetSyntax     = "\x1b[0m" // preserve the syntax renderer's full reset
)

type roleInk struct{ start, end string }

func (i roleInk) paint(s string) string {
	if s == "" || i.start == "" {
		return s
	}
	return i.start + s + i.end
}

type roleDefinition struct {
	name     string // existing theme JSON color key, if any
	terminal roleInk
	ansiSlot int // zero uses the terminal's reported default foreground
}

// The four type levels are fixed. Themes supply foreground colors, not
// attributes. Live and Cursor inherit Accent; Quiet is contrast-checked.
var colorRoles = [roleCount]roleDefinition{
	roleStrong:        {terminal: roleInk{"\x1b[1m", "\x1b[22m"}}, // full foreground + bold
	roleNormal:        {},                                         // full foreground, regular
	roleQuiet:         {name: "muted"},                            // dim foreground, regular
	roleLive:          {terminal: roleInk{"\x1b[3m", "\x1b[23m"}}, // accent + italic
	roleLink:          {name: "link"},                             // underline applied only to an openable target
	roleCursor:        {},                                         // accent marker, optional selected surface
	roleControl:       {name: "control"},
	roleAccent:        {name: "accent", terminal: roleInk{"\x1b[34m", resetForeground}, ansiSlot: 4},
	roleSuccess:       {name: "success", terminal: roleInk{"\x1b[32m", resetForeground}, ansiSlot: 2},
	roleWarning:       {name: "warning", terminal: roleInk{"\x1b[33m", resetForeground}, ansiSlot: 3},
	roleError:         {name: "error", terminal: roleInk{"\x1b[31m", resetForeground}, ansiSlot: 1},
	roleSyntaxKeyword: {name: "syntax_keyword", terminal: roleInk{"\x1b[35m", resetSyntax}, ansiSlot: 5},
	roleSyntaxLiteral: {name: "syntax_literal", terminal: roleInk{"\x1b[36m", resetSyntax}, ansiSlot: 6},
	roleSyntaxComment: {name: "syntax_comment", terminal: roleInk{end: resetSyntax}},
}

type themeColors [roleCount]color.Color

type resolvedRole struct {
	ink    roleInk
	rgb    color.Color
	custom bool
}

func (p *palette) terminalRole(role colorRole) resolvedRole {
	def := colorRoles[role]
	rgb := p.foreground
	if def.ansiSlot != 0 {
		rgb = p.ansiColors[def.ansiSlot]
	}
	return resolvedRole{ink: def.terminal, rgb: rgb}
}

func (p *palette) resolveRoles(allowRGB, light bool) {
	p.requested = p.overrides
	if light && p.lightOverrides != (themeColors{}) {
		p.requested = p.lightOverrides
	}
	for role, def := range colorRoles {
		value := p.terminalRole(colorRole(role))
		if rgb := p.requested[role]; def.name != "" && allowRGB && rgb != nil && p.background != nil &&
			contrastRatio(rgb, p.background) >= minTextContrast && (colorRole(role) != roleQuiet || p.dimmerThanNormal(rgb)) {
			end := resetForeground
			if def.terminal.end == resetSyntax {
				end = resetSyntax
			}
			value = resolvedRole{rgb: rgb, custom: true,
				ink: roleInk{ansi.NewStyle().ForegroundColor(rgb).String(), end}}
		}
		p.roles[role] = value
	}
	if !p.roles[roleQuiet].custom {
		p.roles[roleQuiet] = p.quietRole(allowRGB)
	}
	if !p.roles[roleSyntaxComment].custom {
		quiet := p.roles[roleQuiet]
		p.roles[roleSyntaxComment] = resolvedRole{ink: quiet.ink, rgb: quiet.rgb}
	}
	p.roles[roleStrong] = p.strongRole(allowRGB)
	accent := p.roles[roleAccent]
	p.roles[roleCursor] = accent
	live := colorRoles[roleLive].terminal
	accent.ink = roleInk{accent.ink.start + live.start, live.end + accent.ink.end}
	p.roles[roleLive] = accent
}

func (p *palette) dimmerThanNormal(rgb color.Color) bool {
	return p.foreground == nil || contrastRatio(p.foreground, p.background) < minTextContrast ||
		contrastRatio(rgb, p.background) < contrastRatio(p.foreground, p.background)
}

func contrastEndpoint(bg color.Color) color.Color {
	if contrastRatio(color.White, bg) > contrastRatio(color.Black, bg) {
		return color.White
	}
	return color.Black
}

// Inline emphasis has no cursor or action glyph. Give it a full-contrast
// foreground cue as well as weight when the terminal palette is measurable.
func (p *palette) strongRole(allowRGB bool) resolvedRole {
	strong := p.terminalRole(roleStrong)
	bg, fg := p.background, p.foreground
	if !allowRGB || bg == nil || fg == nil {
		return strong
	}
	full := blendPalette(fg, contrastEndpoint(bg), .35)
	if contrastRatio(full, bg) > contrastRatio(fg, bg) && contrastRatio(full, bg) >= minTextContrast {
		strong.rgb = full
		strong.ink = roleInk{ansi.NewStyle().ForegroundColor(full).String() + strong.ink.start, strong.ink.end + resetForeground}
	}
	return strong
}

func (p *palette) quietRole(allowRGB bool) resolvedRole {
	if p.background == nil || p.noColor {
		return p.terminalRole(roleQuiet)
	}
	bg := p.background
	if !allowRGB {
		// Use the terminal's dim slot only when its reported color is safe.
		if dim := p.ansiColors[8]; dim != nil && p.foreground != nil &&
			contrastRatio(dim, bg) >= minTextContrast && p.dimmerThanNormal(dim) {
			return resolvedRole{rgb: dim, ink: roleInk{"\x1b[90m", resetForeground}}
		}
		return p.terminalRole(roleQuiet)
	}
	fg := p.foreground
	if fg != nil {
		fg = color.NRGBAModel.Convert(fg) // check the 8-bit color the terminal receives
	}
	if fg == nil || contrastRatio(fg, bg) < minTextContrast {
		// Don't guess whether an unreported foreground is black or white.
		// One of these endpoints always meets 4.5:1 on an sRGB background.
		fg = contrastEndpoint(bg)
	}
	// Leave headroom for subtle fills where the palette allows it, but
	// still dim a foreground that starts close to the contrast floor.
	target := min(5.5, (minTextContrast+contrastRatio(fg, bg))/2)
	for mix := .70; mix < .99; mix = (mix + 1) / 2 {
		if dim := blendPalette(bg, fg, mix); contrastRatio(dim, bg) >= target {
			fg = dim
			break
		}
	}
	return resolvedRole{rgb: fg, ink: roleInk{ansi.NewStyle().ForegroundColor(fg).String(), resetForeground}}
}

// strongTone is used for built-in span content such as the plan preview title.
const strongTone x.Tone = "strong"

func roleForTone(tone x.Tone) colorRole {
	switch tone {
	case strongTone:
		return roleStrong
	case x.Muted:
		return roleQuiet
	case x.Accent:
		return roleAccent
	case x.Good:
		return roleSuccess
	case x.Warning:
		return roleWarning
	case x.Danger:
		return roleError
	default:
		return roleNormal
	}
}

// Only the single pinned tool slot can promote extension content to Live.
// A renderer can still use warning or failure colors for those states.
func contentRole(tone x.Tone, liveSlot bool) colorRole {
	if liveSlot && tone != x.Warning && tone != x.Danger {
		return roleLive
	}
	return roleForTone(tone)
}

func (p *palette) swatch(role colorRole) color.Color { return p.roles[role].rgb }

func (m *Model) ink(role colorRole, s string) string { return m.palette.roles[role].ink.paint(s) }
func (m *Model) paint(tone x.Tone, s string) string  { return m.ink(roleForTone(tone), s) }
func strongText(s string) string                     { return colorRoles[roleStrong].terminal.paint(s) }
func (m *Model) quiet(s string) string               { return m.ink(roleQuiet, s) }

func (m *Model) cursorMark(focused bool) string {
	if focused {
		return m.ink(roleCursor, "› ")
	}
	return "  "
}

func (m *Model) cursorRow(row string, width int, focused bool) string {
	if focused {
		return fill(fit(row, width), m.palette.selected)
	}
	return row
}

var (
	linkUnderline  = roleInk{"\x1b[4m", "\x1b[24m"}
	pathUnderline  = roleInk{"\x1b[4:4m", "\x1b[24m"}
	markdownStrike = roleInk{"\x1b[9m", "\x1b[29m"}
	// Copy selection is a terminal interaction, not a menu row or a type level.
	copySelection = roleInk{"\x1b[7m", "\x1b[27m"}
)

func (m *Model) linkText(text string, path, quiet bool) string {
	if !quiet {
		text = m.ink(roleLink, text)
	}
	if path {
		return pathUnderline.paint(text)
	}
	return linkUnderline.paint(text)
}

func (m *Model) liveText(s string) string { return m.ink(roleLive, s) }

func (p *palette) ensureContrast(allowRGB bool) {
	p.contrastAdjusted = false
	if p.background == nil || !allowRGB {
		return
	}
	bg := p.background
	end := color.Color(color.Black)
	if contrastRatio(color.White, bg) > contrastRatio(end, bg) {
		end = color.White
	}
	for role, current := range p.roles {
		if colorRole(role) == roleLive || colorRole(role) == roleStrong || colorRole(role) == roleCursor {
			continue // derived roles are refreshed below
		}
		requested := p.requested[role]
		rgb := current.rgb
		if p.semanticTheme {
			var base colorRole
			switch colorRole(role) {
			case roleSyntaxKeyword:
				base = roleAccent
			case roleSyntaxLiteral:
				base = roleSuccess
			}
			if base != 0 && p.roles[base].rgb != nil {
				rgb = p.roles[base].rgb
				current = resolvedRole{rgb: rgb, ink: roleInk{ansi.NewStyle().ForegroundColor(rgb).String(), resetForeground}}
				p.roles[role] = current
			}
		}
		if rgb == nil && p.semanticTheme && colorRoles[role].ansiSlot != 0 {
			// Some terminals don't report all palette slots. Use a measured
			// base swatch or the terminal foreground rather than guessing ANSI RGB.
			rgb = p.foreground
			if rgb == nil {
				rgb = end
			}
			current = resolvedRole{rgb: rgb, ink: roleInk{ansi.NewStyle().ForegroundColor(rgb).String(), resetForeground}}
			p.roles[role] = current
		}
		if requested != nil && contrastRatio(requested, bg) < 4.5 {
			// Preserve legacy fallback choices. Version 2 treats the four base
			// inks as inputs and raises the requested color itself.
			if p.semanticTheme {
				rgb = requested
			}
			p.contrastAdjusted = true
		}
		if rgb == nil || contrastRatio(rgb, bg) >= 4.5 {
			continue
		}

		// Blend toward the readable endpoint, checking the quantized RGB.
		lo, hi := 0.0, 1.0
		for range 12 {
			mid := (lo + hi) / 2
			if contrastRatio(blendPalette(rgb, end, mid), bg) >= 4.5 {
				hi = mid
			} else {
				lo = mid
			}
		}
		adjusted := blendPalette(rgb, end, hi)
		// Leave a margin for conversion to 8-bit after interpolation.
		if contrastRatio(adjusted, bg) < 4.5 {
			adjusted = end
		}
		ink := roleInk{ansi.NewStyle().ForegroundColor(adjusted).String(), resetForeground}
		if def := colorRoles[role].terminal; def.end == resetSyntax {
			ink.end = resetSyntax
		}
		p.roles[role] = resolvedRole{rgb: adjusted, ink: ink, custom: p.semanticTheme && requested != nil}
		p.contrastAdjusted = true
	}
	if p.semanticTheme {
		// The four semantic inks are the only configurable colors in v2.
		link := p.roles[roleAccent]
		p.roles[roleLink] = link
		comment := p.roles[roleQuiet]
		p.roles[roleSyntaxComment] = comment
	}
	accent := p.roles[roleAccent]
	p.roles[roleCursor] = accent
	if p.roles[roleNormal].ink.start != "" && p.roles[roleStrong].rgb == nil ||
		p.roles[roleStrong].rgb != nil && contrastRatio(p.roles[roleStrong].rgb, bg) < 4.5 {
		normal, attrs := p.roles[roleNormal], colorRoles[roleStrong].terminal
		p.roles[roleStrong] = resolvedRole{rgb: normal.rgb, ink: roleInk{normal.ink.start + attrs.start, attrs.end + normal.ink.end}}
	}
	p.roles[roleLive] = resolvedRole{rgb: accent.rgb, ink: roleInk{accent.ink.start + "\x1b[3m", "\x1b[23m" + accent.ink.end}}
}
