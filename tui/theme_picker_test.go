package tui

import (
	"context"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func darkThemeTerminal(m *Model) {
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	m.Update(tea.BackgroundColorMsg{Color: color.NRGBA{R: 30, G: 30, B: 46, A: 255}})
	m.Update(tea.ForegroundColorMsg{Color: color.NRGBA{R: 205, G: 214, B: 244, A: 255}})
}

func finishThemeWork(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("missing theme work command")
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		if len(msg) != 1 {
			t.Fatalf("expected only one theme job, got %d", len(msg))
		}
		finishThemeWork(t, m, msg[0])
	case workMsg:
		m.finishWork(msg)
	default:
		t.Fatalf("expected theme work result, got %T", msg)
	}
}

func openTestThemes(t *testing.T, m *Model) *dialog {
	t.Helper()
	r := m.route(m.active, coreID)
	b := maxEffects
	cmd := m.openThemes(r, &b)
	d := m.focusedDialog()
	if d == nil || d.themePicker == nil || !d.themePicker.loading {
		t.Fatal("theme picker missing its loading state")
	}
	assertFrame(t, m)
	finishThemeWork(t, m, cmd)
	assertFrame(t, m)
	if d.themePicker.loading {
		t.Fatal("theme picker never loaded")
	}
	return d
}

func chooseThemeForTest(t *testing.T, m *Model, d *dialog, name string) {
	t.Helper()
	i := slices.IndexFunc(d.choices, func(c x.Choice) bool { return c.ID == name })
	if i < 0 {
		t.Fatalf("theme %q not in picker", name)
	}
	for d.selected != i {
		if d.selected < i {
			press(m, tea.KeyDown, 0)
		} else {
			press(m, tea.KeyUp, 0)
		}
	}
}

