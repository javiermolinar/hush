package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestDockDetailClickDoesNotAcceptListRow(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	accepted := false
	budget := maxEffects
	m.openDialog(m.route(m.active, coreID), x.SelectDialog{Title: "Choose a file",
		Choices: []x.Choice{{ID: "1", Label: "Fixture file", Description: "Details with no action"}},
		Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
			accepted = reply.Accepted
			return nil
		}}, &budget)
	assertFrame(t, m)
	f := m.frame
	if f.dialogListWidth == 0 {
		t.Fatal("fixture did not draw split detail")
	}
	for i, target := range f.dialogChoices {
		if target != 0 {
			continue
		}
		y := f.dialogY + i
		if !strings.Contains(ansi.Strip(f.rows[y]), "│ Fixture file") {
			t.Fatal("fixture detail is not adjacent to the list row")
		}
		m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: f.dialogX + f.dialogListWidth + 4, Y: y}))
		if accepted || m.focusedDialog() == nil {
			t.Fatal("click on detail invoked the adjacent list action")
		}
		m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: f.dialogX + 3, Y: y}))
		if !accepted {
			t.Fatal("list action has no mouse path")
		}
		return
	}
	t.Fatal("no rendered choice")
}
