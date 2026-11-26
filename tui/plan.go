package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const planID x.ActionID = menusID + ".plan"
const openPlanOp x.LocalOp = "plan.open"
const planRunTarget = -2
const planScopedTarget = -3
const planWorktreeTarget = -4
const planWorktreeDismissTarget = -5

type planBackend interface {
	WatchPlan(context.Context, sdk.ID) <-chan sdk.PlanUpdate
	RunPlan(context.Context, sdk.PlanSelection) (sdk.ID, error)
}

type planView struct {
	snapshot    sdk.PlanSnapshot
	ready       bool
	err         error
	request     *sdk.PlanSelection     // retained after an indeterminate reply, never retried with a new key
	worktree    *planWorktreeAdmission // shared with other pickers of this thread
	chatRequest *planChatAdmission     // shared by all pickers of the initiating thread
	sending     bool
	awaiting    sdk.ID
	note        string
}

type planPicker struct {
	named        string
	workspace    string // nonempty for a read-only preview from another workspace
	expected     string // a workspace row may not follow a different binding mid-inspection
	view         planView
	cancel       context.CancelFunc
	changing     bool
	focus        int
	details      bool
	scroll       int
	selected     map[string]bool
	selectionRev uint64
}

type planWatchMsg struct {
	token  uint64
	source <-chan sdk.PlanUpdate
	update sdk.PlanUpdate
	ok     bool
}

func (m *Model) planAvailable() bool {
	return m.menuFeaturesFromBackend.Has(x.Plans) && m.active.Valid() && m.connection == x.Connected
}

func (m *Model) stopPlanWatch() {
	if m.planFeed.cancel != nil {
		m.planFeed.cancel()
	}
	m.planFeed = feedLease{}
}

func (m *Model) syncPlanWatch() tea.Cmd {
	b, ok := m.backend.(planBackend)
	if !ok || !m.menuFeaturesFromBackend.Has(x.Plans) || !m.active.Valid() {
		m.stopPlanWatch()
		return nil
	}
	if m.planFeed.token != 0 && m.planFeed.owner == m.active {
		return nil
	}
	m.stopPlanWatch()
	ctx, cancel := context.WithCancel(m.ctx)
	f := feedLease{token: m.token(), owner: m.active, ctx: ctx, cancel: cancel}
	m.planFeed = f
	m.threads[f.owner].plan.ready = false
	return func() tea.Msg { return awaitPlanWatch(f, b.WatchPlan(ctx, f.owner))() }
}

func awaitPlanWatch(f feedLease, source <-chan sdk.PlanUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case v, ok := <-source:
			return planWatchMsg{f.token, source, v, ok}
		}
	}
}

func (m *Model) receivePlanWatch(msg planWatchMsg) tea.Cmd {
	f := m.planFeed
	if f.token == 0 || msg.token != f.token || f.owner != m.active {
		return nil
	}
	v := &m.threads[f.owner].plan
	if !msg.ok {
		m.stopPlanWatch()
		v.ready = false
		return nil
	}
	v.err = msg.update.Err
	v.ready = v.err == nil
	if v.ready {
		next := msg.update.Snapshot
		old := v.snapshot.Plan
		changed := old != nil && (next.Plan == nil || old.ID != next.Plan.ID || old.Revision != next.Plan.Revision)
		if changed || next.Closed != v.snapshot.Closed {
			for _, d := range m.overlays {
				if d.plan != nil && d.plan.named == "" {
					d.plan.selected = nil
					d.plan.focus, d.plan.scroll = 0, 0
				}
			}
			v.note = ""
			if changed {
				v.note = "Plan changed · select the new revision"
				v.awaiting = ""
			}
		}
		if !planRunnable(next.RunAvailability) {
			v.request = nil
			if !next.Chat {
				v.awaiting = ""
			}
		}
		v.snapshot = next
		if next.Run != nil && v.awaiting != "" && !lessID(next.Run.ID, v.awaiting) {
			v.awaiting = ""
		}
		for _, d := range m.overlays {
			if d.plan != nil && d.plan.named == "" {
				for id := range d.plan.selected {
					if v.progress(id).State == "verified" {
						delete(d.plan.selected, id)
						d.plan.selectionRev++
					}
				}
			}
		}
	}
	return awaitPlanWatch(f, msg.source)
}

