package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestPaletteShowsAndRunsAvailableScrollKeys(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	assertFrame(t, m)
	has := func(d *dialog, id string) bool {
		return slices.ContainsFunc(d.paletteItems, func(p paletteItem) bool { return p.ID == "shell:"+id })
	}
	b := maxEffects
	open := func() *dialog {
		t.Helper()
		m.openPalette(m.route(m.active, coreID), &b)
		return m.focusedDialog()
	}
	d := open()
	if has(d, "up") || has(d, "follow") || has(d, "stop") || !has(d, "newline") {
		t.Fatal("no-op scroll or stop bindings shown in a short idle transcript")
	}
	m.closeDialog(d, nil, false)
	short := make([]sdk.Entry, 80)
	for i := range short {
		short[i] = entry(i+3, "assistant", "entry")
	}
	s.merge(short)
	assertFrame(t, m)
	d = open()
	if !has(d, "up") || !has(d, "top") || has(d, "down") || has(d, "follow") {
		t.Fatal("tail should offer upward navigation only")
	}
	m.closeDialog(d, nil, false)
	press(m, tea.KeyHome, tea.ModCtrl)
	assertFrame(t, m)
	d = open()
	if has(d, "up") || !has(d, "down") || !has(d, "follow") {
		t.Fatal("paused transcript did not offer scrolling down and resuming")
	}
	d.query = "ctrl+end"
	d.filter()
	if len(d.filtered) != 1 {
		t.Fatal("Ctrl+End is not searchable")
	}
	m.acceptChoice(d)
	if !s.following || m.focusedDialog() != nil {
		t.Fatal("choosing Resume did not follow the transcript")
	}
}

func TestPaletteStopConfirmsBeforeClosingSuggestions(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	s.info.Status = x.Running
	m.editor.set("/")
	b := maxEffects
	m.openPalette(m.route(m.active, coreID), &b)
	d := m.focusedDialog()
	d.query = "stop run"
	d.filter()
	assertFrame(t, m)
	m.acceptChoice(d)
	if m.stopAt.IsZero() || s.aborting || len(m.slashMatches()) == 0 {
		t.Fatal("palette did not arm just the first stop press with suggestions still open")
	}
	press(m, tea.KeyEscape, 0)
	if !s.aborting || !m.stopAt.IsZero() {
		t.Fatal("the advertised confirming Escape was intercepted by suggestions")
	}
}

func TestPaletteGoldens(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, InterceptedKeys: []string{"ctrl+t", "ctrl+n", "ctrl+g"}})
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 29})
	m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	d := m.focusedDialog()
	if d == nil || !d.palette {
		t.Fatal("palette not focused")
	}
	golden(t, "palette-wide", m)
	if m.View().Cursor == nil {
		t.Fatal("palette search has no cursor")
	}
	y := m.frame.dialogY
	if c, ok := d.choice(); !ok || c.ID != string(switcherID) {
		t.Fatal("the first action must not close the current tab")
	}
	for _, r := range "/copy" {
		m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	if c, ok := d.choice(); !ok || c.ID != coreID+".copy" {
		t.Fatalf("slash alias didn't resolve to Copy last answer: %+v", c)
	}
	golden(t, "palette-filter", m)
	if m.frame.dialogY != y {
		t.Fatal("typing moved the palette title")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	d.query = ""
	d.filter()
	for range len(d.filtered) - 1 {
		press(m, tea.KeyDown, 0)
	}
	golden(t, "palette-narrow-end", m)
	f := m.frame
	var ok bool
	for _, choice := range f.dialogChoices {
		if choice == d.selected {
			ok = true
			break
		}
	}
	if !ok {
		t.Fatal("keyboard selection has no row in a short window")
	}
}

func TestPaletteClicksMapOnlyToActionRows(t *testing.T) {
	var runs int
	actionID := x.ActionID("fixture/palette.take")
	m, _ := demo(t, Options{Fills: FillOff, Extensions: []x.Registration{{
		ID: "fixture/palette", APIVersion: x.APIVersion, Actions: []x.Action{{
			ID: actionID, Title: "Take fixture", Binding: x.Binding{Leader: "k g"},
			Run: func(x.Context, x.Invocation) x.Outcome { runs++; return nil },
		}},
	}}})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	b := maxEffects
	m.openPalette(m.route(m.active, coreID), &b)
	d := m.focusedDialog()
	assertFrame(t, m)
	f := m.frame
	for row, choice := range f.dialogChoices {
		if choice >= 0 {
			continue
		}
		m.mouse(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 3, Y: f.dialogY + row, Button: tea.MouseLeft}))
		if d != m.focusedDialog() || runs != 0 {
			t.Fatal("click on palette chrome activated an action")
		}
	}
	d.query = "take fixture"
	d.filter()
	assertFrame(t, m)
	for row, choice := range m.frame.dialogChoices {
		if choice < 0 {
			continue
		}
		m.mouse(tea.MouseClickMsg(tea.Mouse{X: m.frame.dialogX + 3, Y: m.frame.dialogY + row, Button: tea.MouseLeft}))
		break
	}
	if runs != 1 || m.focusedDialog() != nil {
		t.Fatalf("expected mouse activation and close, got %d runs", runs)
	}
}

func TestPaletteShortcutColumn(t *testing.T) {
	const w = 50
	for _, v := range []struct{ label, key string }{{"Short", "ctrl+x s"}, {"A much longer title", "alt+shift+f"}, {"宽字符 with a long title", "ctrl+g"}} {
		line := ansi.Strip((&Model{}).paletteActionRow(x.Choice{Label: v.label, Description: v.key}, true, w))
		if !strings.HasSuffix(line, v.key) || stringWidth(line) != w {
			t.Fatalf("shortcut does not align: %q", line)
		}
	}
}
