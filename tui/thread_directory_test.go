package tui

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"slices"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type countedStateWatch struct {
	ctx    context.Context
	id     sdk.ID
	window sdk.Window
}

type countedDirectoryWatch struct {
	ctx    context.Context
	ids    []sdk.ID
	source chan sdk.ThreadDirectoryUpdate
}

// The fixture records calls at the Backend (SDK) boundary, not lease creation
// in the model. Each source delivers one snapshot and then stays open.
type tabWatchFixture struct {
	Backend
	version     uint64
	rows        []sdk.ThreadDirectoryRow
	quiet       map[sdk.ID]bool
	states      []countedStateWatch
	directories []countedDirectoryWatch
}

func (b *tabWatchFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"threadDirectory": b.version}}, nil
}

func (b *tabWatchFixture) Watch(ctx context.Context, id sdk.ID, w sdk.Window) <-chan sdk.StateUpdate {
	b.states = append(b.states, countedStateWatch{ctx, id, w})
	src := make(chan sdk.StateUpdate, 1)
	var update sdk.StateUpdate
	update.Snapshot.State.Thread.ID = id
	update.Snapshot.State.Agent.CWD = "/native/" + string(id)
	if !b.quiet[id] {
		update.Snapshot.State.Streaming = &sdk.Streaming{}
	}
	src <- update
	return src
}

func (b *tabWatchFixture) WatchThreadDirectory(ctx context.Context, ids []sdk.ID) <-chan sdk.ThreadDirectoryUpdate {
	src := make(chan sdk.ThreadDirectoryUpdate, 1)
	src <- tabDirectoryReport(b.rows)
	b.directories = append(b.directories, countedDirectoryWatch{ctx, slices.Clone(ids), src})
	return src
}

func tabDirectoryReport(rows []sdk.ThreadDirectoryRow) sdk.ThreadDirectoryUpdate {
	return sdk.ThreadDirectoryUpdate{Report: sdk.ThreadDirectoryReport{Threads: slices.Clone(rows)}}
}

// Drive only the batch requested by the test, without scheduling its messages
// back through the program (which would start unrelated timers and polls).
func tabCommandMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case nil:
		return nil
	case tea.BatchMsg:
		var out []tea.Msg
		for _, child := range msg {
			out = append(out, tabCommandMessages(child)...)
		}
		return out
	default:
		return []tea.Msg{msg}
	}
}

func (b *tabWatchFixture) checkWatches(t *testing.T, m *Model, totalState, totalDirectory int, usingDirectory bool) {
	t.Helper()
	if len(b.states) != totalState || len(b.directories) != totalDirectory {
		t.Fatalf("watch starts: state=%d want %d, directory=%d want %d", len(b.states), totalState, len(b.directories), totalDirectory)
	}
	live := make(map[sdk.ID]sdk.Window)
	for _, call := range b.states {
		if call.ctx.Err() == nil {
			if _, exists := live[call.id]; exists {
				t.Fatalf("multiple live state watches for %s", call.id)
			}
			live[call.id] = call.window
		}
	}
	want := map[sdk.ID]sdk.Window{m.active: {Min: 50, Max: 100}}
	if !usingDirectory {
		for _, id := range m.tabs {
			if id != m.active {
				want[id] = sdk.Window{Min: 1, Max: 1}
			}
		}
	}
	if !reflect.DeepEqual(live, want) {
		t.Fatalf("live state watches = %+v, want %+v", live, want)
	}
	var directories int
	for _, call := range b.directories {
		if call.ctx.Err() == nil {
			directories++
			if !slices.Equal(call.ids, m.tabs) {
				t.Fatalf("directory scope = %v, want %v", call.ids, m.tabs)
			}
		}
	}
	if usingDirectory && directories != 1 || !usingDirectory && directories != 0 {
		t.Fatalf("live directory watches = %d, using directory = %v", directories, usingDirectory)
	}
}

func TestDirectoryReplacesBackgroundStateWatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ids := []sdk.ID{"1", "2", "3", "4", "5"}
	b := &tabWatchFixture{version: sdk.ThreadDirectoryVersion, quiet: make(map[sdk.ID]bool)}
	for _, id := range ids {
		b.rows = append(b.rows, sdk.ThreadDirectoryRow{ID: id, Title: "Tab " + string(id), Status: string(x.Running),
			Checkout: &sdk.ThreadCheckout{CWD: "/directory/" + string(id), Branch: "topic"}})
	}
	m, err := New(ctx, b, Options{ThreadID: ids[0], ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, id := range ids[1:] {
		m.ensureThread(id)
		m.openedTab(id)
	}
	first := m.beginFeed()().(feedMsg)
	m.receiveFeed(first)
	_, legacyStart := m.Update(m.menuProbe()()) // v1 is not the status directory
	legacy := tabCommandMessages(legacyStart)
	var obsolete tabFeed
	for _, msg := range legacy {
		if v, ok := msg.(tabFeed); ok {
			m.receiveTab(v)
			obsolete = v
		}
	}
	b.checkWatches(t, m, 5, 0, false)

	b.version = sdk.ThreadManagementVersion
	_, start := m.Update(m.menuProbe()())
	var directory directoryFeedMsg
	for _, msg := range tabCommandMessages(start) {
		if v, ok := msg.(directoryFeedMsg); ok {
			directory = v
			m.receiveDirectory(v)
		}
	}
	b.checkWatches(t, m, 5, 1, true)
	if m.receiveTab(obsolete) != nil {
		t.Fatal("a cancelled legacy watch was allowed to continue")
	}
	if got := m.threads[ids[1]].info; got.Title != "Tab 2" || got.Branch != "topic" || got.Workspace != "/directory/2" || got.Status != x.Running {
		t.Fatalf("incorrect background metadata: %+v", got)
	}

	liveDirectory := b.directories[0]
	publish := func(update sdk.ThreadDirectoryUpdate, wantNotification string) {
		t.Helper()
		liveDirectory.source <- update
		msg := awaitDirectory(m.directoryFeed, liveDirectory.source)().(directoryFeedMsg)
		cmd := m.receiveDirectory(msg)
		// Unblock the next read without feeding it back into the model.
		liveDirectory.source <- sdk.ThreadDirectoryUpdate{Err: io.EOF}
		var notices []tea.Msg
		var reads int
		for _, msg := range tabCommandMessages(cmd) {
			if _, ok := msg.(directoryFeedMsg); ok {
				reads++
			} else {
				notices = append(notices, msg)
			}
		}
		if reads != 1 {
			t.Fatalf("directory continued with %d reads, want 1", reads)
		}
		if wantNotification == "" {
			if len(notices) != 0 {
				t.Fatalf("unexpected notifications: %v", notices)
			}
		} else if want := tea.Raw("\x1b]9;" + wantNotification + "\x07")(); len(notices) != 1 || !reflect.DeepEqual(notices[0], want) {
			t.Fatalf("notifications = %v, want %v", notices, want)
		}
	}
	publish(sdk.ThreadDirectoryUpdate{Err: io.EOF}, "")
	b.checkWatches(t, m, 5, 1, true)
	// A directory terminal status cannot eclipse a current native run.
	b.rows[0].Status = string(x.Done)
	publish(tabDirectoryReport(b.rows), "")
	if active := m.threads[ids[0]]; active.info.Status != x.Running || !active.activeRun || !active.stream.Present || active.info.Workspace != "/native/1" {
		t.Fatalf("active native run was overridden: %+v", active.info)
	}
	b.rows[0].Status = string(x.Running)
	publish(tabDirectoryReport(b.rows), "")

	b.rows[1].Checkout = &sdk.ThreadCheckout{Branch: "review", CWD: "/directory/review"}
	for _, status := range []x.ThreadStatus{x.Parked, x.Done, x.Failed, x.Done} {
		// This tab's last native snapshot was still streaming.
		b.rows[1].Title, b.rows[1].Status = "Review", string(status)
		publish(tabDirectoryReport(b.rows), fmt.Sprintf("Review · %s", status))
		publish(tabDirectoryReport(b.rows), "")
		if got := m.threads[ids[1]].info; got.Status != status || got.Title != "Review" || got.Branch != "review" || got.Workspace != "/directory/review" {
			t.Fatalf("background metadata did not follow directory: %+v, want status %s", got, status)
		}
	}

	b.rows[0].Status = string(x.Done) // ignored while the native run is active
	publish(tabDirectoryReport(b.rows), "")
	for _, msg := range tabCommandMessages(m.switchThread(ids[2])) {
		if v, ok := msg.(feedMsg); ok {
			m.receiveFeed(v)
		} else if _, ok := msg.(tea.RawMsg); ok {
			t.Fatal("navigation generated a notification from a cached status")
		}
	}
	b.checkWatches(t, m, 6, 1, true)
	if m.receiveFeed(first) != nil {
		t.Fatal("old active state update was accepted")
	}
	b.rows[0].Status = string(x.Running)
	publish(tabDirectoryReport(b.rows), "")
	b.rows[0].Status = string(x.Done)
	publish(tabDirectoryReport(b.rows), "Tab 1 · "+string(x.Done))
	for _, msg := range tabCommandMessages(m.closeThread(ids[4])) {
		if v, ok := msg.(directoryFeedMsg); ok {
			m.receiveDirectory(v)
		}
	}
	b.checkWatches(t, m, 6, 2, true)
	if m.receiveDirectory(directory) != nil {
		t.Fatal("old directory scope was accepted")
	}

	// A closed directory lease restores fallback watches without waiting for
	// a new active-thread connection. Old directory metadata is discarded.
	b.quiet[ids[1]] = true
	current := b.directories[1]
	close(current.source)
	closed := awaitDirectory(m.directoryFeed, current.source)().(directoryFeedMsg)
	fallback := tabCommandMessages(m.receiveDirectory(closed))
	b.checkWatches(t, m, 9, 2, false)
	if got := m.threads[ids[1]].info; got.Title != "" || got.Branch != "" || got.Workspace != "/native/2" || got.Status != x.Running {
		t.Fatalf("directory metadata survived downgrade: %+v", got)
	}
	for _, msg := range fallback {
		if v, ok := msg.(tabFeed); ok {
			cmd := m.receiveTab(v)
			if v.lease.owner == ids[1] {
				// The legacy Done snapshot must not repeat the notification.
				if got := m.threads[ids[1]].info.Status; got != x.Done || cmd == nil {
					t.Fatalf("fallback did not project Done: %s", got)
				}
				v.lease.cancel()
				if msgs := tabCommandMessages(cmd); len(msgs) != 0 {
					t.Fatalf("duplicate Done on fallback: %v", msgs)
				}
			}
			if v.lease.owner == ids[3] {
				// A genuine legacy transition still notifies.
				v.update.Snapshot.State.Streaming = nil
				cmd := m.receiveTab(v)
				v.lease.cancel() // unblock the read continuation
				want := tea.Raw("\x1b]9;" + threadTitle(m.threads[ids[3]].info) + " · " + string(x.Done) + "\x07")()
				if msgs := tabCommandMessages(cmd); len(msgs) != 1 || !reflect.DeepEqual(msgs[0], want) {
					t.Fatalf("legacy completion notifications = %v, want %v", msgs, want)
				}
			}
		}
	}
}

func TestDirectoryCapabilityLossRestartsLegacyWatches(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	b := &tabWatchFixture{version: sdk.ThreadManagementVersion, rows: []sdk.ThreadDirectoryRow{
		{ID: "2", Title: "Directory title", Status: string(x.Done), Checkout: &sdk.ThreadCheckout{Branch: "directory", CWD: "/checkout"}},
	}}
	m, err := New(ctx, b, Options{ThreadID: "1", ReducedMotion: true, ProjectThread: func(s sdk.Snapshot) ThreadMetadata {
		if s.State.Thread.ID == "2" {
			return ThreadMetadata{Title: "Host title", Branch: "host", Status: x.Scheduled}
		}
		return ThreadMetadata{}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for _, id := range []sdk.ID{"2", "3", "4", "5"} {
		m.ensureThread(id)
		m.openedTab(id)
	}
	m.receiveFeed(m.beginFeed()().(feedMsg))
	// Upgrade before a queued legacy command executes. It must not even
	// call Watch, and the initial negotiated session has one state watch.
	queuedLegacy := m.syncDirectory()
	_, start := m.Update(m.menuProbe()())
	for _, msg := range tabCommandMessages(start) {
		if v, ok := msg.(directoryFeedMsg); ok {
			m.receiveDirectory(v)
		}
	}
	if msgs := tabCommandMessages(queuedLegacy); len(msgs) != 0 {
		t.Fatalf("cancelled legacy start delivered %v", msgs)
	}
	b.checkWatches(t, m, 1, 1, true)

	b.version = 0
	_, fallback := m.Update(m.menuProbe()())
	for _, msg := range tabCommandMessages(fallback) {
		if v, ok := msg.(tabFeed); ok {
			m.receiveTab(v)
		}
	}
	b.checkWatches(t, m, 5, 1, false)
	if got := m.threads["2"].info; got.Status != x.Scheduled || got.Title != "Host title" || got.Branch != "host" || got.Workspace != "/native/2" {
		t.Fatalf("legacy host projection = %+v", got)
	}

	b.version = sdk.ThreadManagementVersion
	_, start = m.Update(m.menuProbe()())
	for _, msg := range tabCommandMessages(start) {
		if v, ok := msg.(directoryFeedMsg); ok {
			m.receiveDirectory(v)
		}
	}
	b.checkWatches(t, m, 5, 2, true)
	if got := m.threads["2"].info; got.Status != x.Scheduled || got.Title != "Host title" || got.Branch != "host" || got.Workspace != "/checkout" {
		t.Fatalf("directory overrode explicit host metadata: %+v", got)
	}
}
