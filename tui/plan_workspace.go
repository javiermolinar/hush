package tui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type planWorkspaceBackend interface {
	WatchWorkspacePlans(context.Context, sdk.ID, int) <-chan sdk.PlanWorkspaceUpdate
	WatchNamedPlan(context.Context, sdk.ID, string) <-chan sdk.PlanUpdate
	SetPlanClosed(context.Context, sdk.ID, string, bool) error
	UsePlan(context.Context, sdk.ID, string) error
	PlanHistory(context.Context, sdk.ID, string, sdk.ID) (sdk.PlanHistoryPage, error)
}

type planWorkspacePicker struct {
	cancel                        context.CancelFunc
	lease                         uint64
	page                          int
	rows                          []sdk.PlanSummary
	more, ready, failed, changing bool
}

type planWorkspaceMsg struct {
	lease  uint64
	ctx    context.Context
	source <-chan sdk.PlanWorkspaceUpdate
	update sdk.PlanWorkspaceUpdate
	ok     bool
}

type namedPlanMsg struct {
	token  uint64
	ctx    context.Context
	source <-chan sdk.PlanUpdate
	update sdk.PlanUpdate
	ok     bool
}

type planHistoryPicker struct {
	id, title       string
	page            sdk.PlanHistoryPage
	lines           []string
	offset          int
	loading, failed bool
}

func (m *Model) openDraftPlan(r ownerRoute) tea.Cmd {
	if len(m.overlays) >= maxDialogs {
		return nil
	}
	t := m.threads[r.owner]
	if t.sending || t.attaching {
		return m.notifyRoute(r, "Wait for the pending request", x.Warning)
	}
	d := &dialog{route: r, token: m.token(), promptDock: true, planWorkspace: &planWorkspacePicker{}}
	show := m.pushDialog(d)
	t.sending = true // serialize with first-prompt creation, even if the picker is dismissed
	backend, options := m.backend, m.options.NewThread
	model, thinking := t.info.Model, t.info.Thinking
	create := m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return createDraftThread(call, backend, options, model, thinking)
	}, func(value any, err error, _ *int) tea.Cmd {
		t.sending = false
		if !m.foreground(r) || m.focusedDialog() != d {
			return nil
		}
		if err != nil {
			message := "Could not open plans: " + err.Error()
			if isIndeterminate(err) {
				message = "Thread creation unconfirmed; check /threads before retrying."
			}
			return tea.Batch(m.closeDialog(d, nil, false), m.notifyRoute(r, message, x.Warning))
		}
		id := value.(sdk.ID)
		m.freezeIntro(t)
		dst := m.prepareDraftThread(id, model, thinking)
		dst.draft, dst.attachments, dst.stagedSkill = m.editor.promptDraft, t.attachments, t.stagedSkill
		m.editor.set("")
		t.attachments, t.stagedSkill = nil, stagedSkill{}
		return tea.Batch(m.switchThread(id), m.openPlanWorkspace(m.route(id, r.extension)))
	}, nil)
	return tea.Batch(show, create)
}

func (m *Model) openPlanWorkspace(r ownerRoute) tea.Cmd {
	// A just-created thread may not have received its transcript feed yet.
	if !m.foreground(r) || !r.owner.Valid() || len(m.overlays) >= maxDialogs {
		return nil
	}
	d := &dialog{route: r, token: m.token(), promptDock: true, planWorkspace: &planWorkspacePicker{}}
	return tea.Batch(m.pushDialog(d), m.watchPlanWorkspace(d, 0))
}

func awaitPlanWorkspace(ctx context.Context, lease uint64, source <-chan sdk.PlanWorkspaceUpdate) tea.Cmd {
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
	b, ok := m.backend.(planWorkspaceBackend)
	if !ok || !d.route.owner.Valid() {
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
	lease, owner := p.lease, d.route.owner
	return func() tea.Msg { return awaitPlanWorkspace(ctx, lease, b.WatchWorkspacePlans(ctx, owner, page))() }
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
	case "p":
		if m.threads[d.route.owner].plan.snapshot.RunAvailability == sdk.PlanRunPinned {
			return m.openBoundPlan(d.route)
		}
	case "enter", "c", "h", "v":
		if !p.ready || p.failed || d.selected >= len(p.rows) {
			return nil
		}
		row := p.rows[d.selected]
		switch chord {
		case "c":
			if !row.Closed || row.Status != "complete" {
				return m.changePlanClosed(d, row.ID, !row.Closed)
			}
		case "h":
			return m.openPlanHistory(d.route, row.ID, row.Title)
		case "v":
			return m.openPlanReview(d.route, row.ID, row.Title, row.Revision)
		default:
			return m.inspectPlan(d.route, row.ID)
		}
	}
	return nil
}

