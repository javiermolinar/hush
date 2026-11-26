package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

func TestGoldenQuietReasoning(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	e := entry(3, "assistant", "Use the **documented** setup.")
	e.Messages[0].Blocks = append([]sdk.Block{{Type: "thinking", Text: "## Reasoning heading\n\n**Quiet words** and [the guide](https://example.org)."}}, e.Messages[0].Blocks...)
	s.merge([]sdk.Entry{e})
	if frame := ansi.Strip(assertFrame(t, m)); strings.Contains(frame, "Quiet words") {
		t.Fatal("reasoning must be folded by default")
	}
	press(m, 'o', tea.ModCtrl)
	golden(t, "transcript-quiet-reasoning", m)
	for _, row := range m.frame.rows {
		plain := ansi.Strip(row)
		if strings.Contains(plain, "Reasoning heading") || strings.Contains(plain, "Quiet words") {
			if strings.Contains(row, "\x1b[1m") || strings.Contains(row, "\x1b[3m") {
				t.Fatalf("reasoning inherited emphasis: %q", row)
			}
		}
	}
}
