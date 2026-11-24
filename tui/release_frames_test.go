package tui

import (
	"context"
	"errors"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// These are production-model frames with deterministic terminal colors and
// inputs, not proof of a particular terminal emulator or an installed PTY run.
func TestReleaseFrames(t *testing.T) {
	for _, theme := range []struct {
		name   string
		bg, fg color.NRGBA
	}{
		{"latte", color.NRGBA{R: 239, G: 241, B: 245, A: 255}, color.NRGBA{R: 76, G: 79, B: 105, A: 255}},
		{"mocha", color.NRGBA{R: 30, G: 30, B: 46, A: 255}, color.NRGBA{R: 205, G: 214, B: 244, A: 255}},
	} {
		t.Run(theme.name, func(t *testing.T) {
			var clipboard string
			m, s := demo(t, Options{Fills: FillOn, ReducedMotion: true,
				CopyText: func(_ context.Context, text string) error { clipboard = text; return nil },
				Services: MenuServices{Rename: func(context.Context, x.SetTitle) error { return nil }},
			})
			m.Update(tea.WindowSizeMsg{Width: 100, Height: 22})
			m.Update(tea.BackgroundColorMsg{Color: theme.bg})
			m.Update(tea.ForegroundColorMsg{Color: theme.fg})
			m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			shot := func(name string) string {
				t.Helper()
				frame := assertFrame(t, m)
				if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
					path := filepath.Join(dir, theme.name+"-"+name+".ansi")
					if err := os.WriteFile(path, []byte(frame+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return ansi.Strip(frame)
			}
			m.editor.set("Keep this draft")
			s.queue = []sdk.QueueItem{{SubmissionID: "7", Mode: string(sdk.FollowUp), Text: "Queued fixture"}}
			s.info.Status = x.Running
			if frame := shot("alt-up-before"); !strings.Contains(frame, "follow-up queued · Queued fixture") {
				t.Fatal("Alt+Up fixture has no queued preview")
			}
			press(m, tea.KeyUp, tea.ModAlt)
			shot("alt-up-after")
			// Capture the actual action, not a mocked success. Until the host
			// supports dequeue, the warning is an open release-review item.
			t.Logf("Alt+Up: notice=%q editor=%q", m.notice.text, m.editor.value())
			s.queue = nil
			s.info.Status = x.Idle
			m.notice = noticeState{}

			assertFrame(t, m)
			for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
				plain := ansi.Strip(row)
				if offset := strings.Index(plain, "What changed"); offset >= 0 {
					col := ansi.StringWidth(plain[:offset])
					m.Update(tea.MouseClickMsg(tea.Mouse{X: col, Y: y, Button: tea.MouseLeft}))
					m.Update(tea.MouseMotionMsg(tea.Mouse{X: col + 3, Y: y, Button: tea.MouseLeft}))
					shot("selection")
					_, cmd := m.Update(tea.MouseReleaseMsg(tea.Mouse{X: col + 3, Y: y, Button: tea.MouseLeft}))
					workflowWork(t, m, cmd)
					break
				}
			}
			if frame := shot("copied"); clipboard != "What" || !strings.Contains(frame, "Copied") {
				t.Fatalf("clipboard=%q, frame=%s", clipboard, frame)
			}
			m.notice = noticeState{}

			m.runAction(m.host.byID[paletteID], "")
			d := m.focusedDialog()
			d.query = "rename"
			d.filter()
			m.acceptChoice(d)
			if len(m.overlays) != 2 {
				t.Fatal("no nested rename")
			}
			shot("nested-escape-before")
			press(m, tea.KeyEscape, 0)
			if len(m.overlays) != 1 || !m.overlays[0].palette || !m.stopAt.IsZero() {
				t.Fatal("Esc did not return to the palette only")
			}
			shot("nested-escape-after")
			press(m, tea.KeyEscape, 0)
			s.observed = true
			m.Update(feedMsg{owner: "1", ok: true, update: sdk.StateUpdate{Err: errors.New("backend connection lost")}})
			if frame := shot("reconnecting"); !strings.Contains(frame, "reconnecting") || !strings.Contains(frame, "Keep this draft") {
				t.Fatal("reconnect row erased the prompt")
			}
		})
	}
}