func (m *Model) planWorkspaceRows(d *dialog, w, n int) (rows []string, targets []int) {
	p := d.planWorkspace
	add := func(line string, target int) {
		if len(rows) < n {
			rows, targets = append(rows, ansi.Truncate(line, w, "…")), append(targets, target)
		}
	}
	t := m.threads[d.route.owner]
	title := "Workspace plans"
	if path := t.info.Workspace; path != "" {
		title += " · " + oneLine(filepath.Base(path))
	} else if d.route.owner == "" {
		title += " · " + oneLine(filepath.Base(m.options.NewThread.CWD))
	}
	if n >= 8 {
		add("", -1)
	}
	add(planRunEmphasis(title), -1)
	if n >= 7 {
		add("", -1)
	}
	footer := 1
	if n >= 6 {
		footer++
	}
	showPage := p.page > 0 || p.more
	if showPage && n >= 7 {
		footer++
	}
	budget := max(1, n-len(rows)-footer)
	switch {
	case p.failed:
		add("Could not load plans · r retry", -1)
	case !p.ready:
		add("Loading workspace plans…", -1)
	case len(p.rows) == 0:
		if p.page == 0 {
			add("No plan yet · ask “Create a plan…”", -1)
		} else {
			add("No plans on this page", -1)
		}
	default:
		grouped := budget >= 3 && p.rows[len(p.rows)-1].Closed
		first := d.selected
		if !grouped {
			first = min(max(0, first-budget+1), max(0, len(p.rows)-budget))
		} else {
			cost := 2 // selected row and its group heading
			for first > 0 {
				step := 1
				if p.rows[first-1].Closed != p.rows[first].Closed {
					step++
				}
				if cost+step > budget {
					break
				}
				first, cost = first-1, cost+step
			}
		}
		for i := first; i < len(p.rows) && budget > 0; i++ {
			r := p.rows[i]
			if grouped && (i == first || r.Closed != p.rows[i-1].Closed) {
				if budget < 2 {
					break
				}
				heading := "Open plans"
				if r.Closed {
					heading = "Closed plans"
				}
				add(m.control(heading), -1)
				budget--
			}
			budget--
			status, tone := workspacePlanStatus(r)
			prefix := "  "
			if d.selected == i {
				prefix = m.paint(x.Accent, "› ")
			}
			if v := t.plan.snapshot; v.Plan != nil && v.Plan.ID == r.ID && v.RunAvailability != sdk.PlanRunPinned {
				prefix += m.paint(x.Accent, "• ")
			} else {
				prefix += "  "
			}
			badge := m.paint(tone, status) + m.control(fmt.Sprintf("  %d/%d · r%d", r.Verified, r.Total, r.Revision))
			left := max(4, w-stringWidth(badge)-2)
			line := fit(ansi.Truncate(prefix+oneLine(r.Title), left, "…"), left) + "  " + badge
			if d.selected == i && m.theme.Fills {
				line = fill(fit(line, w), m.palette.selected)
			}
			add(line, i)
		}
	}
	if showPage && n >= 7 {
		add(m.control(fmt.Sprintf("Page %d · ←→ pages", p.page+1)), -1)
	}
	keys := "↵ open  h history"
	compact := "↵ open"
	if p.ready && !p.failed && d.selected < len(p.rows) {
		pick := p.rows[d.selected]
		if m.menuFeaturesFromBackend.Has(x.PlanReview) {
			keys += "  v review"
			compact += "  v review"
		}
		if !pick.Closed {
			keys += "  c close plan"
			compact += "  c close plan"
		} else if pick.Status != "complete" {
			keys += "  c reopen"
			compact += "  c reopen"
		}
	}
	if t.plan.snapshot.RunAvailability == sdk.PlanRunPinned {
		keys += "  p pinned revision"
		compact += "  p pinned"
	}
	add(m.paint(x.Muted, m.hintRow(w, keys, compact, "esc close")), -1)
	if n >= 6 {
		add("", -1)
	}
	return
}