func (m *Model) draftPlanAvailable() bool {
	// With no thread feed, use negotiated capabilities and the local launch
	// directory. An attached client cannot assume its cwd is on the host.
	return m.active == "" && m.menuFeaturesFromBackend.Has(x.Plans|x.PlanWorkspace|x.PlanChat) && m.introScope("").CWD != ""
}

func (m *Model) openPlan(r ownerRoute) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	if m.draftPlanAvailable() {
		return m.openPlanWorkspace(r)
	}
	if !m.planAvailable() {
		return nil
	}
	if m.menuFeaturesFromBackend.Has(x.PlanWorkspace) {
		return m.openPlanWorkspace(r)
	}
	return m.openBoundPlan(r)
}

func (m *Model) planViewFor(d *dialog) *planView {
	if d.plan.named != "" {
		return &d.plan.view
	}
	return &m.threads[d.route.owner].plan
}

func (m *Model) openBoundPlan(r ownerRoute) tea.Cmd { return m.openPlanSteps(r, "") }

func (m *Model) openPlanSteps(r ownerRoute, expected string) tea.Cmd {
	if !m.foreground(r) || !m.planAvailable() || len(m.overlays) >= maxDialogs {
		return nil
	}
	picker := &planPicker{expected: expected}
	if selection := m.threads[r.owner].plan.request; selection != nil && (expected == "" || selection.PlanID == expected) {
		picker.selected = make(map[string]bool, len(selection.StepIDs))
		for _, id := range selection.StepIDs {
			picker.selected[id] = true
		}
	}
	d := &dialog{route: r, token: m.token(), plan: picker}
	m.showChatSelection(d, m.planViewFor(d))
	return tea.Batch(m.pushDialog(d), m.syncPlanWatch())
}

func (v *planView) progress(id string) sdk.PlanProgress {
	for _, p := range v.snapshot.Progress {
		if p.StepID == id {
			return p
		}
	}
	return sdk.PlanProgress{}
}

func (v *planView) title(id string) string {
	for _, s := range v.snapshot.Plan.Steps {
		if s.ID == id {
			return oneLine(s.Title)
		}
	}
	return id
}

func planRunnable(a sdk.PlanRunAvailability) bool {
	return a == sdk.PlanRunFixture || a == sdk.PlanRunScoped
}

func canStopPlan(t *threadState) bool {
	v := &t.plan
	run := v.snapshot.Run
	return v.ready && v.snapshot.RunAvailability == sdk.PlanRunScoped && run != nil &&
		!run.Chat && run.State == "running" && run.OwnerThreadID == t.info.ID
}

func (v *planView) locked() bool {
	return v.snapshot.Closed || v.snapshot.Complete || v.snapshot.Settling || (!v.snapshot.Chat && !planRunnable(v.snapshot.RunAvailability)) || v.sending || v.awaiting != "" || v.snapshot.Run != nil && v.snapshot.Run.State == "running"
}

func (v *planView) missing(s sdk.PlanStep, selected map[string]bool) string {
	for _, id := range s.Requires {
		if !selected[id] && v.progress(id).State != "verified" {
			return id
		}
	}
	if s.After != "" && !selected[s.After] && v.progress(s.After).State != "verified" {
		return s.After
	}
	return ""
}

