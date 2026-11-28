package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const openPlanRunID x.ActionID = menusID + ".plan-run"
const openPlanRunOp x.LocalOp = "plan.openRun"
const planDotLimit = 20

// planActivity is the thread's oldest active run, or else its last one.
type planActivity struct {
	run   *sdk.PlanActivityRun
	ready bool
}

type planActivityMsg struct {
	token  uint64
	source <-chan sdk.PlanActivityUpdate
	update sdk.PlanActivityUpdate
	ok     bool
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
	m.threads[f.owner].planRun.ready = false
	return func() tea.Msg { return awaitPlanActivity(f, b.WatchPlanActivity(ctx, f.owner))() }
}

func awaitPlanActivity(f feedLease, source <-chan sdk.PlanActivityUpdate) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.ctx.Done():
			return nil
		case v, ok := <-source:
			return planActivityMsg{f.token, source, v, ok}
		}
	}
}

func (m *Model) receivePlanActivity(msg planActivityMsg) tea.Cmd {
	f := m.planFeed
	if f.token == 0 || msg.token != f.token || f.owner != m.active {
		return nil
	}
	v := &m.threads[f.owner].planRun
	if !msg.ok {
		m.stopPlanWatch()
		v.ready = false
		return nil
	}
	v.ready = msg.update.Err == nil
	if v.ready {
		v.run = msg.update.Run
	}
	return awaitPlanActivity(f, msg.source)
}

func (m *Model) currentPlanRun(t *threadState) *sdk.PlanActivityRun {
	if t == nil || !t.planRun.ready || !m.menuFeaturesFromBackend.Has(x.Plans) {
		return nil
	}
	return t.planRun.run
}

func (m *Model) openCurrentPlanRun(r ownerRoute, expected sdk.ID) tea.Cmd {
	run := m.currentPlanRun(m.threads[r.owner])
	if run == nil || expected != "" && expected != run.ID {
		return nil
	}
	return m.openPlanSteps(r, m.planCWD(r.owner), run.PlanID)
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
	run := m.currentPlanRun(t)
	if run == nil || run.ID != m.frame.planStrip {
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

func planStripGlyph(run *sdk.PlanActivityRun, s sdk.PlanActivityStep) (string, x.Tone) {
	switch {
	case s.Status == "done":
		return "✓", x.Good
	case s.Status == "blocked":
		return "⏸", x.Warning
	case run.State == "running":
		return "●", x.Warning
	default:
		return "○", x.Muted
	}
}

func planRunSummary(t *threadState, run *sdk.PlanActivityRun) string {
	switch run.State {
	case "running":
		if slices.Contains(t.runInputs, run.SubmissionID) {
			return "Run in progress"
		}
		if slices.ContainsFunc(t.queue, func(q sdk.QueueItem) bool { return q.SubmissionID == run.SubmissionID }) {
			return "Run queued"
		}
		return "Waiting for the turn"
	case "failed":
		return "Run stopped"
	default:
		return "Run finished"
	}
}

func (m *Model) planStripRows(t *threadState, run *sdk.PlanActivityRun, f *frameMemo, w, budget int, keys bool) []displayRow {
	attention := run.State == "failed"
	keys = keys && m.planStripKeys(t)
	f.planStrip = run.ID
	f.planPeekable = keys && budget >= 3 && (run.State == "running" || attention || m.planPeek == run.ID)
	f.planPeek = f.planPeekable && m.planPeek == run.ID
	f.planOpen = keys && (f.planPeek || attention)

	var rows []displayRow
	if f.planPeek {
		rows = m.planPeekRows(t, run, w, budget-1)
	}
	done, blocked := 0, 0
	for _, s := range run.Steps {
		switch s.Status {
		case "done":
			done++
		case "blocked":
			blocked++
		}
	}
	lead, tone, arrow := planRunSummary(t, run), x.Muted, x.Muted
	switch {
	case attention:
		lead, tone, arrow = "✗ "+planRunSummary(t, run), x.Danger, x.Danger
		if run.Failure != "" {
			lead = "✗ " + oneLine(run.Failure)
		}
	case run.State == "running":
		arrow = x.Warning
	case blocked > 0:
		lead += fmt.Sprintf(" · %d blocked", blocked)
		tone = x.Warning
	}
	hint := ""
	switch {
	case f.planOpen && attention:
		hint = "↵ see why"
	case f.planPeek:
		hint = "focused"
	case f.planPeekable:
		hint = "↑ peek"
	}
	prefix := m.paint(arrow, "▸ ") + strongText(fmt.Sprintf("plan r%d", run.Revision))
	main := w
	right := stringWidth(hint)
	if right > 0 && w >= stringWidth(prefix)+right+16 {
		main -= right + 2
	} else {
		hint = ""
	}
	tally := fmt.Sprintf("%d/%d", done, len(run.StepIDs))
	counts := m.quiet(tally)
	if len(run.Steps) <= planDotLimit && len(run.Steps)+2+len(tally) <= main-stringWidth(prefix)-2 {
		var dots strings.Builder
		for _, s := range run.Steps {
			glyph, tone := planStripGlyph(run, s)
			dots.WriteString(m.paint(tone, glyph))
		}
		counts = dots.String() + "  " + counts
	}
	line := prefix + "  " + counts
	if room := main - stringWidth(line) - 2; room > 0 {
		line += "  " + m.paint(tone, ansi.Truncate(lead, room, "…"))
	}
	if hint != "" {
		line = fit(line, w-right) + m.quiet(hint)
	}
	line = ansi.Truncate(line, w, "…")
	action := x.ActionRef{ID: openPlanRunID, Target: string(run.ID)}
	return append(rows, displayRow{text: line, hits: []spanHit{{0, stringWidth(line), action, "Open current plan run"}}})
}

func (m *Model) planPeekRows(t *threadState, run *sdk.PlanActivityRun, w, budget int) []displayRow {
	gutter := m.quiet("│ ")
	rows := make([]displayRow, 0, min(budget, len(run.Steps)+1))
	add := func(text string) {
		rows = append(rows, displayRow{text: gutter + ansi.Truncate(text, w-2, "…")})
	}
	live := run.State == "running" && slices.Contains(t.runInputs, run.SubmissionID)
	for _, s := range run.Steps {
		if len(rows) == budget-1 {
			break
		}
		glyph, tone := planStripGlyph(run, s)
		note := ""
		switch {
		case s.Status == "blocked" && s.Reason != "":
			note = oneLine(s.Reason)
		case s.Status == "pending" && live:
			note = t.planLiveLabel()
		}
		text := m.paint(tone, glyph+" ") + oneLine(s.Title)
		if note != "" && w >= 25 {
			text += m.quiet(" · " + ansi.Truncate(note, w*2/5, "…"))
		}
		add(text)
	}
	add(m.menuKeys(w-2, "↵ open plan"))
	return rows
}

// A run is one turn for all selected steps, so live activity is turn-wide.
func (t *threadState) planLiveLabel() string {
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
		if path := oneLine(args.Path); path != "" {
			switch tool.Name {
			case "read":
				return "Reading " + path
			case "edit", "write":
				return "Editing " + path
			}
		}
		return "Running " + oneLine(tool.Name)
	case liveIdle, liveFailed:
		return "Settling run"
	case liveSending, liveStarting:
		return "Starting"
	case liveWorking:
		return "Working"
	default:
		return "Waiting for model"
	}
}
