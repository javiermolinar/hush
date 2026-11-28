package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const planFixtureWorkspace = "/work/profiles"

func planFixture(workspace string) sdk.Plan {
	readOnly := true
	return sdk.Plan{ID: "fixture-plan", Revision: 7, Title: "Workspace profiles", Workspace: workspace, Runs: []sdk.PlanActiveRun{}, Steps: []sdk.PlanStep{
		{ID: "save", Title: "Save a profile", Prompt: "Store a profile under a stable name.\nCheck that the profile has the expected name.", Status: "pending"},
		{ID: "restore", Title: "Restore saved profiles", Prompt: "Read the saved profile and restore its settings.", Requires: []string{"save"}, Status: "pending"},
		{ID: "doc", Title: "Document discovery", Prompt: "Describe the profile discovery path.", Status: "blocked", Reason: "Profile path undecided"},
		{ID: "review", Title: "Review the profile code", Prompt: "Review save and restore for lost settings.", Requires: []string{"restore"},
			Delegate: &sdk.PlanDelegate{ReadOnly: &readOnly, Thinking: "high"}, Status: "pending"},
	}}
}

// planPickerFixture opens a ready plan of the thread's own workspace.
// Without a thread it uses the launch directory.
func planPickerFixture(m *Model, s *threadState) *dialog {
	m.menuFeaturesFromBackend |= x.Plans
	if m.active.Valid() && s.info.Workspace == "" {
		s.info.Workspace = planFixtureWorkspace
	}
	cwd := m.planCWD(m.active)
	p := m.newPlanPicker(m.active, cwd, "fixture-plan")
	p.ready, p.snapshot = true, sdk.PlanSnapshot{Revision: strings.Repeat("d", 64), Plan: planFixture(cwd)}
	d := &dialog{route: m.route(m.active, menusID), token: m.token(), plan: p}
	m.pushDialog(d)
	return d
}

type planFixtureBackend struct {
	inertBackend
	run      func(sdk.ID, sdk.PlanSelection) (sdk.ID, error)
	worktree func(sdk.PlanWorktreeSource, sdk.PlanSelection) (sdk.PlanWorktreeRun, error)
}

func (planFixtureBackend) Hello(context.Context) (sdk.Hello, error) { return sdk.Hello{}, nil }
func (planFixtureBackend) WatchPlans(context.Context, string, int) <-chan sdk.PlanListUpdate {
	return nil
}
func (planFixtureBackend) WatchPlan(context.Context, string, string) <-chan sdk.PlanUpdate {
	return nil
}
func (planFixtureBackend) WatchPlanActivity(context.Context, sdk.ID) <-chan sdk.PlanActivityUpdate {
	return nil
}
func (planFixtureBackend) PlanHistory(context.Context, string, string, sdk.ID) (sdk.PlanHistoryPage, error) {
	return sdk.PlanHistoryPage{Runs: []sdk.PlanRunRecord{}}, nil
}
func (planFixtureBackend) SetPlanClosed(context.Context, string, string, bool) error { return nil }
func (b planFixtureBackend) RunPlan(_ context.Context, thread sdk.ID, s sdk.PlanSelection) (sdk.ID, error) {
	return b.run(thread, s)
}
func (b planFixtureBackend) RunPlanInWorktree(_ context.Context, src sdk.PlanWorktreeSource, s sdk.PlanSelection) (sdk.PlanWorktreeRun, error) {
	return b.worktree(src, s)
}

func testPlanFrameGeometry(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	// Model-authored titles and prompts are not terminal controls.
	step := &d.plan.snapshot.Plan.Steps[0]
	step.Title += " \x1b]52;c;clipboard\a"
	step.Prompt = strings.Repeat("Long and wide 界 detail ", 35) + "\nEnd of the step prompt"
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {120, 30}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFrame(t, m)
		press(m, tea.KeyTab, 0)
		if content := ansi.Strip(assertFrame(t, m)); !strings.Contains(content, "Long and wide") {
			t.Fatalf("details are not visible at %v:\n%s", size, content)
		}
		for range 40 {
			press(m, tea.KeyDown, 0)
		}
		if content := ansi.Strip(assertFrame(t, m)); !strings.Contains(content, "End of the step prompt") {
			t.Fatalf("cannot reach the end of the details at %v:\n%s", size, content)
		}
		press(m, tea.KeyTab, 0)
		if d.plan.focus != 0 || len(d.plan.selected) != 0 {
			t.Fatal("scrolling details changed the step selection")
		}
	}
}

