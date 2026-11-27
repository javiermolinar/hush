package tui

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

var workerRowsFixture = []sdk.ReviewWorkerActivity{
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("a", 32), Name: "API reader"}, State: "running", Activity: "Read pinned versions of api.go"},
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("b", 32), Name: "Regression reader"}, State: "succeeded"},
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("c", 32), Name: "Missing receipt"}, State: "unresolved"},
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("d", 32), Name: "Slow reader"}, State: "timed_out"},
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("e", 32), Name: "Stopped reader"}, State: "cancelled"},
	{ReviewHandle: sdk.ReviewHandle{Handle: strings.Repeat("f", 32), Name: "Unavailable reader"}, State: "failed"},
}

type workerBackendFixture struct {
	Backend
	version     uint64
	inspected   []string
	stopped     []string
	stopReply   string
	transcripts int
}

func (b *workerBackendFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"delegation": sdk.DelegationVersion, "workerActivity": b.version}}, nil
}

func (b *workerBackendFixture) WatchReviewActivity(context.Context, sdk.ID) <-chan sdk.ReviewActivityUpdate {
	out := make(chan sdk.ReviewActivityUpdate, 1)
	out <- sdk.ReviewActivityUpdate{Workers: slices.Clone(workerRowsFixture)}
	return out
}

func reviewInspectionFixture(owner sdk.ID, worker sdk.ReviewWorkerActivity) sdk.ReviewInspection {
	w := sdk.ReviewWorker{ReviewHandle: worker.ReviewHandle, State: worker.State}
	if w.State == "succeeded" {
		w.Report = "P1 · api.go:1 · Missing default.\nUnverified: live behavior."
		w.NoticeID = "24"
	}
	return sdk.ReviewInspection{Worker: w, Brief: "Inspect the default path \x1b]52;c;clipboard\a", Target: "stale",
		Scope: sdk.ReviewScope{Tracked: []string{"api.go"}},
		Receipt: sdk.ReviewReceipt{ThreadID: owner, RequestID: "request", Base: strings.Repeat("1", 40), SnapshotID: strings.Repeat("2", 64),
			Selection: sdk.ReviewSelection{Model: sdk.Model{Provider: "fixture", ModelID: "reader"}, Thinking: "high"}, Mode: "parallel"},
		Assessment: &sdk.ReviewAssessment{Evidence: []sdk.SuppliedReviewEvidence{{ID: "excerpt", Label: "Caller excerpt", Text: "3 passed"}}},
	}
}

func (b *workerBackendFixture) InspectReviewWorker(_ context.Context, owner sdk.ID, handle string, transcript bool) (sdk.ReviewInspection, error) {
	b.inspected = append(b.inspected, string(owner)+"/"+handle)
	i := slices.IndexFunc(workerRowsFixture, func(w sdk.ReviewWorkerActivity) bool { return w.Handle == handle })
	v := reviewInspectionFixture(owner, workerRowsFixture[i])
	if transcript {
		b.transcripts++
		v.Transcript = &sdk.ReviewTranscript{Entries: []sdk.ReviewTranscriptEntry{{ID: "4", Kind: "reviewer", Text: "Only public activity."}}}
	}
	return v, nil
}

func (b *workerBackendFixture) StopReviewWorker(_ context.Context, owner sdk.ID, handle string) (sdk.ReviewWorker, error) {
	b.stopped = append(b.stopped, string(owner)+"/"+handle)
	state := b.stopReply
	if state == "" {
		state = "stopping"
	}
	return sdk.ReviewWorker{State: state}, nil
}

