package tui

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func planPickerFixture(m *Model, s *threadState) *dialog {
	m.menuFeaturesFromBackend |= x.Plans
	s.plan.ready = true
	s.plan.snapshot = sdk.PlanSnapshot{Revision: strings.Repeat("d", 64), RunAvailability: sdk.PlanRunFixture, Plan: &sdk.PlanDefinition{
		ID: "fixture-plan", Revision: 7, Title: "Workspace profiles", Steps: []sdk.PlanStep{
			{ID: "save", Title: "Save a profile", Description: "Store a profile under a stable name.", Checks: []string{"Profile has the expected name"}, Thinking: "high"},
			{ID: "restore", Title: "Restore saved profiles", Description: "Read the saved profile and restore its settings.", Requires: []string{"save"}, Checks: []string{"Saved settings round-trip"}},
			{ID: "doc", Title: "Document discovery", Description: "Describe the profile discovery path.", Blocker: "Profile path undecided", After: "restore", Checks: []string{"Path is documented"}},
		},
	}}
	m.openPlan(m.route(m.active, menusID))
	return m.focusedDialog()
}

func testPlanFrameGeometry(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	// Model-authored titles, checks and descriptions are not terminal controls.
	step := &s.plan.snapshot.Plan.Steps[0]
	step.Title += " \x1b]52;c;clipboard\a"
	step.Description = strings.Repeat("Long and wide 界 detail ", 35)
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {120, 30}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFrame(t, m)
		press(m, tea.KeyTab, 0)
		if content := ansi.Strip(assertFrame(t, m)); !strings.Contains(content, "Long and wide") {
			t.Fatalf("details are not visible at %v:\n%s", size, content)
		}
		for range 20 {
			press(m, tea.KeyPgDown, 0)
		}
		if content := ansi.Strip(assertFrame(t, m)); !strings.Contains(content, "Thinking: high") && !strings.Contains(content, "Thinking  high") {
			t.Fatalf("cannot reach the end of the details at %v:\n%s", size, content)
		}
		press(m, tea.KeyTab, 0)
		if d.plan.focus != 0 || len(d.plan.selected) != 0 {
			t.Fatal("scrolling details changed the task selection")
		}
	}
}

func TestGoldenPlanPicker(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 116, Height: 24})
	d := planPickerFixture(m, s)
	press(m, tea.KeySpace, 0)
	golden(t, "plan-single-selection", m)
	press(m, tea.KeySpace, 0)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	golden(t, "plan-blocked", m)
	press(m, tea.KeyUp, 0)
	press(m, tea.KeySpace, 0)
	press(m, tea.KeyDown, 0)
	golden(t, "plan-selection", m)
	var lines []string
	for _, row := range m.frame.rows[m.frame.dialogY:m.frame.ruleY] {
		lines = append(lines, strings.TrimSpace(ansi.Strip(row)))
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "↵ Run 2") {
			if i < 1 || lines[i-1] != "" || !strings.Contains(m.frame.rows[m.frame.dialogY+i], "\x1b[1m↵ Run 2") ||
				i+2 != len(lines)-1 || !strings.Contains(m.frame.rows[m.frame.dialogY+i+1], "\x1b[2mspace select") || lines[i+2] != "" {
				t.Fatalf("expected emphasized Run and muted keys separated from body and rule: %q", lines)
			}
			break
		}
	}
	d.plan.selected = nil
	s.plan.snapshot.Run = &sdk.PlanRun{ID: "42", StepIDs: []string{"save", "restore"}, State: "running"}
	s.plan.snapshot.Progress = []sdk.PlanProgress{
		{StepID: "save", State: "verified", EffectiveThinking: "off", WorkerThreadID: "10", Evidence: []string{"Fixture passed: profile name"}},
		{StepID: "restore", State: "running", EffectiveThinking: "off", WorkerThreadID: "11"},
	}
	golden(t, "plan-running", m)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	golden(t, "plan-narrow", m)
}

