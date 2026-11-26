package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

const themesID x.ActionID = coreID + ".themes"
const openThemesOp x.LocalOp = "themes.open"
const themeTemplateChoice = ":template"

type clientThemes struct {
	store  themeStore
	active namedTheme
	save   *themeSave
}

type themeSave struct {
	token  uint64
	picker *dialog
	before namedTheme
}

type themePicker struct {
	before          namedTheme
	themes          map[string]namedTheme
	loading, saving bool
	problem         string
}

type themeSaved struct {
	token uint64
	name  string
	err   error
}

// themeNote says why some of the active theme's colors show the terminal
// palette instead, so a preview or save never falls back silently.
func (m *Model) themeNote() string {
	var requested int
	var fallback []string
	for role, def := range colorRoles {
		if def.name != "" && m.themes.active.colors[role] != nil {
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
	m.palette.overrides = theme.colors
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
	p := &themePicker{before: m.themes.active, loading: true}
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
	token := m.token()
	p.saving = true
	m.themes.save = &themeSave{token: token, picker: d, before: p.before}
	return func() tea.Msg {
		err := store.save(ctx, theme)
		return themeSaved{token: token, name: theme.name, err: err}
	}
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
		p.problem = "Could not save theme: " + msg.err.Error()
		if m.focusedDialog() != d {
			return m.notify(m.active, p.problem, x.Warning)
		}
		return nil
	}
	saved := "Saved theme " + msg.name
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

func (m *Model) themeRows(d *dialog, w, n int) ([]string, []int) {
	p := d.themePicker
	var rows []string
	var hits []int
	add := func(s string, pick int) {
		rows = append(rows, ansi.Truncate(s, w, "…"))
		hits = append(hits, pick)
	}
	if n < 1 {
		return nil, nil
	}
	if n >= 3 {
		// The title row always exists, so a note never moves the choices.
		title := m.paint(x.Accent, d.title)
		if note := m.themeNote(); note != "" && !p.loading && !p.saving {
			title += m.control(" · " + note)
		}
		add(title, -1)
	}
	if p.loading || p.saving {
		status := "Loading client themes…"
		if p.saving {
			status = "Saving theme…"
		}
		add(m.control(status), -1)
		return rows, hits
	}
	budget := n - len(rows)
	if p.problem != "" && budget >= 2 {
		add(m.paint(x.Warning, oneLine(p.problem)), -1)
		budget--
	}
	showKeys := budget >= 2
	if showKeys {
		budget--
	}
	showSample := budget >= 5 && w >= themeSampleWidth
	if showSample {
		budget--
	}
	first := min(max(0, d.selected-budget+1), max(0, len(d.filtered)-budget))
	for i := first; i < min(first+budget, len(d.filtered)); i++ {
		choice := d.choices[d.filtered[i]]
		label, tone := "  "+choice.Label, x.Plain
		if i == d.selected {
			label, tone = "› "+choice.Label, x.Accent
		}
		if choice.ID == p.before.name {
			choice.Description = strings.TrimSpace("current " + choice.Description)
		}
		row := m.paint(tone, label)
		if choice.Description != "" {
			row += m.control(" · " + choice.Description)
		}
		add(row, i)
	}
	if showSample {
		add(m.themeSampleRow(), -1)
	}
	if showKeys {
		action := "apply"
		if c, ok := d.choice(); ok && c.ID == themeTemplateChoice {
			action = "copy"
		}
		full := m.keyHint("↑↓", "preview") + "  " + m.keyHint("enter", action) + "  " + m.keyHint("esc", "cancel")
		add(m.hintRow(w, full, "↑↓  enter  esc", ""), -1)
	}
	return rows, hits
}
