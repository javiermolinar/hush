package tui

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	"github.com/javiermolinar/hush/tui/extension"
)

// Backend is the native client surface used by the shell. A backend must be
// safe for concurrent calls and represent one stable SDK data store.
type Backend interface {
	Watch(context.Context, sdk.ID, sdk.Window) <-chan sdk.StateUpdate
	NewThread(context.Context, sdk.NewThread) (sdk.Thread, error)
	Fork(context.Context, sdk.ID, sdk.ID) (sdk.Thread, error)
	Abort(context.Context, sdk.ID) error
	ConfigureThread(context.Context, sdk.ID, sdk.Configure) error
	ListThreads(context.Context, int, string) (sdk.ThreadPage, error)
	Submit(context.Context, sdk.Input) (sdk.Receipt, error)
	History(context.Context, sdk.ID, sdk.HistoryRange, int) (sdk.HistoryPage, error)
	Compact(context.Context, sdk.ID, *string) (sdk.ID, error)
	Reset(context.Context, sdk.ID, *string) error
}

var _ Backend = (*sdk.Client)(nil)

type threadDirectoryBackend interface {
	ThreadDirectory(context.Context, int, string) (sdk.ThreadDirectoryPage, error)
}

type workspaceDirectoryBackend interface {
	ListThreadDirectory(context.Context, sdk.ThreadDirectoryOptions) (sdk.ThreadDirectoryPage, error)
}

const nativeThreadCursor = "native:"
const workspaceThreadCursor = "workspace:"

type feedLease struct {
	token  uint64
	owner  sdk.ID
	ctx    context.Context
	cancel context.CancelFunc
}
type feedMsg struct {
	token  uint64
	owner  sdk.ID
	source <-chan sdk.StateUpdate
	update sdk.StateUpdate
	ok     bool
}

func (m *Model) stopFeed() {
	if m.feed.cancel != nil {
		m.feed.cancel()
	}
	m.feed = feedLease{}
}

func (m *Model) beginFeed() tea.Cmd {
	m.stopFeed()
	owner := m.active
	if owner == "" {
		return nil
	}
	m.setConnection(extension.Connecting)
	ctx, cancel := context.WithCancel(m.ctx)
	f := feedLease{m.token(), owner, ctx, cancel}
	m.feed = f
	t := m.threads[owner]
	t.loadingOlder, t.loadingGap, t.gapRetries = false, false, 0
	backend := m.backend
	return func() tea.Msg {
		if ctx.Err() != nil {
			return nil
		}
		return awaitFeed(f, backend.Watch(ctx, owner, sdk.Window{Min: 50, Max: 100}))()
	}
}

func awaitFeed(f feedLease, source <-chan sdk.StateUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case update, ok := <-source:
			return feedMsg{f.token, f.owner, source, update, ok}
		}
	}
}

func (m *Model) setConnection(state extension.Connection) {
	if m.connection == state {
		return
	}
	m.connection = state
	m.threads[m.active].revision++
	m.emit(m.active, extension.Event{Kind: extension.ConnectionChanged})
}