func TestThemePickerPreviewCancelAndSave(t *testing.T) {
	store, dir := themeFixture(t)
	pref := filepath.Join(store.home, ".hush", "theme")
	writeThemeFixture(t, dir, "broken.json", `{"version":1,"base":"terminal","colors":{"accent":"not a color"}}`)
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	darkThemeTerminal(m)
	m.editor.set("keep this draft")
	before := assertFrame(t, m)
	// Open via the existing searchable command palette, not a separate menu.
	b := maxEffects
	m.openPalette(m.route(m.active, coreID), &b)
	p := m.focusedDialog()
	p.query = "themes"
	p.filter()
	if c, ok := p.choice(); !ok || c.ID != string(themesID) {
		t.Fatalf("Themes not discoverable in the command palette: %+v", c)
	}
	cmd := m.acceptChoice(p)
	d := m.focusedDialog()
	if d == p || d.themePicker == nil || !d.themePicker.loading {
		t.Fatal("Themes didn't layer over the command palette")
	}
	finishThemeWork(t, m, cmd)
	if !strings.Contains(d.themePicker.problem, "broken.json") || !strings.Contains(d.themePicker.problem, "#RRGGBB") ||
		slices.ContainsFunc(d.choices, func(c x.Choice) bool { return c.ID == "broken" }) {
		t.Fatalf("invalid file was selectable or had no actionable error: %+v", d.themePicker)
	}
	pickerBefore := assertFrame(t, m)
	geometry := *m.frame
	chooseThemeForTest(t, m, d, "violet")
	if m.themes.active.name != "violet" || m.palette.roles[roleAccent].ink.start != "\x1b[38;2;139;157;245m" ||
		m.paint(x.Warning, "warn") != "\x1b[33mwarn\x1b[39m" {
		t.Fatal("arrow preview didn't apply partial roles")
	}
	pickerAfter := assertFrame(t, m)
	if geometry.dialogX != m.frame.dialogX || geometry.dialogY != m.frame.dialogY || geometry.dialogHeight != m.frame.dialogHeight ||
		geometry.ruleY != m.frame.ruleY || ansi.Strip(pickerBefore) == ansi.Strip(pickerAfter) {
		t.Fatal("preview didn't move the selection in place")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != p || m.themes.active.name != terminalTheme {
		t.Fatal("Escape didn't restore palette/action focus")
	}
	press(m, tea.KeyEscape, 0)
	if m.editor.value() != "keep this draft" || assertFrame(t, m) != before {
		t.Fatal("Escape didn't restore the original frame and draft")
	}
	if _, err := os.Stat(pref); !os.IsNotExist(err) {
		t.Fatalf("preview or Esc wrote the preference: %v", err)
	}
	d = openTestThemes(t, m)
	chooseThemeForTest(t, m, d, "violet")
	cmd = press(m, tea.KeyEnter, 0)
	if _, err := os.Stat(pref); !os.IsNotExist(err) || !d.themePicker.saving || cmd == nil {
		t.Fatalf("save ran on the input loop or was not scheduled: %v", err)
	}
	saved, ok := cmd().(themeSaved)
	if !ok || saved.err != nil {
		t.Fatalf("failed to save selection: %+v", saved)
	}
	m.Update(saved)
	if m.focusedDialog() != nil || m.themes.active.name != "violet" {
		t.Fatal("saved choice didn't close the picker and retain the theme")
	}
	reattach, _ := demo(t, Options{Fills: FillOff, WorkspaceLocal: false, ReducedMotion: true})
	if reattach.themes.active.name != "violet" {
		t.Fatal("saved theme did not survive a fresh (remote-workspace) model")
	}
	chosen, _ := demo(t, Options{ThemeName: "ink"})
	if chosen.themes.active.name != "ink" {
		t.Fatal("launch theme didn't override saved name")
	}
	raw, err := os.ReadFile(pref)
	if err != nil || string(raw) != "violet\n" {
		t.Fatalf("launch override wrote the selection: %q %v", raw, err)
	}
}

func TestThemeSlashPreviewsAndAppliesPreset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NO_COLOR", "")
	m := modelFor(t, Options{Fills: FillOff, ReducedMotion: true})
	darkThemeTerminal(m)
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 24})
	open := func() *dialog {
		t.Helper()
		m.editor.set("/the")
		if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "/theme") {
			t.Fatalf("theme not in slash completion:\n%s", body)
		}
		press(m, tea.KeyTab, 0)
		if m.editor.value() != "/theme " {
			t.Fatalf("theme didn't complete: %q", m.editor.value())
		}
		cmd := press(m, tea.KeyEnter, 0)
		d := m.focusedDialog()
		if d == nil || d.themePicker == nil || !d.themePicker.loading || m.editor.value() != "" {
			t.Fatal("/theme did not open its picker from the startup prompt")
		}
		finishThemeWork(t, m, cmd)
		assertFrame(t, m)
		return d
	}
	assertNotSaved := func() {
		t.Helper()
		if _, err := os.Stat(filepath.Join(home, ".hush", "theme")); !os.IsNotExist(err) {
			t.Fatalf("preview wrote the preference: %v", err)
		}
	}
	d := open()
	chooseThemeForTest(t, m, d, "gruvbox")
	body := assertFrame(t, m)
	if m.themes.active.name != "gruvbox" || !strings.Contains(body, "\x1b[38;2;250;189;47mAccent") ||
		!strings.Contains(ansi.Strip(body), "↵ apply") {
		t.Fatalf("missing live preset preview or apply hint:\n%s", ansi.Strip(body))
	}
	assertNotSaved()
	press(m, tea.KeyEscape, 0)
	if m.themes.active.name != terminalTheme || m.focusedDialog() != nil {
		t.Fatal("Esc did not cancel the preset preview")
	}
	assertNotSaved()
	d = open()
	f := m.frame
	row := slices.Index(f.dialogChoices, slices.IndexFunc(d.choices, func(c x.Choice) bool { return c.ID == "rose-pine" }))
	if row < 0 {
		t.Fatal("expected a visible Rosé Pine choice to click")
	}
	click := func() tea.Cmd {
		_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 3, Y: f.dialogY + row, Button: tea.MouseLeft}))
		return cmd
	}
	click()
	if m.themes.active.name != "rose-pine" || d.themePicker.saving {
		t.Fatal("click must preview without applying")
	}
	assertNotSaved()
	assertFrame(t, m)
	cmd := click() // the previewed row: apply, like Enter
	if cmd == nil || !d.themePicker.saving {
		t.Fatal("second click didn't schedule apply")
	}
	assertNotSaved()
	saved, ok := cmd().(themeSaved)
	if !ok || saved.err != nil {
		t.Fatalf("preset apply failed: %+v", saved)
	}
	m.Update(saved)
	reattach := modelFor(t, Options{})
	if m.focusedDialog() != nil || m.themes.active.name != "rose-pine" || m.themes.active != reattach.themes.active {
		t.Fatal("applied preset did not persist")
	}
}

