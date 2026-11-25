package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const pasteLabel29 = "[paste #1 +29 lines]"

func pasteFixture() string {
	return strings.Repeat("\tPasted 文 é 👩‍💻\n", 28) + "Last pasted line"
}

func TestPromptPasteThresholds(t *testing.T) {
	for _, test := range []struct{ name, paste, want string }{
		{"ten-lines", strings.Repeat("small\n", 9) + "end", strings.Repeat("small\n", 9) + "end"},
		{"eleven-lines", strings.Repeat("small\n", 10) + "end", "[paste #1 +11 lines]"},
		{"1000-characters", strings.Repeat("界", 1000), strings.Repeat("界", 1000)},
		{"1001-characters", strings.Repeat("界", 1001), "[paste #1 1001 chars]"},
		{"29-lines", pasteFixture(), pasteLabel29},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := modelFor(t, Options{})
			m.Update(tea.PasteMsg{Content: test.paste})
			checkPromptPaste(t, m, test.want, test.paste)
		})
	}
}

func checkPromptPaste(t *testing.T, m *Model, view, text string) {
	t.Helper()
	if p := &m.editor; p.value() != view || p.area.Value() != view || p.expanded() != text {
		t.Fatalf("editor text %q, textarea %q, expanded %q; want %q and %q", p.value(), p.area.Value(), p.expanded(), view, text)
	}
}

func testFoldedPasteFrameGeometry(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {64, 16}, {100, 24}} {
		m, _ := demo(t, Options{ReducedMotion: true})
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		m.Update(tea.PasteMsg{Content: pasteFixture()})
		m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
		frame := ansi.Strip(assertFrame(t, m))
		if !strings.Contains(frame, pasteLabel29+"?") || strings.Contains(frame, "Last pasted line") {
			t.Fatalf("folded paste or following keystroke not rendered correctly:\n%s", frame)
		}
	}
}

func TestPromptPasteEditing(t *testing.T) {
	m := modelFor(t, Options{ReducedMotion: true})
	p := &m.editor
	a, b := pasteFixture(), strings.Repeat("second\n", 10)+"end"
	const second = "[paste #2 +11 lines]"

	// A literal label stays literal even when the same label exists in the draft.
	const prefix = "界 " + pasteLabel29 + " "
	p.set(prefix)
	m.Update(tea.PasteMsg{Content: a})
	m.Update(tea.PasteMsg{Content: b})
	checkPromptPaste(t, m, prefix+pasteLabel29+second, prefix+a+b)
	press(m, tea.KeyLeft, 0) // skip the second marker
	if p.cursorByte() != len(prefix+pasteLabel29) {
		t.Fatal("left did not step over the marker")
	}
	m.Update(tea.KeyPressMsg{Code: 'é', Text: "é"})
	checkPromptPaste(t, m, prefix+pasteLabel29+"é"+second, prefix+a+"é"+b)
	press(m, tea.KeyRight, 0)
	if !p.atEnd() {
		t.Fatal("right did not step over the marker")
	}
	press(m, tea.KeyBackspace, 0)
	checkPromptPaste(t, m, prefix+pasteLabel29+"é", prefix+a+"é")
	press(m, tea.KeyLeft, 0)
	press(m, tea.KeyLeft, tea.ModShift)
	if got := p.area.SelectedText(); got != pasteLabel29 {
		t.Fatalf("shift+left selected %q, want the whole marker", got)
	}
	// Replacing the selection with identical visible characters drops the payload.
	m.Update(tea.PasteMsg{Content: pasteLabel29})
	checkPromptPaste(t, m, prefix+pasteLabel29+"é", prefix+pasteLabel29+"é")

	p.set("[") // forward deletion must not confuse the two adjacent '[' bytes
	m.Update(tea.PasteMsg{Content: a})
	m.Update(tea.PasteMsg{Content: "]"})
	press(m, tea.KeyLeft, 0)
	press(m, tea.KeyLeft, 0)
	press(m, tea.KeyDelete, 0)
	checkPromptPaste(t, m, "[]", "[]")
}