func (m *Model) receiveFeed(msg feedMsg) tea.Cmd {
	if msg.token != m.feed.token || msg.owner != m.active {
		return nil
	}
	owner := msg.owner
	t := m.threads[owner]
	if !msg.ok {
		m.stopFeed()
		m.setConnection(extension.Disconnected)
		m.introFeed(t, false, false)
		return nil
	}
	next := awaitFeed(m.feed, msg.source)
	if msg.update.Err != nil {
		if errors.Is(msg.update.Err, sdk.ErrStoreChanged) {
			m.stopFeed()
			m.setConnection(extension.Disconnected)
			return m.notify(owner, "History incomplete: "+msg.update.Err.Error(), extension.Danger)
		}
		m.setConnection(extension.Connecting)
		var server *sdk.ServerError
		reachable := errors.As(msg.update.Err, &server)
		m.introFeed(t, reachable, !reachable)
		if t.intro != nil && !t.intro.frozen && !reachable {
			return next // the report already shows the transport state
		}
		return tea.Batch(next, m.notify(owner, msg.update.Err.Error(), extension.Warning))
	}
	s := msg.update.Snapshot.State
	if s.Thread.ID != owner {
		m.stopFeed()
		m.setConnection(extension.Disconnected)
		return m.notify(owner, "Invalid watch thread", extension.Danger)
	}
	old, stream := t.info, t.stream
	wasConnected := m.connection == extension.Connected
	upper := deref(s.Window.LastEntryID)
	before := deref(s.Window.BeforeEntryID)
	if lessID(upper, t.through) || lessID(upper, old.LastEntryID) {
		// A server epoch is not a store identity. Never silently mix a
		// regressed history with retained IDs or rebind outstanding work.
		m.stopFeed()
		m.setConnection(extension.Disconnected)
		return m.notify(owner, "History regressed; reopen the TUI to reattach", extension.Danger)
	}
	if t.gapUpper == "" && (t.through == "" || before == "" || !lessID(t.through, before)) {
		t.through = upper
	}
	if t.gapUpper == "" && lessID(t.through, upper) {
		t.gapUpper, t.gapBefore = upper, before // keep these fences while proving the gap
	}
	t.windowBefore = before
	if before == "" {
		t.atStart = true
	}
	newRows := t.merge(s.Window.Entries)
	t.info = m.threadInfo(t, msg.update.Snapshot)
	t.queue = s.Queue // SDK snapshots are immutable; never passed to extensions.
	t.runInputs = nil
	if s.Run != nil {
		t.runInputs = s.Run.SubmissionIDs
	}
	t.admittedInputs = slices.DeleteFunc(t.admittedInputs, func(item sdk.QueueItem) bool {
		return t.hasSubmission(item.SubmissionID)
	})
	if t.awaiting != "" && (t.hasSubmission(t.awaiting) || t.info.LastEntryID != old.LastEntryID || t.info.Status != old.Status) {
		t.awaiting = ""
	}
	t.stream = detachStream(s.Streaming)
	if !reflect.DeepEqual(stream, t.stream) {
		t.streamRevision++
	}
	oldTools := t.tools
	t.tools = projectTools(s.Tools, s.Streaming, t.entries)
	m.now = time.Now()
	m.applyActivity(t, s, m.now, newRows)
	m.introFeed(t, true, false)
	t.revision++
	m.setConnection(extension.Connected)
	for _, r := range newRows {
		m.emit(owner, extension.Event{Kind: extension.EntryObserved, Entry: r.entry})
	}
	if old.Usage != t.info.Usage {
		m.emit(owner, extension.Event{Kind: extension.UsageChanged})
	}
	old.Usage = t.info.Usage
	if old != t.info {
		m.emit(owner, extension.Event{Kind: extension.StatusChanged})
	}
	if !reflect.DeepEqual(stream, t.stream) {
		m.emit(owner, extension.Event{Kind: extension.StreamChanged, Stream: t.stream})
	}
	m.observeTools(owner, oldTools, t.tools)
	var probe, review tea.Cmd
	if !wasConnected {
		probe = m.menuProbe()
	}
	if m.gitDiffFeed.token != 0 && (old.Status != t.info.Status || slices.ContainsFunc(newRows, func(r *record) bool {
		return slices.ContainsFunc(r.entry.Record.Messages, func(msg sdk.Message) bool { return msg.Role == "tool" })
	})) {
		review = m.expediteGitDiff()
	}
	return tea.Batch(next, m.fetchGap(t), m.maybeOlder(false), m.desktopStatus(owner, old.Status, t.info.Status), probe, review)
}

type historyMsg struct {
	lease        uint64
	owner        sdk.ID
	lower, upper sdk.ID
	forward      bool
	page         sdk.HistoryPage
	err          error
}