func TestPlanDockUsesInputRuleWithoutMovingDraft(t *testing.T) {
	for _, width := range []int{40, 116} {
		m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
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
				t.Fatalf("width %d: panel [%d,%d) did not dock to input rule %d (now %d);",
					width, f.dialogY, f.dialogY+f.dialogHeight, rule, f.ruleY)
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
		s.plan.snapshot.Plan.Steps[0].Description = strings.Repeat("More detail about the task. ", 32)
		press(m, tea.KeyTab, 0)
		check()
		if m.frame.dialogY >= before {
			t.Fatal("a taller panel did not grow upward")
		}
		if m.focusedDialog() != d {
			t.Fatal("detail view escaped the plan dialog")
		}
		press(m, tea.KeyEscape, 0)
		assertFrame(t, m)
		if m.frame.ruleY != rule || m.editor.value() != "Keep this draft\nincluding its second line" {
			t.Fatal("closing the picker moved or changed the draft")
		}
	}
}

type planFixtureBackend struct {
	inertBackend
	run func(sdk.PlanSelection) (sdk.ID, error)
}

func (planFixtureBackend) Hello(context.Context) (sdk.Hello, error)                { return sdk.Hello{}, nil }
func (planFixtureBackend) WatchPlan(context.Context, sdk.ID) <-chan sdk.PlanUpdate { return nil }
func (b planFixtureBackend) RunPlan(_ context.Context, s sdk.PlanSelection) (sdk.ID, error) {
	return b.run(s)
}

func TestPlanPickerRetriesExactScopeAndDiscardsStaleClicks(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	d := planPickerFixture(m, s)
	assertFrame(t, m)
	f := m.frame
	row := -1
	for i, v := range f.dialogChoices {
		if v == 0 {
			row = f.dialogY + i
			break
		}
	}
	if row < 0 {
		t.Fatal("no visible row target")
	}
	click := tea.MouseClickMsg(tea.Mouse{X: f.dialogX + 2, Y: row, Button: tea.MouseLeft})
	s.plan.snapshot.Revision = strings.Repeat("e", 64)
	m.Update(click)
	if len(d.plan.selected) != 0 {
		t.Fatal("a click from an older frame changed the current selection")
	}
	assertFrame(t, m)
	m.Update(click)
	if !d.plan.selected["save"] {
		t.Fatal("click did not select the drawn step")
	}
	assertFrame(t, m)
	var requests []sdk.PlanSelection
	m.backend = planFixtureBackend{run: func(s sdk.PlanSelection) (sdk.ID, error) {
		requests = append(requests, s)
		if len(requests) == 1 {
			return "", &sdk.TransportError{Err: context.DeadlineExceeded, Indeterminate: true}
		}
		return "99", nil
	}}
	cmd := press(m, tea.KeyEnter, 0)
	if cmd == nil {
		t.Fatal("Enter did not schedule admission")
	}
	msg, ok := cmd().(workMsg)
	if !ok || msg.err == nil {
		t.Fatalf("expected indeterminate delivery, got %+v", msg)
	}
	m.Update(msg)
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "retry same Run") {
		t.Fatal("transport loss should offer the same request, not a fresh run")
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if len(requests) != 2 || !reflect.DeepEqual(requests[0], requests[1]) {
		t.Fatalf("retry altered the request: %+v", requests)
	}
}

