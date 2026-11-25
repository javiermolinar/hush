package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func codeCopyPoint(t *testing.T, m *Model, id string, onButton bool) tea.Mouse {
	t.Helper()
	assertFrame(t, m)
	for y, row := range m.frame.rows[:m.frame.transcriptHeight] {
		prefix, header, ok := strings.Cut(row, codeCopyLink(id))
		if !ok {
			continue
		}
		col := stringWidth(prefix)
		if onButton {
			title, _, ok := strings.Cut(header, codeCopyAction)
			if !ok {
				t.Fatal("header has no copy button")
			}
			col += stringWidth(title) + 1
		}
		return tea.Mouse{X: col, Y: y, Button: tea.MouseLeft}
	}
	t.Fatalf("copy header %q not visible", id)
	return tea.Mouse{}
}

func clickCodeCopy(t *testing.T, m *Model, p tea.Mouse) tea.Cmd {
	t.Helper()
	m.Update(tea.MouseClickMsg(p))
	assertFrame(t, m) // Bubble Tea renders between press and release.
	_, cmd := m.Update(tea.MouseReleaseMsg(p))
	if cmd == nil {
		t.Fatal("code copy did not schedule a clipboard write")
	}
	return cmd
}

func TestCodeCopyClick(t *testing.T) {
	for _, name := range []string{"transcript-button", "live-rule"} {
		t.Run(name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true})
			m.Update(tea.WindowSizeMsg{Width: 44, Height: 25})
			want := "echo\t" + strings.Repeat("code ", 40) + "done\n  echo 'complete'"
			lead := "```go\nfirst()\n```\n\n"
			var id sdk.ID
			onButton := name == "transcript-button"
			if onButton {
				id = "3"
				s.merge([]sdk.Entry{entry(3, "assistant", lead+"> ```sh\n> "+strings.ReplaceAll(want, "\n", "\n> ")+"\n> ```")})
			} else {
				s.stream = detachStream(&sdk.Streaming{Blocks: []sdk.Block{{Type: "text", Text: lead + "```sh\n" + want}}})
				s.streamRevision++
			}
			p := codeCopyPoint(t, m, codeID(renderKey{entry: id}, 1), onButton)
			before := m.frame.rows[p.Y]
			cmd := clickCodeCopy(t, m, p)
			cmds, ok := cmd().(tea.BatchMsg)
			if !ok || len(cmds) != 2 || cmds[0]() != tea.SetClipboard(want)() {
				t.Fatal("reduced-motion copy must send the original code to the terminal, with only a label expiration timer")
			}
			assertFrame(t, m)
			row := m.frame.rows[p.Y]
			if !strings.Contains(row, paint(x.Good, "✓ copied")) || strings.Contains(row, "\x1b[32m─") ||
				stringWidth(row) != stringWidth(before) || m.notice.text != "" {
				t.Fatalf("expected stationary inline confirmation only, got %q (notice %q)", row, m.notice.text)
			}
			m.Update(copyLabelExpired{m.copied.token})
			assertFrame(t, m)
			if m.frame.rows[p.Y] != before {
				t.Fatal("copy button did not return after the confirmation expired")
			}
		})
	}
}

func TestCodeCopySweep(t *testing.T) {
	m, _ := demo(t, Options{})
	p := codeCopyPoint(t, m, "2/0/0", true)
	clickCodeCopy(t, m, p)
	assertFrame(t, m)
	token := m.copied.token
	row := m.frame.rows[p.Y]
	plain := ansi.Strip(row)
	for i := 0; i < copySweepFrames; i++ {
		if !strings.Contains(row, paint(x.Good, "✓ copied")) || !strings.Contains(row, "\x1b[32m─") {
			t.Fatalf("sweep frame %d has no green band and inline label: %q", i, row)
		}
		_, next := m.Update(copySweepTick{token})
		if (next != nil) != (i+1 < copySweepFrames) {
			t.Fatalf("wrong sweep timer state after frame %d", i)
		}
		assertFrame(t, m)
		newRow := m.frame.rows[p.Y]
		if newRow == row || ansi.Strip(newRow) != plain {
			t.Fatalf("sweep must change the ink without moving the header: %q -> %q", row, newRow)
		}
		row = newRow
	}
	if strings.Contains(row, "\x1b[32m─") || !strings.Contains(row, paint(x.Good, "✓ copied")) {
		t.Fatalf("label should stay after the band stops: %q", row)
	}
}

func TestCodeCopyHookFailure(t *testing.T) {
	m, _ := demo(t, Options{CopyText: func(context.Context, string) error { return errors.New("clipboard unavailable") }})
	p := codeCopyPoint(t, m, "2/0/0", true)
	cmd := clickCodeCopy(t, m, p)
	if strings.Contains(assertFrame(t, m), "✓ copied") {
		t.Fatal("showed success before the clipboard write")
	}
	m.Update(cmd())
	if strings.Contains(assertFrame(t, m), "✓ copied") || m.notice.text != "clipboard unavailable" || m.notice.tone != x.Danger {
		t.Fatal("clipboard failure must show an error, not a successful copy")
	}
}
