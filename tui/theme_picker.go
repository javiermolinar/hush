package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	x "github.com/javiermolinar/hush/tui/extension"
)

const themesID x.ActionID = coreID + ".themes"
const openThemesOp x.LocalOp = "themes.open"
const themeTemplateChoice = ":template"

// themeFillsTarget is the picker row that cycles the fill preference.
const themeFillsTarget = -20

type clientThemes struct {
	store  themeStore
	active namedTheme
	fills  FillMode // the chosen preference; options.Fills is its effect here
	save   *themeSave
}

type themeSave struct {
	token       uint64
	picker      *dialog
	before      namedTheme
	fillsBefore FillMode
}

type themePicker struct {
	before          namedTheme
	fillsBefore     FillMode
	themes          map[string]namedTheme
	loading, saving bool
	problem         string
}

type themeSaved struct {
	token uint64
	name  string
	fills FillMode // set when the fill preference changed
	err   error
}

// themeNote says why some of the active theme's colors show the terminal
// palette instead, so a preview or save never falls back silently.
func (m *Model) themeNote() string {
	var requested int
	var fallback []string
	for role, def := range colorRoles {
		if def.name != "" && m.palette.requested[role] != nil {
			requested++
			if !m.palette.roles[role].custom {
				fallback = append(fallback, def.name)
			}
		}
	}
	switch {
	case len(fallback) == 0:
		return ""
	case m.palette.noColor:
		return "NO_COLOR is set: showing terminal colors"
	case !m.theme.TrueColor:
		return "needs truecolor: showing terminal colors"
	case m.palette.background == nil:
		return "background unknown: showing terminal colors"
	case len(fallback) == requested:
		return "low contrast here: showing terminal colors"
	case len(fallback) == 1:
		return fallback[0] + " uses its terminal color for contrast"
	case len(fallback) == 2:
		return fallback[0] + " and " + fallback[1] + " use terminal colors for contrast"
	}
	return fmt.Sprintf("%d of %d colors use the terminal palette for contrast", len(fallback), requested)
}

// themeSample shows the previewed roles. Its labels also size the row.
var themeSample = [...]struct {
	role  colorRole
	label string
}{{roleAccent, "Accent"}, {roleLink, "Link"}, {roleSuccess, "OK"}, {roleWarning, "Warn"}, {roleError, "Error"},
	{roleSyntaxKeyword, "if"}, {roleSyntaxLiteral, `"text"`}, {roleSyntaxComment, "// note"}}

var themeSampleWidth = func() int {
	width := 2 * (len(themeSample) - 1)
	for _, s := range themeSample {
		width += stringWidth(s.label)
	}
	return width
}()

func (m *Model) themeSampleRow() string {
	var b strings.Builder
	for i, s := range themeSample {
		if i > 0 {
			b.WriteString("  ")
		}
		label := s.label
		if s.role == roleLink {
			label = "\x1b[4m" + label + "\x1b[24m" // links stay underlined in any palette
		}
		b.WriteString(m.palette.roles[s.role].ink.paint(label))
	}
	return b.String()
}

func (m *Model) applyTheme(theme namedTheme) {
	if m.themes.active == theme {
		return
	}
	m.themes.active = theme
	m.palette.overrides, m.palette.lightOverrides = theme.colors, theme.light
	m.refreshTheme()
}