func TestPromptPasteUsesExpandedBudget(t *testing.T) {
	m := modelFor(t, Options{})
	p := &m.editor
	const prefix = "prefix: "
	a := strings.Repeat("a", maxPromptBytes-len(prefix))
	p.set(prefix)
	m.Update(tea.PasteMsg{Content: a + "b"})
	checkPromptPaste(t, m, prefix, prefix)
	if !strings.Contains(m.notice.text, "32 KiB") {
		t.Fatalf("oversized paste warning missing: %q", m.notice.text)
	}
	m.Update(tea.PasteMsg{Content: a})
	view := p.value()
	checkPromptPaste(t, m, view, prefix+a)
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	press(m, tea.KeyEnter, tea.ModShift)
	m.Update(tea.PasteMsg{Content: "x"})
	checkPromptPaste(t, m, view, prefix+a)
	press(m, tea.KeyLeft, tea.ModShift) // replacement can reclaim the body bytes
	m.Update(tea.PasteMsg{Content: "ok"})
	checkPromptPaste(t, m, prefix+"ok", prefix+"ok")
}

type pasteBackend struct {
	inertBackend
	input sdk.Input
	err   error
}

func (b *pasteBackend) Submit(_ context.Context, input sdk.Input) (sdk.Receipt, error) {
	b.input = input
	return sdk.Receipt{SubmissionID: "42"}, b.err
}

func TestPromptPasteSendAndRecall(t *testing.T) {
	for _, test := range []struct {
		name string
		busy bool
		mod  tea.KeyMod
		mode sdk.BusyMode
	}{{"idle", false, 0, sdk.RejectBusy}, {"steer", true, 0, sdk.Steer}, {"follow-up", true, tea.ModAlt, sdk.FollowUp}} {
		t.Run(test.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true})
			backend := &pasteBackend{}
			m.backend = backend
			if test.busy {
				s.info.Status = x.Running
			}
			// The compact draft looks like a slash command, but the actual
			// draft is multiline and must not run a local action.
			const prefix, suffix = "/not-a-command ", " and this suffix"
			m.editor.set(prefix)
			m.Update(tea.PasteMsg{Content: strings.ReplaceAll(pasteFixture(), "\n", "\r\n")})
			m.Update(tea.PasteMsg{Content: suffix})
			text, view := prefix+pasteFixture()+suffix, prefix+pasteLabel29+suffix
			checkPromptPaste(t, m, view, text)
			workflowWork(t, m, m.promptKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: test.mod}))
			if backend.input.Text != text || backend.input.WhenBusy != test.mode {
				t.Fatalf("submission = %q (%s), want %q (%s)", backend.input.Text, backend.input.WhenBusy, text, test.mode)
			}
			checkPromptPaste(t, m, "", "")
			press(m, tea.KeyUp, 0)
			checkPromptPaste(t, m, view, text)
		})
	}
}

func TestPromptPasteDraftAndFailedSend(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.backend = &pasteBackend{err: errors.New("fixture rejection")}
	a := strings.Repeat("\n", 10_100) + "end" // more logical lines than the textarea can hold
	m.Update(tea.PasteMsg{Content: a})
	m.switchThread("2")
	m.Update(tea.PasteMsg{Content: pasteFixture()})
	m.switchThread("1")
	checkPromptPaste(t, m, "[paste #1 +10101 lines]", a)
	cmd := m.sendPrompt(sdk.Steer)
	m.finishWork(cmd().(workMsg))
	checkPromptPaste(t, m, "[paste #1 +10101 lines]", a)
	press(m, 'c', tea.ModCtrl)
	m.Update(tea.PasteMsg{Content: pasteFixture()})
	checkPromptPaste(t, m, pasteLabel29, pasteFixture())
}

func TestPromptPasteEditAndCopyUseFullText(t *testing.T) {
	var edit, copyText string
	m, _ := demo(t, Options{ReducedMotion: true,
		EditPrompt: func(_ context.Context, s string) (string, error) { edit = s; return s, nil },
		CopyText:   func(_ context.Context, s string) error { copyText = s; return nil },
	})
	m.Update(tea.PasteMsg{Content: pasteFixture()})
	budget := maxEffects
	workflowWork(t, m, m.admit(m.route(m.active, coreID), x.Outcome{x.Local{Op: x.EditPrompt}}, &budget))
	press(m, tea.KeyLeft, tea.ModShift)
	workflowWork(t, m, m.promptKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift}))
	if edit != pasteFixture() || copyText != pasteFixture() {
		t.Fatalf("edit/copy got %q / %q; want full pasted text", edit, copyText)
	}
	checkPromptPaste(t, m, pasteLabel29, pasteFixture())
}