func workspacePlanStatus(p sdk.PlanSummary) (string, x.Tone) {
	switch {
	case p.Status == "complete":
		return "Complete", x.Good
	case p.Closed && p.Status == "running":
		return "Closing", x.Warning
	case p.Closed:
		return "Closed", x.Muted
	case p.Status == "running":
		return "Running", x.Accent
	case p.Status == "attention":
		return "Needs check", x.Warning
	default:
		return "Pending", x.Plain
	}
}

func (m *Model) inspectPlan(r ownerRoute, id string) tea.Cmd {
	if !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	v := &m.threads[r.owner].plan
	if v.snapshot.Plan != nil && v.snapshot.Plan.ID == id && v.snapshot.RunAvailability != sdk.PlanRunPinned {
		return m.openPlanSteps(r, id)
	}
	b, ok := m.backend.(planWorkspaceBackend)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	d := &dialog{route: r, token: m.token(), promptDock: true, plan: &planPicker{named: id, cancel: cancel}}
	token := d.token
	return tea.Batch(m.pushDialog(d), func() tea.Msg { return awaitNamedPlan(ctx, token, b.WatchNamedPlan(ctx, r.owner, id))() })
}

func awaitNamedPlan(ctx context.Context, token uint64, source <-chan sdk.PlanUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-source:
			return namedPlanMsg{token: token, ctx: ctx, source: source, update: update, ok: ok}
		}
	}
}

func (m *Model) receiveNamedPlan(msg namedPlanMsg) tea.Cmd {
	for _, d := range m.overlays {
		if p := d.plan; p != nil && p.named != "" && d.token == msg.token && m.foreground(d.route) {
			if !msg.ok {
				p.view.ready = false
				return nil
			}
			v := &p.view
			v.err, v.ready = msg.update.Err, msg.update.Err == nil
			if v.ready {
				if old, next := v.snapshot.Plan, msg.update.Snapshot.Plan; old == nil || next == nil || old.Revision != next.Revision {
					p.focus, p.scroll = 0, 0
				}
				v.snapshot = msg.update.Snapshot
				// Only the bound picker holds Run requests and retries across closes.
				v.snapshot.RunAvailability = sdk.PlanRunUnbound
			}
			return awaitNamedPlan(msg.ctx, msg.token, msg.source)
		}
	}
	return nil
}

func (m *Model) usePlan(d *dialog, id string) tea.Cmd {
	v := &m.threads[d.route.owner].plan
	if d.plan.changing || v.sending || v.request != nil || v.awaiting != "" {
		return nil
	}
	b, ok := m.backend.(planWorkspaceBackend)
	if !ok {
		return nil
	}
	r := d.route
	d.plan.changing = true
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return nil, b.UsePlan(call, r.owner, id)
	}, func(_ any, err error, _ *int) tea.Cmd {
		d.plan.changing = false
		if err != nil {
			return m.reportPlanWorkspaceError(r, "Could not attach plan", err)
		}
		m.threads[r.owner].plan.ready = false
		if m.planFeed.owner == r.owner {
			m.stopPlanWatch()
		}
		if m.focusedDialog() != d || !m.foreground(r) {
			return m.syncPlanWatch()
		}
		return tea.Batch(m.closeDialog(d, nil, false), m.openPlanSteps(r, id))
	}, nil)
}

func (m *Model) changePlanClosed(d *dialog, id string, closed bool) tea.Cmd {
	b, ok := m.backend.(planWorkspaceBackend)
	if !ok {
		return nil
	}
	var busy *bool
	if d.planWorkspace != nil {
		busy = &d.planWorkspace.changing
	} else {
		busy = &d.plan.changing
	}
	if *busy {
		return nil
	}
	*busy = true
	r := d.route
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return nil, b.SetPlanClosed(call, r.owner, id, closed)
	}, func(_ any, err error, _ *int) tea.Cmd {
		*busy = false
		if err != nil {
			return m.reportPlanWorkspaceError(r, "Could not change plan status", err)
		}
		return nil
	}, nil)
}

