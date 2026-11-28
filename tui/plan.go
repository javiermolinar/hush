package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const planID x.ActionID = menusID + ".plan"
const openPlanOp x.LocalOp = "plan.open"
const planRunTarget = -2
const planWorktreeTimeout = time.Minute

// Plans are global: the list spans workspaces. A run needs a thread in the
// plan's workspace, or a new worktree thread created from it.
type planBackend interface {
	WatchPlans(context.Context, string, int) <-chan sdk.PlanListUpdate
	WatchPlan(context.Context, string, string) <-chan sdk.PlanUpdate
	WatchPlanActivity(context.Context, sdk.ID) <-chan sdk.PlanActivityUpdate
	PlanHistory(context.Context, string, string, sdk.ID) (sdk.PlanHistoryPage, error)
	SetPlanClosed(context.Context, string, string, bool) error
	RunPlan(context.Context, sdk.ID, sdk.PlanSelection) (sdk.ID, error)
	RunPlanInWorktree(context.Context, sdk.PlanWorktreeSource, sdk.PlanSelection) (sdk.PlanWorktreeRun, error)
}

// planWorkspacePicker is the plan list. Its directory authorizes the reads.
type planWorkspacePicker struct {
	cwd                           string
	cancel                        context.CancelFunc
	lease                         uint64
	page                          int
	rows                          []sdk.PlanSummary
	more, ready, failed, changing bool
}

type planPicker struct {
	cwd, id       string
	cancel        context.CancelFunc
	snapshot      sdk.PlanSnapshot
	ready         bool
	err           error
	focus, scroll int
	details       bool
	selected      map[string]bool
	note          string
	changing      bool
	detailHeight  int
	detailKey     string
}

// planRequest is a run request whose reply was not confirmed. It belongs to
// the thread, so closing and reopening the plan retries the same key.
type planRequest struct {
	selection sdk.PlanSelection
	title     string
	worktree  bool
	sending   bool
	thread    sdk.ID // the thread a startup Run here created; a retry reuses it
}

// pendingPlanRun is the thread's outstanding request for any plan.
func (m *Model) pendingPlanRun(owner sdk.ID) *planRequest {
	if t := m.threads[owner]; t != nil {
		return t.planRequest
	}
	return nil
}

// planRequest is the route thread's outstanding request for this plan.
func (m *Model) planRequest(d *dialog) *planRequest {
	if req := m.pendingPlanRun(d.route.owner); req != nil && req.selection.PlanID == d.plan.id {
		return req
	}
	return nil
}

type planHistoryPicker struct {
	cwd, id, title  string
	titles          map[string]string
	page            sdk.PlanHistoryPage
	loading, failed bool
}

type planWorkspaceMsg struct {
	lease  uint64
	ctx    context.Context
	source <-chan sdk.PlanListUpdate
	update sdk.PlanListUpdate
	ok     bool
}

type planMsg struct {
	token  uint64
	ctx    context.Context
	source <-chan sdk.PlanUpdate
	update sdk.PlanUpdate
	ok     bool
}

type planRunReply struct {
	thread, run sdk.ID
}

// planCWD is the directory that authorizes plan reads for a route: the
// thread's working directory, or the launch directory before a thread exists.
func (m *Model) planCWD(owner sdk.ID) string {
	if owner.Valid() {
		if t := m.threads[owner]; t != nil {
			return t.info.Workspace
		}
		return ""
	}
	return m.introScope("").CWD
}

// Without a thread there is no feed to report a connection; the negotiated
// capability and a local launch directory are enough to browse.
func (m *Model) planAvailable() bool {
	_, ok := m.backend.(planBackend)
	return ok && m.menuFeaturesFromBackend.Has(x.Plans) && (m.active == "" || m.connection == x.Connected) && m.planCWD(m.active) != ""
}

func (m *Model) openPlan(r ownerRoute) tea.Cmd {
	if !m.foreground(r) || !m.planAvailable() || len(m.overlays) >= maxDialogs {
		return nil
	}
	d := &dialog{route: r, token: m.token(), planWorkspace: &planWorkspacePicker{cwd: m.planCWD(r.owner)}}
	return tea.Batch(m.pushDialog(d), m.watchPlanWorkspace(d, 0))
}

func awaitPlanWorkspace(ctx context.Context, lease uint64, source <-chan sdk.PlanListUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-source:
			return planWorkspaceMsg{lease: lease, ctx: ctx, source: source, update: update, ok: ok}
		}
	}
}

