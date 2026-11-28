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

// Reconcile both kinds of tab lease together, including when capabilities change.
func (m *Model) syncDirectory() tea.Cmd {
	watcher, ok := m.backend.(directoryWatchBackend)
	m.directoryEnabled = m.directoryEnabled && ok
	fallback := m.syncTabFeeds()
	if m.directoryEnabled && slices.Equal(m.directoryIDs, m.tabs) {
		return nil
	}
	if m.directoryFeed.cancel != nil {
		m.directoryFeed.cancel()
	}
	for _, id := range m.directoryIDs {
		if !m.directoryEnabled || !slices.Contains(m.tabs, id) {
			m.clearDirectory(id)
		}
	}
	m.directoryFeed, m.directoryIDs = feedLease{}, nil
	if !m.directoryEnabled || len(m.tabs) == 0 {
		return fallback
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
		first := t.directory == nil
		if first {
			t.saveDirectoryBase(prior, t.directoryBase.hostStatus)
		}
		t.directory = &row // detached SDK report, only read by the UI loop
		t.info = m.withoutDirectory(t)
		m.mergeDirectory(&t.info, t, t.activeRun && m.connection == x.Connected)
		if m.rexActivity != nil {
			t.rexActivity.observeDirectory(row, first, row.ID == m.active)
		}
		m.tabMetadataChanged(t, prior)
		cmds = append(cmds, m.desktopStatus(row.ID, prior.Status, t.info.Status))
	}
	return tea.Batch(cmds...)
}

// Keep the native/host projection separate from the directory overlay. In
// particular, a directory-only title or checkout must not survive a downgrade.
type tabMetadata struct {
	title, branch, workspace string
	status, beforeFallback   x.ThreadStatus
	archived, hostStatus     bool
}

func (t *threadState) saveDirectoryBase(info x.Thread, hostStatus bool) {
	base := &t.directoryBase
	base.title, base.branch, base.workspace = info.Title, info.Branch, info.Workspace
	base.status, base.archived, base.hostStatus = info.Status, info.Archived, hostStatus
}

func (m *Model) withoutDirectory(t *threadState) x.Thread {
	out := t.info
	if t.directory == nil {
		return out
	}
	base := t.directoryBase
	out.Status, out.Branch, out.Workspace = base.status, base.branch, base.workspace
	if m.options.Services.Rename == nil {
		out.Title = base.title
	}
	if m.options.Services.Archive == nil {
		out.Archived = base.archived
	}
	return out
}

func (m *Model) tabMetadataChanged(t *threadState, prior x.Thread) {
	if t.info != prior {
		t.revision++
		m.emit(t.info.ID, x.Event{Kind: x.StatusChanged})
	}
}

func (m *Model) clearDirectory(id sdk.ID) {
	if t := m.threads[id]; t != nil && t.directory != nil {
		prior, revision := t.info, t.revision
		t.info, t.directory = m.withoutDirectory(t), nil
		m.applyPresentation(t, t.presentation)
		if prior.Status != t.info.Status {
			// Compare the first fresh observation with what the user last saw,
			// not with the cached native status restored during the handoff.
			t.directoryBase.beforeFallback = prior.Status
		}
		if t.revision == revision {
			m.tabMetadataChanged(t, prior)
		}
	}
}

func (m *Model) reprojectBackgroundTab(id sdk.ID) {
	if t := m.threads[id]; t != nil && t.directory != nil && slices.Contains(m.tabs, id) {
		prior := t.info
		t.info = m.withoutDirectory(t)
		m.mergeDirectory(&t.info, t, false)
		// The cached status may be older than the native run. Repaint the
		// tab but notify only on feed updates, not on navigation.
		m.tabMetadataChanged(t, prior)
	}
}

func (m *Model) mergeDirectory(out *x.Thread, t *threadState, nativeRun bool) {
	row := t.directory
	if row == nil {
		return
	}
	base := t.directoryBase
	background := out.ID != m.active
	if row.Checkout != nil {
		if m.options.ProjectThread == nil || base.branch == "" {
			out.Branch = row.Checkout.Branch
		}
		if out.Workspace == "" || background && row.Checkout.CWD != "" {
			out.Workspace = row.Checkout.CWD
		}
	}
	if status := x.ThreadStatus(row.Status); status != "" && !base.hostStatus {
		// Only the active feed can know about a run the directory has not
		// observed yet. A saved native run must not pin a background tab.
		if background || !nativeRun || status == x.Running || status == x.Parked {
			out.Status = status
		}
	}
	if m.options.Services.Rename == nil && (m.options.ProjectThread == nil || base.title == "") {
		out.Title = row.Title
	}
	if m.options.Services.Archive == nil {
		out.Archived = row.Archived
	}
}

func directoryPageResult(page sdk.ThreadDirectoryPage, limit int) x.ClientResult {
	count := len(page.Threads)
	if count > limit || page.Workspaces != nil && len(page.Workspaces) != count ||
		page.WorkspaceMatches != nil && len(page.WorkspaceMatches) != count {
		return x.ClientResult{Err: sdk.ErrProtocol}
	}
	out := x.ClientResult{Cursor: deref(page.NextCursor), Directory: true, Indexing: page.Indexing,
		Workspaces: slices.Clone(page.Workspaces), WorkspaceMatches: slices.Clone(page.WorkspaceMatches)}
	for _, row := range page.Threads {
		out.Threads = append(out.Threads, directoryThread(row))
	}
	return out
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
