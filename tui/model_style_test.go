package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenModelAvailabilityDetail(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	choices, models := modelPickerChoices([]x.CatalogModel{
		{Model: sdk.Model{Provider: "fixture", ModelID: "reasoning-v1"}, Label: "Reasoning", Current: true, ThinkingLevels: []string{"off", "high"}},
		{Model: sdk.Model{Provider: "fixture", ModelID: "writer-v2"}, Label: "Writer", Unavailable: "Sign in to this provider before using Writer"},
	})
	d := &dialog{route: m.route(m.active, menusID), token: m.token(), choices: choices,
		modelSearch: &modelSearchState{thread: m.active, models: models}}
	d.filter()
	d.move(1)
	m.pushDialog(d)
	golden(t, "menu-model-unavailable", m)
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "⏸ Writer") || !strings.Contains(frame, "Sign in to this provider") {
		t.Fatalf("missing model name or availability details:\n%s", frame)
	}
}
