package tui

import (
	"context"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

// ThreadTabs routes external thread opens to one TUI's update loop. It never
// activates the new tab or interrupts the user's current prompt.
type ThreadTabs struct{ requests chan tabOpenMsg }

func NewThreadTabs() *ThreadTabs { return &ThreadTabs{requests: make(chan tabOpenMsg)} }

type tabOpenMsg struct {
	ctx    context.Context
	id     sdk.ID
	result chan error
}

// Open reports registration only. A cancellation after delivery may leave
// the tab registered even when the caller didn't receive confirmation.
func (t *ThreadTabs) Open(ctx context.Context, id sdk.ID) error {
	if !id.Valid() || ctx.Err() != nil {
		return extension.ErrUnavailable
	}
	v := tabOpenMsg{ctx: ctx, id: id, result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return extension.ErrUnavailable
	case t.requests <- v:
	}
	select {
	case <-ctx.Done():
		return ErrPaneOpenUnknown
	case res := <-v.result:
		return res
	}
}

func (t *ThreadTabs) wait(ctx context.Context) tea.Cmd {
	if t == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case v := <-t.requests:
			return v
		}
	}
}

func (m *Model) openThreadTab(v tabOpenMsg) tea.Cmd {
	result := extension.ErrUnavailable
	var watch tea.Cmd
	if v.ctx.Err() == nil && m.ctx.Err() == nil && v.id.Valid() {
		if slices.Contains(m.tabs, v.id) {
			result = nil
		} else if len(m.tabs) < maxOpenTabs {
			m.ensureThread(v.id)
			m.openedTab(v.id)
			watch = m.syncDirectory()
			result = nil
		}
	}
	v.result <- result
	return tea.Batch(m.options.ThreadTabs.wait(m.ctx), watch)
}
