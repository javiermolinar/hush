package tui

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestConfirmationRetainsQuestionAndCannotAcceptBeforePaint(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	b := maxEffects
	accepted := false
	m.openDialog(m.route(m.active, coreID), x.ConfirmDialog{Title: "Confirm reset", Message: "Reset the conversation?",
		Done: func(_ x.Context, reply x.Reply[bool]) x.Outcome { accepted = reply.Accepted && reply.Value; return nil }}, &b)
	d := m.focusedDialog()
	press(m, 'y', 0)
	if accepted || m.focusedDialog() != d {
		t.Fatal("confirmation accepted without drawing its question")
	}
	for h := 1; h <= 6; h++ {
		rows, targets := m.overlayRows(d, h)
		canChoose := slices.ContainsFunc(targets, func(target int) bool { return target >= 0 })
		if len(rows) > h || !strings.Contains(ansi.Strip(strings.Join(rows, "\n")), "Reset the conversation?") || canChoose != (h >= 2) {
			t.Fatalf("%d-row confirmation misplaced its question or choices: %q, %v", h, rows, targets)
		}
	}
	assertFrame(t, m)
	press(m, 'y', 0)
	if !accepted || m.focusedDialog() != nil {
		t.Fatal("visible confirmation did not honor yes")
	}
}

func TestShortOverlayRetainsUsableRowsOverNoticesAndLists(t *testing.T) {
	m, s := demo(t, Options{Intro: &IntroOptions{}, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.ensureThread("2")
	m.openedTab("2")
	s.info.Status = x.Running
	s.activity.task = "20"
	m.notify(m.active, "An example notice", x.Warning)
	m.editor.set("/")
	b := maxEffects
	r := m.route(m.active, coreID)
	m.openPalette(r, &b)
	check := func() {
		t.Helper()
		body := ansi.Strip(assertFrame(t, m))
		if m.frame.dialogHeight < 3 || !strings.Contains(body, "Type to filter") ||
			m.View().Cursor == nil || !slices.ContainsFunc(m.frame.dialogChoices, func(i int) bool { return i >= 0 }) {
			t.Fatalf("overlay doesn't have a search field and a choice:\n%s", body)
		}
	}
	check()
	if m.frame.slashStart != m.height {
		t.Fatal("completion list is competing with the floating overlay")
	}
	press(m, tea.KeyEscape, 0)
	m.editor.set("line one\nline two\nline three")
	m.openDialog(r, x.SelectDialog{Placement: x.Sheet, Title: "Sheet underneath", Choices: []x.Choice{{ID: "1", Label: "First"}}}, &b)
	m.openPalette(r, &b)
	check()
}