func (m *Model) watchPlanWorkspace(d *dialog, page int) tea.Cmd {
	b, ok := m.backend.(planBackend)
	if !ok {
		return nil
	}
	p := d.planWorkspace
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel, p.lease, p.page = cancel, m.token(), page
	p.rows, p.ready, p.more, p.failed = nil, false, false, false
	d.selected, d.token = 0, m.token()
	lease, cwd := p.lease, p.cwd
	return func() tea.Msg { return awaitPlanWorkspace(ctx, lease, b.WatchPlans(ctx, cwd, page))() }
}

func (m *Model) receivePlanWorkspace(msg planWorkspaceMsg) tea.Cmd {
	for _, d := range m.overlays {
		p := d.planWorkspace
		if p == nil || p.lease != msg.lease || !m.foreground(d.route) {
			continue
		}
		if !msg.ok || msg.update.Err != nil {
			p.failed = true
			if !msg.ok {
				return nil
			}
		} else {
			id := ""
			if d.selected < len(p.rows) {
				id = p.rows[d.selected].ID
			}
			p.rows, p.more, p.ready, p.failed = msg.update.Plans, msg.update.More, true, false
			d.selected = min(d.selected, max(0, len(p.rows)-1))
			for i, r := range p.rows {
				if r.ID == id {
					d.selected = i
					break
				}
			}
			d.token = m.token() // stale mouse rows must not open a different plan
		}
		return awaitPlanWorkspace(msg.ctx, msg.lease, msg.source)
	}
	return nil
}

func (m *Model) planWorkspaceKey(d *dialog, chord string) tea.Cmd {
	p := d.planWorkspace
	switch chord {
	case "up":
		d.selected = max(0, d.selected-1)
	case "down":
		d.selected = min(max(0, len(p.rows)-1), d.selected+1)
	case "home":
		d.selected = 0
	case "end":
		d.selected = max(0, len(p.rows)-1)
	case "left":
		if p.page > 0 {
			return m.watchPlanWorkspace(d, p.page-1)
		}
	case "right":
		if p.more {
			return m.watchPlanWorkspace(d, p.page+1)
		}
	case "r":
		return m.watchPlanWorkspace(d, p.page)
	case "enter", "c", "h":
		if !p.ready || p.failed || d.selected >= len(p.rows) {
			return nil
		}
		row := p.rows[d.selected]
		switch chord {
		case "c":
			if row.Workspace == p.cwd && (row.Closed || !row.Running) {
				return m.changePlanClosed(d, &p.changing, p.cwd, row.ID, !row.Closed)
			}
		case "h":
			return m.openPlanHistory(d.route, p.cwd, row.ID, row.Title, nil)
		default:
			return m.openPlanSteps(d.route, p.cwd, row.ID)
		}
	}
	return nil
}

func planSummaryStatus(p sdk.PlanSummary) (label, icon string, tone x.Tone) {
	switch {
	case p.Running:
		return "Running", "◐", x.Warning
	case p.Done == p.Total:
		return "Complete", "✓", x.Good
	case p.Closed:
		return "Closed", "⏸", x.Muted
	case p.Blocked > 0:
		return fmt.Sprintf("%d blocked", p.Blocked), "⏸", x.Warning
	case p.Done > 0:
		return fmt.Sprintf("%d/%d done", p.Done, p.Total), "○", x.Muted
	default:
		return "Pending", "○", x.Muted
	}
}

