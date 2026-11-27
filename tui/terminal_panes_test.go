package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGoldenPaneActions(t *testing.T) {
	var source sdk.ID
	m, _ := demo(t, Options{PaneCommands: true, Fills: FillOff, ReducedMotion: true, OpenThreadPane: func(_ context.Context, id sdk.ID) error {
		source = id
		return nil
	}})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assertFrame(t, m)
	y := m.frame.ruleY
	press(m, '?', 0)
	d := m.focusedDialog()
	m.changeDialogQuery(d, "new hush pane")
	if c, ok := d.choice(); !ok || c.ID != menusID+".new-pane" {
		t.Fatalf("new pane action not selected: %v", c)
	}
	golden(t, "palette-panes", m)
	if m.frame.ruleY != y || m.frame.dialogY+m.frame.dialogHeight != y {
		t.Fatal("pane actions moved the input rule")
	}
	press(m, tea.KeyEscape, 0)
	m.editor.set("keep the draft")
	lead := func(key rune) tea.Cmd {
		press(m, 'x', tea.ModCtrl)
		press(m, 'i', 0)
		return press(m, key, 0)
	}
	pending := lead('e')
	if pending == nil || source != "" || lead('e') != nil {
		t.Fatal("open did not dispatch once off the update loop")
	}
	m.Update(pending())
	if source != "1" || m.active != "1" || m.editor.value() != "keep the draft" {
		t.Fatalf("wrong launch target or changed parent: %s %s %q", source, m.active, m.editor.value())
	}
	lead('y')
	if m.editor.value() != "/pane keep the draft" {
		t.Fatal("the explicit command request did not preserve the draft")
	}
	m.backend = workspaceMetadataFixture{submit: func(input sdk.Input) (sdk.Receipt, error) {
		if input.ThreadID != "1" || !input.PaneControl || input.Text != "/pane keep the draft" {
			t.Fatalf("wrong explicit launch input: %+v", input)
		}
		return sdk.Receipt{SubmissionID: "7"}, nil
	}}
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
}

func TestPaneSlashNameIsReservedAndBlankArgumentsStayLocal(t *testing.T) {
	custom := x.Registration{ID: "fixture", APIVersion: "0.1.0", Actions: []x.Action{{ID: "fixture.pane", Title: "Custom pane",
		Slash: &x.Slash{Name: "pane"}, Run: func(x.Context, x.Invocation) x.Outcome { return nil }}}}
	if _, err := makeRegistry(Options{Extensions: []x.Registration{custom}}); err == nil {
		t.Fatal("extension claimed the reserved /pane name without a native adapter")
	}
	live := true
	m, _ := demo(t, Options{PaneCommands: true, PaneCommandsAvailable: func() bool { return live }, ReducedMotion: true})
	for _, draft := range []string{"/pane \ufeff", "/pane \u00a0", "/pane \n"} {
		m.editor.set(draft)
		press(m, tea.KeyEnter, 0)
		if len(m.pending) != 0 || m.editor.value() != draft || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Describe what to run in the pane") {
			t.Fatalf("blank argument submitted or lost: %q", draft)
		}
	}
	m.editor.set("/pane")
	live = false
	if len(m.slashMatches()) != 0 {
		t.Fatal("disabled pane route still advertises the slash action")
	}
	press(m, tea.KeyEnter, 0)
	if m.editor.value() != "/pane" || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Command panes are unavailable") {
		t.Fatal("unavailable slash action lost its draft or status")
	}
}

func TestPaneActionUnavailablePreservesDraft(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.editor.set("/pane run a command")
	press(m, tea.KeyEnter, 0)
	if m.editor.value() != "/pane run a command" || len(m.pending) != 0 ||
		!strings.Contains(ansi.Strip(assertFrame(t, m)), "Command panes are unavailable") {
		t.Fatal("unsupported terminal submitted the draft or hid the error")
	}
}
