package tui

import (
	"image/color"
	"math"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// FillMode controls background fills. FillAuto disables them in Rex.
type FillMode string

const (
	FillAuto FillMode = "auto"
	FillOn   FillMode = "on"
	FillOff  FillMode = "off"
)

type palette struct {
	background, foreground        color.Color
	ansiColors                    [16]color.Color
	overrides                     themeColors
	noColor                       bool
	roles                         [roleCount]resolvedRole
	soft, key, selected           color.Color
	added, removed                color.Color
	softInk, activeTab, parkedTab string
}

func fill(s string, c color.Color) string {
	if c == nil || s == "" {
		return s
	}
	return lipgloss.NewStyle().Background(c).Render(s)
}

const resetBackground = "\x1b[49m"

// A row's surface resumes after its own nested fills (tabs, chips, diffs)
// and after a full reset from a formatted block.
func surface(s, ink string) string {
	if ink == "" || s == "" {
		return s
	}
	s = strings.ReplaceAll(s, resetBackground, ink)
	s = strings.ReplaceAll(s, ansi.ResetStyle, ansi.ResetStyle+ink)
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+ink)
	return ink + s + resetBackground
}

// Controls need readable text even when the terminal has no measurable solid
// palette. Faint (SGR 2) is for decorative metadata, not labels or key hints.
func (m *Model) control(s string) string {
	return m.palette.roles[roleControl].ink.paint(s)
}

func (m *Model) keyHint(key, label string) string { return m.keyCap(key) + m.control(" "+label) }

func (m *Model) keyCap(key string) string {
	if m.palette.key == nil {
		return m.control(key)
	}
	// Padding is independent of theme so it never changes row geometry.
	if m.palette.roles[roleControl].custom {
		key = m.control(key)
	}
	return fill(key, m.palette.key)
}

type queryBackground struct{ retries int }

func terminalQueries() tea.Cmd {
	return tea.Batch(tea.RequestBackgroundColor, tea.RequestForegroundColor,
		tea.Raw("\x1b]4;1;?\x07\x1b]4;2;?\x07\x1b]4;3;?\x07\x1b]4;4;?\x07\x1b]4;8;?\x07"),
		tea.Tick(300*time.Millisecond, func(time.Time) tea.Msg { return queryBackground{2} }))
}

func (m *Model) terminalMessage(msg tea.Msg) (bool, tea.Cmd) {
	switch v := msg.(type) {
	case tea.BackgroundColorMsg:
		m.palette.background = v.Color
		m.refreshTheme()
	case tea.ForegroundColorMsg:
		m.palette.foreground = v.Color
		m.refreshTheme()
	case tea.ColorProfileMsg:
		m.theme.TrueColor = v.Profile == colorprofile.TrueColor
		m.refreshTheme()
	case uv.UnknownOscEvent:
		p := strings.Split(strings.TrimSuffix(strings.TrimSuffix(string(v), "\x1b\\"), "\x07"), ";")
		if len(p) != 3 || p[0] != "\x1b]4" || len(p[1]) != 1 || !strings.Contains("12348", p[1]) || !strings.HasPrefix(p[2], "rgb:") {
			return true, nil
		}
		parts := strings.Split(p[2][4:], "/")
		if len(parts) != 3 {
			return true, nil
		}
		var rgb [3]uint8
		for i, s := range parts {
			if len(s) < 2 || len(s) > 4 {
				return true, nil
			}
			n, err := parseHex(s[:2])
			if err != nil {
				return true, nil
			}
			rgb[i] = n
		}
		m.palette.ansiColors[p[1][0]-'0'] = color.NRGBA{R: rgb[0], G: rgb[1], B: rgb[2], A: 255}
		m.refreshTheme()
	case uv.UnknownEvent:
		return true, nil
	case queryBackground:
		if m.palette.background == nil && v.retries > 0 {
			return true, tea.Batch(tea.RequestBackgroundColor, tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg { return queryBackground{v.retries - 1} }))
		}
	default:
		return false, nil
	}
	return true, nil
}