func (v *planView) selection(p *planPicker) (ids []string, reason string) {
	claims := make(map[string]bool)
	inputs := 0
	for _, s := range v.snapshot.Plan.Steps {
		if !p.selected[s.ID] {
			continue
		}
		ids = append(ids, s.ID)
		if reason == "" {
			if s.Blocker != "" {
				reason = "Blocked · " + oneLine(s.Blocker)
			} else if v.snapshot.RunAvailability == sdk.PlanRunScoped && s.Edit == nil && s.Scope == nil {
				reason = "Cannot run · " + oneLine(s.Title) + " has no scoped edit"
			} else if missing := v.missing(s, p.selected); missing != "" {
				reason = "Blocked · select " + v.title(missing)
			} else if v.snapshot.RunAvailability == sdk.PlanRunScoped {
				add := func(path string, write bool) {
					prev, exists := claims[path]
					if exists && (prev || write) && reason == "" {
						reason = "Cannot run · overlapping file claims"
					}
					claims[path] = prev || write
					inputs++
				}
				if s.Scope != nil {
					for _, path := range s.Scope.Write {
						add(path, true)
					}
					for _, path := range s.Scope.Read {
						add(path, false)
					}
				} else {
					if prev, exists := claims[s.Edit.Path]; exists && prev {
						reason = "Cannot run · select one edit per file"
					}
					add(s.Edit.Path, true)
				}
			}
		}
	}
	if inputs > 48 && reason == "" {
		reason = "Cannot run · at most 48 file inputs"
	}
	return ids, reason
}

func (p *planPicker) matches(v *planView) bool {
	return p.expected == "" || v.snapshot.Plan != nil && v.snapshot.Plan.ID == p.expected
}

func (m *Model) planKey(d *dialog, chord string) tea.Cmd {
	v := m.planViewFor(d)
	p := d.plan
	if !v.ready || v.snapshot.Plan == nil || !p.matches(v) {
		return nil
	}
	if d.route.owner.Valid() && m.menuFeaturesFromBackend.Has(x.PlanWorkspace) {
		switch chord {
		case "v":
			if p.workspace != "" {
				return nil
			}
			plan := v.snapshot.Plan
			return m.openPlanReview(d.route, plan.ID, plan.Title, plan.Revision)
		case "c":
			if p.workspace == "" && v.snapshot.RunAvailability != sdk.PlanRunPinned && !(v.snapshot.Closed && v.snapshot.Complete) {
				return m.changePlanClosed(d, v.snapshot.Plan.ID, !v.snapshot.Closed)
			}
			return nil
		case "h":
			if v.snapshot.RunAvailability != sdk.PlanRunPinned {
				return m.openPlanHistory(d.route, v.snapshot.Plan.ID, v.snapshot.Plan.Title)
			}
			return nil
		case "a":
			if p.named != "" && p.workspace == "" {
				return m.usePlan(d, p.named)
			}
			return nil
		}
	}
	last := len(v.snapshot.Plan.Steps) - 1
	if p.details {
		switch chord {
		case "up":
			p.scroll = max(0, p.scroll-1)
			return nil
		case "down", "pgdown":
			width := menuDetailWidth(m.pane().width)
			count := len(m.planDetails(v, v.snapshot.Plan.Steps[p.focus], width))
			delta := 1
			if chord == "pgdown" {
				delta = 4
			}
			p.scroll = min(count-1, p.scroll+delta)
			return nil
		case "home":
			p.scroll = 0
			return nil
		case "pgup":
			p.scroll = max(0, p.scroll-4)
			return nil
		}
	}
	switch chord {
	case "up":
		p.focus = max(0, p.focus-1)
	case "down":
		p.focus = min(last, p.focus+1)
	case "home":
		p.focus = 0
	case "end":
		p.focus = last
	case "tab":
		p.details, p.scroll = !p.details, 0
	case "space", " ":
		if !p.details {
			m.togglePlanStep(v, p)
		}
	case "w":
		return m.runPlanWorktree(d, v)
	case "x":
		return m.leavePlanWorktree(d)
	case "enter":
		if v.request == nil && m.planChatAvailable(v) {
			return m.runChatPlan(d, v)
		}
		return m.runPlan(d, v, planRunTarget)
	case "s":
		return m.runPlan(d, v, planScopedTarget)
	}
	return nil
}

func (m *Model) togglePlanStep(v *planView, p *planPicker) {
	if p.workspace != "" || !v.ready || v.snapshot.Plan == nil || !p.matches(v) || v.locked() || v.request != nil ||
		m.threads[m.active].plan.chatRequest != nil || m.threads[m.active].plan.worktree != nil || !(m.planAvailable() || m.planChatAvailable(v)) {
		return
	}
	id := v.snapshot.Plan.Steps[p.focus].ID
	if v.progress(id).State != "verified" {
		v.note = ""
		if p.selected == nil {
			p.selected = make(map[string]bool)
		}
		if p.selected[id] {
			delete(p.selected, id)
		} else {
			p.selected[id] = true
		}
		p.selectionRev++
	}
}