const maxHistoryRetries = 3
const historyRetryBase = 50 * time.Millisecond

func (m *Model) historyCommand(t *threadState, lower, upper sdk.ID, forward bool) tea.Cmd {
	return m.historyAttempt(t, lower, upper, forward, 0)
}

func (m *Model) historyAttempt(t *threadState, lower, upper sdk.ID, forward bool, delay time.Duration) tea.Cmd {
	ctx, backend, lease, owner := m.feed.ctx, m.backend, m.feed.token, t.info.ID
	query := sdk.Before(idPtr(upper))
	if forward {
		query = sdk.After(idPtr(lower), idPtr(upper))
	}
	return func() tea.Msg {
		if delay != 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return nil
			case <-timer.C:
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		page, err := backend.History(call, owner, query, 100)
		return historyMsg{lease, owner, lower, upper, forward, page, err}
	}
}

func (m *Model) fetchGap(t *threadState) tea.Cmd {
	if t.gapUpper == "" || t.loadingGap || t.pageError || m.connection != extension.Connected || m.feed.token == 0 {
		return nil
	}
	t.loadingGap = true
	return m.historyCommand(t, t.through, t.gapUpper, true)
}

func (m *Model) maybeOlder(explicit bool) tea.Cmd {
	t := m.threads[m.active]
	if t.following || t.atStart || t.loadingOlder || len(t.ids) == 0 || m.connection != extension.Connected || m.feed.token == 0 {
		return nil
	}
	index := t.blockIndex(t.anchor.id)
	if index > 5 || !explicit && (t.olderError || t.anchor.id == introEntry) {
		return nil
	}
	t.loadingOlder = true
	t.olderError = false
	return m.historyCommand(t, "", t.ids[0], false)
}

func (m *Model) receiveHistory(v historyMsg) tea.Cmd {
	if m.feed.token != v.lease || v.owner != m.active {
		return nil
	}
	t := m.threads[v.owner]
	if v.forward {
		if !t.loadingGap || t.through != v.lower || t.gapUpper != v.upper {
			return nil
		}
		t.loadingGap = false
	} else {
		t.loadingOlder = false
	}
	err := v.err
	last := v.lower
	if err == nil {
		if v.page.ThreadID != v.owner || len(v.page.Entries) > 100 {
			err = sdk.ErrProtocol
		} else {
			for _, r := range v.page.Entries {
				if !r.ID.Valid() || !lessID(last, r.ID) || (v.forward && lessID(v.upper, r.ID)) || (!v.forward && !lessID(r.ID, v.upper)) {
					err = sdk.ErrProtocol
					break
				}
				last = r.ID
			}
		}
		if err == nil && v.forward && (last == v.lower || (v.page.NextCursor == nil) != (last == v.upper) ||
			v.page.NextCursor != nil && *v.page.NextCursor == "" ||
			!t.historyOverlap(v.lower, last, v.page.Entries)) {
			err = sdk.ErrProtocol
		}
	}
	if err != nil {
		if v.forward {
			if t.gapRetries < maxHistoryRetries && retryableHistory(err) && m.feed.ctx.Err() == nil {
				delay := historyRetryBase << t.gapRetries
				t.gapRetries++
				t.loadingGap = true
				return m.historyAttempt(t, v.lower, v.upper, true, delay)
			}
			t.pageError = true
			t.status[coreID+":scrollback"] = extension.SetStatus{Text: "History incomplete", Tone: extension.Warning}
		} else {
			t.olderError = true
		}
		if errors.Is(err, sdk.ErrStoreChanged) {
			m.stopFeed()
			m.setConnection(extension.Disconnected)
		}
		return m.notify(v.owner, "History incomplete: "+err.Error(), extension.Warning)
	}
	rows := t.merge(v.page.Entries)
	if v.forward {
		t.through = last
		t.gapRetries = 0
		if last == t.gapUpper {
			t.gapUpper, t.gapBefore = "", ""
			if latest := t.info.LastEntryID; lessID(last, latest) {
				t.gapUpper, t.gapBefore = latest, t.windowBefore // a newer observation is a separate fence
			}
		}
	} else if v.page.NextCursor == nil {
		t.atStart = true
	}
	if len(rows) != 0 {
		t.revision++
	}
	for _, row := range rows {
		m.emit(v.owner, extension.Event{Kind: extension.EntryObserved, Entry: row.entry})
	}
	return m.fetchGap(t)
}

