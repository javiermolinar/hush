package tui

import (
	"context"
	"errors"
	"strings"

	tea "charm.land/bubbletea/v2"
	x "github.com/javiermolinar/hush/tui/extension"
)

const (
	openThreadPaneOp   x.LocalOp = "thread.pane"
	stagePaneCommandOp x.LocalOp = "pane.command"
)

// ErrPaneOpenUnknown means placement may have happened. UI actions must not retry it.
var ErrPaneOpenUnknown = errors.New("pane opening could not be confirmed")

func (m *Model) openThreadPane(r ownerRoute, budget *int) tea.Cmd {
	t := m.threads[r.owner]
	open := m.options.OpenThreadPane
	if open == nil || !r.owner.Valid() || t.attaching || !m.foreground(r) || t.openingPane {
		return nil
	}
	t.openingPane = true
	return m.startWork(r, func(ctx context.Context) (any, error) {
		return nil, open(ctx, r.owner)
	}, func(_ any, err error, _ *int) tea.Cmd {
		t.openingPane = false
		switch {
		case errors.Is(err, ErrPaneOpenUnknown):
			return m.notifyRoute(r, "Pane opening was not confirmed; check your terminal", x.Warning)
		case err != nil:
			return m.notifyRoute(r, "New pane unavailable; this thread is unchanged", x.Warning)
		default:
			return m.notifyRoute(r, "Opened a new Hush pane", x.Good)
		}
	}, budget)
}

func (o Options) paneCommandsAvailable() bool {
	return o.PaneCommands && (o.PaneCommandsAvailable == nil || o.PaneCommandsAvailable())
}

func paneSlashWithSeparator(text string) bool {
	return strings.HasPrefix(text, "/pane ") || strings.HasPrefix(text, "/pane\t")
}

func (m *Model) stagePaneCommand(r ownerRoute) tea.Cmd {
	if !m.options.paneCommandsAvailable() || !m.foreground(r) {
		return nil
	}
	before := m.editor.value()
	if paneSlashWithSeparator(before) {
		return nil
	}
	m.editor.replace(0, 0, "/pane ")
	if m.editor.value() == before {
		return m.notifyRoute(r, "The prefix did not fit; edit the draft to request a pane", x.Warning)
	}
	m.editor.resetHistory()
	m.slashHidden = false
	return nil
}
