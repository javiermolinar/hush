package tui

import (
	"context"
	"errors"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const planReviewEntry = "hush.plan.review"

type planReviewBackend interface {
	WatchPlanReview(context.Context, sdk.PlanReviewTarget) <-chan sdk.PlanReviewUpdate
	StartPlanReview(context.Context, sdk.PlanReviewTarget, string) (sdk.ID, error)
	CancelPlanReview(context.Context, sdk.PlanReviewTarget, sdk.ID) error
}

type planReviewLine struct {
	text string
	tone x.Tone
}

type planReviewPicker struct {
	target        sdk.PlanReviewTarget
	view          sdk.PlanReviewView
	title         string
	cancel        context.CancelFunc
	lease         uint64
	ready, failed bool
	sending       bool
	awaiting      sdk.ID
	requestID     string
	note          string
	offset, width int
	body          []planReviewLine
}

type planReviewMsg struct {
	lease  uint64
	ctx    context.Context
	source <-chan sdk.PlanReviewUpdate
	update sdk.PlanReviewUpdate
	ok     bool
}

func (m *Model) openPlanReview(r ownerRoute, id, title string, revision uint64) tea.Cmd {
	if !m.planAvailable() || !m.menuFeaturesFromBackend.Has(x.PlanReview) || !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	d := &dialog{route: r, token: m.token(), planReview: &planReviewPicker{
		target: sdk.PlanReviewTarget{ThreadID: r.owner, PlanID: id, Revision: revision}, title: title,
	}}
	return tea.Batch(m.pushDialog(d), m.watchPlanReview(d))
}

func awaitPlanReview(ctx context.Context, lease uint64, source <-chan sdk.PlanReviewUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case update, ok := <-source:
			return planReviewMsg{lease, ctx, source, update, ok}
		}
	}
}