func TestPlanRunRequiresVisibleAction(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d := planPickerFixture(m, s)
	var requested sdk.PlanSelection
	m.backend = planFixtureBackend{run: func(selection sdk.PlanSelection) (sdk.ID, error) {
		requested = selection
		return "99", nil
	}}
	press(m, tea.KeySpace, 0)
	if body := ansi.Strip(assertFrame(t, m)); !strings.Contains(body, "↵ Run Save a profile") {
		t.Fatal(body)
	}
	checkBlocked := func() {
		t.Helper()
		if cmd := press(m, tea.KeyEnter, 0); cmd != nil || s.plan.sending || s.plan.request != nil {
			t.Fatal("Enter admitted a Run without its action drawn")
		}
		if !d.plan.selected["save"] {
			t.Fatal("resize or Enter discarded the selection")
		}
	}
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	if !d.plan.selected["restore"] {
		t.Fatal("adding a second step failed")
	}
	checkBlocked() // the last frame showed only the first selected step
	press(m, tea.KeySpace, 0)
	press(m, tea.KeyUp, 0)
	assertFrame(t, m)
	m.Update(tea.WindowSizeMsg{Width: 12, Height: 3})
	checkBlocked() // don't reuse the larger frame before View
	minimum := ansi.Strip(assertFrame(t, m))
	checkBlocked()
	if !strings.Contains(minimum, "■ ○ Sav") || strings.Contains(minimum, "↵ Run") {
		t.Fatalf("one-row panel must show the selected step, not a Run or a heading:\n%s", minimum)
	}

	m.Update(tea.WindowSizeMsg{Width: 12, Height: 4})
	checkBlocked() // even with room, wait until the action has been drawn
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "■ ○ Sav") || !strings.Contains(body, "↵ Run 1") {
		t.Fatalf("two-row panel must show the selection and Run:\n%s", body)
	}
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if !reflect.DeepEqual(requested.StepIDs, []string{"save"}) || requested.Revision != 7 {
		t.Fatalf("visible Run did not dispatch the retained selection: %+v", requested)
	}
}

func TestScopedPlanSelectionAndStop(t *testing.T) {
	m, thread := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 116, Height: 24})
	d := planPickerFixture(m, thread)
	v := &thread.plan
	v.snapshot.RunAvailability = sdk.PlanRunScoped
	first := &v.snapshot.Plan.Steps[0]
	second := &v.snapshot.Plan.Steps[1]
	first.Edit = &sdk.PlanFileEdit{Path: "profile.txt", Contains: "theme=mocha"}
	press(m, tea.KeySpace, 0)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	body := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "has no scoped edit") || !strings.Contains(body, "Draft only") {
		t.Fatalf("selection promised to run a draft-only task:\n%s", body)
	}
	second.Edit = first.Edit
	if _, reason := v.selection(d.plan); reason != "Cannot run · select one edit per file" {
		t.Fatalf("expected a duplicate path diagnostic, got %q", reason)
	}
	second.Edit = &sdk.PlanFileEdit{Path: "restored.txt", Contains: "restored=mocha"}
	body = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(body, "Run 2 @ r7") || !strings.Contains(body, "restored.txt") || !strings.Contains(body, `Contains "restored=mocha"`) {
		t.Fatalf("the runnable scope was not shown:\n%s", body)
	}
	v.snapshot.Run = &sdk.PlanRun{ID: "42", StepIDs: []string{first.ID, second.ID}, State: "running", OwnerThreadID: "99"}
	press(m, tea.KeyEscape, 0) // close the picker
	if canStopPlan(thread) {
		t.Fatal("another attached thread cannot stop this run through its own prompt")
	}
	v.snapshot.Run.OwnerThreadID = thread.info.ID
	assertFrame(t, m)
	if !strings.Contains(ansi.Strip(m.frame.rows[m.frame.ruleY]), "esc stop") {
		t.Fatal("scoped Run did not advertise the normal Stop action")
	}
	press(m, tea.KeyEscape, 0)
	if m.stopAt.IsZero() || thread.aborting {
		t.Fatal("first Esc should only confirm Stop")
	}
	if cmd := press(m, tea.KeyEscape, 0); cmd == nil || !thread.aborting {
		t.Fatal("second Esc did not request Stop on the initiating thread")
	}
}