func (m *Model) planActionDrawn(d *dialog, v *planView, target int) bool {
	return m.dialogVisible(d) && m.frame.dialogPlanRev == v.snapshot.Revision &&
		m.frame.dialogPlanSelectionRev == d.plan.selectionRev && slices.Contains(m.frame.dialogChoices, target)
}

func (m *Model) runPlan(d *dialog, v *planView, target int) tea.Cmd {
	if d.plan.workspace != "" || !m.planActionDrawn(d, v, target) || m.threads[d.route.owner].plan.chatRequest != nil || m.threads[d.route.owner].plan.worktree != nil {
		return nil // Enter is only a Run when the current scope's action was drawn.
	}
	b, ok := m.backend.(planBackend)
	if !ok || !m.planAvailable() || !v.ready || v.snapshot.Plan == nil || v.snapshot.Closed || v.snapshot.Complete || v.snapshot.Settling || !planRunnable(v.snapshot.RunAvailability) || v.sending || (v.request == nil && v.locked()) {
		return nil
	}
	var selection sdk.PlanSelection
	if v.request != nil {
		selection = *v.request
	} else {
		ids, reason := v.selection(d.plan)
		if len(ids) == 0 || reason != "" {
			return nil
		}
		p := v.snapshot.Plan
		selection = sdk.PlanSelection{ThreadID: d.route.owner, PlanID: p.ID, Revision: p.Revision, StepIDs: ids, RequestID: submissionKey()}
	}
	v.request, v.sending, v.note = &selection, true, ""
	d.plan.selectionRev++ // a retry must be shown before the same request can be sent again
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return b.RunPlan(ctx, selection)
	}, func(value any, err error, _ *int) tea.Cmd {
		v.sending = false
		if err != nil {
			var reply *sdk.ServerError
			if errors.As(err, &reply) {
				v.request = nil
				v.note = reply.Message
			}
			return nil
		}
		v.request = nil
		d.plan.selected = nil
		if id, ok := value.(sdk.ID); ok && v.snapshot.Plan != nil && v.snapshot.Plan.ID == selection.PlanID && v.snapshot.Plan.Revision == selection.Revision {
			if run := v.snapshot.Run; run == nil || lessID(run.ID, id) {
				v.awaiting = id
			}
		}
		if v.snapshot.RunAvailability == sdk.PlanRunScoped && m.foreground(d.route) && m.focusedDialog() == d {
			// An admitted foreground Run belongs in the transcript, not in
			// the picker. Do not dismiss anything if the user already left it.
			return m.closeRunPicker(d)
		}
		return nil
	}, nil)
}

func (m *Model) closeRunPicker(d *dialog) tea.Cmd {
	closePicker := m.closeDialog(d, nil, false)
	if parent := m.focusedDialog(); parent != nil && parent.planWorkspace != nil {
		return tea.Batch(closePicker, m.closeDialog(parent, nil, false))
	}
	return closePicker
}