func (m *Model) planWorkspaceRows(d *dialog, w, n int) menuFrame {
	p := d.planWorkspace
	c := menuChrome{title: "Plans", scope: "all workspaces", count: menuCount(len(p.rows), "plan", "plans")}
	tail := []string{"r refresh", d.zoomKey()}
	if p.page > 0 || p.more {
		c.scope += fmt.Sprintf(" · page %d", p.page+1)
		tail = append([]string{"←→ pages"}, tail...)
	}
	if p.failed || !p.ready || len(p.rows) == 0 {
		c.keys = chromeLine(m.menuKeys(w, append([]string{"↑↓ move"}, tail...)...))
		message, tone := "No plans yet · ask for a plan in chat", x.Muted
		switch {
		case p.failed:
			message, tone = "Could not load plans · r retry", x.Danger
		case !p.ready:
			message = "Loading plans…"
		case p.page > 0:
			message = "No plans on this page · ← previous page"
		}
		return m.menuShell(w, n, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.paint(tone, message)}, targets: []int{-1}}
		})
	}
	pick := p.rows[d.selected]
	c.consequence = chromeLine(m.menuAction("Open", oneLine(pick.Title)))
	hints := []string{"↵ open", "↑↓ move", "h history"}
	if pick.Workspace == p.cwd {
		if pick.Closed {
			hints = append(hints, "c reopen")
		} else if !pick.Running {
			hints = append(hints, "c close")
		}
	}
	c.keys = chromeLine(m.menuKeys(w, append(hints, tail...)...))
	detail := fmt.Sprintf("%d/%d done · r%d", pick.Done, pick.Total, pick.Revision)
	if pick.Blocked > 0 {
		detail += fmt.Sprintf(" · %d blocked", pick.Blocked)
	}
	if pick.Running {
		detail += "\nA run is active"
	}
	if pick.Workspace != p.cwd {
		detail += "\nWorkspace " + pick.Workspace
	}
	return m.menuShell(w, n, c, func(h int) menuFrame {
		return m.menuColumns(w, h, func(width, height int) []string { return m.menuDetail(pick.Title, detail, width, height) }, func(lw, budget int) menuFrame {
			var out menuFrame
			// Group by open or closed, and by workspace: this one first, then others.
			group := func(i int) string {
				r := p.rows[i]
				switch {
				case r.Closed:
					return "Closed plans"
				case r.Workspace == p.cwd:
					return "This workspace"
				default:
					return "Other workspaces"
				}
			}
			grouped := budget >= 3 && group(0) != group(len(p.rows)-1)
			first := d.selected
			if grouped {
				for cost := 2; first > 0; {
					step := 1
					if group(first-1) != group(first) {
						step++
					}
					if cost+step > budget {
						break
					}
					first, cost = first-1, cost+step
				}
			} else {
				first, _ = menuWindow(d.selected, len(p.rows), budget)
			}
			for i := first; i < len(p.rows) && budget > 0; i++ {
				if grouped && (i == first || group(i) != group(i-1)) {
					if budget < 2 {
						break
					}
					out.add(m.quiet(group(i)), -1)
					budget--
				}
				status, icon, tone := planSummaryStatus(p.rows[i])
				out.add(m.menuRow(lw, 13, menuItem{title: p.rows[i].Title, icon: icon, state: status, tone: tone, focus: i == d.selected}), i)
				budget--
			}
			return out
		})
	})
}

func (m *Model) openPlanSteps(r ownerRoute, cwd, id string) tea.Cmd {
	b, ok := m.backend.(planBackend)
	if !ok || !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	p := m.newPlanPicker(r.owner, cwd, id)
	p.cancel = cancel
	d := &dialog{route: r, token: m.token(), plan: p}
	token := d.token
	return tea.Batch(m.pushDialog(d), func() tea.Msg { return awaitPlan(ctx, token, b.WatchPlan(ctx, cwd, id))() })
}

// newPlanPicker shows the thread's unconfirmed request for this plan, if any.
func (m *Model) newPlanPicker(owner sdk.ID, cwd, id string) *planPicker {
	p := &planPicker{cwd: cwd, id: id}
	if req := m.pendingPlanRun(owner); req != nil && req.selection.PlanID == id {
		p.selected = make(map[string]bool)
		for _, step := range req.selection.StepIDs {
			p.selected[step] = true
		}
	}
	return p
}

func awaitPlan(ctx context.Context, token uint64, source <-chan sdk.PlanUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-source:
			return planMsg{token: token, ctx: ctx, source: source, update: update, ok: ok}
		}
	}
}

func (m *Model) receivePlan(msg planMsg) tea.Cmd {
	for _, d := range m.overlays {
		p := d.plan
		if p == nil || d.token != msg.token {
			continue
		}
		if !msg.ok {
			p.ready = false
			return nil
		}
		p.err, p.ready = msg.update.Err, msg.update.Err == nil
		if p.ready {
			next := msg.update.Snapshot
			if p.snapshot.Plan.Revision != 0 && p.snapshot.Plan.Revision != next.Plan.Revision {
				p.focus, p.scroll, p.selected = 0, 0, nil
				p.note = "Plan changed · select steps of the new revision"
			}
			p.snapshot = next
			p.focus = min(p.focus, len(next.Plan.Steps)-1)
			for id := range p.selected {
				if !planSelectable(&p.snapshot.Plan, id) {
					delete(p.selected, id)
				}
			}
		}
		return awaitPlan(msg.ctx, msg.token, msg.source)
	}
	return nil
}

func planStep(plan *sdk.Plan, id string) *sdk.PlanStep {
	for i := range plan.Steps {
		if plan.Steps[i].ID == id {
			return &plan.Steps[i]
		}
	}
	return nil
}

func planTitle(plan *sdk.Plan, id string) string {
	if s := planStep(plan, id); s != nil {
		return oneLine(s.Title)
	}
	return id
}

