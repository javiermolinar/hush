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
	chooseThemeForTest(t, m, d, "gruvbox-dark")
	body := assertFrame(t, m)
	if m.themes.active.name != "gruvbox-dark" || !strings.Contains(body, "\x1b[38;2;250;189;47mAccent") ||
		!strings.Contains(ansi.Strip(body), "enter apply") {
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
	m.Update(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 3, Y: f.dialogY + row, Button: tea.MouseLeft}))
	if m.themes.active.name != "rose-pine" || d.themePicker.saving {
		t.Fatal("click must preview without applying")
	}
	assertNotSaved()
	cmd := press(m, tea.KeyEnter, 0)
	if cmd == nil {
		t.Fatal("Enter didn't schedule apply")
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