func (v *planView) label(s sdk.PlanStep, selected map[string]bool) (string, x.Tone) {
	switch v.progress(s.ID).State {
	case "running":
		return "Running", x.Accent
	case "verified":
		return "Verified", x.Good
	}
	if v.snapshot.Chat {
		if run := v.snapshot.Run; run != nil && run.Chat && run.State == "running" && slices.Contains(run.StepIDs, s.ID) {
			return "In run", x.Accent
		}
		switch v.taskReport(s.ID).State {
		case "done":
			return "Reported done", x.Good
		case "blocked":
			return "Blocked", x.Warning
		case "incomplete":
			return "Incomplete", x.Warning
		}
		return "Pending", x.Muted
	}
	if v.progress(s.ID).State == "failed" {
		return "Not verified", x.Danger
	}
	if s.Blocker != "" {
		return "Blocked", x.Warning
	}
	if v.snapshot.RunAvailability == sdk.PlanRunScoped && s.Edit == nil && s.Scope == nil {
		return "Draft only", x.Muted
	}
	run := v.snapshot.Run
	active := run != nil && run.State == "running"
	if active && slices.Contains(run.StepIDs, s.ID) {
		return "Queued", x.Muted
	}
	var needs, waits int
	count := func(id string) {
		if v.progress(id).State == "verified" {
			return
		}
		if selected[s.ID] && selected[id] {
			waits++
		} else {
			needs++
		}
	}
	for _, id := range s.Requires {
		count(id)
	}
	if s.After != "" && !slices.Contains(s.Requires, s.After) {
		count(s.After)
	}
	if needs != 0 {
		return fmt.Sprintf("Needs %d", needs), x.Muted
	}
	if active || !planRunnable(v.snapshot.RunAvailability) {
		return "Pending", x.Muted
	}
	if waits != 0 {
		return fmt.Sprintf("After %d", waits), x.Muted
	}
	return "Ready", x.Plain
}

func (v *planView) stepIcon(s sdk.PlanStep, status string) (string, x.Tone) {
	switch v.progress(s.ID).State {
	case "verified":
		return "✓", x.Good
	case "running":
		return "◐", x.Warning
	case "failed":
		return "✗", x.Danger
	}
	switch status {
	case "In run":
		return "◐", x.Warning
	case "Blocked", "Incomplete":
		return "⏸", x.Warning
	default:
		return "○", x.Muted
	}
}

func (m *Model) planRows(d *dialog, w, n int) menuFrame {
	v, p := m.planViewFor(d), d.plan
	plan := v.snapshot.Plan
	c := menuChrome{title: "Plan", keys: chromeLine(m.menuKeys(w, d.zoomKey()))}
	if !v.ready || plan == nil || !p.matches(v) || len(plan.Steps) == 0 {
		status, tone := "No plan yet · ask to create a plan", x.Muted
		switch {
		case v.ready && !p.matches(v):
			status, tone = "Plan binding changed · open it again from the list", x.Warning
		case v.err != nil:
			status, tone = "Plan connection unavailable · reopen to retry", x.Danger
		case !v.ready:
			status = "Loading plan…"
		case plan == nil && v.snapshot.RunAvailability == sdk.PlanRunFixture:
			status = "No plan yet · send !plan-fixture"
		case plan != nil:
			status = "No steps · revise this plan to add steps"
		}
		return m.menuShell(w, n, c, func(int) menuFrame {
			return menuFrame{rows: []string{m.paint(tone, status)}, targets: []int{-1}}
		})
	}
	var verified, done int
	for _, s := range plan.Steps {
		if v.progress(s.ID).State == "verified" {
			verified++
		}
		if v.done(s.ID) {
			done++
		}
	}
	c.title, c.scope = plan.Title, fmt.Sprintf("Plan · r%d", plan.Revision)
	c.count = fmt.Sprintf("%d/%d verified", verified, len(plan.Steps))
	if done != verified {
		c.count = fmt.Sprintf("%d/%d done · %d verified", done, len(plan.Steps), verified)
	}
	t := m.threads[d.route.owner]
	canSelect := p.workspace == "" && !v.locked() && v.request == nil && t.plan.chatRequest == nil && t.plan.worktree == nil
	bar, preview, runnable := m.planRunBar(d, v, w)
	c.consequence = chromeLine(bar + preview)
	if runnable && stringWidth(bar) <= w {
		c.consequence.target = planRunTarget
	}
	move, tab := "↑↓ move", "tab details"
	if p.details {
		move, tab = "↑↓ scroll", "tab steps"
	}
	var hints []string
	if canSelect && !p.details {
		hints = append(hints, "space select")
	}
	hints = append(hints, tab)
	if runnable {
		hints = append(hints, "↵ run")
	}
	scoped := false
	if canSelect && m.planChatAvailable(v) && v.snapshot.RunAvailability == sdk.PlanRunScoped {
		ids, reason := v.selection(p)
		if len(ids) > 0 && reason == "" {
			hints = append(hints, "s scoped")
			scoped = true
		}
	}
	hints = append(hints, move, d.zoomKey())
	if p.workspace == "" && d.route.owner.Valid() && m.menuFeaturesFromBackend.Has(x.PlanReview) {
		hints = append(hints, "v review")
	}
	if d.route.owner.Valid() && m.menuFeaturesFromBackend.Has(x.PlanWorkspace) && v.snapshot.RunAvailability != sdk.PlanRunPinned {
		hints = append(hints, "h history")
		if p.workspace == "" && !v.snapshot.Closed {
			hints = append(hints, "c close plan")
		} else if p.workspace == "" && !v.snapshot.Complete {
			hints = append(hints, "c reopen")
		}
	}
	c.keys = chromeLine(m.menuKeys(w, hints...))
	if scoped && strings.Contains(c.keys.text, "s scoped") {
		c.keys.target = planScopedTarget
	}
	worktree, worktreeAction := m.planWorktreeBar(d, v)
	return m.menuShell(w, n, c, func(h int) menuFrame {
		showWorktree := worktree != "" && n >= 4 && h > 1
		if showWorktree {
			h--
		}
		if h == 1 && p.details && w == menuListWidth(w) {
			return menuFrame{rows: m.planDetailWindow(v, p, w, 1), targets: []int{-1}}
		}
		out := m.menuColumns(w, h, func(width, height int) []string { return m.planDetailWindow(v, p, width, height) }, func(lw, lh int) menuFrame {
			var out menuFrame
			start, end := menuWindow(p.focus, len(plan.Steps), lh)
			for i := start; i < end; i++ {
				s := plan.Steps[i]
				state, stateTone := v.label(s, p.selected)
				glyph, tone := v.stepIcon(s, state)
				if tone == x.Muted {
					tone = stateTone
				}
				item := menuItem{title: s.Title, icon: glyph, state: state, tone: tone, focus: i == p.focus}
				if canSelect || len(p.selected) != 0 {
					item.selection = "□"
					if p.selected[s.ID] {
						item.selection = "■"
					}
				}
				out.add(m.menuRow(lw, 13, item), i)
			}
			return out
		})
		if showWorktree {
			if stringWidth(worktree) > w {
				worktreeAction = -1
			}
			out.add(m.quiet(worktree), worktreeAction)
		}
		return out
	})
}

