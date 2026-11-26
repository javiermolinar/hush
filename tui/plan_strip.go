package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"math/bits"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const openPlanRunID x.ActionID = menusID + ".plan-run"
const openPlanRunOp x.LocalOp = "plan.openRun"
const planDotLimit = 20
const planVerifiedDuration = 3 * time.Second
const planRunFailed uint32 = 1 << 31 // the first 24 bits represent plan steps

type planStripEvents struct {
	verified string
	flash    uint64
	seen     uint32
}

type planVerifiedExpired struct {
	owner sdk.ID
	flash uint64
}

type chatPlanActivityBackend interface {
	WatchChatPlanActivity(context.Context, sdk.ID) <-chan sdk.PlanUpdate
}

func (m *Model) syncChatPlanWatch() tea.Cmd {
	b, ok := m.backend.(chatPlanActivityBackend)
	if !ok || !m.nativeChatPlanActivity || !m.active.Valid() {
		m.stopChatPlanWatch()
		return nil
	}
	if m.chatPlanFeed.token != 0 && m.chatPlanFeed.owner == m.active {
		return nil
	}
	m.stopChatPlanWatch()
	ctx, cancel := context.WithCancel(m.ctx)
	f := feedLease{token: m.token(), owner: m.active, ctx: ctx, cancel: cancel}
	m.chatPlanFeed = f
	m.threads[f.owner].chatPlan.ready = false
	return func() tea.Msg { return awaitPlanWatch(f, b.WatchChatPlanActivity(ctx, f.owner))() }
}

func (m *Model) stopChatPlanWatch() {
	if m.chatPlanFeed.cancel != nil {
		m.chatPlanFeed.cancel()
	}
	m.chatPlanFeed = feedLease{}
}

func (m *Model) receiveChatPlanWatch(msg planWatchMsg) tea.Cmd {
	f := m.chatPlanFeed
	v := &m.threads[f.owner].chatPlan
	v.err, v.ready = msg.update.Err, msg.ok && msg.update.Err == nil
	if !msg.ok {
		m.stopChatPlanWatch()
		return nil
	}
	if v.ready {
		next := msg.update.Snapshot
		if v.snapshot.Run == nil || next.Run == nil || v.snapshot.Run.ID != next.Run.ID {
			v.strip = planStripEvents{}
		}
		v.snapshot = next
	}
	return awaitPlanWatch(f, msg.source)
}

// Watch snapshots can coalesce. Flash the last newly verified step, never an
// already-verified step from the initial snapshot or a duplicate update.
func (m *Model) recordPlanEvents(owner sdk.ID, v *planView, next sdk.PlanSnapshot) tea.Cmd {
	old, run := v.snapshot, next.Run
	if run == nil || old.Run == nil || old.Run.ID != run.ID {
		v.strip = planStripEvents{}
	}
	v.strip.seen &= planFailureMask(next)
	if run == nil || run.Chat || old.Plan == nil || next.Plan == nil || old.Plan.ID != next.Plan.ID || old.Plan.Revision != next.Plan.Revision {
		return nil
	}
	for i := len(next.Progress) - 1; i >= 0; i-- {
		p := next.Progress[i]
		if p.State == "verified" && slices.Contains(run.StepIDs, p.StepID) && v.progress(p.StepID).State != "verified" {
			token := m.token()
			v.strip.verified, v.strip.flash = p.StepID, token
			return tea.Tick(planVerifiedDuration, func(time.Time) tea.Msg { return planVerifiedExpired{owner, token} })
		}
	}
	return nil
}

func planFailureMask(s sdk.PlanSnapshot) uint32 {
	if s.Plan == nil || s.Run == nil {
		return 0
	}
	var mask uint32
	if s.Run.State == "failed" {
		mask = planRunFailed
	}
	if !s.Run.Chat {
		for _, p := range s.Progress {
			if p.State != "failed" || !slices.Contains(s.Run.StepIDs, p.StepID) {
				continue
			}
			if i := slices.IndexFunc(s.Plan.Steps, func(step sdk.PlanStep) bool { return step.ID == p.StepID }); i >= 0 {
				mask |= 1 << i
			}
		}
	}
	return mask
}

func (v *planView) stripFailure() (index int, reason string) {
	unseen := planFailureMask(v.snapshot) &^ v.strip.seen
	if unseen == 0 {
		return -1, ""
	}
	if unseen&^planRunFailed != 0 {
		return bits.TrailingZeros32(unseen), v.planFailureReason("Verification failed")
	}
	return -1, v.planFailureReason("Run failed")
}

