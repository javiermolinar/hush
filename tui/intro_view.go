package tui

import (
	"math"
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
				glyph = "⏸ "
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
		if mono {
			return glyph + s
		}
		progress := i.motion.fadeProgress(k)
		if !isRunStatus && progress < 1 {
			if faded := m.palette.fadeIntro(i.memo.inks.bg, roleForTone(label.tone), s, progress); faded != "" {
				return faded
			}
		}
		if isRunStatus {
			if progress < .65 {
				s = m.quiet(s)
			}
			return m.paint(label.tone, glyph) + s
		}
		if progress < .65 {
			return m.quiet(s)
		}
		return m.paint(label.tone, s)
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