func (m *Model) planRunBar(d *dialog, v *planView, width int) (bar, preview string, runnable bool) {
	p := d.plan
	if p.workspace != "" {
		return m.control("Read-only · " + oneLine(p.workspace)), "", false
	}
	if pending := m.threads[d.route.owner].plan.worktree; pending != nil {
		if pending.selection.PlanID != v.snapshot.Plan.ID {
			return m.control("Resolve the worktree request in its plan"), "", false
		}
		if pending.note != "" {
			return m.paint(x.Warning, pending.note), "", false
		}
		return m.control(pending.label()), "", false
	}
	chat := v.request == nil && m.planChatAvailable(v)
	if chat && m.threads[d.route.owner].plan.chatRequest != nil {
		return m.chatRunBar(d, v, width) // a receipt is valid even if the original revision changed
	}
	switch {
	case v.snapshot.Settling:
		return m.control("Closing Run · current task is settling"), "", false
	case v.snapshot.Complete:
		return m.control("Complete · all tasks verified"), "", false
	case v.snapshot.Closed:
		if !d.route.owner.Valid() {
			return m.control("Closed"), "", false
		}
		return m.control("Closed · c reopen"), "", false
	}
	if chat {
		return m.chatRunBar(d, v, width)
	}
	switch v.snapshot.RunAvailability {
	case sdk.PlanRunUnbound:
		return m.control("a Use in this thread"), "", false
	case sdk.PlanRunPinned:
		return m.control("Pinned revision · cannot run"), "", false
	}
	if !planRunnable(v.snapshot.RunAvailability) {
		return m.control("Draft only · execution unavailable"), "", false
	}
	switch {
	case v.sending || v.awaiting != "":
		return "Admitting selected steps…", "", false
	case v.request != nil:
		label := "↵ retry same Run"
		if stringWidth(label) > width {
			label = "↵ retry"
		}
		return strongText(label), m.paint(x.Muted, " · no receipt"), true
	case v.snapshot.Run != nil && v.snapshot.Run.State == "running":
		return v.activity(), "", false
	}
	ids, reason := v.selection(p)
	if reason != "" {
		return m.paint(x.Warning, reason), "", false
	}
	if v.note != "" {
		note := oneLine(v.note)
		if len(ids) != 0 {
			return strongText("↵ retry"), m.paint(x.Warning, " · "+note), true
		}
		return m.control(note), "", false
	}
	if len(ids) == 0 {
		if v.snapshot.Run != nil {
			if v.snapshot.Run.Failure != "" {
				return m.paint(x.Warning, oneLine(v.snapshot.Run.Failure)), "", false
			}
			return v.activity(), "", false
		}
		return m.control("Select the steps to run"), "", false
	}
	return m.planSelectionBar(v, p, ids, width, "Run")
}

