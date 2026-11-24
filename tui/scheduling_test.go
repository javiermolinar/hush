package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

type scheduleFixture struct {
	menuBackend
	row     sdk.ScheduleItem
	changes []sdk.ScheduleChange
	inputs  []sdk.ScheduleInput
}

func (f *scheduleFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"schedules": sdk.SchedulesVersion}}, nil
}
func (f *scheduleFixture) ListSchedules(_ context.Context, thread sdk.ID) ([]sdk.ScheduleItem, error) {
	if thread != f.row.ThreadID {
		return nil, fmt.Errorf("wrong schedule owner: %s", thread)
	}
	return []sdk.ScheduleItem{f.row}, nil
}
func (f *scheduleFixture) AddSchedule(_ context.Context, input sdk.ScheduleInput) (sdk.ScheduleItem, error) {
	f.inputs = append(f.inputs, input)
	return sdk.ScheduleItem{ScheduleTime: input.ScheduleTime, State: "pending"}, nil
}
func (f *scheduleFixture) ChangeSchedule(_ context.Context, change sdk.ScheduleChange) (sdk.ScheduleItem, error) {
	f.changes = append(f.changes, change)
	row := f.row
	row.State, row.Reason = "pending", ""
	return row, nil
}

func TestScheduleMenuOwnerAndConfirmation(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff})
	when, err := sdk.ParseScheduleTime("2027-02-03T13:08:00+02:15")
	if err != nil {
		t.Fatal(err)
	}
	backend := &scheduleFixture{row: sdk.ScheduleItem{ScheduleTime: when, ID: "77397caf-ceff-47c1-bab3-59b820b87eb5",
		ThreadID: "1", TargetThreadID: "2", State: "needs_attention", Reason: "late",
		Prerequisite: &sdk.SchedulePrerequisite{ThreadID: "3", SubmissionID: "4"}}}
	m.backend = backend
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 28})
	frame := func(s string) {
		t.Helper()
		if text := ansi.Strip(assertFrame(t, m)); !strings.Contains(text, s) {
			t.Fatalf("missing %q in:\n%s", s, text)
		}
	}
	m.finishWork(m.runAction(m.host.byID[menusID+".schedule"], "")().(workMsg))
	frame("Needs you · 2027-02-03T13:08:00+02:15 → #2 · after #3 submission #4")
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	assertFrame(t, m)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 28})
	press(m, tea.KeyEnter, 0)
	frame("Run now (if eligible)")
	press(m, tea.KeyEnter, 0)
	frame("Confirm run?")
	if len(backend.changes) > 0 {
		t.Fatal("decision bypassed confirmation")
	}
	press(m, tea.KeyDown, 0)
	cmd := press(m, tea.KeyEnter, 0)
	m.finishWork(cmd().(workMsg))
	want := sdk.ScheduleChange{ThreadID: "1", ID: backend.row.ID, Op: "run"}
	if len(backend.changes) != 1 || backend.changes[0] != want {
		t.Fatalf("changes: %#v", backend.changes)
	}
	m.runSlashWithArgs(m.host.actions[m.host.byID[menusID+".schedule"]], `add 2 2027-02-03T13:08:00+02:15 "test input" after 3/4 allow-late`)
	frame("after #3 submission #4")
	press(m, tea.KeyDown, 0)
	m.finishWork(press(m, tea.KeyEnter, 0)().(workMsg))
	if len(backend.inputs) != 1 || backend.inputs[0].ThreadID != "1" || backend.inputs[0].TargetThreadID != "2" ||
		backend.inputs[0].Text != "test input" || backend.inputs[0].ScheduleTime != when || !backend.inputs[0].AllowLate ||
		backend.inputs[0].Prerequisite == nil || *backend.inputs[0].Prerequisite != *backend.row.Prerequisite {
		t.Fatalf("inputs: %+v", backend.inputs)
	}
}
