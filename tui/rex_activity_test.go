package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func rexReports(t *testing.T, cmd tea.Cmd, want ...string) tea.RawMsg {
	t.Helper()
	if cmd == nil {
		t.Fatalf("no Rex reports; want %q", want)
	}
	msg := cmd()
	raw, ok := msg.(tea.RawMsg)
	if !ok {
		t.Fatalf("got %T, want a terminal report", msg)
	}
	seq, ok := raw.Msg.(rexActivityOutput)
	if !ok {
		t.Fatalf("got non-activity terminal output: %#v", raw.Msg)
	}
	parts := strings.Split(strings.TrimSuffix(string(seq), "\x07"), "\x07")
	if len(parts) != len(want) {
		t.Fatalf("got %q, want exactly %q", parts, want)
	}
	for i, prefix := range want {
		if !strings.HasPrefix(parts[i], "\x1b]7501;"+prefix) {
			t.Fatalf("report %d = %q, want prefix %q", i, parts[i], prefix)
		}
	}
	return raw
}

func rexQuiet(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd != nil {
		t.Fatal("unexpected Rex report command")
	}
}

func rexDemo(t *testing.T) (*Model, *threadState) {
	t.Helper()
	t.Setenv("TERM_PROGRAM", "rex")
	m, tab := demo(t, Options{ReducedMotion: true})
	m.started = true // no actual backend or terminal to initialize in this fixture
	return m, tab
}

func TestRexActivityNativeLifecycle(t *testing.T) {
	m, tab := rexDemo(t)
	observe := func(s sdk.State) {
		t.Helper()
		s.Thread.ID = "1"
		before := tab.info.Status
		tab.info = m.threadInfo(tab, sdk.Snapshot{State: s})
		if cmd := m.desktopStatus("1", before, tab.info.Status); cmd != nil {
			t.Fatal("legacy desktop notification in Rex")
		}
		if v := m.View(); v.ProgressBar != nil || v.ProgramStatus != nil {
			t.Fatal("Rex must not also use renderer progress or unscoped status")
		}
	}
	publish := func(state tea.ProgramState) {
		t.Helper()
		msg := rexReports(t, m.syncRexActivity(), "state="+string(state)+":id=hush/1:app=hush")
		_, cmd := m.Update(msg)
		rexQuiet(t, cmd)
	}
	m.focused = false
	m.threads["1"].info.Title = "Status\x07\x1b]9;injected"
	_, cmd := m.Update(rexReports(t, m.syncRexActivity(), "state=clear:id=hush", "state=idle:id=hush/1:app=hush"))
	rexQuiet(t, cmd)

	observe(sdk.State{Failure: &sdk.Failure{TaskID: "5", Reason: "model_error"}})
	rexQuiet(t, m.syncRexActivity()) // attaching to an old failure isn't a new error
	observe(sdk.State{Run: &sdk.Run{TaskID: "6"}})
	publish(tea.ProgramStateWorking)
	for _, status := range []string{"sleeping", "waiting", "running"} {
		observe(sdk.State{Run: &sdk.Run{TaskID: "6"}, Tools: []sdk.Tool{{CallID: "tool", Status: status}}})
		rexQuiet(t, m.syncRexActivity())
	}
	observe(sdk.State{})
	publish(tea.ProgramStateDone)
	observe(sdk.State{})
	rexQuiet(t, m.syncRexActivity())

	observe(sdk.State{Run: &sdk.Run{TaskID: "7"}})
	publish(tea.ProgramStateWorking)
	observe(sdk.State{Failure: &sdk.Failure{TaskID: "7", Reason: "aborted"}})
	publish(tea.ProgramStateIdle)

	// A failure or completed answer can arrive without a visible running snapshot.
	observe(sdk.State{Failure: &sdk.Failure{TaskID: "8", Reason: "model_error"}})
	publish(tea.ProgramStateError)
	observe(sdk.State{})
	publish(tea.ProgramStateIdle)
	answer := sdk.Entry{ID: "10", Kind: "pi.assistant", Messages: []sdk.Message{{Role: "assistant", StopReason: "stop"}}}
	s := sdk.State{Window: sdk.TranscriptWindow{LastEntryID: &answer.ID, Entries: []sdk.Entry{answer}}}
	observe(s)
	publish(tea.ProgramStateDone)
	observe(s)
	rexQuiet(t, m.syncRexActivity())
}