func TestReviewWorkersActionsAndScope(t *testing.T) {
	var panes []string
	var paneErr error
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true, OpenReviewPane: func(_ context.Context, parent sdk.ID, handle string) error {
		panes = append(panes, string(parent)+"/"+handle)
		return paneErr
	}})
	s.info.Status = x.Parked
	b := &workerBackendFixture{Backend: inertBackend{}}
	m.backend = b
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("Leave this draft untouched")
	m.Update(m.menuProbe()())
	plain := ansi.Strip(assertFrame(t, m))
	rule := m.frame.ruleY
	if strings.Contains(plain, "workers") || m.workers.token != 0 || m.openWorkers(m.route(m.active, coreID)) != nil {
		t.Fatal("activity appeared without negotiated support")
	}
	b.version = sdk.WorkerActivityVersion
	_, watch := m.Update(m.menuProbe()())
	if watch == nil {
		t.Fatal("activity watch was not started")
	}
	m.Update(watch())
	if m.notice.text != "" || len(m.overlays) != 0 || len(b.inspected) != 0 || b.transcripts != 0 || len(panes) != 0 {
		t.Fatal("attach was not silent and passive")
	}
	assertFrame(t, m)
	hit := slices.IndexFunc(m.frame.hits[rule], func(hit spanHit) bool { return hit.action.ID == workerActivityID })
	if hit < 0 {
		t.Fatal("no hit at the worker count")
	}
	m.Update(tea.MouseClickMsg(tea.Mouse{X: m.frame.hits[rule][hit].first, Y: rule, Button: tea.MouseLeft}))
	roster := m.focusedDialog()
	if roster == nil || roster.worker == nil {
		t.Fatal("click did not open the worker dock")
	}
	assertFrame(t, m)
	if m.frame.ruleY != rule || m.frame.dialogY+m.frame.dialogHeight != rule {
		t.Fatal("roster moved the prompt rule")
	}
	press(m, tea.KeyDown, 0)
	inspect := press(m, tea.KeyEnter, 0)
	m.Update(inspect())
	details := m.focusedDialog()
	frame := assertFrame(t, m)
	for _, want := range []string{"Review evidence", "Stale snapshot", "Missing default", "Leave this draft untouched"} {
		if !strings.Contains(ansi.Strip(frame), want) {
			t.Fatalf("no %q in:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "\x1b]52;") || len(b.stopped) != 0 || b.transcripts != 0 || len(panes) != 0 {
		t.Fatal("evidence leaked controls or caused an action")
	}
	transcript := press(m, tea.KeyEnter, 0)
	m.Update(transcript())
	if b.transcripts != 1 || len(panes) != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Only public activity") {
		t.Fatal("transcript did not stay in the dock until explicit pane open")
	}
	open := press(m, 'o', 0)
	if open == nil || press(m, 'o', 0) != nil {
		t.Fatal("repeated open while placement is pending")
	}
	m.Update(open())
	if !slices.Equal(panes, []string{"1/" + workerRowsFixture[1].Handle}) {
		t.Fatalf("wrong pane target: %v", panes)
	}
	paneErr = sdk.ErrPaneUnavailable
	rejected := press(m, 'o', 0)
	if rejected == nil {
		t.Fatal("confirmed placement blocked another open")
	}
	m.Update(rejected())
	if b.transcripts != 1 || len(b.stopped) != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "the transcript is still here") {
		t.Fatal("pane failure did not preserve the transcript and worker")
	}
	paneErr = nil
	retry := press(m, 'o', 0)
	if retry == nil {
		t.Fatal("rejected placement blocked another open")
	}
	m.Update(retry())
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != details {
		t.Fatal("Escape from transcript did not go back one level")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != roster {
		t.Fatal("Escape from evidence did not return to the roster")
	}
	assertFrame(t, m)
	press(m, tea.KeyUp, 0)
	m.Update(press(m, tea.KeyEnter, 0)())
	assertFrame(t, m)
	m.Update(press(m, 'x', 0)())
	if !slices.Equal(b.stopped, []string{"1/" + workerRowsFixture[0].Handle}) {
		t.Fatalf("Stop did not address exactly one worker: %v", b.stopped)
	}
	lease := m.workers.feedLease
	m.switchThread("2")
	if lease.ctx.Err() == nil || m.focusedDialog() != nil || m.workerBadgeRow(80).text != "" {
		t.Fatal("switch did not detach observation and scope views to the new parent")
	}
	m.Update(workerFeedMsg{lease: lease.token, ok: true, update: sdk.ReviewActivityUpdate{Workers: workerRowsFixture}})
	if len(m.workers.rows) != 0 || len(b.stopped) != 1 {
		t.Fatal("stale feed or tab switch changed workers")
	}
	m.switchThread("1")
	if m.editor.value() != "Leave this draft untouched" || !slices.Equal(m.tabs, []sdk.ID{"1", "2"}) {
		t.Fatalf("inspection changed the draft or tabs: %q %v", m.editor.value(), m.tabs)
	}
	assertFrame(t, m)
	press(m, 'x', tea.ModCtrl)
	press(m, 'i', 0)
	press(m, 'j', 0)
	if d := m.focusedDialog(); d == nil || d.worker == nil || d.worker.handle != "" {
		t.Fatal("leader chord did not open the roster")
	}
}

