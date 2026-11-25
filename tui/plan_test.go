package tui

import (
	"context"
	"reflect"
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
	m.Update(tea.WindowSizeMsg{Width: 116, Height: 22})
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
			if f.ruleY != rule || f.dialogY+f.dialogHeight != rule || f.dialogFramed {
				t.Fatalf("width %d: panel [%d,%d) did not dock to input rule %d (now %d); framed=%t",
					width, f.dialogY, f.dialogY+f.dialogHeight, rule, f.ruleY, f.dialogFramed)
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

func TestScopedPlanSelectionAndStop(t *testing.T) {
	m, thread := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 116, Height: 22})
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
	d := &dialog{route: m.route(m.active, menusID), token: m.token(), planWorkspace: p, selected: 2, promptDock: true}
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
