package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenPickerEmptyAndErrorStates(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Keep the draft")
	budget := maxEffects
	m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Choose a model",
		Choices: []x.Choice{{ID: "fixture", Label: "Fixture model"}}}, &budget)
	d := m.focusedDialog()
	d.query = "zzzz"
	d.filter()
	golden(t, "menu-empty", m)
	assertNoTarget := func() {
		t.Helper()
		for _, target := range m.frame.dialogChoices {
			if target >= 0 {
				t.Fatal("an empty panel exposes an acceptance target")
			}
		}
	}
	assertNoTarget()
	d.modelSearch = &modelSearchState{failed: true}
	golden(t, "menu-search-error", m)
	assertNoTarget()
	if !strings.Contains(ansi.Strip(m.View().Content), "edit the filter to retry") {
		t.Fatal("search failure is missing the next step")
	}
	press(m, tea.KeyEnter, 0)
	if m.focusedDialog() != d || m.editor.value() != "Keep the draft" {
		t.Fatal("Enter changed state in an empty panel")
	}
}