// A done step is final and a step can be in only one active run.
func planSelectable(plan *sdk.Plan, id string) bool {
	s := planStep(plan, id)
	return s != nil && !plan.Closed && s.Status != "done" && plan.Running(id) == nil
}

// planSelection is the selected steps in plan order, or else the focused one.
func (p *planPicker) planSelection() []string {
	plan := &p.snapshot.Plan
	var ids []string
	for _, s := range plan.Steps {
		if p.selected[s.ID] {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 && p.focus < len(plan.Steps) && planSelectable(plan, plan.Steps[p.focus].ID) {
		ids = []string{plan.Steps[p.focus].ID}
	}
	return ids
}

// planRunKey identifies the run the consequence line offers now: the plan's
// watch revision and the steps Enter would send. Empty when it can't run.
func (m *Model) planRunKey(d *dialog) string {
	p := d.plan
	if !p.ready || m.planRequest(d) != nil || m.planBlocker(d) != "" {
		return ""
	}
	return p.snapshot.Revision + ":" + strings.Join(p.planSelection(), ",")
}

// planBlocker says why neither run action can start now, or returns "".
func (m *Model) planBlocker(d *dialog) string {
	p := d.plan
	plan := &p.snapshot.Plan
	switch {
	case plan.Complete:
		return "Complete · every step is done"
	case plan.Closed:
		if plan.Workspace == p.cwd {
			return "Closed · c reopen"
		}
		return "Closed"
	case plan.Workspace != m.planCWD(d.route.owner):
		return "Runs from " + filepath.Base(plan.Workspace) + " · open the plan from a thread there"
	case m.planRequest(d) != nil && m.planRequest(d).sending:
		return "Starting the run…"
	}
	if req := m.pendingPlanRun(d.route.owner); req != nil && m.planRequest(d) == nil {
		return "Retry the unconfirmed run of " + oneLine(req.title) + " first"
	}
	ids := p.planSelection()
	if len(ids) == 0 {
		if p.focus < len(plan.Steps) {
			s := plan.Steps[p.focus]
			if s.Status == "done" {
				return "Done · select a pending step"
			}
			if run := plan.Running(s.ID); run != nil {
				return "Running in thread #" + string(run.ThreadID)
			}
		}
		return "Select the steps to run"
	}
	delegated := 0
	for _, id := range ids {
		if planStep(plan, id).Delegate != nil {
			delegated++
		}
	}
	if delegated > 5 {
		return "A run can delegate at most 5 steps"
	}
	return ""
}

func (m *Model) togglePlanStep(d *dialog) {
	p := d.plan
	if !p.ready || m.pendingPlanRun(d.route.owner) != nil || p.focus >= len(p.snapshot.Plan.Steps) {
		return
	}
	id := p.snapshot.Plan.Steps[p.focus].ID
	if !planSelectable(&p.snapshot.Plan, id) {
		return
	}
	if p.selected == nil {
		p.selected = make(map[string]bool)
	}
	if p.selected[id] {
		delete(p.selected, id)
	} else {
		p.selected[id] = true
	}
	p.note = ""
}

func (m *Model) planKey(d *dialog, chord string) tea.Cmd {
	p := d.plan
	if !p.ready {
		return nil
	}
	steps := p.snapshot.Plan.Steps
	switch chord {
	case "up":
		if p.details {
			p.scroll = max(0, p.scroll-1)
		} else {
			p.focus, p.scroll = max(0, p.focus-1), 0
		}
	case "down":
		if p.details {
			p.scroll++
		} else {
			p.focus, p.scroll = min(len(steps)-1, p.focus+1), 0
		}
	case "home":
		p.focus, p.scroll = 0, 0
	case "end":
		p.focus, p.scroll = len(steps)-1, 0
	case "tab":
		p.details, p.scroll = !p.details, 0
	case "space":
		if !p.details {
			m.togglePlanStep(d)
		}
	case "enter":
		return m.runPlan(d, false)
	case "w":
		return m.runPlan(d, true)
	case "h":
		titles := make(map[string]string, len(steps))
		for _, s := range steps {
			titles[s.ID] = oneLine(s.Title)
		}
		return m.openPlanHistory(d.route, p.cwd, p.id, p.snapshot.Plan.Title, titles)
	case "c":
		plan := &p.snapshot.Plan
		if plan.Workspace == p.cwd && (plan.Closed || len(plan.Runs) == 0) {
			return m.changePlanClosed(d, &p.changing, p.cwd, p.id, !plan.Closed)
		}
	}
	return nil
}

func (m *Model) runPlan(d *dialog, worktree bool) tea.Cmd {
	b, ok := m.backend.(planBackend)
	p := d.plan
	if !ok || !p.ready || !m.dialogVisible(d) {
		return nil
	}
	r := d.route
	t := m.threads[r.owner]
	if t == nil || t.sending || t.aborting || t.attaching {
		return nil
	}
	req := m.planRequest(d)
	if req == nil {
		// Run only what the drawn frame offered. A plan update, a new
		// selection or a focus move since then needs a new frame first.
		if key := m.planRunKey(d); key == "" || key != m.frame.dialogPlanRun {
			return nil
		}
		plan := &p.snapshot.Plan
		req = &planRequest{worktree: worktree, title: plan.Title, selection: sdk.PlanSelection{PlanID: plan.ID, Revision: plan.Revision,
			StepIDs: p.planSelection(), RequestID: submissionKey()}}
		t.planRequest = req
	} else if req.sending || req.worktree != worktree {
		return nil
	}
	req.sending, p.note = true, ""
	selection := req.selection
	if worktree {
		source := sdk.PlanWorktreeSource{Thread: r.owner}
		if !r.owner.Valid() {
			source = sdk.PlanWorktreeSource{CWD: m.planCWD("")}
			if model := t.info.Model; model != (sdk.Model{}) {
				source.Model, source.ThinkingLevel = &model, t.info.Thinking
				if source.ThinkingLevel == "" {
					source.ThinkingLevel = "off"
				}
			}
		}
		return m.startWork(r, func(ctx context.Context) (any, error) {
			ctx, cancel := context.WithTimeout(ctx, planWorktreeTimeout)
			defer cancel()
			return b.RunPlanInWorktree(ctx, source, selection)
		}, func(value any, err error, _ *int) tea.Cmd {
			req.sending = false
			if err != nil {
				m.planRunFailed(t, req, p, err, "Could not start the worktree run")
				return nil
			}
			m.clearPlanRequest(t, req)
			p.selected = nil
			out, _ := value.(sdk.PlanWorktreeRun)
			if !m.foreground(r) || m.focusedDialog() != d {
				return nil
			}
			return m.switchThread(out.ThreadID)
		}, nil)
	}
	backend, options := m.backend, m.options.NewThread
	model, thinking := t.info.Model, t.info.Thinking
	target := r.owner
	if !r.owner.Valid() {
		t.sending = true // serialize the first thread with ordinary prompt submission
		target = req.thread
	}
	return m.startWork(r, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		thread := target
		if !thread.Valid() {
			id, err := createDraftThread(ctx, backend, options, model, thinking)
			if err != nil {
				return planRunReply{}, err
			}
			thread = id
		}
		run, err := b.RunPlan(ctx, thread, selection)
		return planRunReply{thread, run}, err
	}, func(value any, err error, _ *int) tea.Cmd {
		req.sending = false
		if !r.owner.Valid() {
			t.sending = false
		}
		out, _ := value.(planRunReply)
		if !r.owner.Valid() && out.thread.Valid() {
			req.thread = out.thread // receipts are per thread; a retry must not create another
		}
		if err != nil {
			if !r.owner.Valid() && !out.thread.Valid() {
				// Without a thread there is nothing to retry against.
				m.clearPlanRequest(t, req)
				if isIndeterminate(err) {
					p.note = "Thread creation unconfirmed · check /threads before retrying"
					return nil
				}
			}
			m.planRunFailed(t, req, p, err, "Could not run the selected steps")
			return nil
		}
		m.clearPlanRequest(t, req)
		p.selected = nil
		if !m.foreground(r) || m.focusedDialog() != d {
			return nil
		}
		if !r.owner.Valid() {
			// Only Run creates a startup thread. Keep the unsent draft in the
			// composer without submitting any of it as part of the run.
			m.freezeIntro(t)
			dst := m.prepareDraftThread(out.thread, model, thinking)
			dst.draft, dst.attachments, dst.stagedSkill = m.editor.promptDraft, t.attachments, t.stagedSkill
			m.editor.set("")
			t.attachments, t.stagedSkill = nil, stagedSkill{}
			return m.switchThread(out.thread)
		}
		return m.closePlanDialogs(d)
	}, nil)
}

// An indeterminate reply keeps the request so the same key is retried.
func (m *Model) planRunFailed(t *threadState, req *planRequest, p *planPicker, err error, fallback string) {
	var reply *sdk.ServerError
	switch {
	case isIndeterminate(err) && t.planRequest == req:
		p.note = "Run unconfirmed · press the same key to retry it"
		return
	case errors.As(err, &reply) && reply.Message != "":
		p.note = oneLine(reply.Message)
	default:
		p.note = fallback
	}
	m.clearPlanRequest(t, req)
}

func (m *Model) clearPlanRequest(t *threadState, req *planRequest) {
	if t.planRequest == req {
		t.planRequest = nil
	}
}

// closePlanDialogs returns to chat after a run starts in this thread.
func (m *Model) closePlanDialogs(d *dialog) tea.Cmd {
	cmds := []tea.Cmd{m.closeDialog(d, nil, false)}
	for {
		parent := m.focusedDialog()
		if parent == nil || parent.planWorkspace == nil && parent.planHistory == nil {
			break
		}
		cmds = append(cmds, m.closeDialog(parent, nil, false))
	}
	return tea.Batch(cmds...)
}

func (m *Model) changePlanClosed(d *dialog, busy *bool, cwd, id string, closed bool) tea.Cmd {
	b, ok := m.backend.(planBackend)
	if !ok || *busy {
		return nil
	}
	*busy = true
	r := d.route
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return nil, b.SetPlanClosed(call, cwd, id, closed)
	}, func(_ any, err error, _ *int) tea.Cmd {
		*busy = false
		if err != nil {
			var reply *sdk.ServerError
			text := "Could not change the plan"
			if errors.As(err, &reply) && reply.Message != "" {
				text = oneLine(reply.Message)
			}
			return m.notifyRoute(r, text, x.Warning)
		}
		return nil
	}, nil)
}

