package tui

import (
	"image/color"

	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

type colorRole uint8

const (
	roleText colorRole = iota
	roleLive
	roleControl
	roleMuted
	roleAccent
	roleLink
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
	resetFaint      = "\x1b[22m"
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
	name        string // empty for terminal-owned text
	terminal    roleInk
	ansiSlot    int    // zero uses the terminal's reported default foreground
	rgbEmphasis string // emphasis that remains when a role uses RGB ink
}

// Terminal stays the built-in palette. Reported ANSI RGB values are for
// derived surfaces and gradients; ordinary text keeps the terminal's SGR roles.
var colorRoles = [roleCount]roleDefinition{
	roleText:          {},
	roleLive:          {}, // derived from Accent
	roleControl:       {name: "control"},
	roleMuted:         {name: "muted", terminal: roleInk{"\x1b[2m", resetFaint}, ansiSlot: 8},
	roleAccent:        {name: "accent", terminal: roleInk{"\x1b[34m", resetForeground}, ansiSlot: 4},
	roleLink:          {name: "link"}, // underline is independent of color
	roleSuccess:       {name: "success", terminal: roleInk{"\x1b[32m", resetForeground}, ansiSlot: 2},
	roleWarning:       {name: "warning", terminal: roleInk{"\x1b[33m", resetForeground}, ansiSlot: 3},
	roleError:         {name: "error", terminal: roleInk{"\x1b[31m", resetForeground}, ansiSlot: 1},
	roleSyntaxKeyword: {name: "syntax_keyword", terminal: roleInk{"\x1b[35m", resetSyntax}, ansiSlot: 5},
	roleSyntaxLiteral: {name: "syntax_literal", terminal: roleInk{"\x1b[36m", resetSyntax}, ansiSlot: 6},
	roleSyntaxComment: {name: "syntax_comment", terminal: roleInk{"\x1b[2;3m", resetSyntax}, rgbEmphasis: "\x1b[3m"},
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
			contrastRatio(rgb, p.background) >= 4.5 {
			end := resetForeground
			if def.terminal.end == resetSyntax {
				end = resetSyntax
			}
			value = resolvedRole{rgb: rgb, custom: true,
				ink: roleInk{ansi.NewStyle().ForegroundColor(rgb).String() + def.rgbEmphasis, end}}
		}
		p.roles[role] = value
	}
	accent := p.roles[roleAccent]
	p.roles[roleLive] = resolvedRole{rgb: accent.rgb, ink: roleInk{accent.ink.start + "\x1b[3m", "\x1b[23m" + accent.ink.end}}

}

func roleForTone(tone x.Tone) colorRole {
	switch tone {
	case x.Muted:
		return roleMuted
	case x.Accent:
		return roleAccent
	case x.Good:
		return roleSuccess
	case x.Warning:
		return roleWarning
	case x.Danger:
		return roleError
	default:
		return roleText
	}
}

func (p *palette) swatch(role colorRole) color.Color { return p.roles[role].rgb }

func (p *palette) diffFill(tone x.Tone) color.Color {
	switch tone {
	case x.Good:
		return p.added
	case x.Danger:
		return p.removed
	default:
		return nil
	}
}

func (m *Model) paint(tone x.Tone, s string) string {
	return m.palette.roles[roleForTone(tone)].ink.paint(s)
}

// Strong and Quiet are type levels, independent of the row's state color.
var strongInk = roleInk{"\x1b[1m", "\x1b[22m"}

func strongText(s string) string       { return strongInk.paint(s) }
func (m *Model) quiet(s string) string { return m.paint(x.Muted, s) }

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
	copySelection  = roleInk{"\x1b[7m", "\x1b[27m"}
)

func (m *Model) liveText(s string) string { return m.palette.roles[roleLive].ink.paint(s) }

func (m *Model) linkText(text string, path, quiet bool) string {
	if quiet {
		return text
	}
	text = m.palette.roles[roleLink].ink.paint(text)
	if path {
		return pathUnderline.paint(text)
	}
	return linkUnderline.paint(text)
}

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
		if colorRole(role) == roleLive {
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
		comment := p.roles[roleMuted]
		p.roles[roleSyntaxComment] = comment
	}
	accent := p.roles[roleAccent]
	p.roles[roleLive] = resolvedRole{rgb: accent.rgb, ink: roleInk{accent.ink.start + "\x1b[3m", "\x1b[23m" + accent.ink.end}}
}
