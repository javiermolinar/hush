package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestLeaderDockKeepsExistingKeyboardPath(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Keep the draft")
	assertFrame(t, m)
	rule := m.frame.ruleY
	press(m, 'x', tea.ModCtrl)
	assertFrame(t, m)
	var clicked bool
	for y, hits := range m.frame.hits[:rule] {
		for _, hit := range hits {
			if hit.action.ID == leaderStepID && hit.action.Target == "\ni" {
				m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: hit.first, Y: y}))
				clicked = true
				break
			}
		}
	}
	if !clicked || m.leader != "i" {
		t.Fatal("clicking the chord prefix did not advance the existing leader")
	}
	assertFrame(t, m)
	if m.frame.ruleY != rule || m.editor.value() != "Keep the draft" {
		t.Fatal("leader panel moved the rule or edited the draft")
	}
	press(m, 'u', 0)
	if d := m.focusedDialog(); m.leader != "" || d == nil || d.title != "Usage details" {
		t.Fatal("the existing ctrl+x i u path did not open usage")
	}
}
