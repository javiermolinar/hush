package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const planWorktreeTimeout = time.Minute

type planWorktreeBackend interface {
	PreparePlanWorktree(context.Context, sdk.PlanSelection, sdk.PlanWorktreeSource) (sdk.PlanWorktree, error)
	StartPlanWorktree(context.Context, sdk.PlanWorktree, sdk.PlanSelection) (sdk.ID, error)
}

type planWorktreeAdmission struct {
	selection     sdk.PlanSelection
	source        sdk.PlanWorktreeSource
	checkout      sdk.PlanWorktree
	pending       bool
	prepareFailed bool
	starting      bool
	note          string
}

func (p *planWorktreeAdmission) label() string {
	switch {
	case !p.checkout.ThreadID.Valid():
		return "Naming and creating worktree…"
	case p.pending:
		return "Admitting worktree Run…"
	default:
		return "Prepared · " + p.checkout.Checkout.Branch
	}
}

func (m *Model) planWorktreeBar(d *dialog, v *planView) (string, int) {
	if d.plan.workspace != "" || !m.planChatAvailable(v) || !m.menuFeaturesFromBackend.Has(x.PlanWorktree) {
		return "", -1
	}
	if _, ok := m.backend.(planWorktreeBackend); !ok {
		return "", -1
	}
	t := m.threads[d.route.owner]
	if t.sending || t.aborting || t.attaching || t.plan.request != nil || t.plan.sending || t.plan.chatRequest != nil {
		return "", -1
	}
	if p := t.plan.worktree; p != nil {
		if p.pending || p.selection.PlanID != v.snapshot.Plan.ID {
			return "", -1
		}
		switch {
		case p.prepareFailed:
			return "x dismiss worktree error", planWorktreeDismissTarget
		case !p.checkout.ThreadID.Valid():
			return "w retry setup · x stop tracking", planWorktreeTarget
		case p.starting:
			return "w retry worktree Run", planWorktreeTarget
		default:
			return "w review trust · x keep idle", planWorktreeTarget
		}
	}
	if !v.locked() && len(v.chatSelection(d.plan)) > 0 {
		return "w Run in new worktree", planWorktreeTarget
	}
	return "", -1
}

func (m *Model) runPlanWorktree(d *dialog, v *planView) tea.Cmd {
	b, ok := m.backend.(planWorktreeBackend)
	if !ok || !m.planActionDrawn(d, v, planWorktreeTarget) || !v.ready || v.snapshot.Plan == nil || !d.plan.matches(v) {
		return nil
	}
	if _, target := m.planWorktreeBar(d, v); target != planWorktreeTarget {
		return nil
	}
	r := d.route
	t := m.threads[r.owner]
	if request := t.plan.worktree; request != nil {
		if request.checkout.ThreadID.Valid() {
			return m.reviewPlanWorktree(d, request, b)
		}
		return m.preparePlanWorktree(d, request, b)
	}
	plan := v.snapshot.Plan
	request := &planWorktreeAdmission{selection: sdk.PlanSelection{
		ThreadID: r.owner, PlanID: plan.ID, Revision: plan.Revision, StepIDs: v.chatSelection(d.plan), RequestID: submissionKey(),
	}}
	if r.owner == "" {
		request.source.CWD = m.introScope("").CWD
		if model := t.info.Model; model != (sdk.Model{}) {
			request.source.Model, request.source.ThinkingLevel = &model, t.info.Thinking
		}
	}
	t.plan.worktree, v.note = request, ""
	return m.preparePlanWorktree(d, request, b)
}