func (m *Model) watchPlanReview(d *dialog) tea.Cmd {
	b, ok := m.backend.(planReviewBackend)
	if !ok {
		return nil
	}
	p := d.planReview
	if p.cancel != nil {
		p.cancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	p.cancel, p.lease, p.ready, p.failed = cancel, m.token(), false, false
	p.offset, p.body = 0, nil
	q, lease := p.target, p.lease
	return func() tea.Msg { return awaitPlanReview(ctx, lease, b.WatchPlanReview(ctx, q))() }
}

func (m *Model) receivePlanReview(msg planReviewMsg) tea.Cmd {
	for _, d := range m.overlays {
		p := d.planReview
		if p == nil || p.lease != msg.lease || !m.foreground(d.route) {
			continue
		}
		if !msg.ok || msg.update.Err != nil {
			p.failed, p.ready = true, false
			if !msg.ok {
				return nil
			}
		} else {
			p.view, p.ready, p.failed, p.body = msg.update.View, true, false, nil
			p.title = p.view.Title
			if row := p.view.Review; p.awaiting != "" && row != nil && !lessID(row.ID, p.awaiting) {
				p.awaiting = ""
			}
		}
		return awaitPlanReview(msg.ctx, msg.lease, msg.source)
	}
	return nil
}

func (p *planReviewPicker) canStart() bool {
	return p.ready && !p.sending && p.awaiting == "" && p.view.CanStart
}

func (m *Model) planReviewKey(d *dialog, chord string) tea.Cmd {
	p := d.planReview
	switch chord {
	case "r":
		return m.watchPlanReview(d)
	case "left", "right":
		if !p.ready || p.sending || p.awaiting != "" {
			return nil
		}
		next := p.target.Revision
		if chord == "left" && next > 1 {
			next--
		} else if chord == "right" && next < p.view.HeadRevision {
			next++
		}
		if next != p.target.Revision {
			p.target.Revision, p.note, p.requestID = next, "", ""
			return m.watchPlanReview(d)
		}
	case "up", "down", "pgup", "pgdown", "home":
		delta := map[string]int{"up": -1, "down": 1, "pgup": -4, "pgdown": 4, "home": -p.offset}[chord]
		p.offset = min(max(0, p.offset+delta), max(0, len(p.lines(m.pane().width))-1))
	case "enter":
		if p.canStart() {
			return m.sendPlanReview(d, "")
		}
	case "x", "o":
		if !p.ready || p.sending || p.view.Review == nil {
			return nil
		}
		r := p.view.Review
		if chord == "x" && r.State == "running" && r.ThreadID == d.route.owner {
			return m.sendPlanReview(d, r.ID)
		}
		if chord == "o" && r.ReviewerThreadID.Valid() {
			budget := maxEffects
			return m.admit(d.route, x.Outcome{x.Client{Op: x.Switch{ThreadID: r.ReviewerThreadID}}}, &budget)
		}
	}
	return nil
}

func (m *Model) sendPlanReview(d *dialog, stopping sdk.ID) tea.Cmd {
	b, ok := m.backend.(planReviewBackend)
	if !ok || !m.planAvailable() || !m.foreground(d.route) {
		return nil
	}
	p := d.planReview
	p.sending, p.note = true, ""
	if !stopping.Valid() && p.requestID == "" {
		p.requestID = submissionKey()
	}
	q, request := p.target, p.requestID
	return m.startWork(d.route, func(ctx context.Context) (any, error) {
		call, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		if stopping.Valid() {
			return nil, b.CancelPlanReview(call, q, stopping)
		}
		return b.StartPlanReview(call, q, request)
	}, func(value any, err error, _ *int) tea.Cmd {
		p.sending = false
		if err != nil {
			p.note = "Review request not confirmed · try again"
			var reply *sdk.ServerError
			if errors.As(err, &reply) {
				p.note, p.requestID = oneLine(reply.Message), ""
			}
			return nil
		}
		p.requestID = ""
		if id, ok := value.(sdk.ID); ok && (p.view.Review == nil || lessID(p.view.Review.ID, id)) {
			p.awaiting = id // a receipt never substitutes for the committed watch
		}
		return nil
	}, nil)
}

func (p *planReviewPicker) lines(width int) []planReviewLine {
	if p.body != nil && p.width == width {
		return p.body
	}
	body := make([]planReviewLine, 0, 12)
	add := func(text string, tone x.Tone) {
		for _, line := range wrapped(oneLine(text), width) {
			body = append(body, planReviewLine{line, tone})
		}
	}
	row := p.view.Review
	if row == nil {
		add("No review yet. A fresh reviewer sees this revision and its captured draft requests only.", x.Plain)
		add("No workspace access · advisory findings only", x.Muted)
	} else {
		reviewer := row.Model.ModelID + " · thinking " + row.Thinking
		if row.ReviewerThreadID.Valid() {
			reviewer = fmt.Sprintf("Reviewer #%s · %s", row.ReviewerThreadID, reviewer)
		}
		add(reviewer, x.Muted)
		switch row.State {
		case "running":
			add("Reviewing the saved revision…", x.Accent)
		case "failed":
			add(row.Failure, x.Warning)
			if !p.view.CanStart && p.target.Revision == p.view.HeadRevision {
				add("Review attempt limit reached", x.Muted)
			}
		case "complete":
			if report := row.Report; report != nil {
				add(report.Summary, x.Plain)
				add(fmt.Sprintf("Requirements: %s · Acceptance: %s · Dependencies: %s", report.Checks.Requirements,
					report.Checks.Acceptance, report.Checks.Dependencies), x.Muted)
				if len(report.Findings) == 0 {
					add("No findings reported · not an approval", x.Muted)
				}
				for _, f := range report.Findings {
					label, tone := "Advisory", x.Plain
					if f.Severity == "blocking" {
						label, tone = "Blocking", x.Warning
					}
					label += " · " + f.Area
					if f.StepID != "" {
						label += " · " + f.StepID
					}
					add(label+": "+f.Message, tone)
				}
				for _, limitation := range report.Limitations {
					add("Limit: "+limitation, x.Muted)
				}
			}
		}
	}
	if row == nil || row.State != "complete" {
		switch p.view.Source {
		case "missing":
			add("Original requirements not captured", x.Warning)
		case "partial":
			add("Original requirements only partly captured", x.Warning)
		}
	}
	p.body, p.width = body, width
	return body
}

func (m *Model) planReviewRows(d *dialog, w, n int) (rows []string, targets []int) {
	p := d.planReview
	add := func(line string) {
		if len(rows) < n {
			rows, targets = append(rows, ansi.Truncate(line, w, "…")), append(targets, -1)
		}
	}
	add(strongText(fmt.Sprintf("Plan review · r%d · %s", p.target.Revision, oneLine(p.title))))
	keys := "↑↓ scroll"
	if p.failed {
		add("Could not load review · r refresh")
	} else if !p.ready {
		add("Loading review…")
	} else {
		if p.view.HeadRevision > p.target.Revision {
			add(m.paint(x.Warning, fmt.Sprintf("Stale revision · head is r%d", p.view.HeadRevision)))
		}
		if p.note != "" {
			add(m.paint(x.Warning, p.note))
		} else if p.sending || p.awaiting != "" {
			add(m.control("Updating review…"))
		}
		lines := p.lines(w)
		height := max(0, n-len(rows)-2)
		start := min(p.offset, max(0, len(lines)-height))
		for _, line := range lines[start:min(len(lines), start+height)] {
			add(m.paint(line.tone, line.text))
		}
	}
	prefix := ""
	if p.canStart() {
		prefix = "↵ start review  "
		if p.view.Review != nil {
			prefix = "↵ retry review  "
		}
	} else if r := p.view.Review; p.ready && r != nil && r.State == "running" && r.ThreadID == d.route.owner {
		prefix = "x stop review  "
	}
	if p.ready && p.view.Review != nil && p.view.Review.ReviewerThreadID.Valid() {
		keys += "  o reviewer"
	}
	if p.ready && (p.target.Revision > 1 || p.view.HeadRevision > p.target.Revision) {
		keys += "  ←→ revisions"
	}
	add(m.paint(x.Muted, m.hintRow(w, prefix+keys, prefix+keys, "esc back")))
	if n >= 5 {
		add("")
	}
	return
}
