package tui

import (
	"context"
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
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

func TestPaletteShowsHistoryKeysAtDraftEdges(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	s.history = []promptDraft{{text: "older prompt"}, {text: "first line\nsecond line"}}
	has := func(id string) bool {
		return slices.ContainsFunc(m.shellPaletteItems(s), func(p paletteItem) bool { return p.ID == "shell:"+id })
	}
	press(m, tea.KeyUp, 0) // recall the last prompt; its cursor starts at the bottom
	if has("older") || !has("newer") {
		t.Fatal("only the newer history key should be shown at the bottom of a multiline draft")
	}
	press(m, tea.KeyUp, 0) // move to its first row
	if !has("older") || has("newer") {
		t.Fatal("only the older history key should be shown at the top of a multiline draft")
	}
}

func TestPaletteCompleteDoesNotSwitchThreads(t *testing.T) {
	const draft = "Read @ma"
	for _, tc := range []struct {
		name   string
		revoke bool
		want   string
	}{
		{name: "available", want: "Read @main.go "},
		{name: "revoked", revoke: true, want: draft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := demo(t, Options{ReducedMotion: true})
			openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true},
				list: func(context.Context, sdk.WorkspaceFileTarget, string, string, int, string) (sdk.WorkspaceFilePage, error) {
					return sdk.WorkspaceFilePage{Entries: []sdk.WorkspaceFileEntry{{Name: "main.go"}}}, nil
				}})
			m.editor.set(draft)
			pathCompletionPage(t, m, m.refreshFilePicker())
			m.ensureThread("2").draft.text = "second draft"
			m.openedTab("2")
			press(m, 'x', tea.ModCtrl)
			press(m, 'p', 0)
			d := m.focusedDialog()
			if d == nil || !d.palette {
				t.Fatal("command palette did not open")
			}
			d.query = "complete current command or file"
			d.filter()
			if choice, ok := d.choice(); !ok || choice.ID != "shell:complete" {
				t.Fatal("palette did not offer file completion")
			}
			if tc.revoke {
				m.Update(fileScopeMsg{generation: m.fileScope.generation})
				if _, _, open := m.fileSuggestions(); open || m.focusedDialog() != d || m.editor.revision != d.promptRevision {
					t.Fatal("scope revocation did not leave a stale palette item for the unchanged draft")
				}
			}
			press(m, tea.KeyEnter, 0)
			if m.active != "1" || m.editor.value() != tc.want || m.focusedDialog() != nil {
				t.Fatalf("completion left thread %q with draft %q or kept the palette open", m.active, m.editor.value())
			}
			if tc.revoke {
				press(m, tea.KeyTab, 0)
				if m.active != "2" || m.editor.value() != "second draft" {
					t.Fatal("keyboard Tab no longer switches threads without completion")
				}
			}
		})
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
	rule := m.frame.ruleY
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
	if m.frame.dialogY+m.frame.dialogHeight != rule || m.frame.ruleY != rule {
		t.Fatal("filter moved the input rule or undocked the palette")
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

func TestPaletteTypingFRemainsAFilter(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	budget := maxEffects
	m.openPalette(m.route(m.active, coreID), &budget)
	assertFrame(t, m)
	m.Update(tea.KeyPressMsg{Code: 'f', Text: "f"})
	if d := m.focusedDialog(); d.query != "f" || d.full {
		t.Fatal("zoom intercepted the existing type-to-filter shortcut")
	}
}