func (v *planView) planFailureReason(fallback string) string {
	if run := v.snapshot.Run; run != nil && run.Failure != "" {
		return oneLine(run.Failure)
	}
	return fallback
}

func (m *Model) currentPlanRun(t *threadState) *planView {
	if !m.planAvailable() || t == nil {
		return nil
	}
	bound, chat := &t.plan, &t.chatPlan
	hasBound := bound.ready && bound.snapshot.Plan != nil && bound.snapshot.Run != nil
	hasChat := m.nativeChatPlanActivity && chat.ready && chat.snapshot.Plan != nil && chat.snapshot.Run != nil
	if !hasBound {
		if hasChat {
			return chat
		}
		return nil
	}
	if !hasChat {
		return bound
	}
	b, c := bound.snapshot.Run, chat.snapshot.Run
	switch {
	case b.ID == c.ID, canStopPlan(t):
		return bound
	case c.State == "running":
		return chat
	case b.State == "running", lessID(c.ID, b.ID):
		return bound
	default:
		return chat
	}
}

func (m *Model) openCurrentPlanRun(r ownerRoute, expected sdk.ID) tea.Cmd {
	if !m.foreground(r) || len(m.overlays) >= maxDialogs {
		return nil
	}
	t := m.threads[r.owner]
	v := m.currentPlanRun(t)
	if v == nil || expected != "" && expected != v.snapshot.Run.ID {
		return nil
	}
	if v == &t.plan {
		return m.openPlanSteps(r, v.snapshot.Plan.ID)
	}
	p := &planPicker{activityRun: v.snapshot.Run.ID}
	m.focusPlanFailure(t, v, p)
	return m.pushDialog(&dialog{route: r, token: m.token(), plan: p})
}

func (m *Model) focusPlanFailure(t *threadState, v *planView, p *planPicker) {
	i, reason := v.stripFailure()
	if reason == "" {
		return
	}
	if i < 0 {
		i = slices.IndexFunc(v.snapshot.Plan.Steps, func(step sdk.PlanStep) bool { return step.ID == v.snapshot.Run.StepIDs[0] })
	}
	p.focus, p.details = max(0, i), true
	seen := planFailureMask(v.snapshot)
	for _, peer := range []*planView{&t.plan, &t.chatPlan} {
		if peer.snapshot.Run != nil && peer.snapshot.Run.ID == v.snapshot.Run.ID {
			peer.strip.seen |= seen
		}
	}
}

func (m *Model) planStripKeys(t *threadState) bool {
	return m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching && m.leader == ""
}

// Only claim keys for a strip that was drawn, for the same run. Other keys
// dismiss peek and keep going through the regular composer key map.
func (m *Model) planStripKey(t *threadState, chord string) (bool, tea.Cmd) {
	peek := m.planPeek
	m.planPeek = ""
	if peek != "" && (chord == "esc" || chord == "down") {
		return true, nil
	}
	if !m.currentFrame() || m.frame.planStrip == "" || !m.planStripKeys(t) {
		return false, nil
	}
	v := m.currentPlanRun(t)
	if v == nil || v.snapshot.Run.ID != m.frame.planStrip {
		return false, nil
	}
	if chord == "up" && m.frame.planPeekable {
		m.planPeek = m.frame.planStrip
		return true, nil
	}
	if chord == "enter" && (m.frame.planOpen || m.frame.planPeekable && peek == m.frame.planStrip) {
		return true, m.openCurrentPlanRun(m.route(m.active, menusID), m.frame.planStrip)
	}
	return false, nil
}

type stripProgress struct {
	status []planStepState
	counts [planStepStates]int
	first  int // first running step in authored order
}

func chatRunLive(t *threadState, r *sdk.PlanRun) bool {
	return r.Chat && r.OwnerThreadID == t.info.ID && slices.Contains(t.runInputs, r.SubmissionID)
}

func planStripProgress(t *threadState, v *planView) stripProgress {
	chatLive := chatRunLive(t, v.snapshot.Run)
	steps := v.snapshot.Plan.Steps
	p := stripProgress{status: make([]planStepState, len(steps)), first: -1}
	for i, step := range steps {
		s := v.stepState(step.ID, chatLive)
		p.status[i] = s
		p.counts[s]++
		if s == planStepRunning && p.first < 0 {
			p.first = i
		}
	}
	return p
}

