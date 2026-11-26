package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenPlanRunSummaryAndProgress(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	planPickerFixture(m, s)
	press(m, tea.KeyEscape, 0)
	old := entry(3, "assistant", "Plan run · A legacy plan · r6\nSelected: First, revised, then second")
	old.Kind = "hush.plan.chat"
	latest := entry(4, "assistant", "Plan run · r7 · Workspace profiles · 2 steps")
	latest.Kind = "hush.plan.run"
	s.merge([]sdk.Entry{old, latest})
	golden(t, "plan-summary", m)
	text := ansi.Strip(m.View().Content)
	if strings.Contains(text, "Selected:") || strings.Contains(text, "First, revised") || !strings.Contains(text, "Workspace profiles · 2 steps") {
		t.Fatalf("plan transcript should be compact:\n%s", text)
	}
	s.plan.snapshot.Run = &sdk.PlanRun{ID: "42", StepIDs: []string{"save", "restore"}, State: "running"}
	s.plan.snapshot.Progress = []sdk.PlanProgress{{StepID: "save", State: "verified"}, {StepID: "restore", State: "running"}}
	s.tools = []x.Tool{{CallID: "1", Name: "bash", Live: true}}
	s.activity.intent = "Checking profiles"
	assertFrame(t, m)
	rule := m.frame.ruleY
	progress := ansi.Strip(m.frame.rows[rule-1])
	if !strings.Contains(progress, "✓") || !strings.Contains(progress, "Restore saved profiles") ||
		!strings.Contains(ansi.Strip(m.frame.rows[rule]), "Checking profiles") {
		t.Fatalf("plan progress should not replace the live rule: %q, %q", progress, m.frame.rows[rule])
	}
}