func TestDraftPlanCommandNeedsLocalWorkspaceCapability(t *testing.T) {
	m := modelFor(t, Options{WorkspaceLocal: true})
	m.editor.set("/plan")
	for _, check := range []struct {
		features x.Features
		local    bool
		visible  bool
	}{
		{0, true, false},
		{x.Plans, true, false},
		{x.Plans | x.PlanWorkspace, true, false},
		{x.Plans | x.PlanWorkspace | x.PlanChat, false, false},
		{x.Plans | x.PlanWorkspace | x.PlanChat, true, true},
	} {
		m.menuFeaturesFromBackend, m.options.WorkspaceLocal = check.features, check.local
		if got := len(m.slashMatches()) == 1; got != check.visible {
			t.Fatalf("/plan visibility = %v with features %v and local = %v", got, check.features, check.local)
		}
	}
}

type draftPlanFixtureBackend struct {
	inertBackend
	create func(string, sdk.Model, string) (sdk.Thread, error)
	run    func(sdk.PlanSelection) (sdk.ID, error)
}

func (b draftPlanFixtureBackend) NewCatalogThread(_ context.Context, cwd string, model sdk.Model, thinking string) (sdk.Thread, error) {
	return b.create(cwd, model, thinking)
}

func (b draftPlanFixtureBackend) RunPlanInChat(_ context.Context, selection sdk.PlanSelection) (sdk.ID, error) {
	return b.run(selection)
}

func (draftPlanFixtureBackend) WatchWorkspacePlansAt(context.Context, string, int) <-chan sdk.PlanWorkspaceUpdate {
	return nil
}

func (draftPlanFixtureBackend) WatchNamedPlanAt(context.Context, string, string) <-chan sdk.PlanUpdate {
	return nil
}

func chatPlanFixtureSnapshot() sdk.PlanSnapshot {
	return sdk.PlanSnapshot{RunAvailability: sdk.PlanRunUnbound, Revision: strings.Repeat("c", 64), Chat: true,
		Plan: &sdk.PlanDefinition{ID: "a-plan", Revision: 1, Title: "Profiles", Steps: []sdk.PlanStep{
			{ID: "save", Title: "Save profiles"},
			{ID: "restore", Title: "Restore profiles", Requires: []string{"save"}, Blocker: "Check the saved path"},
		}}}
}

func TestStartupPlanBrowseDoesNotCreateThread(t *testing.T) {
	m := modelFor(t, Options{WorkspaceLocal: true})
	m.menuFeaturesFromBackend = x.Plans | x.PlanWorkspace | x.PlanChat
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 25})
	model := sdk.Model{Provider: "fixture", ModelID: "test"}
	draft := m.threads[""]
	draft.info.Model, draft.info.Thinking = model, "high"
	m.editor.set("keep this draft")
	creates := 0
	m.backend = draftPlanFixtureBackend{create: func(cwd string, chosen sdk.Model, thinking string) (sdk.Thread, error) {
		creates++
		if cwd != "/workspace" || chosen != model || thinking != "high" {
			t.Fatalf("creation lost its workspace or model: %q, %+v, %q", cwd, chosen, thinking)
		}
		return sdk.Thread{ID: "42"}, nil
	}, run: func(s sdk.PlanSelection) (sdk.ID, error) {
		if s.ThreadID != "42" || !reflect.DeepEqual(s.StepIDs, []string{"restore"}) {
			t.Fatalf("Run didn't target the first materialized thread: %+v", s)
		}
		return "43", nil
	}}
	m.runAction(m.host.byID[planID], "")
	d := m.focusedDialog()
	if d == nil || d.planWorkspace == nil || d.route.owner != "" || draft.sending || creates != 0 {
		t.Fatal("opening the plan browser must not admit work or need a thread")
	}
	m.receivePlanWorkspace(planWorkspaceMsg{lease: d.planWorkspace.lease, ok: true, update: sdk.PlanWorkspaceUpdate{
		Plans: []sdk.PlanSummary{{ID: "a-plan", Title: "Profiles", Revision: 1, Total: 2}},
	}})
	if cmd := m.planWorkspaceKey(d, "enter"); cmd == nil {
		t.Fatal("startup browse couldn't inspect a plan")
	}
	d = m.focusedDialog()
	m.receiveNamedPlan(namedPlanMsg{token: d.token, ok: true, update: sdk.PlanUpdate{Snapshot: chatPlanFixtureSnapshot()}})
	if m.active != "" || m.editor.value() != "keep this draft" || creates != 0 || draft.sending {
		t.Fatal("inspect changed the composer or materialized a thread")
	}
	assertFrame(t, m)
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Run here Restore profiles") {
		t.Fatal(frame)
	}
	workflowWork(t, m, m.planKey(d, "enter"))
	if creates != 1 || m.active != "42" || m.editor.value() != "keep this draft" || draft.sending || m.focusedDialog() != nil {
		t.Fatal("only Run should materialize the first thread and leave its draft intact")
	}
}

