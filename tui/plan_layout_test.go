package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestPlanStepNavigationKeepsPanelFixed(t *testing.T) {
	for _, width := range []int{64, 132} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := modelFor(t, Options{ThreadID: "1", Intro: &IntroOptions{}, ReducedMotion: true, Fills: FillOff})
			m.connection = x.Connected
			m.Update(tea.WindowSizeMsg{Width: width, Height: 38})
			s := m.threads[m.active]
			s.info.Model, s.info.Thinking = sdk.Model{Provider: "fixture", ModelID: "test-model"}, "high"
			m.editor.set("Keep the draft")
			d := planPickerFixture(m, s)
			d.plan.snapshot.Plan.Steps[1].Prompt = strings.Repeat("A long explanation that needs several lines. ", 40)
			assertFrame(t, m)
			top, rule := m.frame.dialogY, m.frame.ruleY
			chrome := slices.Clone(m.frame.rows[rule:])
			check := func(focus int, detail string) {
				t.Helper()
				assertFrame(t, m)
				f := m.frame
				if f.dialogY != top || f.ruleY != rule || !slices.Equal(chrome, f.rows[rule:]) {
					t.Fatalf("focus %d moved the plan or prompt: plan %d → %d, rule %d → %d", focus, top, f.dialogY, rule, f.ruleY)
				}
				if d.plan.focus != focus || !strings.Contains(ansi.Strip(strings.Join(f.rows[top:rule], "\n")), detail) {
					t.Fatalf("focus %d did not show %q", focus, detail)
				}
			}
			press(m, tea.KeyDown, 0)
			check(1, "A long explanation")
			press(m, tea.KeyUp, 0)
			check(0, "Store a profile")
			for i, button := range []tea.MouseButton{tea.MouseWheelDown, tea.MouseWheelUp} {
				m.Update(tea.MouseWheelMsg(tea.Mouse{Button: button, X: m.frame.dialogX + 2, Y: top + 1}))
				check(1-i, []string{"A long explanation", "Store a profile"}[i])
			}
		})
	}
}