func TestExternalPromptEditPreservesDraft(t *testing.T) {
	large := "original\t文\n" + strings.Repeat("\n", 10_100) + "retained suffix"
	changed := strings.Replace(large, "original", "edited", 1)
	for _, source := range []string{"hook", "terminal"} {
		t.Run(source, func(t *testing.T) {
			for _, tc := range []struct {
				name, initial, edited, view, full string
				preserved, send, stale            bool
			}{
				{name: "changed-large", initial: large, edited: changed, view: "[paste #1 +10102 lines]", full: changed, send: true},
				{name: "large-new-result", initial: "ordinary", edited: changed, view: "[paste #1 +10102 lines]", full: changed, send: true},
				{name: "shortened-fold-with-tab", initial: large, edited: "a\tb", view: "[paste #1 3 chars]", full: "a\tb", send: true},
				{name: "plain-edit", initial: "ordinary", edited: "Edit\r\n文\tline\x1b", view: "Edit\n文 line", full: "Edit\n文 line"},
				{name: "unchanged", initial: large, edited: large, view: "[paste #1 +10102 lines]", full: large, preserved: true},
				{name: "oversized", initial: large, edited: strings.Repeat("e", maxPromptBytes+1), view: "[paste #1 +10102 lines]", full: large, preserved: true},
				{name: "stale", initial: large, edited: changed, view: "[paste #1 +10102 lines]?", full: large + "?", preserved: true, stale: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					m, _ := demo(t, Options{ReducedMotion: true, EditPrompt: func(_ context.Context, draft string) (string, error) {
						if draft != tc.initial {
							t.Fatal("editor did not receive the expanded draft")
						}
						return tc.edited, nil
					}})
					m.Update(tea.PasteMsg{Content: tc.initial})
					r := m.route(m.active, coreID)
					var cmd tea.Cmd
					if source == "hook" {
						budget := maxEffects
						cmd = m.admit(r, x.Outcome{x.Local{Op: x.EditPrompt}}, &budget)
					} else {
						dir := t.TempDir()
						path := filepath.Join(dir, "prompt.txt")
						if err := os.WriteFile(path, []byte(tc.edited), 0600); err != nil {
							t.Fatal(err)
						}
						m.editorTemp = dir
						cmd = m.finishPromptEdit(promptEditFinished{route: r, rev: m.editor.revision, dir: dir, path: path})
					}
					if tc.stale {
						m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
					}
					rev := m.editor.revision
					m.finishWork(cmd().(workMsg))
					if source == "terminal" && m.editorTemp != "" {
						t.Fatal("editor cleanup did not clear the temp path")
					}
					if p := &m.editor; p.value() != tc.view || p.area.Value() != tc.view || p.expanded() != tc.full {
						t.Fatalf("unexpected draft after edit: display has %d bytes, expanded has %d bytes; want %d and %d", len(p.value()), len(p.expanded()), len(tc.view), len(tc.full))
					}
					if tc.preserved != (m.editor.revision == rev) {
						t.Fatal("unexpected editor revision after completion")
					}
					if tc.name == "oversized" && !strings.Contains(m.notice.text, "prompt limit") {
						t.Fatal("oversized edit did not report the limit")
					}
					if tc.send {
						backend := &pasteBackend{}
						m.backend = backend
						workflowWork(t, m, m.sendPrompt(sdk.RejectBusy))
						if backend.input.Text != tc.full {
							t.Fatal("submission did not preserve the edited draft")
						}
						press(m, tea.KeyUp, 0)
						checkPromptPaste(t, m, tc.view, tc.full)
					}
				})
			}
		})
	}
}

func TestPromptPasteDoesNotFoldDialogInput(t *testing.T) {
	m := modelFor(t, Options{})
	budget := maxEffects
	m.openDialog(m.route(m.active, coreID), x.InputDialog{Title: "Input", Multiline: true}, &budget)
	paste := strings.Repeat("dialog text\n", 10) + "end"
	m.Update(tea.PasteMsg{Content: paste})
	if got := m.focusedDialog().input.value(); got != paste || m.editor.value() != "" {
		t.Fatalf("dialog paste folded or went to the prompt: %q", got)
	}
}