func freshThemeHome(t *testing.T, noColor string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("NO_COLOR", noColor)
	t.Setenv("TERM_PROGRAM", "")
	return home
}

func TestThemeStarterRowCopiesOnClick(t *testing.T) {
	home := freshThemeHome(t, "")
	var clipboard string
	m := modelFor(t, Options{Fills: FillOff, ReducedMotion: true, CopyText: func(_ context.Context, text string) error {
		clipboard = text
		return nil
	}})
	darkThemeTerminal(m)
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 40})
	d := openTestThemes(t, m)
	f := m.frame
	row := slices.Index(f.dialogChoices, slices.IndexFunc(d.choices, func(c x.Choice) bool { return c.ID == themeTemplateChoice }))
	if row < 0 || d.selected == f.dialogChoices[row] {
		t.Fatal("expected a visible, unselected starter row")
	}
	_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 3, Y: f.dialogY + row, Button: tea.MouseLeft}))
	finishThemeWork(t, m, cmd)
	if clipboard != ThemeTemplate() || m.focusedDialog() != nil || m.themes.active.name != terminalTheme {
		t.Fatal("one click on the starter row didn't copy it")
	}
	if _, err := os.Stat(filepath.Join(home, ".hush")); !os.IsNotExist(err) {
		t.Fatalf("copying the starter wrote config: %v", err)
	}
}

func TestThemePickerExplainsFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name, theme, noColor, want string
		profile                    colorprofile.Profile
		bg                         color.Color
	}{
		{"dark terminal", "gruvbox", "", "", colorprofile.TrueColor, themeRGB(0x282828)},
		{"light terminal", "gruvbox", "", "", colorprofile.TrueColor, themeRGB(0xeff1f5)},
		{"no truecolor", "gruvbox", "", "needs truecolor: showing terminal colors", colorprofile.ANSI256, themeRGB(0x282828)},
		{"NO_COLOR", "gruvbox", "1", "NO_COLOR is set: showing terminal colors", colorprofile.TrueColor, themeRGB(0x282828)},
		{"unknown background", "gruvbox", "", "background unknown: showing terminal colors", colorprofile.TrueColor, nil},
		{"one role", "gruvbox", "", "error uses its terminal color for contrast", colorprofile.TrueColor, themeRGB(0x32302f)},
		{"two roles", "rose-pine", "", "muted and syntax_comment use terminal colors for contrast", colorprofile.TrueColor, themeRGB(0x282c34)},
		{"every role", "gruvbox", "", "low contrast here: showing terminal colors", colorprofile.TrueColor, themeRGB(0x6b6b6b)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshThemeHome(t, tc.noColor)
			m := modelFor(t, Options{Fills: FillOff, ReducedMotion: true})
			m.Update(tea.ColorProfileMsg{Profile: tc.profile})
			if tc.bg != nil {
				m.Update(tea.BackgroundColorMsg{Color: tc.bg})
			}
			m.Update(tea.WindowSizeMsg{Width: 96, Height: 24})
			d := openTestThemes(t, m)
			before := *m.frame
			chooseThemeForTest(t, m, d, tc.theme)
			body := ansi.Strip(assertFrame(t, m))
			if got := m.themeNote(); got != tc.want || tc.want != "" && !strings.Contains(body, "Themes · "+tc.want) {
				t.Fatalf("note %q, want %q:\n%s", got, tc.want, body)
			}
			if m.frame.dialogY != before.dialogY || m.frame.dialogHeight != before.dialogHeight {
				t.Fatal("the note moved the picker")
			}
			cmd := press(m, tea.KeyEnter, 0)
			saved, ok := cmd().(themeSaved)
			if !ok || saved.err != nil {
				t.Fatalf("save failed: %+v", saved)
			}
			m.Update(saved)
			want := "Saved theme " + tc.theme
			if tc.want != "" {
				want += " · " + tc.want
			}
			if m.focusedDialog() != nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), want) {
				t.Fatalf("save notice should read %q:\n%s", want, ansi.Strip(assertFrame(t, m)))
			}
		})
	}
}