func TestRunNamedPlanHerePreservesThreadAndDraftAcrossReceiptRetry(t *testing.T) {
	m, thread := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace | x.PlanChat
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 25})
	m.editor.set("keep my unsent prompt")
	var requests []sdk.PlanSelection
	m.backend = draftPlanFixtureBackend{run: func(s sdk.PlanSelection) (sdk.ID, error) {
		requests = append(requests, s)
		if len(requests) == 1 {
			return "", &sdk.TransportError{Err: context.DeadlineExceeded, Indeterminate: true}
		}
		return "42", nil
	}}
	open := func() {
		p := &planPicker{named: "a-plan"}
		d := &dialog{route: m.route(m.active, menusID), token: m.token(), plan: p}
		m.pushDialog(d)
		m.receiveNamedPlan(namedPlanMsg{token: d.token, ok: true, update: sdk.PlanUpdate{Snapshot: chatPlanFixtureSnapshot()}})
		assertFrame(t, m)
	}
	open()
	press(m, tea.KeyDown, 0)
	press(m, tea.KeySpace, 0)
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Run here Restore profiles") {
		t.Fatal(frame)
	}
	cmd := press(m, tea.KeyEnter, 0)
	if cmd == nil {
		t.Fatal("Enter didn't admit Run here")
	}
	m.finishWork(cmd().(workMsg))
	press(m, tea.KeyEscape, 0)
	open()
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "retry Run here") {
		t.Fatal(frame)
	}
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if len(requests) != 2 || !reflect.DeepEqual(requests[0], requests[1]) || requests[0].ThreadID != "1" ||
		!reflect.DeepEqual(requests[0].StepIDs, []string{"restore"}) || thread.plan.snapshot.Plan != nil {
		t.Fatalf("Run did not preserve exact scope and binding: %+v", requests)
	}
	if m.active != "1" || m.editor.value() != "keep my unsent prompt" || m.focusedDialog() != nil {
		t.Fatal("Run switched threads, disturbed the composer or left the picker open")
	}
}

func TestPlanWorkspaceDockAndStatuses(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 104, Height: 24})
	enableGitDiffFixture(m, s)
	m.editor.set("keep this draft")
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace
	assertFrame(t, m)
	rule := m.frame.ruleY
	p := &planWorkspacePicker{lease: 84, ready: true, rows: []sdk.PlanSummary{
		{ID: "running", Title: "Workspace profiles", Status: "running", Revision: 2, Verified: 1, Total: 3},
		{ID: "needs-check", Title: "Track config files", Status: "attention", Revision: 1, Total: 2},
		{ID: "closed", Title: "Review history", Status: "pending", Revision: 7, Verified: 2, Total: 5, Closed: true},
	}}
	d := &dialog{route: m.route(m.active, menusID), token: m.token(), planWorkspace: p, selected: 2}
	m.pushDialog(d)
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Workspace plans", "Open plans", "Closed plans", "Running", "Needs check", "2/5 · r7", "c reopen", "± 4 files", "keep this draft"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	f := m.frame
	if f.ruleY != rule || f.dialogY+f.dialogHeight != rule {
		t.Fatal("workspace picker did not dock above the existing input rule")
	}
	// A reorder caused by a peer opening or closing a plan keeps focus by ID.
	token := d.token
	rows := append([]sdk.PlanSummary{p.rows[2]}, p.rows[:2]...)
	m.receivePlanWorkspace(planWorkspaceMsg{lease: p.lease, ok: true, update: sdk.PlanWorkspaceUpdate{Plans: rows}})
	if d.selected != 0 || d.token == token {
		t.Fatal("workspace update did not preserve focus and invalidate stale mouse targets")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	assertFrame(t, m)
}