func TestGoldenReviewWorkerPaneReentry(t *testing.T) {
	for _, tc := range []struct {
		name             string
		replyWhileClosed bool
	}{
		{name: "pending on return"},
		{name: "unknown on return", replyWhileClosed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := workerRowsFixture[0], workerRowsFixture[1]
			var panes []string
			m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true, OpenReviewPane: func(_ context.Context, owner sdk.ID, handle string) error {
				panes = append(panes, string(owner)+"/"+handle)
				if owner == "1" && handle == first.Handle {
					return ErrPaneOpenUnknown
				}
				return nil
			}})
			b := &workerBackendFixture{Backend: inertBackend{}}
			m.backend = b
			m.menuFeaturesFromBackend |= x.WorkerActivity
			m.workers = workerFeed{feedLease: feedLease{owner: m.active}, ready: true, rows: workerRowsFixture}
			m.editor.set("Unsent draft")
			m.openWorkers(m.route(m.active, coreID))
			runKey := func(k rune) tea.Msg {
				t.Helper()
				cmd := press(m, k, 0)
				if cmd == nil {
					t.Fatalf("key %q did not dispatch", k)
				}
				return cmd()
			}
			checkBlocked := func(label string) {
				t.Helper()
				if press(m, 'o', 0) != nil {
					t.Fatalf("duplicate pane open dispatched while %q", label)
				}
				frame := ansi.Strip(assertFrame(t, m))
				if !strings.Contains(frame, label) || !strings.Contains(frame, "Only public activity") || strings.Contains(frame, "o open pane") {
					t.Fatalf("pane state or transcript not shown correctly:\n%s", frame)
				}
			}

			m.Update(runKey(tea.KeyEnter)) // evidence
			m.Update(runKey(tea.KeyEnter)) // transcript
			result := runKey('o')
			press(m, tea.KeyEscape, 0)
			if tc.replyWhileClosed {
				m.Update(result)
			}
			m.Update(runKey(tea.KeyEnter)) // a new dialog for the same transcript
			if !tc.replyWhileClosed {
				checkBlocked("Opening transcript pane…")
				m.Update(result)
			}
			const unknown = "Pane opening was not confirmed; check your terminal"
			checkBlocked(unknown)
			m.Update(runKey('r'))
			checkBlocked(unknown)
			if tc.replyWhileClosed {
				golden(t, "review-worker-pane-unknown", m)
			}

			press(m, tea.KeyEscape, 0)
			press(m, tea.KeyEscape, 0)
			press(m, tea.KeyDown, 0)
			m.Update(runKey(tea.KeyEnter))
			m.Update(runKey(tea.KeyEnter))
			m.Update(runKey('o'))

			m.switchThread("2")
			m.workers.rows, m.workers.ready = workerRowsFixture, true
			m.openWorkers(m.route(m.active, coreID))
			m.Update(runKey(tea.KeyEnter))
			m.Update(runKey(tea.KeyEnter))
			m.Update(runKey('o'))
			if !slices.Equal(panes, []string{"1/" + first.Handle, "1/" + second.Handle, "2/" + first.Handle}) || len(b.stopped) != 0 {
				t.Fatalf("unexpected pane or worker actions: opened %v, stopped %v", panes, b.stopped)
			}
		})
	}
}

func TestReviewWorkerWatchRebaselinesAfterClosure(t *testing.T) {
	for _, tc := range []struct {
		name           string
		probeBeforeEOF bool
	}{
		{"EOF then probe", false},
		{"probe then EOF", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
			b := &workerBackendFixture{Backend: inertBackend{}, version: sdk.WorkerActivityVersion}
			m.backend = b
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m.editor.set("Unsent draft")
			probe := func() tea.Cmd {
				_, cmd := m.Update(m.menuProbe()())
				return cmd
			}
			m.Update(probe()())
			old := m.workers.feedLease
			m.openWorkers(m.route(m.active, coreID))
			d := m.focusedDialog()
			d.selected = 1
			source := make(chan sdk.ReviewActivityUpdate, 1)
			source <- sdk.ReviewActivityUpdate{Err: io.EOF}
			close(source)
			_, next := m.Update(awaitWorkers(old, source)())
			if next == nil || m.workers.token != old.token || !m.workers.failed {
				t.Fatal("error did not leave SDK in control until channel close")
			}
			if tc.probeBeforeEOF && probe() != nil {
				t.Fatal("started a second watch before the first one closed")
			}
			_, restart := m.Update(next())
			if !tc.probeBeforeEOF {
				if restart != nil || m.workers.token != 0 {
					t.Fatal("closed watch retried without a fresh probe")
				}
				restart = probe()
			}
			if restart == nil || old.ctx.Err() == nil || m.workers.token == old.token || !m.workers.failed ||
				!slices.Equal(m.workers.rows, workerRowsFixture) {
				t.Fatal("renegotiation did not replace the closed lease and retain its last baseline")
			}
			frame := ansi.Strip(assertFrame(t, m))
			if !strings.Contains(frame, "disconnected") || !strings.Contains(frame, "Regression reader") {
				t.Fatalf("retained roster was not marked disconnected:\n%s", frame)
			}
			m.Update(workerFeedMsg{lease: old.token, ok: true}) // stale messages cannot clear the retained roster
			baseline := restart().(workerFeedMsg)
			baseline.update.Workers[1].State = "timed_out"
			m.Update(baseline)
			if m.workers.failed || m.workers.rows[1].State != "timed_out" || m.focusedDialog() != d || d.selected != 1 ||
				m.editor.value() != "Unsent draft" || len(b.stopped) != 0 || m.notice.text != "" {
				t.Fatal("rebaseline failed or changed focus, draft, worker lifetime, or notifications")
			}
			if _, retry := m.Update(workerFeedMsg{lease: baseline.lease}); retry != nil || m.workers.token != 0 {
				t.Fatal("a second failure retried without new connection evidence")
			}
		})
	}
}