func TestPresetFollowsTerminalBackground(t *testing.T) {
	freshThemeHome(t, "")
	m := modelFor(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	preset, _ := presetTheme("rose-pine")
	m.applyTheme(preset)
	for _, tc := range []struct {
		bg   color.Color
		want themeColors
	}{{themeRGB(0x191724), preset.colors}, {themeRGB(0xfaf4ed), rosePineDawn}, {themeRGB(0x1e1e2e), preset.colors}} {
		m.Update(tea.BackgroundColorMsg{Color: tc.bg})
		for role, def := range colorRoles {
			if def.name != "" && (!m.palette.roles[role].custom || m.palette.roles[role].rgb != tc.want[role]) {
				t.Fatalf("on %v, %s didn't use the set for that background", tc.bg, def.name)
			}
		}
		if m.themes.active.name != "rose-pine" || m.themeNote() != "" {
			t.Fatalf("background change replaced the choice or fell back: %q", m.themeNote())
		}
	}
	// A JSON theme has one set, used on any background.
	custom := namedTheme{name: "custom", colors: themeColors{roleAccent: themeRGB(0x402886)}}
	m.applyTheme(custom)
	if m.palette.roles[roleAccent].custom || m.themeNote() != "low contrast here: showing terminal colors" {
		t.Fatal("dark ink applied on a dark background")
	}
	m.Update(tea.BackgroundColorMsg{Color: themeRGB(0xfaf4ed)})
	if !m.palette.roles[roleAccent].custom || m.palette.roles[roleAccent].rgb != custom.colors[roleAccent] {
		t.Fatal("a one-set theme didn't apply on a light background")
	}
}

func TestFillsPreferenceSavesFromThemePicker(t *testing.T) {
	home := freshThemeHome(t, "")
	pref := filepath.Join(home, ".hush", "fills")
	m := modelFor(t, Options{ReducedMotion: true})
	darkThemeTerminal(m)
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 24})
	if m.themes.fills != FillAuto || m.options.Fills != FillOn || !m.theme.Fills {
		t.Fatalf("fresh fills: %q %q %v", m.themes.fills, m.options.Fills, m.theme.Fills)
	}
	d := openTestThemes(t, m)
	press(m, tea.KeyRight, 0)
	press(m, tea.KeyRight, 0)
	body := ansi.Strip(assertFrame(t, m))
	if m.themes.fills != FillOff || m.theme.Fills || !strings.Contains(body, "fills  auto  on  off") || !strings.Contains(body, "←→ fills") {
		t.Fatalf("→ didn't preview fills off:\n%s", body)
	}
	press(m, tea.KeyEscape, 0)
	if m.themes.fills != FillAuto || !m.theme.Fills || m.focusedDialog() != nil {
		t.Fatal("Esc didn't restore the fill preference")
	}
	if _, err := os.Stat(pref); !os.IsNotExist(err) {
		t.Fatalf("preview wrote the fill preference: %v", err)
	}
	d = openTestThemes(t, m)
	f := m.frame
	row := slices.Index(f.dialogChoices, themeFillsTarget)
	if row < 0 {
		t.Fatal("fills row not visible")
	}
	for range 2 { // click cycles: auto → on → off
		m.Update(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 3, Y: f.dialogY + row, Button: tea.MouseLeft}))
	}
	if m.themes.fills != FillOff || d.themePicker.saving {
		t.Fatalf("clicking the fills row: %q", m.themes.fills)
	}
	cmd := press(m, tea.KeyEnter, 0)
	saved, ok := cmd().(themeSaved)
	if !ok || saved.err != nil || saved.fills != FillOff {
		t.Fatalf("Enter didn't save fills: %+v", saved)
	}
	m.Update(saved)
	if raw, err := os.ReadFile(pref); err != nil || string(raw) != "off\n" || !strings.Contains(m.notice.text, "fills off") {
		t.Fatalf("saved %q (%v), notice %q", raw, err, m.notice.text)
	}
	if next := modelFor(t, Options{}); next.themes.fills != FillOff || next.options.Fills != FillOff {
		t.Fatal("saved fills didn't load on the next launch")
	}
	if next := modelFor(t, Options{Fills: FillOn}); next.themes.fills != FillOn || next.options.Fills != FillOn {
		t.Fatal("--fills didn't override the saved choice")
	}
	if raw, _ := os.ReadFile(pref); string(raw) != "off\n" {
		t.Fatal("a launch override changed the saved choice")
	}
}