func planStepDisplay(plan *sdk.Plan, s sdk.PlanStep) (label, mark string, tone x.Tone) {
	switch {
	case s.Status == "done":
		return "Done", "✓", x.Good
	case plan.Running(s.ID) != nil:
		return "Running", "◐", x.Warning
	case s.Status == "blocked":
		return "Blocked", "⏸", x.Warning
	default:
		return "Pending", "○", x.Muted
	}
}

func (m *Model) planRows(d *dialog, w, n int) menuFrame {
	p := d.plan
	c := menuChrome{title: "Plan", keys: chromeLine(m.menuKeys(w, d.zoomKey()))}
	if !p.ready {
		status, tone := "Loading plan…", x.Muted
		if p.err != nil {
			status, tone = "Could not load the plan · esc back and reopen", x.Danger
		}
		return m.menuShell(w, n, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.paint(tone, status)}, targets: []int{-1}}
		})
	}
	plan := &p.snapshot.Plan
	done := 0
	for _, s := range plan.Steps {
		if s.Status == "done" {
			done++
		}
	}
	c.title, c.scope = plan.Title, fmt.Sprintf("r%d", plan.Revision)
	if plan.Workspace != p.cwd {
		c.scope += " · " + filepath.Base(plan.Workspace)
	}
	c.count = fmt.Sprintf("%d/%d done", done, len(plan.Steps))
	blocker := m.planBlocker(d)
	bar, runnable := m.planRunBar(d, w, blocker)
	c.consequence = chromeLine(bar)
	if runnable && stringWidth(bar) <= w {
		c.consequence.target = planRunTarget
	}
	move, tab := "↑↓ move", "tab details"
	if p.details {
		move, tab = "↑↓ scroll", "tab steps"
	}
	var hints []string
	canSelect := !plan.Closed && m.pendingPlanRun(d.route.owner) == nil && plan.Workspace == m.planCWD(d.route.owner)
	if canSelect && !p.details {
		hints = append(hints, "space select")
	}
	if runnable {
		hints = append(hints, "↵ run here", "w new worktree")
	}
	hints = append(hints, tab, move, "h history")
	if plan.Workspace == p.cwd {
		if plan.Closed {
			hints = append(hints, "c reopen")
		} else if len(plan.Runs) == 0 {
			hints = append(hints, "c close")
		}
	}
	hints = append(hints, d.zoomKey())
	c.keys = chromeLine(m.menuKeys(w, hints...))
	return m.menuShell(w, n, c, func(h int) menuFrame {
		if h == 1 && p.details && w == menuListWidth(w) {
			return menuFrame{rows: m.planDetailWindow(p, w, 1), targets: []int{-1}}
		}
		return m.menuColumns(w, h, func(width, height int) []string { return m.planDetailWindow(p, width, height) }, func(lw, lh int) menuFrame {
			var out menuFrame
			start, end := menuWindow(p.focus, len(plan.Steps), lh)
			for i := start; i < end; i++ {
				s := plan.Steps[i]
				state, glyph, tone := planStepDisplay(plan, s)
				item := menuItem{title: s.Title, icon: glyph, state: state, tone: tone, focus: i == p.focus}
				if canSelect || len(p.selected) != 0 {
					switch {
					case p.selected[s.ID]:
						item.selection = "■"
					case planSelectable(plan, s.ID):
						item.selection = "□"
					default:
						item.selection = " " // done or running: keep the column, offer no box
					}
				}
				out.add(m.menuRow(lw, 13, item), i)
			}
			return out
		})
	})
}