func TestGoldenPlanPicker(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d := planPickerFixture(m, s)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Run 1 here @ r7  Save a profile") {
		t.Fatalf("Enter does not say it runs the focused step:\n%s", body)
	}
	golden(t, "plan-steps", m)
	press(m, tea.KeySpace, 0)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Run 2 here @ r7  Save a profile → Restore saved profiles") {
		t.Fatalf("the selection preview is missing:\n%s", body)
	}
	golden(t, "plan-selection", m)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeyTab, 0)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Delegate: read-only subagent") || !strings.Contains(body, "Thinking: high") {
		t.Fatalf("the delegate specification is missing:\n%s", body)
	}
	golden(t, "plan-delegate", m)
	press(m, tea.KeyEscape, 0)

	d = planPickerFixture(m, s)
	d.plan.snapshot.Plan.Steps[0].Status = "done"
	d.plan.snapshot.Plan.Runs = []sdk.PlanActiveRun{{ID: "41", ThreadID: "8", StepIDs: []string{"restore"}}}
	press(m, tea.KeySpace, 0)
	if len(d.plan.selected) != 0 {
		t.Fatal("a done step was selectable")
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	if body := ansi.Strip(assertFrame(t, m)); len(d.plan.selected) != 0 || !strings.Contains(body, "Running in thread #8") {
		t.Fatalf("a step in an active run was selectable or unexplained:\n%s", body)
	}
	golden(t, "plan-running", m)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	golden(t, "plan-narrow", m)
}

func TestGoldenPlanZoom(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	planPickerFixture(m, s)
	press(m, 'f', 0)
	golden(t, "plan-full", m)
}

func TestPlanDockUsesInputRuleWithoutMovingDraft(t *testing.T) {
	for _, width := range []int{40, 116} {
		m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 36})
		enableGitDiffFixture(m, s)
		s.queue = []sdk.QueueItem{{Mode: "followUp", Text: "queued example"}}
		m.editor.set("Keep this draft\nincluding its second line")
		assertFrame(t, m)
		rule := m.frame.ruleY
		prompt := ansi.Strip(strings.Join(m.frame.rows[rule+1:], "\n"))
		d := planPickerFixture(m, s)
		check := func() {
			t.Helper()
			assertFrame(t, m)
			f := m.frame
			if f.ruleY != rule || f.dialogY+f.dialogHeight != rule {
				t.Fatalf("width %d: panel [%d,%d) did not dock to input rule %d (now %d)", width, f.dialogY, f.dialogY+f.dialogHeight, rule, f.ruleY)
			}
			if got := ansi.Strip(strings.Join(f.rows[rule+1:], "\n")); got != prompt {
				t.Fatalf("width %d: prompt moved or changed:\n%s\nwant\n%s", width, got, prompt)
			}
			if row := ansi.Strip(f.rows[rule]); !strings.Contains(row, "± 4 files") {
				t.Fatalf("width %d: input rule lost its changes badge: %q", width, row)
			}
		}
		check()
		before := m.frame.dialogY
		d.plan.snapshot.Plan.Steps[0].Prompt = strings.Repeat("More detail about the step. ", 32)
		d.plan.snapshot.Revision = strings.Repeat("e", 64)
		press(m, tea.KeyTab, 0)
		check()
		if m.frame.dialogY >= before {
			t.Fatal("a taller panel did not grow upward")
		}
		press(m, tea.KeyEscape, 0)
		assertFrame(t, m)
		if m.frame.ruleY != rule || m.editor.value() != "Keep this draft\nincluding its second line" {
			t.Fatal("closing the picker moved or changed the draft")
		}
	}
}

