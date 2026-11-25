package tui

import (
	"image/color"
	"math"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

var hushPixels = [waveRows]string{
	"X...X X...X .XXXX X...X",
	"X...X X...X X.... X...X",
	"XXXXX X...X .XXX. XXXXX",
	"X...X X...X ....X X...X",
	"X...X .XXX. XXXX. X...X",
}

const wordColumns = 2 * len("X...X X...X .XXXX X...X")

var waveGlyphs = [...]string{" ", "▁", "▂", "▃", "▄", "▅", "▆", "▇", "█"}

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

func (v introInks) banner(a *introMotion, w int) []displayRow {
	if w < 50-len(margin)*2 {
		return []displayRow{{text: fit("hush", w)}}
	}
	visibleWave := 0
	if w >= 70-len(margin)*2 {
		visibleWave = min(waveColumns, w-wordColumns-2)
	}
	out := make([]displayRow, waveRows)
	alpha := a.opacity()
	for y, row := range hushPixels {
		var b strings.Builder
		b.Grow(128 + 24*visibleWave)
		var current introPaint
		write := func(s string, next introPaint) {
			if next != current {
				current.end(&b, next)
				next.set(&b)
				current = next
			}
			b.WriteString(s)
		}
		for x, pixel := range []byte(row) {
			if pixel != 'X' || alpha == 0 {
				b.WriteString("  ")
				continue
			}
			ink := v.word[x]
			if alpha < 1 {
				switch {
				case v.bg.hasRGB && ink.hasRGB:
					ink = mixIntro(v.bg, ink, alpha)
				case alpha < .70:
					ink = v.faint
				}
			}
			write("██", ink)
		}
		if visibleWave > 0 {
			b.WriteString("  ")
			for x, height := range a.wave.height[:visibleWave] {
				level := min(8, max(0, int(math.Round(height))-(waveRows-1-y)*8))
				if y == waveRows-1 {
					level = max(1, level)
				}
				if level == 0 {
					b.WriteByte(' ')
					continue
				}
				write(waveGlyphs[level], v.wave[x])
			}
		}
		write("", introPaint{})
		used := wordColumns
		if visibleWave > 0 {
			used += 2 + visibleWave
		}
		b.WriteString(strings.Repeat(" ", w-used))
		out[y].text = b.String()
	}
	return out
}

type introKey struct {
	width      int
	theme      uint64
	profile    colorprofile.Profile
	frame, rev uint64
	catalog    bool
}

type introMemo struct {
	bannerKey, reportKey introKey
	banner, report, rows []displayRow
	inkKey               introKey
	inks                 introInks
	sampleID             uint64
	cost                 time.Duration
}

func (i *introState) mono() bool { return i.noColor || i.profile < colorprofile.ANSI256 }

func (m *Model) introRows(i *introState, w int) []displayRow {
	v, a := i.memo, &i.motion
	key := introKey{width: w, theme: m.theme.Revision, profile: i.profile}
	if v.inkKey != key || v.banner == nil {
		v.inks = makeIntroInks(&m.palette, i.mono(), m.options.Fills != FillOff, i.profile == colorprofile.TrueColor)
		v.inkKey = key
	}
	bannerKey := key
	bannerKey.frame = a.frame
	if v.bannerKey != bannerKey || v.banner == nil {
		v.banner = v.inks.banner(a, w)
		v.bannerKey = bannerKey
	}
	reportKey := key
	reportKey.rev = i.revision
	reportKey.catalog = m.menuFeatures().Has(x.Catalog)
	if a.fading() {
		reportKey.frame = a.frame
	}
	if v.reportKey != reportKey || v.report == nil {
		v.report = m.introReportRows(i, w, v.report[:0])
		v.reportKey = reportKey
	}
	v.rows = append(v.rows[:0], v.banner...)
	v.rows = append(v.rows, v.report...)
	return v.rows
}

func (m *Model) introReportRows(i *introState, w int, out []displayRow) []displayRow {
	mono := i.mono()
	render := func(k, width int) string {
		label := i.labels[k]
		s := strings.TrimSpace(label.text)
		if s == "" {
			return ""
		}
		isRunStatus := k >= introAttention && k <= introAttentionThird
		glyph := ""
		if isRunStatus {
			glyph = "✗ "
			if label.tone == x.Warning {
				glyph = "◑ "
			}
			title := label.title
			if stringWidth(title)+stringWidth(s)+5 > width {
				title = ansi.Truncate(title, max(4, width/3), "…")
			}
			s = title + "   " + s
			width -= stringWidth(glyph)
			if stringWidth(s)+stringWidth(label.age) <= width {
				s += label.age
			}
		} else if k == introReview && stringWidth(s) > width {
			s, _, _ = strings.Cut(s, " · ")
		}
		s = ansi.Truncate(s, width, "…")
		progress := i.motion.fadeProgress(k)
		if !isRunStatus && !mono && progress < 1 && i.memo.inks.bg.hasRGB {
			role := roleForTone(label.tone)
			// The terminal palette fades neutral and secondary labels
			// toward its normal foreground, not toward ANSI bright black.
			if role != roleAccent && role != roleError && role != roleWarning && !m.palette.roles[role].custom {
				role = roleText
			}
			if c := m.palette.swatch(role); c != nil {
				var b strings.Builder
				mixIntro(i.memo.inks.bg, rgbPaint(c), .45+.55*progress).set(&b)
				b.WriteString(s)
				b.WriteString(resetForeground)
				return b.String()
			}
		}
		if isRunStatus {
			if !mono {
				glyph = m.paint(label.tone, glyph)
			}
			if progress < .65 {
				s = m.paint(x.Muted, s)
			}
			return glyph + s
		}
		if !mono || label.tone == x.Muted {
			s = m.paint(label.tone, s)
		}
		if progress < .65 && label.tone != x.Muted {
			s = m.paint(x.Muted, s)
		}
		return s
	}
	add := func(s string) { out = append(out, displayRow{text: fit(s, w)}) }
	add("")
	left, right := render(introHeading, w), render(introStatus, w)
	if right == "" {
		add(left)
	} else if stringWidth(left)+2+stringWidth(right) <= w {
		add(fit(left, w-stringWidth(right)) + right)
	} else {
		add(left)
		add(right)
	}
	if s := render(introModel, w); s != "" {
		add(s)
	}
	add("")
	for k := introProblem; k < introFieldCount; k++ {
		if i.labels[k].text == "" {
			continue
		}
		var title string
		switch k {
		case introProblem:
			title = "runtime"
		case introSessions:
			title = "sessions"
		case introAttention:
			title = "run status"
		case introSkills:
			title = "skills"
		case introContext:
			title = "context"
		}
		labelWidth := min(12, w/2)
		prefix := fit(ansi.Truncate(title, labelWidth, "…"), labelWidth)
		if !mono && title != "" {
			prefix = m.paint(x.Muted, prefix)
		}
		body := render(k, w-labelWidth)
		row := displayRow{text: fit(prefix+body, w)}
		if k == introReview {
			row.hits = []spanHit{{labelWidth, labelWidth + stringWidth(body), x.ActionRef{ID: switcherID}, "open thread list"}}
		}
		out = append(out, row)
		if !i.frozen && i.owner != nil && i.labels[k].actions.task != "" {
			ctx := m.renderContext(i.owner, w-labelWidth, 1, false)
			if hints := fitRunHints(nil, m.recoveryHints(i.labels[k].actions, false), ctx.Width); len(hints) > 0 {
				n := 1024
				for _, part := range m.layoutLine(ctx, hints, &n, 1) {
					hintRow := displayRow{text: strings.Repeat(" ", labelWidth)}
					mergeRow(&hintRow, part, "")
					out = append(out, hintRow)
				}
			}
		}
	}
	add("")
	return out
}

type introFrame struct {
	view             tea.View
	at, first, count int
}

func (m *Model) measureIntroFrame() {
	i := m.intro
	i.memo.sampleID = i.motion.renderID
	i.memo.cost = i.motion.clock.now().Sub(i.motion.renderAt)
}

// The renderer still receives a complete tea.View, but on an intro tick we
// only lay out and swap the intro rows. The transcript, live activity, widgets,
// footer and editor are reused. A tick changes only ink, not the review hit
// region. Bubble Tea diffs the resulting terminal cells.
func (m *Model) patchIntro() (tea.View, bool) {
	f := m.frame
	span := &f.intro
	if !m.currentFrame() || f.theme != m.theme.Revision || span.count == 0 {
		return tea.View{}, false
	}
	pane := m.pane()
	rows := m.introRows(m.intro, pane.width)
	if span.first+span.count > len(rows) {
		return tea.View{}, false
	}
	for k, r := range rows[span.first : span.first+span.count] {
		y := span.at + k
		f.rows[y] = margin[:pane.edge] + r.text
		if f.hasAbove || f.hasBelow {
			f.rows[y] += margin[:pane.edge-1] + m.scrollCell(f, y)
		}
	}
	span.view.SetContent(strings.Join(f.rows, "\n"))
	return span.view, true
}