func TestPlanCatalogShowsForeignReadOnlyPlans(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 104, Height: 24})
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace | x.PlanCatalog | x.PlanReview
	r := m.route(m.active, menusID)
	workspace := "/projects/source-checkout"
	rows := []sdk.PlanSummary{{ID: "foreign", Title: "Other plan", Revision: 1, Total: 1, Status: "pending", Workspace: workspace}}
	list := &dialog{route: r, token: m.token(), planWorkspace: &planWorkspacePicker{ready: true, rows: rows}}
	m.pushDialog(list)
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"all workspaces", "Other plan", "source-checkout", "↵ preview", "h history"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q:\n%s", want, frame)
		}
	}
	for _, forbid := range []string{"v review", "c close plan"} {
		if strings.Contains(frame, forbid) {
			t.Fatalf("foreign plan offered %q", forbid)
		}
	}
	p := &planPicker{named: "foreign", workspace: workspace}
	detail := &dialog{route: r, token: m.token(), plan: p}
	m.pushDialog(detail)
	m.receiveNamedPlan(namedPlanMsg{token: detail.token, ok: true, update: sdk.PlanUpdate{Snapshot: sdk.PlanSnapshot{
		Plan:            &sdk.PlanDefinition{ID: "foreign", Revision: 1, Title: "Other plan", Steps: []sdk.PlanStep{{ID: "read", Title: "Read it"}}},
		RunAvailability: sdk.PlanRunDisabled,
		Workspace:       workspace,
	}}})
	frame = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Read-only · "+workspace) || strings.Contains(frame, "Use in this thread") {
		t.Fatalf("wrong preview controls:\n%s", frame)
	}
	if p.view.snapshot.RunAvailability != sdk.PlanRunDisabled || slices.Contains(m.frame.dialogChoices, planRunTarget) {
		t.Fatal("cross-workspace preview was made runnable")
	}
	m.Update(tea.WindowSizeMsg{Width: 39, Height: 11})
	assertFrame(t, m)
}

type planWorktreeFixture struct {
	draftPlanFixtureBackend
	prepare func(sdk.PlanSelection, sdk.PlanWorktreeSource) (sdk.PlanWorktree, error)
	start   func(sdk.PlanWorktree, sdk.PlanSelection) (sdk.ID, error)
}

func (b planWorktreeFixture) PreparePlanWorktree(_ context.Context, q sdk.PlanSelection, source sdk.PlanWorktreeSource) (sdk.PlanWorktree, error) {
	return b.prepare(q, source)
}

func (b planWorktreeFixture) StartPlanWorktree(_ context.Context, w sdk.PlanWorktree, q sdk.PlanSelection) (sdk.ID, error) {
	return b.start(w, q)
}