// planRunBar says what Enter does now, or why it can't run.
func (m *Model) planRunBar(d *dialog, width int, blocker string) (string, bool) {
	p := d.plan
	plan := &p.snapshot.Plan
	if req := m.planRequest(d); req != nil && !req.sending {
		label := "↵ retry the same run"
		if req.worktree {
			label = "w retry the same worktree run"
		}
		return strongText(label) + m.paint(x.Muted, " · no receipt"), !req.worktree
	}
	if blocker != "" {
		tone := x.Muted
		if strings.HasPrefix(blocker, "A run can") {
			tone = x.Warning
		}
		return m.paint(tone, blocker), false
	}
	ids := p.planSelection()
	head := fmt.Sprintf("↵ Run %d here @ r%d", len(ids), plan.Revision)
	if stringWidth(head) > width {
		return strongText(fmt.Sprintf("↵ Run %d", len(ids))), true
	}
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, planTitle(plan, id))
	}
	tail := strings.Join(labels, " → ")
	if p.note != "" {
		tail = p.note
	}
	room := width - stringWidth(head) - 2
	if room <= 3 {
		return strongText(head), true
	}
	tone := x.Muted
	if p.note != "" {
		tone = x.Warning
	}
	return strongText(head) + m.paint(tone, "  "+ansi.Truncate(tail, room, "…")), true
}