func TestReviewWorkerRosterRequestsStopWithoutInspection(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	m.menuFeaturesFromBackend |= x.WorkerActivity
	b := &workerBackendFixture{Backend: inertBackend{}, stopReply: "unresolved"}
	m.backend = b
	rows := []sdk.ReviewWorkerActivity{workerRowsFixture[2], {ReviewHandle: workerRowsFixture[0].ReviewHandle, State: "unresolved"}}
	m.workers = workerFeed{feedLease: feedLease{owner: m.active}, ready: true, rows: rows}
	m.openWorkers(m.route(m.active, coreID))
	d := m.focusedDialog()
	assertFrame(t, m)
	if cmd := press(m, 'x', 0); cmd != nil {
		m.Update(cmd())
	} else {
		t.Fatal("roster has no keyboard Stop")
	}
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "Stop was not confirmed") {
		t.Fatal("an unresolved reply did not leave Stop unconfirmed")
	}
	press(m, tea.KeyDown, 0)
	assertFrame(t, m)
	clicked := false
	for y, line := range m.frame.rows {
		plain := ansi.Strip(line)
		if x := strings.Index(plain, "x stop worker"); x >= 0 {
			_, cmd := m.Update(tea.MouseClickMsg(tea.Mouse{X: stringWidth(plain[:x]), Y: y, Button: tea.MouseLeft}))
			if cmd == nil {
				t.Fatal("the rendered Stop hint has no mouse action")
			}
			m.Update(cmd())
			clicked = true
			break
		}
	}
	if !clicked || !slices.Equal(b.stopped, []string{"1/" + rows[0].Handle, "1/" + rows[1].Handle}) || len(b.inspected) != 0 || m.focusedDialog() != d {
		t.Fatalf("wrong Stop targets or inspection: %v, %v", b.stopped, b.inspected)
	}
	m.workers.failed = true
	if strings.Contains(ansi.Strip(assertFrame(t, m)), "x stop worker") || press(m, 'x', 0) != nil {
		t.Fatal("Stop was offered from a disconnected roster")
	}
}

func TestGoldenReviewWorkerRoster(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, _ := demo(t, Options{Fills: FillOn, ReducedMotion: true})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.menuFeaturesFromBackend |= x.WorkerActivity
	m.workers = workerFeed{feedLease: feedLease{owner: m.active}, ready: true, rows: workerRowsFixture}
	m.editor.set("Unsent draft")
	m.openWorkers(m.route(m.active, coreID))
	golden(t, "review-worker-roster", m)
	m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
	preset, _ := presetTheme("panther")
	for _, bg := range []uint32{0xeff1f5, 0x1e1e2e} {
		m.Update(tea.BackgroundColorMsg{Color: themeRGB(bg)})
		for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
			preset.surface = mode
			m.applyTheme(preset)
			for _, width := range []int{42, 80} {
				m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
				frame := ansi.Strip(assertFrame(t, m))
				if m.palette.surfaceMode != mode || !strings.Contains(frame, "Review workers") ||
					m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY || !strings.Contains(frame, "Unsent draft") {
					t.Fatalf("%x %s %d: worker dock does not fit\n%s", bg, mode, width, frame)
				}
			}
		}
	}
}