func TestPlanWorktreeTrustStartsExactSelectionAndKeepsOriginalDraft(t *testing.T) {
	const cwd = "/managed/preferences-fixture"
	granted := false
	trust := &WorkspaceTrustOptions{Review: func(_ context.Context, path string) (WorkspaceTrustPreview, error) {
		if path != cwd {
			t.Fatalf("review requested for %q, not the new checkout", path)
		}
		return WorkspaceTrustPreview{Path: path, Decide: func(_ context.Context, yes bool) error { granted = yes; return nil }}, nil
	}}
	m, source := demo(t, Options{ReducedMotion: true, Fills: FillOff, WorkspaceTrust: trust})
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace | x.PlanChat | x.PlanWorktree
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 25})
	m.editor.set("source draft")
	original := m.active
	var selection sdk.PlanSelection
	var prepares, starts int
	checkout := sdk.PlanWorktree{ThreadID: "41", Title: "Preferences fixture"}
	checkout.Checkout.CWD, checkout.Checkout.Branch = cwd, "hush/preferences-fixture"
	m.backend = planWorktreeFixture{prepare: func(q sdk.PlanSelection, source sdk.PlanWorktreeSource) (sdk.PlanWorktree, error) {
		prepares++
		if q.ThreadID != original || source != (sdk.PlanWorktreeSource{}) || !reflect.DeepEqual(q.StepIDs, []string{"restore"}) {
			t.Fatalf("bad worktree choice: %+v %+v", q, source)
		}
		selection = q
		return checkout, nil
	}, start: func(w sdk.PlanWorktree, q sdk.PlanSelection) (sdk.ID, error) {
		starts++
		if !granted || w != checkout || !reflect.DeepEqual(q, selection) {
			t.Fatalf("unreviewed or changed checkout start: %+v %+v", w, q)
		}
		return "42", nil
	}}
	d := &dialog{route: m.route(original, menusID), token: m.token(), plan: &planPicker{
		named: "a-plan", view: planView{ready: true, snapshot: chatPlanFixtureSnapshot()}, focus: 1,
	}}
	m.pushDialog(d)
	if frame := ansi.Strip(assertFrame(t, m)); strings.Contains(frame, "w Run in new worktree") {
		t.Fatal("worktree was offered without selected tasks")
	}
	m.planKey(d, "space")
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "w Run in new worktree") || !strings.Contains(frame, "Run here Restore profiles") {
		t.Fatal(frame)
	}
	prepared := m.planKey(d, "w")().(workMsg)
	review := m.finishWork(prepared)
	if m.active != original || prepares != 1 || starts != 0 || m.editor.value() != "source draft" || m.focusedDialog().workTrust == nil {
		t.Fatal("prepare changed threads or started implementation before trust")
	}
	workspaceTrustStep(t, m, review)
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Trust this workspace?") || !strings.Contains(frame, cwd) {
		t.Fatal(frame)
	}
	if m.workspaceTrustKey(m.focusedDialog(), "enter") != nil {
		t.Fatal("Enter must not grant trust")
	}
	start := workspaceTrustStep(t, m, m.workspaceTrustKey(m.focusedDialog(), "t"))
	workflowWork(t, m, start)
	if m.active != checkout.ThreadID || starts != 1 || !granted || source.draft.text != "source draft" || m.editor.value() != "" {
		t.Fatal("the run didn't open the worktree while retaining the source draft")
	}
}