func (m *Model) planDetailWindow(p *planPicker, width, height int) []string {
	plan := &p.snapshot.Plan
	rows := m.planDetails(p, plan.Steps[p.focus], width)
	if len(rows) >= height {
		from := min(p.scroll, len(rows)-height)
		p.scroll = from
		return rows[from : from+height]
	}
	// Size the pane to the tallest step so moving the cursor doesn't resize it.
	key := fmt.Sprintf("%s:%d:%d", p.snapshot.Revision, width, height)
	if p.detailKey != key {
		tallest := len(rows)
		for i, step := range plan.Steps {
			if i != p.focus && tallest < height {
				tallest = max(tallest, len(m.planDetails(p, step, width)))
			}
		}
		p.detailKey, p.detailHeight = key, min(tallest, height)
	}
	if p.detailHeight > len(rows) {
		rows = append(rows, make([]string, p.detailHeight-len(rows))...)
	}
	return rows
}

func (m *Model) planDetails(p *planPicker, s sdk.PlanStep, width int) []string {
	plan := &p.snapshot.Plan
	rows := wrapped(strongText(oneLine(s.Title)), width)
	for _, line := range strings.Split(s.Prompt, "\n") {
		for _, part := range wrapped(oneLine(line), width) {
			rows = append(rows, m.quiet(part))
		}
	}
	add := func(label, value string, tone x.Tone) {
		const labelWidth = 10
		if width < 40 {
			for _, line := range wrapped(label+": "+value, width) {
				rows = append(rows, m.paint(tone, line))
			}
			return
		}
		prefix := m.paint(x.Muted, fit(label, labelWidth))
		for i, line := range wrapped(value, width-labelWidth) {
			if i != 0 {
				prefix = strings.Repeat(" ", labelWidth)
			}
			rows = append(rows, prefix+m.paint(tone, line))
		}
	}
	for _, id := range s.Requires {
		add("Needs", planTitle(plan, id), x.Plain)
	}
	if dl := s.Delegate; dl != nil {
		mode := "read-only subagent"
		if dl.Writes() {
			mode = "writing subagent"
		}
		if dl.Worktree != "" {
			mode += " · " + oneLine(dl.Worktree)
		}
		if dl.PWD != "" {
			mode += " · " + oneLine(dl.PWD)
		}
		add("Delegate", mode, x.Plain)
		if dl.Thinking != "" {
			add("Thinking", dl.Thinking, x.Plain)
		}
	}
	if run := plan.Running(s.ID); run != nil {
		add("Running", "thread #"+string(run.ThreadID), x.Warning)
	}
	if s.Status == "blocked" && s.Reason != "" {
		add("Blocked", oneLine(s.Reason), x.Warning)
	}
	return rows
}

func (m *Model) openPlanHistory(r ownerRoute, cwd, id, title string, titles map[string]string) tea.Cmd {
	if !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	d := &dialog{route: r, token: m.token(), planHistory: &planHistoryPicker{cwd: cwd, id: id, title: title, titles: titles}}
	return tea.Batch(m.pushDialog(d), m.loadPlanHistory(d, ""))
}