func TestPlanRunHereRetriesTheSameRequestAndDiscardsStaleClicks(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	assertFrame(t, m)
	f := m.frame
	row := -1
	for i, v := range f.dialogChoices {
		if v == 1 {
			row = f.dialogY + i
			break
		}
	}
	if row < 0 {
		t.Fatal("no visible row target")
	}
	click := tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 2, Y: row, Button: tea.MouseLeft})
	d.plan.snapshot.Revision = strings.Repeat("e", 64)
	m.Update(click)
	if len(d.plan.selected) != 0 {
		t.Fatal("a click from an older frame changed the current selection")
	}
	assertFrame(t, m)
	m.Update(click)
	if !d.plan.selected["restore"] {
		t.Fatal("a click did not select the drawn step")
	}
	assertFrame(t, m)
	type call struct {
		thread    sdk.ID
		selection sdk.PlanSelection
	}
	var calls []call
	m.backend = planFixtureBackend{run: func(thread sdk.ID, s sdk.PlanSelection) (sdk.ID, error) {
		calls = append(calls, call{thread, s})
		if len(calls) == 1 {
			return "", &sdk.TransportError{Err: context.DeadlineExceeded, Indeterminate: true}
		}
		return "99", nil
	}}
	msg, ok := press(m, tea.KeyEnter, 0)().(workMsg)
	if !ok || msg.err == nil {
		t.Fatalf("expected an indeterminate reply, got %+v", msg)
	}
	m.Update(msg)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "retry the same run") {
		t.Fatalf("a lost reply should offer the same request, not a fresh run:\n%s", body)
	}
	// The request belongs to the thread: another plan waits for it, and
	// reopening this plan offers the same retry with its selection.
	press(m, tea.KeyEscape, 0)
	other := planPickerFixture(m, s)
	other.plan.id, other.plan.snapshot.Plan.ID = "other-plan", "other-plan"
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Retry the unconfirmed run of Workspace profiles first") {
		t.Fatalf("another plan of the thread started while a run was unconfirmed:\n%s", body)
	}
	press(m, tea.KeyEnter, 0)
	press(m, tea.KeyEscape, 0)
	d = planPickerFixture(m, s)
	if body := ansi.Strip(assertFrame(t, m)); !d.plan.selected["restore"] || !strings.Contains(body, "retry the same run") {
		t.Fatalf("reopening the plan lost the unconfirmed request:\n%s", body)
	}
	press(m, tea.KeyUp, 0)
	press(m, tea.KeySpace, 0) // the selection is locked while a request is outstanding
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if len(calls) != 2 || !reflect.DeepEqual(calls[0], calls[1]) || calls[0].thread != s.info.ID ||
		!reflect.DeepEqual(calls[0].selection.StepIDs, []string{"restore"}) || calls[0].selection.Revision != 7 {
		t.Fatalf("the retry changed the request: %+v", calls)
	}
	if m.focusedDialog() == d {
		t.Fatal("a started run did not return to chat")
	}
}

type startupPlanBackend struct {
	planFixtureBackend
	create func() (sdk.Thread, error)
}

func (b startupPlanBackend) NewThread(context.Context, sdk.NewThread) (sdk.Thread, error) {
	return b.create()
}

func TestStartupRunHereRetryReusesTheThreadItCreated(t *testing.T) {
	m := modelFor(t, Options{WorkspaceLocal: true, ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 25})
	m.editor.set("keep this draft")
	d := planPickerFixture(m, m.threads[""])
	if d.plan.cwd != "/workspace" {
		t.Fatalf("a startup plan should use the launch directory, got %q", d.plan.cwd)
	}
	created := 0
	var targets []sdk.ID
	m.backend = startupPlanBackend{create: func() (sdk.Thread, error) {
		created++
		return sdk.Thread{ID: sdk.ID(fmt.Sprint(40 + created))}, nil
	}, planFixtureBackend: planFixtureBackend{run: func(thread sdk.ID, _ sdk.PlanSelection) (sdk.ID, error) {
		targets = append(targets, thread)
		if len(targets) == 1 {
			return "", &sdk.TransportError{Err: context.DeadlineExceeded, Indeterminate: true}
		}
		return "77", nil
	}}}
	assertFrame(t, m)
	msg, ok := press(m, tea.KeyEnter, 0)().(workMsg)
	if !ok || msg.err == nil {
		t.Fatalf("expected an unconfirmed run, got %+v", msg)
	}
	m.Update(msg)
	if body := ansi.Strip(assertFrame(t, m)); m.planRequest(d) == nil || !strings.Contains(body, "retry the same run") {
		t.Fatalf("an unconfirmed run should keep its request:\n%s", body)
	}
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if created != 1 || !reflect.DeepEqual(targets, []sdk.ID{"41", "41"}) {
		t.Fatalf("the retry created another thread: created %d, runs in %v", created, targets)
	}
	if m.active != "41" || m.editor.value() != "keep this draft" || m.focusedDialog() != nil {
		t.Fatal("the run did not open its thread with the draft intact")
	}
}

