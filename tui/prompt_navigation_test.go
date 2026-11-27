package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestPromptArrowsMoveBetweenDraftLines(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	s.history = []promptDraft{{text: "previous prompt"}}
	m.Update(tea.PasteMsg{Content: "first\n\nfina"})
	m.Update(tea.KeyPressMsg{Code: 'l', Text: "l"})

	for _, step := range []struct {
		key  rune
		line int
	}{
		{tea.KeyUp, 1}, {tea.KeyDown, 2}, {tea.KeyUp, 1}, {tea.KeyUp, 0},
	} {
		press(m, step.key, 0)
		if m.editor.area.Line() != step.line || m.editor.value() != "first\n\nfinal" {
			t.Fatalf("after %v: line %d, draft %q; want line %d of the original draft", tea.KeyPressMsg{Code: step.key}, m.editor.area.Line(), m.editor.value(), step.line)
		}
	}
	m.Update(tea.KeyPressMsg{Code: '!', Text: "!"})
	if got := m.editor.value(); got != "first!\n\nfinal" {
		t.Fatalf("editing the first line produced %q", got)
	}
}

func TestPromptArrowsMoveBetweenWrappedRowsBeforeHistory(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	const older, latest = "older prompt", "latest\nline"
	s.history = []promptDraft{{text: older}, {text: latest}}
	draft := strings.Repeat("x", m.editor.area.Width()) + "tail"
	m.Update(tea.PasteMsg{Content: draft})

	for i, step := range []struct {
		key    rune
		draft  string
		cursor int
	}{
		{tea.KeyUp, draft, 4},
		{tea.KeyDown, draft, len(draft)},
		{tea.KeyUp, draft, 4},
		{tea.KeyUp, latest, len(latest)}, // top of the wrapped draft
		{tea.KeyUp, latest, 4},           // edit a recalled multiline prompt
		{tea.KeyUp, older, len(older)},
		{tea.KeyDown, latest, len(latest)},
		{tea.KeyDown, draft, len(draft)}, // bottom of history restores the draft
	} {
		press(m, step.key, 0)
		if got, at := m.editor.value(), m.editor.cursorByte(); got != step.draft || at != step.cursor {
			t.Fatalf("step %d: draft %q at %d, want %q at %d", i+1, got, at, step.draft, step.cursor)
		}
	}
}