func (m *Model) reportPlanWorkspaceError(r ownerRoute, fallback string, err error) tea.Cmd {
	var reply *sdk.ServerError
	if errors.As(err, &reply) {
		fallback = oneLine(reply.Message)
	}
	return m.notifyRoute(r, fallback, x.Warning)
}

func (m *Model) openPlanHistory(r ownerRoute, id, title string) tea.Cmd {
	if !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	d := &dialog{route: r, token: m.token(), promptDock: true, planHistory: &planHistoryPicker{id: id, title: title}}
	return tea.Batch(m.pushDialog(d), m.loadPlanHistory(d, ""))
}

func (m *Model) loadPlanHistory(d *dialog, before sdk.ID) tea.Cmd {
	p := d.planHistory
	b, ok := m.backend.(planWorkspaceBackend)
	if !ok || p.loading {
		return nil
	}
	p.loading, p.failed = true, false
	r, id := d.route, p.id
	return m.startWork(r, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return b.PlanHistory(call, r.owner, id, before)
	}, func(value any, err error, _ *int) tea.Cmd {
		p.loading, p.failed = false, err != nil
		if page, ok := value.(sdk.PlanHistoryPage); ok && err == nil {
			p.page, p.lines, p.offset = page, planHistoryLines(page), 0
		}
		return nil
	}, nil)
}

func (m *Model) planHistoryKey(d *dialog, chord string) tea.Cmd {
	p := d.planHistory
	end := max(0, len(p.lines)-1)
	switch chord {
	case "up":
		p.offset = max(0, p.offset-1)
	case "down":
		p.offset = min(end, p.offset+1)
	case "home":
		p.offset = 0
	case "left", "r":
		return m.loadPlanHistory(d, "")
	case "right":
		if p.page.Next.Valid() {
			return m.loadPlanHistory(d, p.page.Next)
		}
	}
	return nil
}

func planHistoryLines(page sdk.PlanHistoryPage) []string {
	var out []string
	for i, run := range page.Runs {
		if i > 0 {
			out = append(out, "")
		}
		out = append(out, fmt.Sprintf("Run #%s · r%d · %s", run.ID, run.Revision, run.State),
			"  Thread #"+string(run.ThreadID)+" · "+strings.Join(run.StepIDs, ", "))
		if run.PublicationUncertain {
			out = append(out, "  Publication uncertain · inspect workspace")
		}
		if run.Failure != "" {
			out = append(out, "  "+oneLine(run.Failure))
		}
		for _, turn := range run.Turns {
			out = append(out, fmt.Sprintf("  %s · turn #%s · %s", turn.StepID, turn.EntryID, turn.State))
		}
		for _, w := range run.Workers {
			out = append(out, fmt.Sprintf("  %s → worker #%s · %s", w.StepID, w.ThreadID, w.State))
		}
	}
	return out
}

func (m *Model) planHistoryRows(d *dialog, w, n int) (rows []string, targets []int) {
	p := d.planHistory
	add := func(line string) {
		if len(rows) < n {
			rows, targets = append(rows, ansi.Truncate(line, w, "…")), append(targets, -1)
		}
	}
	add(planRunEmphasis("Run history · " + oneLine(p.title)))
	if n >= 6 {
		add("")
	}
	switch {
	case p.loading:
		add("Loading run history…")
	case p.failed:
		add("Could not load run history · r retry")
	case len(p.page.Runs) == 0:
		add("No recorded runs")
	default:
		lines := p.lines
		height := max(0, n-len(rows)-2)
		start := min(p.offset, max(0, len(lines)-height))
		for _, line := range lines[start:min(len(lines), start+height)] {
			add(m.control(line))
		}
	}
	add(m.paint(x.Muted, m.hintRow(w, "↑↓ scroll  ← latest  → older  r refresh", "↑↓ scroll", "esc back")))
	if n >= 5 {
		add("")
	}
	return
}