func retryableHistory(err error) bool {
	var server *sdk.ServerError
	if errors.Is(err, sdk.ErrStoreChanged) || errors.Is(err, sdk.ErrStoreIncompatible) ||
		errors.Is(err, sdk.ErrProtocol) || errors.Is(err, sdk.ErrClosed) ||
		errors.Is(err, context.Canceled) || errors.As(err, &server) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var transport *sdk.TransportError
	if !errors.As(err, &transport) {
		return false
	}
	var timeout net.Error
	return (errors.As(err, &timeout) && timeout.Timeout()) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrShortWrite) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

// A page must account for every already observed entry in its proven range.
// IDs are global and sparse; an unobserved numeric hole is not evidence of loss.
func (t *threadState) historyOverlap(lower, last sdk.ID, entries []sdk.Entry) bool {
	// The immediate predecessor named by the reconnect window is committed
	// even when its payload hasn't been received yet.
	if lessID(lower, t.gapBefore) && !lessID(last, t.gapBefore) &&
		!slices.ContainsFunc(entries, func(e sdk.Entry) bool { return e.ID == t.gapBefore }) {
		return false
	}
	i := t.index(lower)
	if i < len(t.ids) && t.ids[i] == lower {
		i++
	}
	for _, entry := range entries {
		if i == len(t.ids) || lessID(last, t.ids[i]) {
			return true
		}
		if lessID(t.ids[i], entry.ID) {
			return false
		}
		if t.ids[i] == entry.ID {
			if !reflect.DeepEqual(t.entries[entry.ID].entry, detachEntry(entry)) {
				return false
			}
			i++
		}
	}
	return i == len(t.ids) || lessID(last, t.ids[i])
}

func lessID(a, b sdk.ID) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}
func idPtr(id sdk.ID) *sdk.ID {
	if id == "" {
		return nil
	}
	return &id
}
func deref[T any](p *T) T {
	if p != nil {
		return *p
	}
	var zero T
	return zero
}
func ptr[T any](v T) *T { return &v }
func optionalText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func projectTools(values []sdk.Tool, stream *sdk.Streaming, entries map[sdk.ID]*record) []extension.Tool {
	args := make(map[string]string)
	if stream != nil {
		for _, b := range stream.Blocks {
			if b.Type == "toolCall" && len(args) < 128 {
				args[b.ID] = limitText(string(b.Arguments), 8192)
			}
		}
	}
	out := make([]extension.Tool, 0, min(64, len(values)))
	for _, v := range values[:min(64, len(values))] {
		t := extension.Tool{CallID: v.CallID, Name: v.Name, Status: v.Status, TaskID: deref(v.TaskID), ResultEntryID: deref(v.ResultEntryID),
			ArgumentsJSON: args[v.CallID], Output: limitText(deref(v.Output), 32768), DroppedBytes: deref(v.DroppedBytes), DroppedLines: deref(v.DroppedLines),
			Live: v.Status == "running", Truncated: len(deref(v.Output)) > 32768}
		if r := entries[t.ResultEntryID]; r != nil {
			var text []string
			for _, msg := range r.entry.Record.Messages {
				if msg.CallID != t.CallID {
					continue
				}
				t.Failed = deref(msg.IsError)
				for _, b := range msg.Blocks {
					if b.Type == "text" {
						text = append(text, b.Text)
					}
				}
			}
			t.ResultText = limitText(strings.Join(text, "\n"), 32768)
		}
		out = append(out, t)
	}
	return out
}

