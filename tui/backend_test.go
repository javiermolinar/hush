package tui

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestBackendE2E(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "hush-tui-test-") // keep the Unix socket path short on macOS
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	skill := filepath.Join(root, "skills", "fixture-review")
	if err := os.MkdirAll(skill, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: fixture-review\ndescription: An integration skill\n---\nTest instructions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "hush.sock")
	cmd := exec.CommandContext(ctx, "node", bundle, "--faux", "--data-dir", root, "--pi-events")
	cmd.Env = append(os.Environ(), "HOME="+root, "PI_CODING_AGENT_DIR="+root, "NODE_NO_WARNINGS=1")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("backend not ready: %q %v", scan.Text(), scan.Err())
	}
	sdkClient := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: map[string][]uint64{"piEvents": {sdk.PiEventsVersion}, "intro": {sdk.IntroActionsVersion, sdk.IntroVersion}, "threadDirectory": {sdk.ThreadManagementVersion, sdk.ThreadDirectoryVersion}}})
	defer sdkClient.Close()
	thread, err := sdkClient.NewThread(ctx, sdk.NewThread{CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	var sawStream, sawTool bool
	m, err := New(ctx, sdkClient, Options{ThreadID: thread.ID, ReducedMotion: true, Intro: &IntroOptions{}, Extensions: []x.Registration{{
		ID: "test/backend", APIVersion: x.APIVersion, Events: []x.Subscription{
			{Kind: x.StreamChanged, Observe: func(_ x.Context, e x.Event) { sawStream = sawStream || e.Stream.Present }},
			{Kind: x.ToolOutput, Observe: func(_ x.Context, e x.Event) { sawTool = sawTool || strings.Contains(e.Tool.Output, "fixture entered") }},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.editor.area.Focus()
	first := m.beginFeed()().(feedMsg)
	if first.update.Err != nil || !first.ok {
		t.Fatalf("first feed %+v", first)
	}
	m.receiveFeed(first)
	report, ok := m.beginIntro()().(backendReportMsg)
	if !ok || !report.ok || report.update.Err != nil {
		t.Fatalf("failed to load negotiated intro report: %+v", report)
	}
	m.Update(report)
	body := ansi.Strip(assertFrame(t, m))
	if row := ansi.Strip(m.frame.rows[m.height-1]); !strings.Contains(row, filepath.Base(root)) || !strings.Contains(row, "faux-1") || !strings.Contains(row, "ctx ") {
		t.Fatalf("missing pinned metadata: %q", row)
	}
	for _, want := range []string{"● attached", " open · ", "skills      1 loaded", "context", "(estimate)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing live intro field %q:\n%s", want, body)
		}
	}
	m.editor.set("/skills")
	list := press(m, tea.KeyEnter, 0)
	if list == nil {
		t.Fatal("the backend did not enable /skills")
	}
	m.finishWork(list().(workMsg))
	if m.sheet == nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "fixture-review") {
		t.Fatal("/skills did not show the backend's loaded skill")
	}
	press(m, tea.KeyEnter, 0)
	if m.sheet != nil || m.editor.value() != "/skill:fixture-review " {
		t.Fatal("choosing a skill did not stage an invocation for review")
	}
	state := m.threads[m.active]
	wait := func(predicate func() bool) {
		t.Helper()
		for !predicate() {
			msg := awaitFeed(m.feed, first.source)()
			f, ok := msg.(feedMsg)
			if !ok || !f.ok || f.update.Err != nil {
				t.Fatalf("feed lost: %+v; context: %v", f, ctx.Err())
			}
			m.receiveFeed(f)
			assertFrame(t, m)
		}
	}
	send := func(s string, mode sdk.BusyMode) {
		t.Helper()
		m.editor.set(s)
		cmd := m.sendPrompt(mode)
		if cmd == nil {
			t.Fatal("send did not produce a command")
		}
		w := cmd().(workMsg)
		if w.err != nil {
			t.Fatal(w.err)
		}
		m.finishWork(w)
	}
	skillSubmit := press(m, tea.KeyEnter, 0)
	if !state.sending || skillSubmit == nil {
		t.Fatal("staged /skill invocation was not routed to normal submission")
	}
	msg := skillSubmit().(workMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	m.finishWork(msg)
	wait(func() bool { return strings.Contains(state.lastAnswer, "Test instructions") && !threadBusy(state) })
	directory, err := sdkClient.ThreadDirectory(ctx, 50, "")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, row := range directory.Threads {
		if row.ID == thread.ID && row.Title == "fixture-review skill" && row.LastMessageMS != 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("directory did not project a visible title without skill bodies: %+v", directory)
	}
	send("hello **durable**", sdk.Steer)
	wait(func() bool {
		return strings.Contains(state.lastAnswer, "hello **durable**") && !threadBusy(state)
	})
	if !strings.Contains(ansi.Strip(m.View().Content), "Faux: hello durable") {
		t.Fatal("assistant Markdown not visible")
	}
	waitReport := func(predicate func() bool) {
		t.Helper()
		for !predicate() {
			msg, ok := awaitBackendReport(m.reportFeed, report.source)().(backendReportMsg)
			if !ok || !msg.ok || msg.update.Err != nil {
				t.Fatalf("footer report lost after submit: %+v", msg)
			}
			m.Update(msg)
		}
	}
	waitReport(func() bool { return !state.footer.context.Estimated && state.info.Title == "fixture-review skill" })
	if row := ansi.Strip(m.frame.rows[m.height-1]); state.footer.context.Tokens == 0 || !state.intro.frozen || !strings.Contains(ansi.Strip(m.View().Content), "ctx ") {
		t.Fatalf("missing reported context after submit: %q", row)
	}

	// The title key opens the real rename prompt. Its new title arrives on the
	// report watch even after the intro itself has frozen in scrollback.
	m.Update(m.menuProbe()())
	press(m, 'r', tea.ModCtrl)
	dialog := m.focusedDialog()
	if dialog == nil || dialog.input == nil || dialog.title != "Rename thread" {
		t.Fatal("rename key did not open the title editor")
	}
	dialog.input.set("  Review  deployment  ")
	rename := press(m, tea.KeyEnter, 0)()
	if batch, ok := rename.(tea.BatchMsg); ok {
		rename = batch[0]() // only the backend operation, not the editor's refocus command
	}
	m.finishWork(rename.(workMsg))
	waitReport(func() bool { return state.info.Title == "Review deployment" })

	send("!error", sdk.Steer)
	wait(func() bool { return state.activity.failure.TaskID != "" })
	waitReport(func() bool { return state.currentIssue() != nil })
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "synthetic failure · /retry · /model · /dismiss") {
		t.Fatalf("live line missed the stored cause and recovery actions:\n%s", body)
	}
	failed := state.activity.failure.TaskID
	retry := m.runAction(m.host.byID[retryFailureID], "")
	if ret := retry().(workMsg); ret.err != nil {
		t.Fatal(ret.err)
	} else {
		m.finishWork(ret)
	}
	wait(func() bool { return state.activity.failure.TaskID != "" && state.activity.failure.TaskID != failed })
	waitReport(func() bool { return state.currentIssue() != nil })
	dismiss := m.runAction(m.host.byID[dismissFailureID], "")
	if ret := dismiss().(workMsg); ret.err != nil {
		t.Fatal(ret.err)
	} else {
		m.finishWork(ret)
	}
	waitReport(func() bool { return state.dismissedFailure(state.activity.failure.TaskID) })
	if state.info.Status != x.Idle || strings.Contains(ansi.Strip(assertFrame(t, m)), "no answer · synthetic failure") {
		t.Fatal("dismissed run still shown as an active failure")
	}
	// A real fixture tool holds the run open while a steer and follow-up are
	// admitted. Native abort waits for idle and withdraws the remaining queue.
	send("!crash", sdk.Steer)
	wait(func() bool { return state.info.Status == x.Running && sawTool })
	send("steer this", sdk.Steer)
	send("then review", sdk.FollowUp)
	wait(func() bool { return len(state.queue) >= 2 })
	if state.queue[0].Mode != "steer" || state.queue[1].Mode != "followUp" {
		t.Fatalf("wrong queue: %+v", state.queue)
	}
	press(m, tea.KeyEscape, 0)
	if state.aborting || m.stopAt.IsZero() {
		t.Fatal("first Esc should only arm stop")
	}
	stop := press(m, tea.KeyEscape, 0)
	if stop == nil {
		t.Fatal("second Esc didn't request abort")
	}
	w := stop().(workMsg)
	if w.err != nil {
		t.Fatal(w.err)
	}
	m.finishWork(w)
	if got := m.editor.value(); got != "" || !strings.Contains(m.notice.text, "cannot verify withdrawals") {
		t.Fatalf("legacy stop must not re-draft the observed queue: %q, %q", got, m.notice.text)
	}
	wait(func() bool { return state.info.Status != x.Running && len(state.queue) == 0 })
	if !sawStream {
		t.Log("short reference provider spans may coalesce; tool output and streaming-layout tests assert the observed path and per-token frames separately")
	}

	// Start with a different stored configuration so an ignored menu action
	// cannot pass by observing the backend's default model and level.
	if err := sdkClient.ConfigureThread(ctx, thread.ID, sdk.Configure{
		Model: sdk.Clear[sdk.Model](), ThinkingLevel: sdk.Set("low"),
	}); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { return state.info.Model == (sdk.Model{}) && state.info.Thinking == "low" })
	m.Update(m.menuProbe()())
	if !m.contextFor(m.active).Features.Has(x.Catalog) {
		t.Fatal("live catalog not available")
	}
	lookup := m.runAction(m.host.byID[menusID+".model"], "")
	m.finishWork(lookup().(workMsg))
	if m.sheet == nil || m.sheet.title != "Select model" {
		t.Fatalf("catalog menu did not open: %+v", m.notice)
	}
	m.sheet.query = "faux/faux-1"
	m.sheet.filter()
	if choice, ok := m.sheet.choice(); !ok || choice.Label != "faux/faux-1" {
		t.Fatalf("fixture model missing from catalog: %+v", choice)
	}
	m.acceptChoice(m.sheet)
	if m.sheet == nil || m.sheet.title != "Thinking level" {
		t.Fatal("model thinking menu did not open")
	}
	change := m.acceptChoice(m.sheet)
	changed := change().(workMsg)
	if changed.err != nil {
		t.Fatal(changed.err)
	}
	m.finishWork(changed)
	wait(func() bool { return state.info.Model.ModelID == "faux-1" && state.info.Thinking == "off" })

	// Fork through the picker and verify the actual backend history stops at
	// the exact selected immutable entry, not at the latest turn.
	at := state.ids[0]
	m.editor.set("")
	press(m, tea.KeyEscape, 0)
	query := press(m, tea.KeyEscape, 0)
	if query == nil {
		t.Fatal("double Escape did not open fork picker")
	}
	m.finishWork(query().(workMsg))
	if m.sheet == nil {
		t.Fatal("fork picker did not open")
	}
	for i, j := range m.sheet.filtered {
		if m.sheet.choices[j].ID == string(at) {
			m.sheet.selected = i
			break
		}
	}
	fork := m.acceptChoice(m.sheet)
	m.finishWork(fork().(workMsg))
	if m.active == thread.ID || !m.active.Valid() {
		t.Fatal("fork did not activate the new thread")
	}
	page, err := sdkClient.History(ctx, m.active, sdk.Before(nil), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].ID != at {
		t.Fatalf("fork did not end at %s: %+v", at, page)
	}

	// Start the scoped directory lease for both open tabs. The previous
	// commands were driven manually, without scheduling their Batch output.
	m.directoryEnabled = false
	m.syncDirectory()
	m.directoryEnabled = true
	leaseMsg := m.syncDirectory()().(directoryFeedMsg)
	m.receiveDirectory(leaseMsg)
	child := m.threads[m.active]
	waitDirectory := func(predicate func() bool) {
		t.Helper()
		for !predicate() {
			msg, ok := awaitDirectory(m.directoryFeed, leaseMsg.source)().(directoryFeedMsg)
			if !ok || !msg.ok || msg.update.Err != nil {
				t.Fatalf("directory feed lost: %+v (%v)", msg, ctx.Err())
			}
			m.receiveDirectory(msg)
		}
	}
	if _, err := sdkClient.RenameThread(ctx, thread.ID, "Background title"); err != nil {
		t.Fatal(err)
	}
	waitDirectory(func() bool {
		return state.info.Title == "Background title" && child.info.Title == "/skill:fixture-review" && child.info.Status == x.Idle
	})
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Background title") || !strings.Contains(frame, "◌ /skill:fixture-review") {
		t.Fatalf("missing directory tab state:\n%s", frame)
	}
	if !m.contextFor(m.active).Features.Has(x.Archive | x.Worktrees) {
		t.Fatal("directory actions were not capability-gated")
	}
	m.runAction(m.host.byID[menusID+".archive"], "")
	if m.sheet == nil || m.sheet.title != "Archive thread?" {
		t.Fatal("native archive did not require confirmation")
	}
	assertFrame(t, m)
	archive := press(m, 'y', 0)
	m.finishWork(archive().(workMsg))
	waitDirectory(func() bool { return child.info.Archived })
	threads := m.runAction(m.host.byID[switcherID], "")
	m.finishWork(threads().(workMsg))
	for _, choice := range m.sheet.choices {
		if choice.ID == string(child.info.ID) {
			t.Fatal("picker listed archived thread")
		}
	}
}