func (m *Model) refreshTheme() {
	m.theme.Revision++
	p := &m.palette
	bg := p.background
	m.theme.BackgroundKnown = bg != nil
	m.theme.Fills = bg != nil && m.theme.TrueColor && m.options.Fills != FillOff && !p.noColor
	p.resolveRoles(m.theme.TrueColor && !p.noColor)
	p.soft, p.key, p.selected = nil, nil, nil
	p.added, p.removed = nil, nil
	p.softInk, p.activeTab, p.parkedTab = "", "", ""
	if bg == nil {
		return
	}
	l, a, b := oklab(bg)
	m.theme.Light = l >= 0.6
	if !m.theme.Fills {
		return
	}
	p.selected = p.roleFill(roleAccent, .15)
	p.added = p.roleFill(roleSuccess, .20)
	p.removed = p.roleFill(roleError, .20)
	step := 0.045
	if m.theme.Light {
		step = -0.03
	}
	soft := fromLab(l+step, a, b)
	next, _, _ := oklab(soft)
	if math.Abs(next-l) >= 0.015 {
		p.soft, p.key = soft, fromLab(l+1.8*step, a, b)
		p.softInk = ansi.NewStyle().BackgroundColor(soft).String()
	}
	foreground := p.swatch(roleText)
	stateFill := func(role colorRole, amount float64) string {
		blend := p.roleFill(role, amount)
		if blend == nil || foreground != nil && contrastRatio(blend, foreground) < 4.5 {
			return ""
		}
		return ansi.NewStyle().BackgroundColor(blend).String()
	}
	p.activeTab, p.parkedTab = stateFill(roleAccent, .26), stateFill(roleWarning, .20)

	controls := p.roles[roleControl]
	if controls.custom && (p.selected == nil || contrastRatio(controls.rgb, p.selected) >= 4.5) &&
		(p.key == nil || contrastRatio(controls.rgb, p.key) >= 4.5) {
		return
	}
	p.roles[roleControl] = p.terminalRole(roleControl)
	if ink := blendPalette(bg, foreground, .78); ink != nil && contrastRatio(ink, bg) >= 4.5 &&
		(p.selected == nil || contrastRatio(ink, p.selected) >= 4.5) {
		p.roles[roleControl] = resolvedRole{rgb: ink, ink: roleInk{ansi.NewStyle().ForegroundColor(ink).String(), resetForeground}}
	}
}

func (p *palette) roleFill(role colorRole, amount float64) color.Color {
	swatch := p.roles[role]
	fill := blendPalette(p.background, swatch.rgb, amount)
	// Keep the default fills byte-for-byte, but don't blend a custom
	// foreground toward itself if it would lose contrast on the result.
	if swatch.custom && fill != nil && contrastRatio(fill, swatch.rgb) < 4.5 {
		return nil
	}
	return fill
}

func linearColor(x uint32) float64 {
	f := float64(x) / 65535
	if f <= 0.04045 {
		return f / 12.92
	}
	return math.Pow((f+0.055)/1.055, 2.4)
}

func contrastRatio(a, b color.Color) float64 {
	luminance := func(c color.Color) float64 {
		r, g, b, _ := c.RGBA()
		return .2126*linearColor(r) + .7152*linearColor(g) + .0722*linearColor(b)
	}
	first, second := luminance(a), luminance(b)
	return (max(first, second) + .05) / (min(first, second) + .05)
}

func oklab(c color.Color) (float64, float64, float64) {
	rr, gg, bb, _ := c.RGBA()
	r, g, b := linearColor(rr), linearColor(gg), linearColor(bb)
	l := math.Cbrt(0.4122214708*r + 0.5363325363*g + 0.0514459929*b)
	m := math.Cbrt(0.2119034982*r + 0.6806995451*g + 0.1073969566*b)
	s := math.Cbrt(0.0883024619*r + 0.2817188376*g + 0.6299787005*b)
	return 0.2104542553*l + 0.793617785*m - 0.0040720468*s, 1.9779984951*l - 2.428592205*m + 0.4505937099*s, 0.0259040371*l + 0.7827717662*m - 0.808675766*s
}

func fromLab(L, a, b float64) color.Color {
	l, m, s := L+0.3963377774*a+0.2158037573*b, L-0.1055613458*a-0.0638541728*b, L-0.0894841775*a-1.291485548*b
	l, m, s = l*l*l, m*m*m, s*s*s
	u8 := func(v float64) uint8 {
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		return uint8(math.Round(max(0, min(1, v)) * 255))
	}
	return color.NRGBA{R: u8(4.0767416621*l - 3.3077115913*m + 0.2309699292*s), G: u8(-1.2684380046*l + 2.6097574011*m - 0.3413193965*s), B: u8(-0.0041960863*l - 0.7034186147*m + 1.707614701*s), A: 255}
}

func blendPalette(first, second color.Color, amount float64) color.Color {
	if first == nil || second == nil {
		return nil
	}
	l, a, b := oklab(first)
	p, q, r := oklab(second)
	return fromLab(l+(p-l)*amount, a+(q-a)*amount, b+(r-b)*amount)
}

var foregroundSGR = regexp.MustCompile("^\x1b\\[(?:[012345679]|2[2345789]|3[0-79]|9[0-7])(?:;(?:[012345679]|2[2345789]|3[0-79]|9[0-7]))*m$")

// Child views may use simple foreground/emphasis SGR, never cursor motion,
// hyperlinks, clipboard commands, raw terminal writes or background colors.
func scopeView(s string, n int) string {
	var b strings.Builder
	var state byte
	for s = limitText(s, n); s != ""; {
		part, w, count, next := ansi.DecodeSequence(s, state, nil)
		if count <= 0 {
			break
		}
		if w > 0 || part == "\n" {
			b.WriteString(safeText(part))
		} else if part == ansi.ResetStyle || len(part) > 2 && part[0] == 27 && foregroundSGR.MatchString(part) {
			b.WriteString(part)
		} else if len(part) == 1 && part[0] < 32 {
			b.WriteString(safeText(part))
		}
		state, s = next, s[count:]
	}
	return b.String()
}