func TestPlanRunsOnlyTheDrawnSelection(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	m.backend = planFixtureBackend{run: func(sdk.ID, sdk.PlanSelection) (sdk.ID, error) { return "9", nil }}
	assertFrame(t, m)
	f := m.frame
	bar := -1
	for i, v := range f.dialogChoices {
		if v == planRunTarget {
			bar = f.dialogY + i
		}
	}
	if bar < 0 {
		t.Fatal("the consequence line has no run target")
	}
	click := tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 2, Y: bar, Button: tea.MouseLeft})
	press(m, tea.KeyDown, 0) // Enter would now run Restore, but the frame still offers Save
	press(m, tea.KeyEnter, 0)
	m.Update(click)
	press(m, 'w', 0)
	if m.planRequest(d) != nil {
		t.Fatalf("a run started from a stale frame: %+v", m.planRequest(d).selection)
	}
	assertFrame(t, m)
	d.plan.snapshot.Revision = strings.Repeat("e", 64) // a plan update arrives before the next frame
	m.Update(click)
	if m.planRequest(d) != nil {
		t.Fatal("a run started across a plan update the frame didn't show")
	}
	assertFrame(t, m)
	m.Update(click)
	if m.planRequest(d) == nil || !reflect.DeepEqual(m.planRequest(d).selection.StepIDs, []string{"restore"}) {
		t.Fatalf("the drawn run bar did not run the drawn selection: %+v", m.planRequest(d))
	}
}

func TestPlanWorktreeRunSendsTheSelectionFromThisThread(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	assertFrame(t, m)
	var got []sdk.PlanWorktreeSource
	var selection sdk.PlanSelection
	m.backend = planFixtureBackend{worktree: func(src sdk.PlanWorktreeSource, sel sdk.PlanSelection) (sdk.PlanWorktreeRun, error) {
		got, selection = append(got, src), sel
		if len(got) == 1 {
			return sdk.PlanWorktreeRun{}, &sdk.ServerError{Code: "invalid_request", Message: "Could not create the worktree: no committed HEAD."}
		}
		out := sdk.PlanWorktreeRun{RunID: "12", ThreadID: "77", Title: "Profiles"}
		return out, nil
	}}
	if msg, ok := press(m, 'w', 0)().(workMsg); !ok || msg.err == nil {
		t.Fatalf("expected a rejection, got %+v", msg)
	} else {
		m.Update(msg)
	}
	if body := ansi.Strip(assertFrame(t, m)); m.planRequest(d) != nil || !strings.Contains(body, "no committed HEAD") {
		t.Fatalf("a rejected worktree run should explain itself and allow a new request:\n%s", body)
	}
	workflowWork(t, m, press(m, 'w', 0))
	if len(got) != 2 || got[0] != (sdk.PlanWorktreeSource{Thread: s.info.ID}) || !reflect.DeepEqual(selection.StepIDs, []string{"restore"}) {
		t.Fatalf("unexpected worktree request: %+v %+v", got, selection)
	}
	if m.planRequest(d) != nil || len(d.plan.selected) != 0 {
		t.Fatal("a started worktree run kept its request or selection")
	}
}

func TestPlanRunNeedsAThreadInThePlansWorkspace(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	d.plan.snapshot.Plan.Workspace = "/work/elsewhere"
	calls := 0
	m.backend = planFixtureBackend{run: func(sdk.ID, sdk.PlanSelection) (sdk.ID, error) { calls++; return "1", nil },
		worktree: func(sdk.PlanWorktreeSource, sdk.PlanSelection) (sdk.PlanWorktreeRun, error) {
			calls++
			return sdk.PlanWorktreeRun{}, nil
		}}
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "Runs from elsewhere") || strings.Contains(body, "space select") {
		t.Fatalf("a plan of another workspace should explain where it runs:\n%s", body)
	}
	press(m, tea.KeyEnter, 0)
	press(m, 'w', 0)
	if calls != 0 {
		t.Fatal("a plan of another workspace started a run")
	}
}