func (m *Model) observeTools(owner sdk.ID, old, current []extension.Tool) {
	prior := make(map[string]extension.Tool, len(old))
	for _, t := range old {
		prior[t.CallID] = t
	}
	for _, t := range current {
		p, seen := prior[t.CallID]
		if t.Live && !p.Live {
			m.emit(owner, extension.Event{Kind: extension.ToolStarted, Tool: t})
		}
		if t.Output != p.Output || t.DroppedBytes != p.DroppedBytes || t.DroppedLines != p.DroppedLines {
			m.emit(owner, extension.Event{Kind: extension.ToolOutput, Tool: t})
		}
		if (t.ResultEntryID != "" || t.Status == "done") && (!seen || t.ResultEntryID != p.ResultEntryID || t.Status != p.Status || t.ResultText != p.ResultText) {
			m.emit(owner, extension.Event{Kind: extension.ToolResult, Tool: t})
		}
	}
}

func pageLimit(n int) int {
	if n <= 0 {
		return 100
	}
	return min(n, 100)
}

func (m *Model) clientJob(r ownerRoute, operation extension.Operation) func(context.Context) (any, error) {
	backend := m.backend
	base := m.options.NewThread
	var job func(context.Context) extension.ClientResult
	if menu, ok := m.menuJob(r, operation); ok {
		return func(ctx context.Context) (any, error) { return menu(ctx), nil }
	}
	target := func(id sdk.ID) sdk.ID {
		if id == "" {
			return r.owner
		}
		return id
	}
	switch op := operation.(type) {
	case extension.Steer:
		return m.clientJob(r, extension.Submit{ThreadID: op.ThreadID, Text: op.Text, RequestID: op.RequestID, WhenBusy: sdk.Steer})
	case extension.Submit:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			key := op.RequestID
			if key == "" {
				key = submissionKey()
			}
			if strings.TrimSpace(op.Text) == "" || len(op.Text) > maxPromptBytes || (op.WhenBusy != "" && op.WhenBusy != sdk.Steer && op.WhenBusy != sdk.FollowUp && op.WhenBusy != sdk.RejectBusy) {
				return extension.ClientResult{Err: sdk.ErrProtocol}
			}
			v, err := backend.Submit(ctx, sdk.Input{ThreadID: id, RequestID: key, Text: op.Text, WhenBusy: op.WhenBusy})
			return extension.ClientResult{Err: err, SubmissionID: v.SubmissionID}
		}
	case extension.NewThread:
		job = func(ctx context.Context) extension.ClientResult {
			v := base
			if op.Workspace != "" {
				v.CWD = op.Workspace
			}
			if op.Model != (sdk.Model{}) {
				v.Model = &op.Model
			}
			out, err := backend.NewThread(ctx, v)
			return extension.ClientResult{Err: err, ThreadID: out.ID}
		}
	case extension.Fork:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			v, err := backend.Fork(ctx, id, op.AtEntryID)
			return extension.ClientResult{Err: err, ThreadID: v.ID}
		}
	case extension.Abort:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			return extension.ClientResult{Err: backend.Abort(ctx, id)}
		}
	case extension.Configure:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			return extension.ClientResult{Err: backend.ConfigureThread(ctx, id, op.Change)}
		}
	case extension.Compact:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			v, err := backend.Compact(ctx, id, optionalText(op.Instructions))
			return extension.ClientResult{Err: err, TaskID: v}
		}
	case extension.Reset:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			return extension.ClientResult{Err: backend.Reset(ctx, id, optionalText(op.Handoff))}
		}
	case extension.ListThreads:
		launch := ""
		if op.LaunchScope && m.options.WorkspaceLocal {
			launch = base.CWD
		}
		job = func(ctx context.Context) extension.ClientResult {
			limit := pageLimit(op.Limit)
			workspaceCursor, continued := strings.CutPrefix(op.Cursor, workspaceThreadCursor)
			if launch != "" && (op.Cursor == "" || continued) {
				if source, ok := backend.(workspaceDirectoryBackend); ok {
					page, err := source.ListThreadDirectory(ctx, sdk.ThreadDirectoryOptions{
						Limit: limit, Cursor: workspaceCursor, Workspace: launch, BrowseAll: op.BrowseAll,
					})
					if err == nil {
						if len(page.Threads) > limit || len(page.WorkspaceMatches) != len(page.Threads) {
							return extension.ClientResult{Err: sdk.ErrProtocol}
						}
						out := extension.ClientResult{Directory: true, Indexing: page.Indexing,
							WorkspaceScoped: !op.BrowseAll, WorkspaceMatches: slices.Clone(page.WorkspaceMatches)}
						if page.NextCursor != nil {
							out.Cursor = workspaceThreadCursor + *page.NextCursor
						}
						for _, t := range page.Threads {
							out.Threads = append(out.Threads, directoryThread(t))
						}
						return out
					}
					if continued || !(sdk.IsCode(err, "unsupported_capability") || op.BrowseAll && sdk.IsCode(err, "not_found")) {
						return extension.ClientResult{Err: err}
					}
				}
			}
			if continued {
				return extension.ClientResult{Err: sdk.ErrProtocol}
			}
			if op.LaunchScope && m.options.WorkspaceLocal && !op.BrowseAll {
				return extension.ClientResult{ScopeUnavailable: true}
			}
			cursor, legacy := strings.CutPrefix(op.Cursor, nativeThreadCursor)
			if source, ok := backend.(threadDirectoryBackend); ok && !legacy {
				page, err := source.ThreadDirectory(ctx, limit, cursor)
				switch {
				case err == nil:
					out := extension.ClientResult{Cursor: deref(page.NextCursor), Directory: true, Indexing: page.Indexing}
					for _, t := range page.Threads[:min(limit, len(page.Threads))] {
						out.Threads = append(out.Threads, directoryThread(t))
					}
					return out
				case cursor == "" && (sdk.IsCode(err, "unsupported_capability") || sdk.IsCode(err, "internal")):
					// The bare native server has no metadata. Preserve basic
					// switching if the optional directory isn't available.
				default:
					return extension.ClientResult{Err: err}
				}
			}
			v, err := backend.ListThreads(ctx, limit, cursor)
			if err != nil {
				return extension.ClientResult{Err: err}
			}
			out := extension.ClientResult{}
			if v.NextCursor != nil {
				out.Cursor = nativeThreadCursor + *v.NextCursor
			}
			for _, t := range v.Threads[:min(limit, len(v.Threads))] {
				thread := extension.Thread{ID: t.ID, Forked: t.Parent != nil}
				if t.Parent != nil {
					thread.ParentID = t.Parent.ThreadID
				}
				out.Threads = append(out.Threads, thread)
			}
			return out
		}
	case extension.History:
		id := target(op.ThreadID)
		job = func(ctx context.Context) extension.ClientResult {
			v, err := backend.History(ctx, id, sdk.Before(idPtr(op.BeforeEntryID)), pageLimit(op.Limit))
			if err != nil {
				return extension.ClientResult{Err: err}
			}
			if v.ThreadID != id {
				return extension.ClientResult{Err: sdk.ErrProtocol}
			}
			out := extension.ClientResult{Cursor: deref(v.NextCursor)}
			for _, record := range v.Entries[:min(pageLimit(op.Limit), len(v.Entries))] {
				out.Entries = append(out.Entries, detachEntry(record))
			}
			return out
		}
	default:
		job = func(context.Context) extension.ClientResult {
			return extension.ClientResult{Err: extension.ErrUnavailable}
		}
	}
	return func(ctx context.Context) (any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return job(ctx), nil
	}
}

func isIndeterminate(err error) bool {
	var e *sdk.TransportError
	return errors.As(err, &e) && e.Indeterminate
}