func TestFillsAutoAndInvalidPreference(t *testing.T) {
	home := freshThemeHome(t, "")
	t.Setenv("TERM_PROGRAM", "rex")
	m := modelFor(t, Options{ReducedMotion: true})
	darkThemeTerminal(m)
	if m.themes.fills != FillAuto || m.options.Fills != FillOff || !strings.HasSuffix(ansi.Strip(m.themeFillsRow(80)), "· off in Rex") {
		t.Fatalf("auto in Rex: %q %q", m.options.Fills, ansi.Strip(m.themeFillsRow(80)))
	}
	if err := os.MkdirAll(filepath.Join(home, ".hush"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeThemeFixture(t, filepath.Join(home, ".hush"), "fills", "on\n")
	if m := modelFor(t, Options{}); m.options.Fills != FillOn {
		t.Fatal("a saved on didn't apply in Rex")
	}
	writeThemeFixture(t, filepath.Join(home, ".hush"), "fills", "sometimes")
	m = modelFor(t, Options{})
	if m.themes.fills != FillAuto || m.fillsProblem != "~/.hush/fills: expected auto, on or off" {
		t.Fatalf("invalid preference: %q %q", m.themes.fills, m.fillsProblem)
	}
}

func TestThemeSlashKeepsExistingPrefixes(t *testing.T) {
	freshThemeHome(t, "")
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	for prefix, want := range map[string]string{"/t": "thinking", "/th": "thinking", "/the": "theme"} {
		m.editor.set(prefix)
		if matches := m.slashMatches(); len(matches) == 0 || matches[0].action.Slash.Name != want {
			t.Errorf("Enter on %s should run /%s", prefix, want)
		}
	}
}

func TestInvalidThemePreservesLastUsablePalette(t *testing.T) {
	store, dir := themeFixture(t)
	m, _ := demo(t, Options{ThemeName: "violet", Fills: FillOff, ReducedMotion: true})
	darkThemeTerminal(m)
	start := m.themes.active
	d := openTestThemes(t, m)
	chooseThemeForTest(t, m, d, "ink")
	writeThemeFixture(t, dir, "ink.json", `{"version":2,"base":"terminal","colors":{}}`)
	cmd := press(m, tea.KeyEnter, 0)
	m.Update(cmd())
	if m.focusedDialog() != d || m.themes.active != start || !strings.Contains(d.themePicker.problem, "version 1") {
		t.Fatalf("late invalid edit replaced the last usable palette: %+v", d.themePicker)
	}
	if _, err := os.Stat(filepath.Join(store.home, ".hush/theme")); !os.IsNotExist(err) {
		t.Fatalf("saved an invalid choice: %v", err)
	}
	press(m, tea.KeyEscape, 0)
	writeThemeFixture(t, dir, "ink.json", inkThemeJSON)
	writeThemeFixture(t, dir, "violet.json", "not JSON")
	d = openTestThemes(t, m)
	if m.themes.active != start || !strings.Contains(d.themePicker.problem, "violet.json") {
		t.Fatal("reload discarded the last usable theme")
	}
	press(m, tea.KeyEscape, 0)
	invalid, _ := demo(t, Options{ThemeName: "violet"})
	if invalid.themes.active.name != terminalTheme || !strings.Contains(invalid.themeProblem, "violet.json") {
		t.Fatal("invalid explicit launch theme neither reported an error nor fell back")
	}
	last, err := store.load("ink")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.save(t.Context(), last); err != nil {
		t.Fatal(err)
	}
	invalid, _ = demo(t, Options{ThemeName: "violet"})
	if invalid.themes.active != last || !strings.Contains(invalid.themeProblem, "violet.json") {
		t.Fatal("invalid explicit launch theme replaced a valid saved choice")
	}
}

func TestThemeTemplateCopiesWithoutSaving(t *testing.T) {
	store, _ := themeFixture(t)
	var clipboard string
	m, _ := demo(t, Options{ThemeName: "ink", CopyText: func(_ context.Context, text string) error {
		clipboard = text
		return nil
	}})
	d := openTestThemes(t, m)
	chooseThemeForTest(t, m, d, themeTemplateChoice)
	finishThemeWork(t, m, press(m, tea.KeyEnter, 0))
	if clipboard != ThemeTemplate() || m.themes.active.name != "ink" || m.focusedDialog() != nil {
		t.Fatal("template did not copy or changed the selected theme")
	}
	if _, err := os.Stat(filepath.Join(store.home, ".hush/theme")); !os.IsNotExist(err) {
		t.Fatalf("copying template wrote the selection: %v", err)
	}
}

func TestThemeCapabilityAndContrastFallbacks(t *testing.T) {
	themeFixture(t)
	for _, tc := range []struct {
		name, selected, term, noColor string
		bg                            color.Color
		profile                       colorprofile.Profile
		fills                         FillMode
		wantRGB, wantFill             bool
	}{
		{"dark", "violet", "", "", color.Black, colorprofile.TrueColor, FillAuto, true, true},
		{"light", "ink", "", "", color.White, colorprofile.TrueColor, FillAuto, true, true},
		{"poor contrast", "violet", "", "", color.White, colorprofile.TrueColor, FillAuto, false, true},
		{"unknown background", "violet", "", "", nil, colorprofile.TrueColor, FillAuto, false, false},
		{"no truecolor", "violet", "", "", color.Black, colorprofile.ANSI256, FillAuto, false, false},
		{"NO_COLOR", "violet", "", "1", color.Black, colorprofile.TrueColor, FillAuto, false, false},
		{"Rex", "violet", "rex", "", color.Black, colorprofile.TrueColor, FillAuto, true, false},
		{"fills off", "violet", "", "", color.Black, colorprofile.TrueColor, FillOff, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM_PROGRAM", tc.term)
			t.Setenv("NO_COLOR", tc.noColor)
			m, _ := demo(t, Options{ThemeName: tc.selected, Fills: tc.fills, ReducedMotion: true})
			m.Update(tea.ColorProfileMsg{Profile: tc.profile})
			if tc.bg != nil {
				m.Update(tea.BackgroundColorMsg{Color: tc.bg})
			}
			body := assertFrame(t, m)
			if m.themes.active.name != tc.selected || strings.Contains(m.paint(x.Accent, "text"), "\x1b[38;2;") != tc.wantRGB ||
				m.theme.Fills != tc.wantFill || !tc.wantFill && strings.Contains(body, "\x1b[48;") {
				t.Fatalf("unexpected color policy: selected=%q theme=%+v accent=%q", m.themes.active.name, m.theme, m.paint(x.Accent, "text"))
			}
			if tc.wantRGB {
				if contrastRatio(m.palette.swatch(roleAccent), tc.bg) < 4.5 {
					t.Fatal("custom ink did not meet minimum contrast")
				}
				m.Update(tea.ColorProfileMsg{Profile: colorprofile.ANSI256})
				if m.themes.active.name != tc.selected || m.palette.roles[roleAccent].custom || m.theme.Fills {
					t.Fatal("capability downgrade kept custom ink or lost selection")
				}
			}
		})
	}
}

func testThemeFrameGeometry(t *testing.T) {
	themeFixture(t)
	for _, size := range [][2]int{{12, 3}, {20, 5}, {30, 12}, {96, 24}} {
		m, _ := demo(t, Options{Fills: FillOn})
		darkThemeTerminal(m)
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		d := openTestThemes(t, m)
		chooseThemeForTest(t, m, d, "violet")
		assertFrame(t, m)
		press(m, tea.KeyEscape, 0)
		assertFrame(t, m)
	}
}

func TestGoldenThemePicker(t *testing.T) {
	themeFixture(t)
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	darkThemeTerminal(m)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d := openTestThemes(t, m)
	chooseThemeForTest(t, m, d, "violet")
	golden(t, "menu-themes", m)
}