func TestGoldenPlanListAndHistory(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.menuFeaturesFromBackend |= x.Plans
	s.info.Workspace = planFixtureWorkspace
	rows := []sdk.PlanSummary{
		{ID: "a", Title: "Workspace profiles", Revision: 7, Workspace: planFixtureWorkspace, Total: 4, Done: 1, Blocked: 1},
		{ID: "b", Title: "Export settings", Revision: 2, Workspace: planFixtureWorkspace, Total: 3, Running: true},
		{ID: "c", Title: "Theme cleanup", Revision: 1, Workspace: "/work/themes", Total: 2, Done: 2},
		{ID: "d", Title: "Old migration", Revision: 4, Workspace: planFixtureWorkspace, Total: 5, Closed: true},
	}
	m.pushDialog(&dialog{route: m.route(m.active, menusID), token: m.token(),
		planWorkspace: &planWorkspacePicker{cwd: planFixtureWorkspace, ready: true, rows: rows}})
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"This workspace", "Other workspaces", "Closed plans", "1 blocked", "Running", "Complete", "↵ Open  Workspace profiles"} {
		if !strings.Contains(body, want) {
			t.Fatalf("plan list lacks %q:\n%s", want, body)
		}
	}
	golden(t, "plan-list", m)
	press(m, tea.KeyEscape, 0)
	titles := map[string]string{"save": "Save a profile", "restore": "Restore saved profiles"}
	page := sdk.PlanHistoryPage{Runs: []sdk.PlanRunRecord{
		{ID: "51", Revision: 7, ThreadID: "8", StepIDs: []string{"save", "restore"}, State: "finished",
			Outcomes: []sdk.PlanOutcome{{StepID: "save", Status: "done", Detail: "Wrote profile.json"}}},
		{ID: "40", Revision: 6, ThreadID: "9", StepIDs: []string{"save"}, State: "failed", Outcomes: []sdk.PlanOutcome{},
			Failure: "The run turn did not finish. Inspect any changes before retrying.", Worktree: true},
	}}
	m.pushDialog(&dialog{route: m.route(m.active, menusID), token: m.token(),
		planHistory: &planHistoryPicker{cwd: planFixtureWorkspace, id: "a", title: "Workspace profiles", titles: titles, page: page}})
	body = ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Run of 2 steps", "Save a profile: done · Wrote", "Restore saved profiles: not", "↵ Open  thread #8"} {
		if !strings.Contains(body, want) {
			t.Fatalf("history lacks %q:\n%s", want, body)
		}
	}
	golden(t, "plan-history", m)
}

func installPlanStripFixture(m *Model, t *threadState, total int) {
	m.menuFeaturesFromBackend |= x.Plans
	m.now = time.Unix(1000, 0)
	t.info.Status = x.Running
	t.activity.task, t.activity.since = "40", m.now.Add(-8*time.Second)
	start := entry(3, "assistant", "Plan run · r3 · Tooling and delegation · 3 steps")
	start.Kind = "hush.plan.chat"
	t.merge([]sdk.Entry{start})
	titles := []string{"Enforce tool profiles", "Prove the delegate contract", "Pin the read-only review target", "Document the assignment"}
	steps := make([]sdk.PlanActivityStep, total)
	ids := make([]string, total)
	for i := range steps {
		title := fmt.Sprintf("Pending step %d", i+1)
		if i < len(titles) {
			title = titles[i]
		}
		ids[i] = fmt.Sprintf("s%d", i)
		steps[i] = sdk.PlanActivityStep{ID: ids[i], Title: title, Status: "pending"}
	}
	steps[0].Status = "done"
	if total > 2 {
		steps[2].Status, steps[2].Reason = "blocked", "The review target moved"
	}
	t.runInputs = []sdk.ID{"9"}
	t.planRun = planActivity{ready: true, run: &sdk.PlanActivityRun{
		PlanRunRecord: sdk.PlanRunRecord{ID: "41", Revision: 3, ThreadID: t.info.ID, StepIDs: ids, State: "running", SubmissionID: "9",
			Outcomes: []sdk.PlanOutcome{{StepID: "s0", Status: "done", Detail: "Profiles enforced"}}},
		PlanID: "tooling", Title: "Tooling and delegation", Steps: steps,
	}}
}