func (m *Model) planStripRows(t *threadState, v *planView, f *frameMemo, w, budget int, keys bool) []displayRow {
	plan, run := v.snapshot.Plan, v.snapshot.Run
	p := planStripProgress(t, v)
	failedStep, reason := v.stripFailure()
	attention := reason != ""
	keys = keys && m.planStripKeys(t)
	f.planStrip = run.ID
	f.planPeekable = keys && budget >= 3 && (run.State == "running" || attention || m.planPeek == run.ID)
	f.planPeek = f.planPeekable && m.planPeek == run.ID
	f.planOpen = keys && (f.planPeek || attention)

	var rows []displayRow
	if f.planPeek {
		rows = m.planPeekRows(t, v, p, w, budget-1)
	}
	lead, suffix, hint, tone := "", "", "", x.Muted
	arrow := x.Muted
	if run.State == "running" {
		arrow = x.Warning
	}
	switch {
	case attention:
		arrow, tone = x.Danger, x.Danger
		lead = "✗ " + reason
		if failedStep >= 0 {
			lead = oneLine(plan.Steps[failedStep].Title) + ": " + reason
		}
	case v.strip.verified != "":
		lead, suffix, tone = "✓ "+v.title(v.strip.verified), " verified", x.Good
	case f.planPeek:
	case p.first >= 0:
		lead = oneLine(plan.Steps[p.first].Title)
		if n := p.counts[planStepRunning] - 1; n > 0 {
			suffix = fmt.Sprintf(" +%d", n)
		}
	default:
		lead = planRunSummary(t, run)
	}
	switch {
	case f.planOpen && attention:
		hint = "↵ see why"
	case f.planPeek:
		hint = "focused"
	case f.planPeekable:
		hint = "↑ peek"
	}

	label := fmt.Sprintf("plan r%d", plan.Revision)
	prefix := m.paint(arrow, "▸ ") + strongText(label)
	main := w
	right := stringWidth(hint)
	if right > 0 && w >= stringWidth(prefix)+right+16 {
		main -= right + 2
	} else {
		hint = ""
	}
	line := prefix + "  " + m.planStripCounts(p, main-stringWidth(prefix)-2)
	room := main - stringWidth(line) - 2
	if attention && room < 14 {
		line = prefix // keep room for the failure reason on a narrow pane
		room = main - stringWidth(line) - 2
	}
	if room > 0 && lead != "" {
		if failedStep >= 0 && stringWidth(lead) > room {
			titleWidth := room - min(stringWidth(reason), room*2/3) - 2
			if titleWidth > 3 {
				lead = ansi.Truncate(oneLine(plan.Steps[failedStep].Title), titleWidth, "…") + ": " + reason
			} else {
				lead = "✗ " + reason
			}
		}
		if n := stringWidth(suffix); room > n+3 {
			lead = ansi.Truncate(lead, room-n, "…") + suffix
		} else {
			lead = ansi.Truncate(lead, room, "…")
		}
		line += "  " + m.paint(tone, lead)
	}
	if hint != "" {
		line = fit(line, w-right) + m.quiet(hint)
	}
	line = ansi.Truncate(line, w, "…")
	action := x.ActionRef{ID: openPlanRunID, Target: string(run.ID)}
	return append(rows, displayRow{text: line, hits: []spanHit{{0, stringWidth(line), action, "Open current plan run"}}})
}

func (m *Model) planStripCounts(p stripProgress, w int) string {
	tally := fmt.Sprintf("%d/%d", p.counts[planStepVerified], len(p.status))
	if len(p.status) <= planDotLimit && len(p.status)+2+len(tally) <= w {
		var dots strings.Builder
		for _, status := range p.status {
			s := planStepStyles[status]
			dots.WriteString(m.paint(s.tone, s.mark))
		}
		return dots.String() + "  " + m.quiet(tally)
	}
	var counts []string
	for _, status := range [...]planStepState{planStepRunning, planStepVerified, planStepFailed, planStepPending, planStepReported, planStepBlocked} {
		if n := p.counts[status]; n != 0 {
			s := planStepStyles[status]
			counts = append(counts, m.paint(s.tone, fmt.Sprintf("%d %s", n, s.label)))
		}
	}
	text := strings.Join(counts, m.quiet(" · "))
	if stringWidth(text) > w && w < 24 {
		return m.quiet(ansi.Truncate(tally, w, "…"))
	}
	return ansi.Truncate(text, w, "…")
}

