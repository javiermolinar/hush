package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenLiveRulePrioritizesStateModelAndChanges(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	enableGitDiffFixture(m, s)
	s.info.Model, s.info.Thinking = sdk.Model{Provider: "fixture", ModelID: "test-model"}, "high"
	m.now = time.Unix(200, 0)
	s.tools = []x.Tool{{CallID: "1", Name: "edit", Live: true}}
	s.activity.intent = "Editing go.mod"
	s.activity.tools = map[string]time.Time{"1": m.now.Add(-12 * time.Second)}
	golden(t, "live-rule", m)
	rule := strings.TrimSpace(ansi.Strip(m.frame.rows[m.frame.ruleY]))
	if !strings.Contains(rule, "Editing go.mod · 12s") || !strings.Contains(rule, "test-model high") || !strings.HasSuffix(rule, s.gitDiff.badge) {
		t.Fatalf("live state, model, and changes should share the rule: %q", rule)
	}
}
