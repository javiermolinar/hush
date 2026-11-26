package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func testCompactFrameGeometry(t *testing.T) {
	for _, size := range [][2]int{{30, 12}, {80, 8}, {20, 5}, {12, 3}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
			resize := func(w, h int) {
				m.Update(tea.WindowSizeMsg{Width: w, Height: h})
			}
			resize(size[0], size[1])
			token := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 4)
			url := "https://example.org/" + token
			wide := strings.Repeat("界", 46)
			result := "check this failure " + token + " " + wide
			s.merge([]sdk.Entry{
				entry(3, "user", "inspect "+token+" "+wide),
				entry(4, "assistant", "See ["+url+"](https://example.org/)\n\n"+wide),
				toolResultEntry(5, "compact", "bash", result, false, time.Unix(100, 0)),
			})
			if !s.toggleBurst("5/0") || !s.toggleBurst("5/0/0") {
				t.Fatal("missing expandable tool result")
			}
			assertFrame(t, m)
			if !m.frame.hasTranscript {
				t.Fatal("no scrollable transcript")
			}
			press(m, tea.KeyPgUp, 0)
			assertFrame(t, m)
			if s.following {
				t.Fatal("page up did not scroll compact transcript")
			}
			start := s.anchor
			m.Update(tea.MouseWheelMsg(tea.Mouse{X: 1, Y: 0, Button: tea.MouseWheelUp}))
			assertFrame(t, m)
			if s.anchor == start {
				t.Fatal("wheel did not move compact scroll anchor")
			}
			press(m, tea.KeyEnd, tea.ModCtrl)
			// Check every rendered part of the long entries, including the
			// rows too far above the viewport to fit in this particular frame.
			for _, tc := range []struct {
				id   sdk.ID
				want []string
			}{
				{"3", []string{token, wide}},
				{"4", []string{url, wide}},
				{"5", []string{token, wide}},
			} {
				w := m.pane().width
				var got strings.Builder
				rows := m.blockRows(s, s.blockIndex(tc.id), w)
				for _, row := range rows {
					if n := ansi.StringWidth(row.text); n > w {
						t.Fatalf("entry %s: row wider than content area (%d): %q", tc.id, w, row.text)
					}
					got.WriteString(strings.Join(strings.Fields(ansi.Strip(row.text)), ""))
				}
				for _, want := range tc.want {
					if !strings.Contains(got.String(), want) {
						t.Fatalf("entry %s clipped or changed %q: %q", tc.id, want, got.String())
					}
				}
			}

			draft := "Review " + token + " " + wide + " done"
			m.editor.set(draft)
			view := ansi.Strip(assertFrame(t, m))
			if !strings.Contains(view, "done") || m.View().Cursor == nil || !m.frame.hasTranscript {
				t.Fatalf("draft or transcript hidden:\n%s", view)
			}
			if size[1] < shortPaneHeight && strings.Contains(view, "Workspace workspace") {
				t.Fatal("optional status took space from the transcript")
			}
			first, second, chosen := "Option 1", "Option 2", ""
			b := maxEffects
			m.openDialog(m.route(m.active, coreID), x.SelectDialog{
				Title: "Choose", Placement: x.Sheet,
				Choices: []x.Choice{{ID: "1", Label: first}, {ID: "2", Label: second}},
				Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
					if reply.Accepted {
						chosen = reply.Value
					}
					return nil
				},
			}, &b)
			press(m, tea.KeyDown, 0)
			frame := ansi.Strip(assertFrame(t, m))
			f := m.frame
			row := f.dialogY
			for _, target := range f.dialogChoices {
				if target == 1 {
					break
				}
				row++
			}
			if row == f.dialogY+f.dialogHeight || !strings.Contains(frame, second) {
				t.Fatalf("compact dialog didn't draw selected choice:\n%s", frame)
			}
			m.Update(tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 2, Y: row, Button: tea.MouseLeft}))
			if chosen != "2" || m.sheet != nil || m.editor.value() != draft {
				t.Fatal("click did not select the drawn choice or changed the underlying draft")
			}
			m.openPalette(m.route(m.active, coreID), &b)
			assertFrame(t, m)
			if !m.dialogVisible(m.focusedDialog()) {
				t.Fatal("palette hidden in a usable pane")
			}
			press(m, tea.KeyEscape, 0)
			resize(80, 24)
			if got := ansi.Strip(assertFrame(t, m)); !strings.Contains(got, "you ›") ||
				!strings.Contains(got, "done") || !strings.Contains(got, "Workspace workspace") || m.editor.value() != draft {
				t.Fatalf("normal geometry or draft not restored:\n%s", got)
			}
		})
	}

	t.Run("nested-code", func(t *testing.T) {
		for _, tc := range []struct{ lang, code string }{
			{"go", `return "界é🐋"`},
			{"diff", "+added 界é🐋"},
			{"", "plain 界é🐋"},
		} {
			t.Run(tc.lang, func(t *testing.T) {
				m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
				source := "> > > > ```" + tc.lang + "\n> > > > " + tc.code + "\n> > > > ```"
				s.merge([]sdk.Entry{entry(3, "assistant", source)})
				for _, size := range [][2]int{{80, 24}, {12, 3}, {80, 24}} {
					m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
					assertFrame(t, m)
					w := m.pane().width
					rows := m.blockRows(s, s.blockIndex("3"), w)
					var got strings.Builder
					for _, row := range rows {
						if n := ansi.StringWidth(row.text); n > w {
							t.Fatalf("code row spans %d > %d cells: %q", n, w, row.text)
						}
						text := strings.NewReplacer("│", "", "↪", "", "─", "").Replace(ansi.Strip(row.text))
						got.WriteString(strings.Join(strings.Fields(text), ""))
					}
					if want := strings.ReplaceAll(tc.code, " ", ""); !strings.Contains(got.String(), want) {
						t.Fatalf("%v: code lost content: got %q, want %q", size, got.String(), want)
					}
				}
			})
		}
	})

	t.Run("minimum-and-undrawn-confirmation", func(t *testing.T) {
		m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
		for _, size := range [][2]int{{30, 2}, {11, 5}, {0, 0}} {
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			if got := assertFrame(t, m); size[0] > 0 && !strings.Contains(got, "too small") {
				t.Fatalf("missing size warning: %q", got)
			}
			if m.View().Cursor != nil || m.frame.hasTranscript {
				t.Fatal("showed content in an unusable pane")
			}
		}
		m.Update(tea.WindowSizeMsg{Width: 12, Height: 3})
		accepted := false
		b := maxEffects
		m.openDialog(m.route(m.active, coreID), x.ConfirmDialog{
			Message: "Delete entry?",
			Done: func(_ x.Context, reply x.Reply[bool]) x.Outcome {
				accepted = reply.Accepted
				return nil
			},
		}, &b)
		assertFrame(t, m)
		press(m, 'y', 0)
		press(m, tea.KeyEnter, 0)
		if accepted || m.focusedDialog() == nil {
			t.Fatal("accepted when the choice did not fit")
		}
		m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
		assertFrame(t, m)
		press(m, 'y', 0)
		if !accepted || m.focusedDialog() != nil {
			t.Fatal("could not accept a visible compact choice")
		}
	})

	t.Run("completion", func(t *testing.T) {
		m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOff})
		m.Update(tea.WindowSizeMsg{Width: 20, Height: 5})
		m.editor.set("/fo")
		assertFrame(t, m)
		press(m, tea.KeyTab, 0)
		if got := m.editor.value(); got != "/fork " {
			t.Fatalf("compact slash completion: %q", got)
		}
		openBrowseScope(t, m, fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true, Home: true}})
		m.editor.set("/")
		m.openPathRoots()
		if got := ansi.Strip(assertFrame(t, m)); !strings.Contains(got, "Workspace") {
			t.Fatalf("compact file picker didn't show its choice:\n%s", got)
		}
	})
}