func TestPlanWorktreeStartupRetryKeepsExactSourceAndCanLeaveIdle(t *testing.T) {
	const sourceCWD = "/project/component"
	const cwd = "/managed/long path's \\name/" + "inside-component/inside-component/inside-component/inside-component/inside-component"
	selected := sdk.Model{Provider: "staged", ModelID: "chosen"}
	var copied string
	m := modelFor(t, Options{ReducedMotion: true, Fills: FillOff, WorkspaceLocal: true, NewThread: sdk.NewThread{CWD: sourceCWD},
		CopyText: func(_ context.Context, text string) error { copied = text; return nil }})
	m.connection = x.Connected
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace | x.PlanChat | x.PlanWorktree
	m.Update(tea.WindowSizeMsg{Width: 96, Height: 22})
	m.editor.set("keep this draft")
	m.threads[""].info.Model, m.threads[""].info.Thinking = selected, "high"
	var selection sdk.PlanSelection
	var prepares int
	checkout := sdk.PlanWorktree{ThreadID: "41", Title: "Preferences fixture"}
	checkout.Checkout.CWD, checkout.Checkout.Branch = cwd, "hush/preferences-fixture"
	m.backend = planWorktreeFixture{prepare: func(q sdk.PlanSelection, source sdk.PlanWorktreeSource) (sdk.PlanWorktree, error) {
		prepares++
		if q.ThreadID != "" || source.CWD != sourceCWD || source.Model == nil || *source.Model != selected || source.ThinkingLevel != "high" {
			t.Fatalf("bad startup source: %+v %+v", q, source)
		}
		if prepares == 1 {
			selection = q
			return sdk.PlanWorktree{}, &sdk.TransportError{Err: context.DeadlineExceeded, Indeterminate: true}
		}
		if !reflect.DeepEqual(q, selection) {
			t.Fatalf("retry changed selection: %+v != %+v", q, selection)
		}
		return checkout, nil
	}, start: func(sdk.PlanWorktree, sdk.PlanSelection) (sdk.ID, error) {
		t.Fatal("reviewing instructions or leaving idle must not start the run")
		return "", nil
	}}
	d := &dialog{route: m.route("", menusID), token: m.token(), plan: &planPicker{
		named: "a-plan", view: planView{ready: true, snapshot: chatPlanFixtureSnapshot()}, focus: 1,
	}}
	m.pushDialog(d)
	m.planKey(d, "space")
	assertFrame(t, m)
	m.finishWork(m.planKey(d, "w")().(workMsg))
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Worktree setup unconfirmed") || !strings.Contains(frame, "w retry setup") {
		t.Fatal(frame)
	}
	m.threads[""].info.Model, m.threads[""].info.Thinking = sdk.Model{Provider: "later", ModelID: "different"}, "off"
	m.finishWork(m.planKey(d, "w")().(workMsg))
	trust := m.focusedDialog()
	frame = ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "y copy grant command") || !strings.Contains(frame, "w start after granting") {
		t.Fatal(frame)
	}
	workflowWork(t, m, m.workspaceTrustKey(trust, "y"))
	want := `hush trust grant --cwd '/managed/long path'\''s '\\'name/inside-component/inside-component/inside-component/inside-component/inside-component'`
	if copied != want {
		t.Fatalf("copied wrong command: %q", copied)
	}
	m.workspaceTrustKey(trust, "esc")
	assertFrame(t, m)
	m.planKey(d, "w")
	if m.focusedDialog().workTrust == nil || prepares != 2 {
		t.Fatal("w must reopen the same trust instructions, not start or reallocate")
	}
	m.workspaceTrustKey(m.focusedDialog(), "esc")
	assertFrame(t, m)
	m.planKey(d, "x")
	if m.active != "" || m.threads[""].plan.worktree != nil || m.editor.value() != "keep this draft" {
		t.Fatal("leaving the worktree idle must retain the startup draft and leave no pending request")
	}
}

func TestGoldenPlanZoom(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	d := planPickerFixture(m, s)
	for i := range 20 {
		s.plan.snapshot.Plan.Steps = append(s.plan.snapshot.Plan.Steps, sdk.PlanStep{
			ID: fmt.Sprintf("extra-%d", i), Title: fmt.Sprintf("Additional task %d", i+1),
		})
	}
	assertFrame(t, m)
	height, rule := m.frame.dialogHeight, m.frame.ruleY
	press(m, 'f', 0)
	golden(t, "plan-full", m)
	if m.frame.dialogHeight <= height || m.frame.ruleY != rule || m.focusedDialog() != d {
		t.Fatal("zoom did not expand the plan at the same input rule")
	}
	press(m, 'f', 0)
	assertFrame(t, m)
	if m.frame.dialogHeight != height {
		t.Fatal("zoom did not restore the fitted height")
	}
}