func (m *Model) planSelectionBar(v *planView, p *planPicker, ids []string, width int, label string) (bar, preview string, runnable bool) {
	revision := fmt.Sprintf(" @ r%d", v.snapshot.Plan.Revision)
	compact := fmt.Sprintf("↵ %s %d", label, len(ids))
	if stringWidth(compact) > width {
		compact = fmt.Sprintf("↵ Run %d", len(ids))
	}
	if stringWidth(compact+revision) > width {
		return strongText(compact), "", true
	}
	count, dependent := v.unselected(p, ids[0])
	remaining := ""
	if count != 0 {
		remaining = fmt.Sprintf("%d unselected", count)
	}
	if len(ids) == 1 {
		prefix := "↵ " + label + " "
		if stringWidth(prefix+revision) >= width {
			return strongText(compact), "", true
		}
		title := ansi.Truncate(v.title(ids[0]), max(1, width-stringWidth(prefix+revision)), "…")
		bar = strongText(prefix + title + revision)
		room := width - stringWidth(bar) - 2
		if dependent != "" {
			label := dependent + " stays pending"
			if count > 1 {
				label += fmt.Sprintf(" · +%d unselected", count-1)
			}
			if stringWidth(label) <= room {
				remaining = label
			} else if label := dependent + " stays pending"; stringWidth(label) <= room {
				remaining = label
			}
		}
		if remaining != "" && stringWidth(remaining) <= room {
			return bar, m.paint(x.Muted, "  "+remaining), true
		}
		return bar, "", true
	}
	bar = strongText(compact + revision)
	room := width - stringWidth(bar) - 2
	if room <= 0 {
		return bar, "", true
	}
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		labels = append(labels, v.title(id))
	}
	order := strings.Join(labels, " → ")
	if remaining != "" && stringWidth(order+" · "+remaining) <= room {
		order += " · " + remaining
	}
	return bar, m.paint(x.Muted, "  "+ansi.Truncate(order, room, "…")), true
}

func (v *planView) unselected(p *planPicker, first string) (count int, dependent string) {
	for _, s := range v.snapshot.Plan.Steps {
		if p.selected[s.ID] || v.done(s.ID) {
			continue
		}
		count++
		if dependent == "" && s.Blocker == "" && (s.After == first || slices.Contains(s.Requires, first)) {
			dependent = oneLine(s.Title)
		}
	}
	return count, dependent
}

func (m *Model) planDetailWindow(v *planView, p *planPicker, width, height int) []string {
	rows := m.planDetails(v, v.snapshot.Plan.Steps[p.focus], width)
	from := min(p.scroll, max(0, len(rows)-height))
	return rows[from:min(len(rows), from+height)]
}