func TestRexActivityBackgroundTabs(t *testing.T) {
	m, _ := rexDemo(t)
	m.ensureThread("2")
	m.openedTab("2")
	m.directoryIDs = []sdk.ID{"1", "2"}
	m.directoryFeed = feedLease{ctx: t.Context(), token: 1}
	ack := func(want ...string) {
		t.Helper()
		_, cmd := m.Update(rexReports(t, m.syncRexActivity(), want...))
		rexQuiet(t, cmd)
	}
	row := sdk.ThreadDirectoryRow{ID: "2", Title: "Background", Status: string(x.Done), LastMessageMS: 10}
	publish := func() {
		m.receiveDirectory(directoryFeedMsg{token: 1, ok: true, update: tabDirectoryReport([]sdk.ThreadDirectoryRow{
			{ID: "1", Status: string(x.Failed)}, row,
		})})
	}
	publish()
	ack("state=clear:id=hush", "state=idle:id=hush/1:app=hush", "state=idle:id=hush/2:app=hush")
	row.Status = string(x.Running)
	publish()
	ack("state=working:id=hush/2:app=hush")
	row.Status = string(x.Parked)
	publish()
	rexQuiet(t, m.syncRexActivity())
	row.Status, row.LastMessageMS = string(x.Done), 20
	publish()
	ack("state=done:id=hush/2:app=hush")
	row.Title = "Renamed background"
	publish()
	m.switchThread("2")
	rexQuiet(t, m.syncRexActivity())
	m.switchThread("1")
	rexQuiet(t, m.syncRexActivity())
	// Restore the scoped directory lease after navigation in this fixture.
	m.directoryIDs = []sdk.ID{"1", "2"}
	m.directoryFeed = feedLease{ctx: t.Context(), token: 1}
	publish() // baseline the last answer again, without reporting completion
	row.Status = string(x.Running)
	publish()
	ack("state=working:id=hush/2:app=hush")
	row.Status = string(x.Done)
	publish() // the graph went quiet, but the index still shows the old answer
	ack("state=idle:id=hush/2:app=hush")
	row.Status, row.LastMessageMS = string(x.Failed), 30
	publish()
	ack("state=error:id=hush/2:app=hush")
	publish()
	rexQuiet(t, m.syncRexActivity())
	m.closeThread("2")
	ack("state=clear:id=hush/2")
}

func TestRexActivityInputAndOrderedQuit(t *testing.T) {
	m, tab := rexDemo(t)
	initial := rexReports(t, m.syncRexActivity(), "state=clear:id=hush", "state=idle:id=hush/1")
	tab.rexActivity.observe(x.Running, false)
	rexQuiet(t, m.syncRexActivity()) // don't overtake the queued idle report
	_, cmd := m.Update(initial)
	working := rexReports(t, cmd, "state=working:id=hush/1")
	_, cmd = m.Update(working)
	rexQuiet(t, cmd)

	d := &dialog{workTrust: &workspaceTrustDialog{cancel: func() {}}}
	m.sheet = d
	rexQuiet(t, m.syncRexActivity()) // checking trust is not waiting for a human
	d.workTrust.cancel, d.workTrust.decide = nil, func(_ context.Context, _ bool) error { return nil }
	_, cmd = m.Update(rexReports(t, m.syncRexActivity(), "state=blocked:id=hush/input:kind=permission:app=hush"))
	rexQuiet(t, cmd)
	m.sheet = nil
	_, cmd = m.Update(rexReports(t, m.syncRexActivity(), "state=clear:id=hush/input"))
	rexQuiet(t, cmd)

	done := func(x.Context, any, bool) x.Outcome { return nil }
	m.sheet = &dialog{token: 5, input: &promptEditor{}, done: done}
	_, cmd = m.Update(rexReports(t, m.syncRexActivity(), "state=blocked:id=hush/input:kind=question:app=hush"))
	rexQuiet(t, cmd)
	m.sheet = &dialog{token: 6, input: &promptEditor{}, done: done}
	blocked := rexReports(t, m.syncRexActivity(), "state=blocked:id=hush/input:kind=question:app=hush")
	m.sheet = nil
	// Quit while the input report is still queued. Never claim success or
	// leave a working record for Rex to interpret as a shell completion.
	cmd = press(m, 'd', tea.ModCtrl)
	rexQuiet(t, cmd)
	_, cmd = m.Update(blocked)
	last := rexReports(t, cmd, "state=idle:id=hush/1", "state=clear:id=hush/input")
	_, cmd = m.Update(last)
	if cmd == nil {
		t.Fatal("quit didn't resume after writing status")
	}
	if msg := cmd(); msg != (tea.QuitMsg{}) {
		t.Fatalf("got %#v instead of quit after retiring activity", msg)
	}
}