func TestGoldenPlanStrip(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	installPlanStripFixture(m, s, 3)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "plan r3") || !strings.Contains(body, "1/3") ||
		!strings.Contains(body, "Run in progress") {
		t.Fatalf("the strip does not show the live run:\n%s", body)
	}
	golden(t, "plan-strip-running", m)
	press(m, tea.KeyUp, 0)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "The review target moved") || !strings.Contains(body, "↵ open plan") {
		t.Fatalf("peek lacks the steps or the open key:\n%s", body)
	}
	golden(t, "plan-strip-peek", m)
	press(m, tea.KeyEscape, 0)
	run := s.planRun.run
	run.State, run.Failure, s.info.Status, s.runInputs = "failed", "The run turn did not finish.", x.Idle, nil
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "✗ The run turn did not finish.") || !strings.Contains(body, "↵ see why") {
		t.Fatalf("a stopped run should say why:\n%s", body)
	}
	golden(t, "plan-strip-failed", m)
	m.backend = planFixtureBackend{}
	press(m, tea.KeyEnter, 0)
	if d := m.focusedDialog(); d == nil || d.plan == nil || d.plan.id != "tooling" || d.plan.cwd != s.info.Workspace {
		t.Fatalf("Enter did not open the run's plan: %+v", m.focusedDialog())
	}
}

func TestPlanStripFollowsTheThreadsSubmission(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	installPlanStripFixture(m, s, 3)
	s.runInputs = nil
	s.queue = []sdk.QueueItem{{Mode: "followUp", Text: "Plan run", SubmissionID: "9"}}
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Run queued") {
		t.Fatalf("a queued run input should say so:\n%s", body)
	}
	s.queue = nil
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Waiting for the turn") {
		t.Fatalf("an unplaced run input should wait:\n%s", body)
	}
	s.planRun.run.State = "finished"
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "Run finished · 1 blocked") || strings.Contains(body, "↑ peek") {
		t.Fatalf("a finished run should summarize without peeking:\n%s", body)
	}
}

// TestPlanPickerE2E drives the real TUI loop, Go SDK, socket and backend on
// the faux provider: save a plan, run a step here, and read its history.
func TestPlanPickerE2E(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "hush-plan-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	workspace, dir := filepath.Join(root, "project"), filepath.Join(root, "data")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "HUSH_PROFILE=hush", "NODE_NO_WARNINGS=1", "LANG=C"}
	grant := exec.CommandContext(ctx, "node", bundle, "trust", "grant", "--cwd", workspace)
	grant.Env = env
	if out, err := grant.CombinedOutput(); err != nil {
		t.Fatalf("grant fixture directory: %v: %s", err, out)
	}
	proc := exec.CommandContext(ctx, "node", bundle, "--profile", "hush", "--faux", "--cwd", workspace, "--data-dir", dir)
	proc.Dir, proc.Env, proc.Stderr = workspace, env, os.Stderr
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = proc.Wait() }()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("backend did not start: %s %v", scan.Text(), scan.Err())
	}
	client := sdk.NewClient(filepath.Join(dir, "hush.sock"), sdk.ClientOptions{
		Capabilities: map[string][]uint64{"plan": {sdk.PlanVersion}, "storeIdentity": {sdk.StoreIdentityVersion}},
	})
	defer client.Close()
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, ReducedMotion: true, Fills: FillOff})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer func() {
		if t.Failed() {
			t.Logf("last frame:\n%s", ansi.Strip(m.View().Content))
		}
	}()
	loop := newProgramLoop(t, m)
	loop.deliver(tea.WindowSizeMsg{Width: 124, Height: 26})
	loop.run(m.Init())
	key := func(code rune) string { return loop.deliver(tea.KeyPressMsg{Code: code}) }
	input := func(text string) {
		loop.deliver(tea.PasteMsg{Content: text})
		key(tea.KeyEnter)
	}
	await := func(want string) {
		t.Helper()
		contains := func(frame string) bool { return strings.Contains(frame, want) }
		if !contains(ansi.Strip(m.View().Content)) {
			loop.until(want, contains)
		}
	}

	loop.until("plan capability and thread readiness", func(string) bool { return m.planAvailable() })
	input("/plan")
	await("No plans yet")
	key(tea.KeyEscape)
	input("!plan-fixture")
	await(`Saved "Workspace profiles" as plan`)
	input("/plan")
	await("↵ Open  Workspace profiles")
	key(tea.KeyEnter)
	await("Run 1 here @ r1  Save a profile")
	key(tea.KeySpace)
	key(tea.KeyDown)
	key(tea.KeySpace)
	await("Run 2 here @ r1  Save a profile → Restore saved profiles")
	key(tea.KeyEnter)
	await("Plan run · r1 · Workspace profiles · 2 steps")
	await("Run finished")
	input("/plan")
	await("↵ Open  Workspace profiles")
	key(tea.KeyEnter)
	await("Run 1 here @ r1  Save a profile")
	key('h')
	await("↵ Open  thread #" + string(thread.ID))
	await("Save a profile: not reported")
}