func (m *Model) planDetails(v *planView, s sdk.PlanStep, width int) []string {
	rows := wrapped(strongText(oneLine(s.Title)), width)
	for _, line := range wrapped(oneLine(s.Description), width) {
		rows = append(rows, m.quiet(line))
	}
	add := func(label, value string) {
		if width < 40 {
			rows = append(rows, wrapped(label+": "+value, width)...)
			return
		}
		const labelWidth = 10
		prefix := m.paint(x.Muted, fit(label, labelWidth))
		for i, line := range wrapped(value, width-labelWidth) {
			if i != 0 {
				prefix = strings.Repeat(" ", labelWidth)
			}
			rows = append(rows, prefix+line)
		}
	}
	for _, id := range s.Requires {
		add("Needs", v.title(id))
	}
	if s.After != "" {
		add("After", v.title(s.After))
	}
	if s.Edit != nil {
		add("Edit", oneLine(s.Edit.Path))
		add("Verify", "Contains "+oneLine(fmt.Sprintf("%q", s.Edit.Contains)))
	}
	if scope := s.Scope; scope != nil {
		for _, path := range scope.Write {
			add("Write", oneLine(path))
		}
		for _, path := range scope.Read {
			add("Read only", oneLine(path))
		}
		add("Accept", oneLine(scope.Acceptance.Label))
		for _, line := range strings.Split(scope.Acceptance.Script, "\n") {
			add("", oneLine(line))
		}
	}
	for _, check := range s.Checks {
		add("Check", oneLine(check))
	}
	result := v.progress(s.ID)
	requested := s.Thinking
	if requested == "" {
		requested = "inherit"
	}
	if result.EffectiveThinking != "" {
		requested += " → " + result.EffectiveThinking
	}
	add("Thinking", requested)
	for _, evidence := range result.Evidence {
		rows = append(rows, wrapped(oneLine(evidence), width)...)
	}
	if s.Blocker != "" {
		label := "Blocked"
		if v.snapshot.Chat {
			label = "Plan note"
		}
		add(label, oneLine(s.Blocker))
	}
	if report := v.taskReport(s.ID); report.State != "" {
		add("Reported", oneLine(report.Detail))
	}
	return rows
}

func (v *planView) activity() string {
	r := v.snapshot.Run
	if r == nil {
		return ""
	}
	if r.Chat {
		switch r.State {
		case "done":
			return "Plan · Run here finished"
		case "failed":
			return "Plan · Run here interrupted"
		case "closed":
			return "Plan · Run closed"
		default:
			return "Plan · Run here in progress"
		}
	}
	n := 0
	for _, id := range r.StepIDs {
		if v.progress(id).State == "verified" {
			n++
		}
	}
	s := fmt.Sprintf("Plan · %d/%d verified", n, len(r.StepIDs))
	if r.State == "failed" {
		s += " · needs verification"
	} else if r.State == "closed" {
		s += " · Run closed"
	} else if r.State == "running" {
		for _, id := range r.StepIDs {
			if v.progress(id).State == "running" {
				return s + " · " + v.title(id)
			}
		}
	}
	return s
}

func (m *Model) planActivity(t *threadState, width int) displayRow {
	v := &t.plan
	run := v.snapshot.Run
	if width < 10 || !v.ready || !m.planAvailable() || run == nil {
		return displayRow{}
	}
	var marks strings.Builder
	if len(run.StepIDs) <= 20 {
		for _, id := range run.StepIDs {
			switch v.progress(id).State {
			case "verified":
				marks.WriteString(m.paint(x.Good, "✓"))
			case "running":
				marks.WriteString(m.paint(x.Warning, "◐"))
			case "failed":
				marks.WriteString(m.paint(x.Danger, "✗"))
			default:
				mark, tone := "○", x.Muted
				if run.Chat {
					switch v.taskReport(id).State {
					case "blocked", "incomplete":
						mark, tone = "⏸", x.Warning
					default:
						if run.State == "running" {
							mark, tone = "◐", x.Warning
						}
					}
				}
				marks.WriteString(m.paint(tone, mark))
			}
		}
	}
	label := v.activity()
	if marks.Len() != 0 && stringWidth(marks.String())+12 < width {
		label = marks.String() + "  " + m.quiet(label)
	} else {
		label = m.quiet(label)
	}
	text := ansi.Truncate(label, width, "…")
	return displayRow{text: text, hits: []spanHit{{0, stringWidth(text), x.ActionRef{ID: planID}, "Open plan"}}}
}