func (m *Model) openThemes(r ownerRoute, budget *int) tea.Cmd {
	if !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	if m.themes.save != nil {
		return m.notifyRoute(r, "Wait for the theme choice to save", x.Warning)
	}
	store := m.themes.store
	if store.home == "" {
		return m.notifyRoute(r, "Themes require an absolute client home directory", x.Warning)
	}
	p := &themePicker{before: m.themes.active, fillsBefore: m.themes.fills, loading: true}
	d := &dialog{route: r, token: m.token(), budget: budget, title: "Themes", themePicker: p}
	open := m.pushDialog(d)
	load := m.startWork(r, func(ctx context.Context) (any, error) { return store.list(ctx), nil },
		func(value any, err error, _ *int) tea.Cmd {
			if !m.foreground(r) || !slices.Contains(m.overlays, d) {
				return nil
			}
			p.loading = false
			if err != nil {
				p.problem = "Could not read themes: " + err.Error()
			} else {
				book := value.(themeBook)
				p.problem = book.problem
				p.themes = make(map[string]namedTheme, len(book.choices))
				for _, theme := range book.choices {
					p.themes[theme.name] = theme
				}
				slices.SortFunc(book.choices[1:], func(a, b namedTheme) int { return strings.Compare(a.name, b.name) })
				for _, theme := range book.choices {
					d.choices = append(d.choices, x.Choice{ID: theme.name, Label: theme.name})
				}
				if _, ok := p.themes[p.before.name]; !ok {
					// Keep the current in-memory palette if a file was
					// removed or invalidated. It is not a saveable choice.
					d.choices = append(d.choices, x.Choice{ID: p.before.name, Label: p.before.name, Description: "unavailable on disk"})
				}
			}
			d.choices = append(d.choices, x.Choice{ID: themeTemplateChoice, Label: "Copy starter JSON"})
			d.filter()
			if i := slices.IndexFunc(d.choices, func(c x.Choice) bool { return c.ID == p.before.name }); i >= 0 {
				d.selected = i
			}
			return nil
		}, budget)
	return tea.Batch(open, load)
}

func (m *Model) previewTheme(d *dialog) {
	pick, ok := d.choice()
	if !ok {
		return
	}
	p := d.themePicker
	if theme, exists := p.themes[pick.ID]; exists {
		m.applyTheme(theme)
	} else if pick.ID == p.before.name {
		m.applyTheme(p.before)
	}
}

func (m *Model) acceptTheme(d *dialog) tea.Cmd {
	p := d.themePicker
	if p.loading || p.saving || m.themes.save != nil {
		return nil
	}
	pick, ok := d.choice()
	if !ok {
		return nil
	}
	if pick.ID == themeTemplateChoice {
		b := maxEffects
		return tea.Batch(m.closeDialog(d, nil, false), m.copyText(d.route, ThemeTemplate(), &b))
	}
	theme, valid := p.themes[pick.ID]
	if !valid {
		return nil
	}
	m.applyTheme(theme)
	store, ctx := m.themes.store, m.ctx
	var fills FillMode
	if m.themes.fills != p.fillsBefore {
		fills = m.themes.fills
	}
	token := m.token()
	p.saving = true
	m.themes.save = &themeSave{token: token, picker: d, before: p.before, fillsBefore: p.fillsBefore}
	return func() tea.Msg {
		err := store.save(ctx, theme)
		if err == nil && fills != "" {
			err = store.saveFills(ctx, fills)
		}
		return themeSaved{token: token, name: theme.name, fills: fills, err: err}
	}
}

// previewFills shows a fill preference now. Enter saves it with the theme;
// Esc restores the previous one.
func (m *Model) previewFills(mode FillMode) {
	if m.themes.fills == mode {
		return
	}
	m.themes.fills = mode
	m.options.Fills = effectiveFills(mode)
	m.refreshTheme()
}

func (m *Model) moveFills(d *dialog, delta int) {
	if p := d.themePicker; !p.loading && !p.saving {
		m.previewFills(FillMode(cycleValue(fillModes, string(m.themes.fills), delta)))
	}
}

func (m *Model) themeFillsRow(w int) string {
	row := m.control("fills") + "  " + m.configOptions(fillModes, string(m.themes.fills), max(1, w-7))
	why := ""
	switch {
	case m.themes.fills == FillAuto && m.options.Fills == FillOff:
		why = "off in Rex"
	case m.options.Fills == FillOff || m.theme.Fills:
	case m.palette.noColor:
		why = "off with NO_COLOR"
	case !m.theme.TrueColor:
		why = "need truecolor"
	default:
		why = "need a reported background"
	}
	if why != "" {
		row += m.paint(x.Muted, " · "+why)
	}
	return row
}

