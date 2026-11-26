package tui

import (
	"image/color"
	"math"
	"strconv"
	"strings"
)

// Intro animation uses interpolated foreground roles, not component colors.
type introPaint struct {
	rgb    [3]uint8
	ink    roleInk
	hasRGB bool
}

func rgbPaint(c color.Color) introPaint {
	if c == nil {
		return introPaint{}
	}
	r, g, b, _ := c.RGBA()
	return introPaint{rgb: [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}, hasRGB: true}
}

func mixIntro(a, b introPaint, t float64) introPaint {
	if !a.hasRGB || !b.hasRGB {
		if t < 0.5 {
			return a
		}
		return b
	}
	for k, v := range a.rgb {
		a.rgb[k] = uint8(math.Round(float64(v)*(1-t) + float64(b.rgb[k])*t))
	}
	return a
}

func (p introPaint) set(b *strings.Builder) {
	if !p.hasRGB {
		b.WriteString(p.ink.start)
		return
	}
	var arr [20]byte
	s := append(arr[:0], "\x1b[38;2;"...)
	for k, n := range p.rgb {
		if k != 0 {
			s = append(s, ';')
		}
		s = strconv.AppendUint(s, uint64(n), 10)
	}
	_, _ = b.Write(append(s, 'm'))
}

func (p introPaint) foreground() bool { return p.hasRGB || p.ink.end == resetForeground }

func (p introPaint) end(b *strings.Builder, next introPaint) {
	if p.foreground() {
		if !next.foreground() {
			b.WriteString(resetForeground)
		}
	} else {
		b.WriteString(p.ink.end)
	}
}

type introInks struct {
	word      [wordColumns / 2]introPaint
	wave      [waveColumns]introPaint
	bg, faint introPaint
}

func makeIntroInks(p *palette, mono, blend, trueColor bool) introInks {
	var v introInks
	if mono {
		return v
	}
	accent, secondary := introPaint{ink: p.roles[roleAccent].ink}, introPaint{ink: p.roles[roleText].ink}
	v.faint = introPaint{ink: p.roles[roleMuted].ink}
	muted, border := v.faint, v.faint
	if trueColor {
		if c := p.swatch(roleAccent); c != nil {
			accent = rgbPaint(c)
		}
		secondary = rgbPaint(p.swatch(roleText))
		bg := rgbPaint(p.background)
		reported := rgbPaint(p.swatch(roleMuted))
		if blend && bg.hasRGB && secondary.hasRGB {
			target := secondary
			if p.roles[roleMuted].custom {
				target = reported
			}
			muted = mixIntro(bg, target, .63)
			border = mixIntro(bg, target, .38)
			v.bg = bg
		} else if reported.hasRGB {
			// Translucent terminals use the reported palette, not the solid
			// background color they report but don't actually paint.
			border = reported
			muted = mixIntro(border, secondary, .45)
		}
	}
	for n := range v.word {
		v.word[n] = mixIntro(accent, secondary, float64(n)/float64(len(v.word)-1))
	}
	for n := range v.wave {
		p := 2 * float64(n) / (waveColumns - 1)
		if p <= 1 {
			v.wave[n] = mixIntro(accent, muted, p)
		} else {
			v.wave[n] = mixIntro(muted, border, p-1)
		}
	}
	return v
}

func (p *palette) fadeIntro(bg introPaint, role colorRole, text string, progress float64) string {
	if !bg.hasRGB {
		return ""
	}
	// Fade secondary terminal labels from the normal ink, not ANSI bright black.
	if role != roleAccent && role != roleError && role != roleWarning && !p.roles[role].custom {
		role = roleText
	}
	ink := rgbPaint(p.swatch(role))
	if !ink.hasRGB {
		return ""
	}
	var b strings.Builder
	mixIntro(bg, ink, .45+.55*progress).set(&b)
	b.WriteString(text)
	b.WriteString(resetForeground)
	return b.String()
}
