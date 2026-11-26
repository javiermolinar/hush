package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type planChatBackend interface {
	WatchWorkspacePlansAt(context.Context, string, int) <-chan sdk.PlanWorkspaceUpdate
	WatchNamedPlanAt(context.Context, string, string) <-chan sdk.PlanUpdate
	RunPlanInChat(context.Context, sdk.PlanSelection) (sdk.ID, error)
}

// Keep an indeterminate receipt on the initiating thread even when the named
// picker is dismissed. Retrying must not submit the selection a second time.
type planChatAdmission struct {
	selection sdk.PlanSelection
	sending   bool
}

type planChatReply struct{ thread, run sdk.ID }

func (m *Model) planChatAvailable(v *planView) bool {
	return v.snapshot.Workspace == "" && v.snapshot.Chat && m.menuFeaturesFromBackend.Has(x.PlanChat) && (m.planAvailable() || m.draftPlanAvailable())
}

func (v *planView) taskReport(id string) sdk.PlanTaskReport {
	for _, report := range v.snapshot.Reports {
		if report.StepID == id {
			return report
		}
	}
	return sdk.PlanTaskReport{}
}

func (v *planView) done(id string) bool {
	return v.progress(id).State == "verified" || v.taskReport(id).State == "done"
}

func (v *planView) chatSelection(p *planPicker) (ids []string) {
	for _, s := range v.snapshot.Plan.Steps {
		if p.selected[s.ID] && v.progress(s.ID).State != "verified" {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

func (m *Model) showChatSelection(d *dialog, v *planView) {
	p := d.plan
	if pending := m.threads[d.route.owner].plan.chatRequest; pending != nil && v.snapshot.Plan != nil &&
		pending.selection.PlanID == v.snapshot.Plan.ID && pending.selection.Revision == v.snapshot.Plan.Revision {
		p.selected = make(map[string]bool, len(pending.selection.StepIDs))
		for _, id := range pending.selection.StepIDs {
			p.selected[id] = true
		}
	}
}

func (m *Model) runChatPlan(d *dialog, v *planView) tea.Cmd {
	b, ok := m.backend.(planChatBackend)
	if !ok || d.plan.workspace != "" || !m.planChatAvailable(v) || !m.planActionDrawn(d, v, planRunTarget) || !v.ready || v.snapshot.Plan == nil || !d.plan.matches(v) {
		return nil
	}
	r := d.route
	t := m.threads[r.owner]
	if t.sending || t.aborting || t.attaching || t.plan.request != nil || t.plan.sending || t.plan.worktree != nil {
		return nil
	}
	pending := t.plan.chatRequest
	if pending == nil {
		if v.locked() {
			return nil
		}
		ids := v.chatSelection(d.plan)
		if len(ids) == 0 {
			return nil
		}
		p := v.snapshot.Plan
		pending = &planChatAdmission{selection: sdk.PlanSelection{ThreadID: r.owner, PlanID: p.ID, Revision: p.Revision, StepIDs: ids, RequestID: submissionKey()}}
		t.plan.chatRequest = pending
	} else if pending.sending || pending.selection.PlanID != v.snapshot.Plan.ID {
		return nil
	}
	pending.sending, v.note = true, ""
	d.plan.selectionRev++
	selection := pending.selection
	backend, options := m.backend, m.options.NewThread
	model, thinking := t.info.Model, t.info.Thinking
	if r.owner == "" {
		t.sending = true // serialize the first thread with ordinary prompt submission
	}
	return m.startWork(r, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		if selection.ThreadID == "" {
			id, err := createDraftThread(ctx, backend, options, model, thinking)
			if err != nil {
				return planChatReply{}, err
			}
			selection.ThreadID = id
		}
		run, err := b.RunPlanInChat(ctx, selection)
		return planChatReply{selection.ThreadID, run}, err
	}, func(value any, err error, _ *int) tea.Cmd {
		pending.sending = false
		if r.owner == "" {
			t.sending = false
		}
		out, _ := value.(planChatReply)
		if out.thread.Valid() {
			pending.selection.ThreadID = out.thread
		}
		if err != nil {
			if !isIndeterminate(err) || !out.thread.Valid() {
				t.plan.chatRequest = nil
				var reply *sdk.ServerError
				if errors.As(err, &reply) {
					v.note = reply.Message
				} else if isIndeterminate(err) {
					v.note = "Thread creation unconfirmed · check /threads before retrying"
				} else {
					v.note = "Could not run the selected tasks"
				}
			}
			return nil
		}
		t.plan.chatRequest = nil
		d.plan.selected = nil
		if v.snapshot.Plan != nil && v.snapshot.Plan.ID == pending.selection.PlanID && v.snapshot.Plan.Revision == pending.selection.Revision &&
			(v.snapshot.Run == nil || lessID(v.snapshot.Run.ID, out.run)) {
			v.awaiting = out.run
		}
		if !m.foreground(r) || m.focusedDialog() != d {
			return nil
		}
		if r.owner == "" {
			// Only Run materializes a startup thread. Keep the unsent draft in
			// the composer without submitting any of it as part of the run.
			m.freezeIntro(t)
			dst := m.prepareDraftThread(out.thread, model, thinking)
			dst.draft, dst.attachments, dst.stagedSkill = m.editor.promptDraft, t.attachments, t.stagedSkill
			m.editor.set("")
			t.attachments, t.stagedSkill = nil, stagedSkill{}
			return m.switchThread(out.thread)
		}
		return m.closeRunPicker(d)
	}, nil)
}

func (m *Model) chatRunBar(d *dialog, v *planView, width int) (string, string, bool) {
	t := m.threads[d.route.owner]
	if pending := t.plan.chatRequest; pending != nil {
		switch {
		case pending.sending:
			return "Admitting selected tasks…", "", false
		case pending.selection.PlanID != v.snapshot.Plan.ID:
			return m.control("Confirm the previous Run in its plan"), "", false
		default:
			label := fmt.Sprintf("↵ retry Run here @ r%d", pending.selection.Revision)
			if stringWidth(label) > width {
				label = "↵ retry"
			}
			return strongText(label), "", true
		}
	}
	if t.sending || t.aborting || t.attaching || t.plan.sending || t.plan.request != nil {
		return m.control("Wait for the pending request"), "", false
	}
	if v.awaiting != "" {
		return "Admitting selected tasks…", "", false
	}
	if v.snapshot.Run != nil && v.snapshot.Run.State == "running" {
		return v.activity(), "", false
	}
	ids := v.chatSelection(d.plan)
	if len(ids) == 0 {
		if v.note != "" {
			return m.control(oneLine(v.note)), "", false
		}
		if v.snapshot.Run != nil && v.snapshot.Run.Failure != "" {
			return m.paint(x.Warning, oneLine(v.snapshot.Run.Failure)), "", false
		}
		if slices.IndexFunc(v.snapshot.Plan.Steps, func(s sdk.PlanStep) bool { return !v.done(s.ID) }) < 0 {
			return m.control("All tasks reported done · select to revisit"), "", false
		}
		return m.control("Select tasks to run here"), "", false
	}
	bar, preview, _ := m.planSelectionBar(v, d.plan, ids, width, "Run here")
	if v.note != "" {
		preview = m.paint(x.Warning, " · "+oneLine(v.note))
	}
	return bar, preview, true
}
