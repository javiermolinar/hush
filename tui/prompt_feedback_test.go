package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type admitOnlyBackend struct{ inertBackend }

func (admitOnlyBackend) Submit(context.Context, sdk.Input) (sdk.Receipt, error) {
	return sdk.Receipt{SubmissionID: "42"}, nil
}

func TestPromptFeedbackBeforeFirstModelContent(t *testing.T) {
	m := modelFor(t, Options{ThreadID: "1"})
	m.backend = admitOnlyBackend{}
	m.connection = x.Connected
	m.editor.set("hello")
	viewHas := func(label string) {
		t.Helper()
		if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, label) {
			t.Fatalf("missing %q:\n%s", label, frame)
		}
	}
	op := m.sendPrompt(sdk.Steer)
	viewHas("Sending")
	m.finishWork(op().(workMsg))
	viewHas("Starting")

	s := sdk.State{Thread: sdk.Thread{ID: "1"}, Run: &sdk.Run{TaskID: "22", SubmissionIDs: []sdk.ID{"42"}},
		Streaming: &sdk.Streaming{Attempt: 1}}
	feed := func() {
		m.receiveFeed(feedMsg{owner: "1", ok: true, update: sdk.StateUpdate{Snapshot: sdk.Snapshot{State: s}}})
	}
	feed()
	viewHas("Waiting for model")
	if m.threads["1"].awaiting != "" {
		t.Fatal("run observation did not consume the admission bridge")
	}
	if m.syncClock() == nil {
		t.Fatal("initial model wait is not animated")
	}
	s.Streaming.Blocks = []sdk.Block{{Type: "omitted", Kind: "redactedThinking"}}
	feed()
	viewHas("Thinking")
	s.Streaming.Blocks = append(s.Streaming.Blocks, sdk.Block{Type: "text", Text: "answer begins"})
	feed()
	viewHas("answer begins")
	viewHas("Writing answer")
	frame := ansi.Strip(assertFrame(t, m))
	for _, stale := range []string{"Starting", "Waiting for model", "Thinking"} {
		if strings.Contains(frame, stale) {
			t.Fatalf("stale %q after visible answer", stale)
		}
	}
	if m.syncClock() != nil || m.clockPending != 0 {
		t.Fatal("indicator timer kept running after model started answering")
	}
}

func TestReasoningDurationStopsAtFirstAnswer(t *testing.T) {
	m, state := demo(t, Options{})
	s := sdk.State{Run: &sdk.Run{TaskID: "3"}, Streaming: &sdk.Streaming{Attempt: 1, Blocks: []sdk.Block{{Type: "thinking", Text: "reasoning"}}}}
	start := time.Unix(100, 0)
	m.applyActivity(state, s, start, nil)
	s.Streaming.Blocks = append(s.Streaming.Blocks, sdk.Block{Type: "text", Text: "first token"})
	m.applyActivity(state, s, start.Add(2*time.Second), nil)
	result := entry(4, "assistant", "final")
	result.Messages[0].Blocks = append([]sdk.Block{{Type: "thinking", Text: "reasoning"}}, result.Messages[0].Blocks...)
	s.Run, s.Streaming = nil, nil
	m.applyActivity(state, s, start.Add(7*time.Second), state.merge([]sdk.Entry{result}))
	if got := state.entries["4"].thought; got != 2*time.Second {
		t.Fatalf("thought duration %s includes response generation", got)
	}
}

func TestEditorCursor(t *testing.T) {
	for _, tt := range []struct {
		name          string
		reducedMotion bool
	}{{"animated", false}, {"reduced-motion", true}} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := demo(t, Options{ReducedMotion: tt.reducedMotion})
			check := func(where string) *tea.Cursor {
				t.Helper()
				c := m.View().Cursor
				if c == nil || c.Shape != tea.CursorBlock || c.Blink == tt.reducedMotion {
					t.Fatalf("%s cursor = %+v; want block cursor with blink %t", where, c, !tt.reducedMotion)
				}
				return c
			}
			m.editor.set("hello")
			before := check("prompt")
			press(m, tea.KeyLeft, 0)
			if after := check("prompt"); after.X != before.X-1 || after.Y != before.Y {
				t.Fatalf("cursor did not follow the editor: %v -> %v", before.Position, after.Position)
			}
			m.editor.set("")
			m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
			if c := check("palette"); c.Y >= m.frame.transcriptHeight {
				t.Fatalf("palette cursor is still in the prompt: %v", c.Position)
			}
			m.Update(tea.BlurMsg{})
			if c := m.View().Cursor; c != nil {
				t.Fatalf("cursor visible while unfocused: %+v", c)
			}
			m.Update(tea.FocusMsg{})
			check("refocused palette")
		})
	}
}

func TestSlashSheetKeepsSelectionInView(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 22})
	m.editor.set("/")
	matches := m.slashMatches()
	if len(matches) <= 5 {
		t.Fatal("fixture needs more than one page of commands")
	}
	frame := m.View()
	if frame.Cursor == nil {
		t.Fatal("missing prompt cursor")
	}
	body := ansi.Strip(frame.Content)
	if strings.Contains(body, "enter send") || !strings.Contains(body, "enter run") || strings.Contains(body, "\n  commands\n") {
		t.Fatalf("incorrect command chrome:\n%s", body)
	}
	for range len(matches) - 1 {
		press(m, tea.KeyDown, 0)
	}
	frame = m.View()
	last := matches[len(matches)-1].action
	if !strings.Contains(ansi.Strip(frame.Content), "› /"+last.Slash.Name) {
		t.Fatal("keyboard focus disappeared below the list")
	}
	press(m, tea.KeyTab, 0)
	if m.editor.value() != "/"+last.Slash.Name+" " {
		t.Fatal("Tab did not complete the highlighted command")
	}
}