func (m *Model) preparePlanWorktree(d *dialog, p *planWorktreeAdmission, b planWorktreeBackend) tea.Cmd {
	r := d.route
	p.pending, p.note = true, ""
	d.plan.selectionRev++
	q, source := p.selection, p.source
	return m.startWork(r, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, planWorktreeTimeout)
		defer cancel()
		return b.PreparePlanWorktree(ctx, q, source)
	}, func(value any, err error, _ *int) tea.Cmd {
		p.pending = false
		if err != nil {
			var reply *sdk.ServerError
			retry := isIndeterminate(err) || errors.As(err, &reply) && (reply.Code == "busy" || reply.Code == "limit")
			p.prepareFailed = !retry
			p.note = planWorktreeMessage(err, "Could not create worktree", "Worktree setup unconfirmed")
			return nil
		}
		p.checkout = value.(sdk.PlanWorktree)
		if !m.foreground(r) || m.focusedDialog() != d {
			return nil
		}
		return m.reviewPlanWorktree(d, p, b)
	}, nil)
}

func (m *Model) leavePlanWorktree(d *dialog) tea.Cmd {
	p := &m.threads[d.route.owner].plan
	v := m.planViewFor(d)
	if pending := p.worktree; pending != nil && !pending.pending && !pending.starting && v.snapshot.Plan.ID == pending.selection.PlanID &&
		(m.planActionDrawn(d, v, planWorktreeTarget) || m.planActionDrawn(d, v, planWorktreeDismissTarget)) {
		p.worktree = nil
		d.plan.selectionRev++
		if !pending.prepareFailed {
			return m.notifyRoute(d.route, "Worktree request left idle · check /threads", x.Muted)
		}
	}
	return nil
}

func (m *Model) reviewPlanWorktree(d *dialog, p *planWorktreeAdmission, backend planWorktreeBackend) tea.Cmd {
	if p.starting {
		return m.startPlanWorktree(d, p, backend)
	}
	if len(m.overlays) >= maxDialogs {
		return nil
	}
	cwd := p.checkout.Checkout.CWD
	grant := func() tea.Cmd { return m.startPlanWorktree(d, p, backend) }
	w := &workspaceTrustDialog{cwd: cwd, onGrant: grant}
	trust := &dialog{route: d.route, token: m.token(), workTrust: w}
	opt := m.options.WorkspaceTrust
	if opt == nil || opt.Review == nil {
		w.attached, w.onGrant, w.onStart = true, nil, grant
		w.command = workspaceGrantCommand(cwd)
		return m.pushDialog(trust)
	}
	ctx, cancel := context.WithTimeout(m.ctx, workspaceTrustTimeout)
	w.cancel = cancel
	return tea.Batch(m.pushDialog(trust), reviewWorkspaceTrust(ctx, cancel, opt.Review, cwd, trust.token, false))
}

func (m *Model) startPlanWorktree(d *dialog, p *planWorktreeAdmission, backend planWorktreeBackend) tea.Cmd {
	r := d.route
	t := m.threads[r.owner]
	if !m.foreground(r) || m.focusedDialog() != d || t.plan.worktree != p || p.pending {
		return nil
	}
	v := m.planViewFor(d)
	if v.snapshot.Plan == nil || v.snapshot.Plan.ID != p.selection.PlanID || !v.ready || d.plan.workspace != "" {
		return nil
	}
	p.pending, p.starting, p.note = true, true, ""
	d.plan.selectionRev++
	checkout, selection := p.checkout, p.selection
	return m.startWork(r, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, planWorktreeTimeout)
		defer cancel()
		return backend.StartPlanWorktree(ctx, checkout, selection)
	}, func(_ any, err error, _ *int) tea.Cmd {
		p.pending = false
		if err != nil {
			p.starting = isIndeterminate(err)
			p.note = planWorktreeMessage(err, "Could not start worktree Run", "Worktree Run unconfirmed")
			return nil
		}
		t.plan.worktree = nil
		d.plan.selected = nil
		if m.foreground(r) && m.focusedDialog() == d {
			return m.switchThread(checkout.ThreadID)
		}
		return nil
	}, nil)
}

func planWorktreeMessage(err error, fallback, uncertain string) string {
	var reply *sdk.ServerError
	if errors.As(err, &reply) && reply.Message != "" {
		return oneLine(reply.Message)
	}
	if isIndeterminate(err) {
		return uncertain
	}
	return fallback
}