func (m *Model) receiveThemeSaved(msg themeSaved) tea.Cmd {
	save := m.themes.save
	if save == nil || msg.token != save.token {
		return nil
	}
	m.themes.save = nil
	d := save.picker
	p := d.themePicker
	p.saving = false
	if msg.err != nil {
		m.applyTheme(save.before)
		m.previewFills(save.fillsBefore)
		p.problem = "Could not save theme: " + msg.err.Error()
		if m.focusedDialog() != d {
			return m.notify(m.active, p.problem, x.Warning)
		}
		return nil
	}
	saved := "Saved theme " + msg.name
	if msg.fills != "" {
		saved += " · fills " + string(msg.fills)
	}
	if note := m.themeNote(); note != "" {
		saved += " · " + note
	}
	return tea.Batch(m.closeDialog(d, msg.name, true), m.notify(m.active, saved, x.Good))
}

func (m *Model) themePickerKey(d *dialog, key string) tea.Cmd {
	p := d.themePicker
	if key == "esc" {
		if !p.saving {
			return m.closeDialog(d, nil, false)
		}
		return nil // the choice was already committed with Enter
	}
	if p.loading || p.saving || !m.dialogCanInput(d) {
		return nil
	}
	switch key {
	case "up":
		m.moveTheme(d, -1)
	case "down":
		m.moveTheme(d, 1)
	case "left":
		m.moveFills(d, -1)
	case "right":
		m.moveFills(d, 1)
	case "enter":
		return m.acceptTheme(d)
	}
	return nil
}

func (m *Model) moveTheme(d *dialog, delta int) {
	if !d.themePicker.loading && !d.themePicker.saving {
		d.move(delta)
		m.previewTheme(d)
	}
}

func (m *Model) themeRows(d *dialog, w, n int) menuFrame {
	p := d.themePicker
	c := menuChrome{title: d.title, count: menuCount(len(p.themes), "theme", "themes")}
	if p.loading || p.saving {
		status := "Loading client themes…"
		if p.saving {
			status = "Saving theme…"
		}
		return m.menuShell(w, n, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.quiet(status)}, targets: []int{-1}}
		})
	}
	c.scope = m.themeNote()
	current, ok := d.choice()
	if ok {
		switch {
		case current.ID == themeTemplateChoice:
			c.consequence = chromeLine(m.menuAction("Copy starter JSON", "Keep the current theme"))
		case current.Description == "unavailable on disk":
			c.consequence = chromeLine(m.quiet("Theme is no longer on disk · choose another theme"))
		default:
			c.consequence = chromeLine(m.menuAction("Save theme", current.Label+" · keep the previewed fills"))
		}
	}
	c.keys = chromeLine(m.menuKeys(w, "↑↓ move", "←→ fills", "↵ apply", d.zoomKey()))
	return m.menuShell(w, n, c, func(h int) menuFrame {
		var out menuFrame
		if p.problem != "" && h >= 3 {
			out.add(m.paint(x.Warning, oneLine(p.problem)), -1)
			h--
		}
		fills := h >= 3
		sample := h >= 5 && w >= themeSampleWidth
		if fills {
			h--
		}
		if sample {
			h--
		}
		list := m.menuColumns(w, h, func(width, height int) []string {
			description := current.Description
			if current.ID == themeTemplateChoice {
				description = "Copy an editable theme template to the clipboard"
			}
			if description == "" {
				description = "Preview the theme and fills before saving"
			}
			return m.menuDetail(current.Label, description, width, height)
		}, func(lw, lh int) menuFrame {
			var list menuFrame
			lo, hi := menuWindow(d.selected, len(d.filtered), lh)
			for i := lo; i < hi; i++ {
				choice := d.choices[d.filtered[i]]
				item := menuItem{title: choice.Label, focus: i == d.selected}
				if choice.ID == p.before.name {
					item.state, item.tone = "Current", x.Muted
				}
				list.add(m.menuRow(lw, 7, item), i)
			}
			return list
		})
		out.appendBody(list)
		if sample {
			out.add(m.themeSampleRow(), -1)
		}
		if fills {
			out.add(m.themeFillsRow(w), themeFillsTarget)
		}
		return out
	})
}
