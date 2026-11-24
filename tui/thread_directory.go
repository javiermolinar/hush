package tui

import (
	"context"
	"slices"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type directoryWatchBackend interface {
	WatchThreadDirectory(context.Context, []sdk.ID) <-chan sdk.ThreadDirectoryUpdate
}

type worktreeBackend interface {
	CreateThread(context.Context, sdk.ThreadCreation) (sdk.ThreadDirectoryRow, error)
}

type archiveBackend interface {
	SetThreadArchived(context.Context, sdk.ID, bool) error
}

type directoryFeedMsg struct {
	token  uint64
	source <-chan sdk.ThreadDirectoryUpdate
	update sdk.ThreadDirectoryUpdate
	ok     bool
}

func (m *Model) syncDirectory() tea.Cmd {
	if m.directoryEnabled && slices.Equal(m.directoryIDs, m.tabs) {
		return nil
	}
	if m.directoryFeed.cancel != nil {
		m.directoryFeed.cancel()
	}
	m.directoryFeed, m.directoryIDs = feedLease{}, nil
	watcher, ok := m.backend.(directoryWatchBackend)
	if !ok || !m.directoryEnabled || len(m.tabs) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	lease := feedLease{token: m.token(), ctx: ctx, cancel: cancel}
	ids := slices.Clone(m.tabs)
	m.directoryFeed, m.directoryIDs = lease, ids
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		return awaitDirectory(lease, watcher.WatchThreadDirectory(ctx, ids))()
	}
}

func awaitDirectory(f feedLease, src <-chan sdk.ThreadDirectoryUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case v, ok := <-src:
			return directoryFeedMsg{f.token, src, v, ok}
		}
	}
}

func (m *Model) receiveDirectory(msg directoryFeedMsg) tea.Cmd {
	if msg.token != m.directoryFeed.token || msg.token == 0 {
		return nil
	}
	if !msg.ok || sdk.IsCode(msg.update.Err, "unsupported_capability") {
		m.directoryEnabled = false
		m.menuFeaturesFromBackend &^= x.Worktrees | x.Archive
		for _, id := range m.directoryIDs {
			if t := m.threads[id]; t != nil && t.directory != nil {
				t.directory = nil
				if m.options.ProjectThread == nil {
					t.info.Branch = ""
				}
				if m.options.Services.Archive == nil {
					t.info.Archived = false
				}
				m.applyPresentation(t, t.presentation)
			}
		}
		return m.syncDirectory()
	}
	next := awaitDirectory(m.directoryFeed, msg.source)
	if msg.update.Err != nil {
		return next
	}
	cmds := []tea.Cmd{next}
	for _, row := range msg.update.Report.Threads {
		t := m.threads[row.ID]
		if t == nil || !slices.Contains(m.directoryIDs, row.ID) {
			continue
		}
		prior := t.info
		t.directory = &row // detached SDK report, only read by the UI loop
		m.mergeDirectory(&t.info, t)
		if prior != t.info {
			t.revision++
			m.emit(row.ID, x.Event{Kind: x.StatusChanged})
			cmds = append(cmds, m.desktopStatus(row.ID, prior.Status, t.info.Status))
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) mergeDirectory(out *x.Thread, t *threadState) {
	row := t.directory
	if row == nil {
		return
	}
	if m.options.ProjectThread == nil {
		out.Branch = ""
		if row.Checkout != nil {
			out.Branch = row.Checkout.Branch
			if out.Workspace == "" {
				out.Workspace = row.Checkout.CWD
			}
		}
		if status := x.ThreadStatus(row.Status); status != "" {
			// A native run may arrive before the directory catches up. Don't
			// hide it behind an older idle or done report.
			if !t.activeRun || status == x.Running || status == x.Parked {
				out.Status = status
			}
		}
		if m.options.Services.Rename == nil {
			out.Title = row.Title
		}
	}
	if m.options.Services.Archive == nil {
		out.Archived = row.Archived
	}
}

func directoryThread(t sdk.ThreadDirectoryRow) x.Thread {
	thread := x.Thread{ID: t.ID, ParentID: t.ParentID, Forked: t.ParentID != "", Title: t.Title, Status: x.ThreadStatus(t.Status), Archived: t.Archived}
	if t.Checkout != nil {
		thread.Branch, thread.Workspace = t.Checkout.Branch, t.Checkout.CWD
	}
	if t.LastMessageMS > 0 {
		thread.LastMessageAt = time.UnixMilli(int64(t.LastMessageMS)).Local()
		thread.LastMessageAt.Zone() // resolve the timezone off the UI loop
	}
	return thread
}
