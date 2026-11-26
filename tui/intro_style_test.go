package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenIntroWithDock(t *testing.T) {
	m, _ := introFixture(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Keep this draft")
	assertFrame(t, m)
	rule := m.frame.ruleY
	budget := maxEffects
	m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Choose a workspace",
		Choices: []x.Choice{{ID: "1", Label: "Hush", Description: "/work/hush"}}}, &budget)
	golden(t, "intro-dock", m)
	if m.frame.ruleY != rule || m.frame.dialogY+m.frame.dialogHeight != rule {
		t.Fatal("panel did not dock below the intro without moving the composer")
	}
}