func planRunSummary(t *threadState, run *sdk.PlanRun) string {
	switch run.State {
	case "running":
		if run.Chat {
			if run.OwnerThreadID != t.info.ID {
				return "Running in another thread"
			}
			if chatRunLive(t, run) {
				return "Run here in progress"
			}
			if slices.ContainsFunc(t.queue, func(q sdk.QueueItem) bool { return q.SubmissionID == run.SubmissionID }) {
				return "Run queued"
			}
			return "Waiting for turn"
		}
		return "Starting next step"
	case "done":
		return "Run here finished"
	case "verified":
		return "Run verified"
	case "failed":
		return "Run failed"
	case "closed":
		return "Run closed"
	}
	return ""
}

func (m *Model) planPeekRows(t *threadState, v *planView, p stripProgress, w, budget int) []displayRow {
	plan, run := v.snapshot.Plan, v.snapshot.Run
	gutter := m.quiet("│ ")
	rows := make([]displayRow, 0, min(budget, p.counts[planStepRunning]+2))
	add := func(text string) {
		rows = append(rows, displayRow{text: gutter + ansi.Truncate(text, w-2, "…")})
	}
	body := budget - 1 // keep a row for keys
	var shown int
	if p.first < 0 {
		add(m.quiet(planRunSummary(t, run)))
	} else {
		shared := run.Chat && p.counts[planStepRunning] > 1
		if shared && body > 1 {
			add(m.quiet("Shared turn · " + t.planLiveLabel(nil)))
		}
		for i, step := range plan.Steps {
			if len(rows) == body {
				break
			}
			if p.status[i] != planStepRunning {
				continue
			}
			label := ""
			switch {
			case run.Chat && (!shared || body == 1):
				label = t.planLiveLabel(nil)
				if shared {
					label = "Shared turn · " + label
				}
			case !run.Chat:
				progress := v.progress(step.ID)
				if progress.WorkerThreadID != t.info.ID {
					label = "Running in worker thread"
				} else if progress.EntryID != "" && t.entries[progress.EntryID] == nil {
					label = "Starting step"
				} else {
					label = t.planLiveLabel(&step)
				}
			}
			titleWidth := w - 4 // gutter and glyph
			status := ""
			if label != "" && w >= 25 {
				status = m.quiet(" · " + ansi.Truncate(label, w*2/5, "…"))
				titleWidth -= stringWidth(status)
			}
			add(m.paint(x.Warning, "● ") + ansi.Truncate(oneLine(step.Title), titleWidth, "…") + status)
			shown++
		}
	}
	keys := m.menuKeys(w-2, "↵ open plan")
	var note string
	if hidden := p.counts[planStepRunning] - shown; hidden > 0 {
		note = fmt.Sprintf("+%d running", hidden)
	} else if pending := p.counts[planStepPending]; pending > 0 {
		note = fmt.Sprintf("%d pending", pending)
	}
	if note != "" && stringWidth(keys)+2+len(note) <= w-2 {
		keys = m.quiet(note) + "  " + keys
	}
	add(keys)
	return rows
}

// Only use live activity belonging to this run. A normal Run here has one
// submission for all selected steps, so the tool/phase is turn-wide.
func (t *threadState) planLiveLabel(step *sdk.PlanStep) string {
	phase, tool, _ := t.liveStatus()
	switch phase {
	case liveCompacting:
		return "Compacting context"
	case liveRetry:
		return "Retrying model"
	case liveThinking:
		return "Thinking"
	case liveResponding:
		return "Writing answer"
	case liveToolPending, liveToolParked:
		return "Waiting for " + oneLine(tool.Name)
	case liveToolRunning:
		if intent := t.activity.intent; intent != "" {
			return oneLine(intent)
		}
		if tool.Name == "bash" {
			return "Running shell command"
		}
		var args struct{ Path string }
		if len(tool.ArgumentsJSON) <= 4<<10 {
			_ = json.Unmarshal([]byte(tool.ArgumentsJSON), &args)
		}
		if args.Path == "" && step != nil && step.Edit != nil {
			args.Path = step.Edit.Path
		}
		path := oneLine(args.Path)
		if path != "" {
			switch tool.Name {
			case "read", "hush_plan_read_file":
				return "Reading " + path
			case "edit", "write":
				return "Editing " + path
			case "hush_plan_replace_text", "hush_plan_stage_file":
				return "Proposing edit to " + path
			}
		}
		switch tool.Name {
		case "hush_plan_read_file":
			return "Reading assigned file"
		case "hush_plan_replace_text", "hush_plan_stage_file":
			return "Proposing edit"
		default:
			return "Running " + oneLine(tool.Name)
		}
	case liveIdle, liveFailed:
		if step != nil {
			return "Settling step"
		}
		return "Settling run"
	case liveSending, liveStarting:
		return "Starting"
	case liveWorking:
		return "Working"
	default:
		return "Waiting for model"
	}
}