func (m *Model) loadPlanHistory(d *dialog, before sdk.ID) tea.Cmd {
	p := d.planHistory
	b, ok := m.backend.(planBackend)
	if !ok || p.loading {
		return nil
	}
	p.loading, p.failed = true, false
	r, cwd, id := d.route, p.cwd, p.id
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return b.PlanHistory(call, cwd, id, before)
	}, func(value any, err error, _ *int) tea.Cmd {
		p.loading, p.failed = false, err != nil
		if page, ok := value.(sdk.PlanHistoryPage); ok && err == nil {
			p.page, d.selected = page, 0
			d.token = m.token()
		}
		return nil
	}, nil)
}

func (m *Model) planHistoryKey(d *dialog, chord string) tea.Cmd {
	p := d.planHistory
	runs := p.page.Runs
	switch chord {
	case "up":
		d.selected = max(0, d.selected-1)
	case "down":
		d.selected = min(max(0, len(runs)-1), d.selected+1)
	case "home":
		d.selected = 0
	case "left", "r":
		return m.loadPlanHistory(d, "")
	case "right":
		if p.page.Next.Valid() {
			return m.loadPlanHistory(d, p.page.Next)
		}
	case "enter":
		if d.selected < len(runs) {
			return m.switchThread(runs[d.selected].ThreadID)
		}
	}
	return nil
}

func planRunDisplay(run sdk.PlanRunRecord) (label, icon string, tone x.Tone) {
	switch run.State {
	case "running":
		return "Running", "◐", x.Warning
	case "failed":
		return "Stopped", "✗", x.Danger
	default:
		return "Finished", "✓", x.Muted
	}
}

func (p *planHistoryPicker) stepTitle(id string) string {
	if title := p.titles[id]; title != "" {
		return title
	}
	return id
}

func (m *Model) planHistoryRows(d *dialog, w, n int) menuFrame {
	p := d.planHistory
	runs := p.page.Runs
	c := menuChrome{title: "Run history", scope: oneLine(p.title), count: menuCount(len(runs), "run", "runs")}
	hints := []string{"↑↓ move", "r refresh"}
	if p.page.Next.Valid() {
		hints = append(hints, "→ older", "← latest")
	}
	if p.loading || p.failed || len(runs) == 0 {
		c.keys = chromeLine(m.menuKeys(w, append(hints[1:], d.zoomKey())...))
		message, tone := "No runs yet", x.Muted
		switch {
		case p.loading:
			message = "Loading run history…"
		case p.failed:
			message, tone = "Could not load run history · r retry", x.Danger
		}
		return m.menuShell(w, n, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.paint(tone, message)}, targets: []int{-1}}
		})
	}
	pick := runs[min(d.selected, len(runs)-1)]
	c.consequence = chromeLine(m.menuAction("Open", "thread #"+string(pick.ThreadID)))
	c.keys = chromeLine(m.menuKeys(w, append(append([]string{"↵ open thread"}, hints...), d.zoomKey())...))
	return m.menuShell(w, n, c, func(h int) menuFrame {
		return m.menuColumns(w, h, func(width, height int) []string {
			where := "Thread #" + string(pick.ThreadID)
			if pick.Worktree {
				where += " · new worktree"
			}
			lines := []string{where, fmt.Sprintf("Revision %d", pick.Revision)}
			if pick.Failure != "" {
				lines = append(lines, oneLine(pick.Failure))
			}
			for _, id := range pick.StepIDs {
				status := "not reported"
				for _, o := range pick.Outcomes {
					if o.StepID == id {
						status = o.Status + " · " + oneLine(o.Detail)
					}
				}
				lines = append(lines, p.stepTitle(id)+": "+status)
			}
			return m.menuDetail(fmt.Sprintf("Run of %d %s", len(pick.StepIDs), plural(len(pick.StepIDs), "step", "steps")),
				strings.Join(lines, "\n"), width, height)
		}, func(lw, budget int) menuFrame {
			var out menuFrame
			start, end := menuWindow(d.selected, len(runs), budget)
			for i := start; i < end; i++ {
				run := runs[i]
				label, icon, tone := planRunDisplay(run)
				titles := make([]string, 0, len(run.StepIDs))
				for _, id := range run.StepIDs {
					titles = append(titles, p.stepTitle(id))
				}
				out.add(m.menuRow(lw, 10, menuItem{title: strings.Join(titles, ", "), icon: icon, state: label, tone: tone, focus: i == d.selected}), i)
			}
			return out
		})
	})
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
