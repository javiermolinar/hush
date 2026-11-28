package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// Rex derives its Activity panel and alerts from OSC 7501. Use one stable
// record per open thread so navigating between Hush tabs doesn't replay alerts.
type rexActivity struct {
	reported map[sdk.ID]tea.ProgramState
	blocked  rexInputRequest
	pending  bool
}

type rexActivityOutput string

type rexThreadActivity struct {
	state           tea.ProgramState
	failure         sdk.ID
	directoryDoneMS uint64
	directoryFailMS uint64
	directoryFailed bool
}

func (r *rexThreadActivity) observe(status x.ThreadStatus, freshOutcome bool) {
	var state tea.ProgramState
	switch status {
	case x.Running, x.Parked:
		// Parked includes timers and waiting on child work, not just input.
		state = tea.ProgramStateWorking
	case x.Done:
		state = tea.ProgramStateDone
	case x.Failed:
		state = tea.ProgramStateError
	default:
		state = tea.ProgramStateIdle
	}
	if (state == tea.ProgramStateDone || state == tea.ProgramStateError) &&
		r.state != state && r.state != tea.ProgramStateWorking && !freshOutcome {
		state = tea.ProgramStateIdle // don't announce retained results on attach
	}
	r.state = state
}

func (r *rexThreadActivity) observeNative(status x.ThreadStatus, t *threadState, s sdk.State) {
	failure := deref(s.CompactionFailure).TaskID
	if failure == "" {
		failure = deref(s.Failure).TaskID
	}
	fresh := status == x.Failed && t.observed && failure != "" && failure != r.failure
	r.failure = failure
	if status == x.Idle || status == x.Done {
		switch {
		case s.Failure != nil:
			status = x.Idle // e.g. reset or a continue with nothing to answer
		case t.observed && rexNewAnswer(s.Window.Entries, t.info.LastEntryID):
			// A short turn can settle between snapshots, without a Working report.
			status, fresh = x.Done, true
		case r.state == tea.ProgramStateDone && t.info.LastEntryID == deref(s.Window.LastEntryID):
			status = x.Done
		}
	}
	r.observe(status, fresh)
}

func (r *rexThreadActivity) observeDirectory(row sdk.ThreadDirectoryRow, first, active bool) {
	status := x.ThreadStatus(row.Status)
	if first {
		// Don't later announce an answer that already existed when the watch began.
		r.directoryDoneMS = max(r.directoryDoneMS, row.LastMessageMS)
	}
	var fresh bool
	switch status {
	case x.Done:
		fresh = row.LastMessageMS > r.directoryDoneMS
		r.directoryDoneMS = max(r.directoryDoneMS, row.LastMessageMS)
		if !fresh && r.state != tea.ProgramStateDone {
			// The task graph can go quiet before the index catches up, briefly
			// exposing the previous outcome. That's not a new completion.
			status = x.Idle
		}
	case x.Failed:
		fresh = !first && (!r.directoryFailed || row.LastMessageMS > r.directoryFailMS)
		r.directoryFailed, r.directoryFailMS = true, max(r.directoryFailMS, row.LastMessageMS)
		if !fresh && r.state != tea.ProgramStateError {
			status = x.Idle
		}
	}
	if !active {
		r.observe(status, fresh)
	}
}

func rexNewAnswer(entries []sdk.Entry, after sdk.ID) bool {
	for i := len(entries) - 1; i >= 0 && lessID(after, entries[i].ID); i-- {
		entry := entries[i]
		switch entry.Kind {
		case "pi.assistant":
			return slices.ContainsFunc(entry.Messages, func(msg sdk.Message) bool {
				return msg.Role == "assistant" && (msg.StopReason == "stop" || msg.StopReason == "length")
			})
		case "pi.user", "pi.reset":
			return false
		}
	}
	return false
}

func rexRecordID(id sdk.ID) string {
	if id == "" {
		return "hush/draft"
	}
	return "hush/" + string(id)
}

type rexInputRequest struct {
	token   uint64
	kind    tea.ProgramStatusKind
	message string
}

func (m *Model) rexInputWait() rexInputRequest {
	if d := m.focusedDialog(); d != nil {
		switch {
		case d.workTrust != nil && d.workTrust.cancel == nil && (d.workTrust.decide != nil || d.workTrust.onStart != nil):
			return rexInputRequest{d.token, tea.ProgramStatusKindPermission, "Workspace trust required"}
		case d.confirm && d.done != nil:
			return rexInputRequest{d.token, tea.ProgramStatusKindPermission, "Confirmation required"}
		case d.input != nil && d.done != nil:
			return rexInputRequest{d.token, tea.ProgramStatusKindQuestion, "Input required"}
		}
	}
	return rexInputRequest{}
}

func (m *Model) syncRexActivity() tea.Cmd {
	a := m.rexActivity
	if a == nil || !m.started || a.pending {
		return nil
	}
	var out strings.Builder
	if a.reported == nil {
		// The pane can still have completed records from an earlier Hush.
		out.WriteString(ansi.ClearProgramStatusID("hush"))
		a.reported = make(map[sdk.ID]tea.ProgramState)
	}
	report := func(id sdk.ID, state tea.ProgramState) {
		if a.reported[id] != state {
			out.WriteString(ansi.SetProgramStatus(tea.ProgramStatus{
				ID: rexRecordID(id), App: "hush", State: state, Title: threadTitle(m.threads[id].info),
			}))
			a.reported[id] = state
		}
	}
	for id, state := range a.reported {
		if id != m.active && !slices.Contains(m.tabs, id) {
			delete(a.reported, id)
			out.WriteString(ansi.ClearProgramStatusID(rexRecordID(id)))
		} else if m.quitting && state == tea.ProgramStateWorking {
			report(id, tea.ProgramStateIdle) // closing a view isn't completing a run
		}
	}
	var blocked rexInputRequest
	if !m.quitting {
		blocked = m.rexInputWait()
		for _, id := range m.tabs {
			t := m.threads[id]
			state := t.rexActivity.state
			if state == "" || state == tea.ProgramStateError && id == m.active && t.dismissedFailure(t.rexActivity.failure) {
				state = tea.ProgramStateIdle
			}
			report(id, state)
		}
		if m.active == "" {
			report("", tea.ProgramStateIdle)
		}
	}
	if blocked != a.blocked {
		const input = "hush/input"
		if blocked.kind == "" {
			out.WriteString(ansi.ClearProgramStatusID(input))
		} else {
			out.WriteString(ansi.SetProgramStatus(tea.ProgramStatus{
				ID: input, App: "hush", State: tea.ProgramStateBlocked, Kind: blocked.kind, Message: blocked.message,
			}))
		}
		a.blocked = blocked
	}
	if out.Len() == 0 {
		if m.quitting {
			return tea.Quit
		}
		return nil
	}
	// Tea handles RawMsg before Update sees it. Keep one batch in flight so
	// asynchronous commands can't publish an older state after a newer one.
	a.pending = true
	return tea.Raw(rexActivityOutput(out.String()))
}
