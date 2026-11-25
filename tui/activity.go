package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const activityID = "hush/activity"
const expandRowID x.ActionID = activityID + ".expand"
const forkPickerID x.ActionID = "hush/menus.fork"

func activityRegistration() x.Registration {
	return x.Registration{ID: activityID, APIVersion: x.APIVersion, Actions: []x.Action{{
		ID: expandRowID, Title: "Expand activity row", Binding: x.Binding{Leader: "a"},
		Available: func(c x.Context) bool { return c.Thread.ID != "" },
		Run: func(_ x.Context, v x.Invocation) x.Outcome {
			if v.Target == "" {
				return x.Outcome{x.Local{Op: x.ToggleDetails}}
			}
			return x.Outcome{x.Local{Op: x.ToggleActivity, Text: v.Target}}
		},
	}, {
		ID: inspectPlanPreviewID, Title: "Inspect plan preview", Binding: x.Binding{Leader: "i v"},
		Run: func(_ x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: inspectPlanPreviewOp, Text: v.Target}}
		},
	}, {
		ID: savePlanPreviewID, Title: "Save to /plan", Binding: x.Binding{Leader: "i w"}, Requires: x.PlanDraft,
		Run: func(_ x.Context, v x.Invocation) x.Outcome {
			return x.Outcome{x.Local{Op: savePlanPreviewOp, Text: v.Target}}
		},
	}}}
}

// blockRange identifies the expanded children, not the summary or the
// surrounding space. Only the built-in activity projection supplies these.
type blockRange struct{ start, end int }

func (m *Model) activityEntry(ctx x.RenderContext, t *threadState, r *record) (x.Content, []blockRange) {
	var out x.Content
	var cards []blockRange
	gap := x.Block{Lines: []x.Line{nil}}
	e := r.entry
	if e.Record.Kind == planPreviewEntry && len(e.Record.Messages) == 1 && len(e.Record.Messages[0].Blocks) == 1 {
		return m.planPreviewContent(ctx, t, e.Record.ID, e.Record.Messages[0].Blocks[0].Text), nil
	}
	if e.Record.Kind == planReviewEntry && len(e.Record.Messages) == 1 && len(e.Record.Messages[0].Blocks) == 1 {
		heading, body, _ := strings.Cut(e.Record.Messages[0].Blocks[0].Text, "\n")
		return x.Content{{Text: heading, Tone: x.Accent}, {Text: body}}, nil
	}
	if e.Record.Kind == "pi.compaction" {
		out = append(out, x.Block{Text: "─  context compacted", Tone: x.Muted})
		if !ctx.Expanded {
			return out, nil
		}
	} else if len(e.Record.Messages) == 0 && e.Record.Kind != "model" {
		label := "─  " + e.Record.Kind + " · transcript fork"
		if f := m.options.RestorableEntry; f != nil && f(ctx.Thread.ID, e.Record.ID) {
			label = "─  " + e.Record.Kind + " · file snapshot"
		}
		if m.canHintFork() {
			label += " · esc esc"
		}
		return x.Content{{Text: label, Tone: x.Muted}}, nil
	}
	needsGap := false
	space := func() {
		if needsGap {
			out = append(out, gap)
			needsGap = false
		}
	}
	for i, msg := range e.Record.Messages {
		if msg.Role == "toolResult" {
			if burst := r.bursts[i]; burst != nil {
				out = append(out, gap)
				start := len(out)
				out = append(out, m.burstContent(ctx, t, burst)...)
				if start+1 < len(out) {
					cards = append(cards, blockRange{start + 1, len(out)})
				}
				needsGap = true
			}
			continue
		}
		needsGap = needsGap || r.afterBurst&(1<<i) != 0
		shownThought := false
		for _, b := range msg.Blocks {
			switch b.Type {
			case "thinking":
				if !ctx.ReasoningExpanded || strings.TrimSpace(b.Text) == "" {
					continue
				}
				if !shownThought {
					space()
					label := "Thought"
					if r.thought.Round(time.Second) > 0 {
						label += " · " + elapsedLabel(r.thought)
					}
					out = append(out, x.Block{Text: label, Tone: x.Muted})
					shownThought = true
				}
			case "toolCall":
				continue
			case "text":
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				space()
			}
			out = append(out, messageBlocks(ctx, msg.Role, []sdk.Block{b})...)
		}
		if msg.Error != "" {
			space()
			out = append(out, x.Block{Text: msg.Error, Tone: x.Danger})
		}
	}
	return out, cards
}

func (m *Model) applyActivity(t *threadState, s sdk.State, now time.Time, newRows []*record) {
	old := t.activity
	v := activityState{thinkAt: old.thinkAt, thought: old.thought, tools: make(map[string]time.Time)}
	if s.Run != nil {
		v.task = s.Run.TaskID
	}
	if len(s.Compactions) > 0 {
		v.compacting = true
	}
	if s.Failure != nil && failedRunCause(s.Failure.Reason) != "" {
		v.failure = *s.Failure
	}
	v.compactionFailure = deref(s.CompactionFailure)
	phase, tool, _ := t.toolStatus()
	if phase == liveToolRunning {
		v.intent = t.projected.pending[tool.CallID].intent
	}
	if s.Streaming != nil {
		v.attempt = s.Streaming.Attempt
		intent := ""
		for _, b := range s.Streaming.Blocks {
			if b.Type == "toolCall" && phase == liveToolRunning && b.ID == tool.CallID && v.intent == "" {
				v.intent = intent
			}
			if isReasoning(b) {
				v.thinking = true
			}
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				v.responding = true
				v.thinking = false
				if phase == liveToolRunning && v.intent == "" {
					intent = progressIntent(b.Text)
				}
			}
		}
	}
	sameAttempt := v.task == old.task && v.attempt == old.attempt
	elapsed := old.thought
	if old.thinking && !old.thinkAt.IsZero() && (!v.thinking || !sameAttempt) {
		elapsed = max(0, now.Sub(old.thinkAt))
		v.thinkAt = time.Time{}
	}
	v.thought = elapsed
	if v.thinking && (!old.thinking || !sameAttempt) {
		v.thinkAt = now
		v.thought = 0
	}
	for _, r := range newRows {
		hasThought := false
		for _, msg := range r.entry.Record.Messages {
			for _, b := range msg.Blocks {
				if isReasoning(b) {
					hasThought = true
					break
				}
			}
		}
		if hasThought && elapsed > 0 {
			r.thought = elapsed
			r.cache = blockCache{}
			elapsed = 0
			if !v.thinking {
				v.thought = 0
			}
		}
	}
	if v.task != old.task && !v.thinking {
		v.thought = 0
	}
	for _, tool := range t.tools {
		if tool.Status == "done" {
			continue
		}
		started := old.tools[tool.CallID]
		if started.IsZero() || old.task != v.task {
			started = now
		}
		v.tools[tool.CallID] = started
	}
	t.activity = v
	v.phase, _, _ = t.liveStatus()
	if v.phase != old.phase || v.task != old.task || old.since.IsZero() {
		v.since = now
	} else {
		v.since = old.since
	}
	t.activity = v
}

type activityState struct {
	phase                            livePhase
	since                            time.Time
	task                             sdk.ID
	attempt                          uint64
	thinkAt                          time.Time
	thought                          time.Duration
	compacting, thinking, responding bool
	failure, compactionFailure       sdk.Failure
	tools                            map[string]time.Time
	intent                           string
}

// Use an explicit, short progress aside immediately before a tool call, not
// arbitrary prose, private reasoning, or the last line printed by the tool.
func progressIntent(text string) string {
	line := strings.TrimSpace(text)
	line = strings.TrimSpace(line[strings.LastIndexByte(line, '\n')+1:])
	if len(line) < 3 || len(line) > 192 || (line[0] != '*' && line[0] != '_') || line[len(line)-1] != line[0] {
		return ""
	}
	inner := strings.TrimSpace(line[1 : len(line)-1])
	if inner == "" || strings.ContainsRune(inner, rune(line[0])) {
		return ""
	}
	return oneLine(inner)
}

func isReasoning(b sdk.Block) bool {
	return b.Type == "thinking" && b.Text != "" || b.Type == "omitted" && b.Kind == "redactedThinking"
}

func (t *threadState) waitingForModel() bool {
	a := t.activity
	if a.responding || a.thinking || a.compacting || t.stream.Value.Retry != nil {
		return false
	}
	if t.awaiting == "" && a.task == "" && !t.stream.Present {
		return false
	}
	for _, tool := range t.tools {
		if tool.Status != "done" {
			return false
		}
	}
	return true
}

type livePhase uint8

const (
	liveIdle livePhase = iota
	liveCompacting
	liveRetry
	liveToolRunning
	liveToolParked
	liveToolPending
	liveThinking
	liveSending
	liveStarting
	liveWaiting
	liveResponding
	liveWorking
	liveFailed
)

func (t *threadState) liveStatus() (livePhase, *x.Tool, int) {
	a := t.activity
	if a.compacting {
		return liveCompacting, nil, 0
	}
	if t.stream.Value.Retry != nil {
		return liveRetry, nil, 0
	}
	if phase, tool, count := t.toolStatus(); phase != liveIdle {
		return phase, tool, count
	} else if count > 0 && !a.thinking && !a.responding {
		return liveWaiting, nil, 0
	}
	switch {
	case a.thinking:
		return liveThinking, nil, 0
	case t.sending && a.task == "" && !t.stream.Present:
		return liveSending, nil, 0
	case t.awaiting != "" && a.task == "" && !t.stream.Present:
		return liveStarting, nil, 0
	case a.responding:
		return liveResponding, nil, 0
	case t.waitingForModel():
		return liveWaiting, nil, 0
	case t.visibleFailure().TaskID != "" && !t.activeRun:
		return liveFailed, nil, 0
	case threadBusy(t):
		return liveWorking, nil, 0
	default:
		return liveIdle, nil, 0
	}
}

func (t *threadState) toolStatus() (livePhase, *x.Tool, int) {
	var running, parked, pending *x.Tool
	var outstanding int
	for i := range t.tools {
		tool := &t.tools[i]
		switch tool.Status {
		case "sleeping", "waiting":
			if parked == nil {
				parked = tool
			}
		case "done":
			continue
		case "pending":
			if pending == nil {
				pending = tool
			}
		default:
			if tool.Live && running == nil {
				running = tool
			}
		}
		outstanding++
	}
	switch {
	case running != nil:
		return liveToolRunning, running, outstanding
	case parked != nil:
		return liveToolParked, parked, outstanding
	case pending != nil:
		return liveToolPending, pending, outstanding
	}
	return liveIdle, nil, outstanding
}

func (t *threadState) visibleFailure() sdk.Failure {
	f := t.activity.failure
	if f.TaskID != "" && failedRunCause(f.Reason) != "" && !t.dismissedFailure(f.TaskID) {
		return f
	}
	return t.activity.compactionFailure
}

func (m *Model) liveFailureContent(ctx x.RenderContext, t *threadState) x.Content {
	f := t.visibleFailure()
	issue := t.currentIssue()
	var hints x.Line
	if issue != nil && issue.TaskID == f.TaskID {
		f.Detail = issue.Detail
		if m.connection == x.Connected {
			hints = m.recoveryHints(issueActions(t.info.ID, issue), true)
		}
	} else if t.presentation == nil && (f.Reason == "model_error" || f.Reason == "no_model") && m.menuFeatures().Has(x.Catalog) {
		hints = x.Line{{Text: "/model to switch", Tone: x.Muted, Action: x.ActionRef{ID: modelPickerID}, Hint: "choose a model"}}
	}
	label := runFailureText(f.Reason, f.Detail)
	if f == t.activity.compactionFailure {
		label = "compaction failed"
	}
	row := x.Line{{Text: "✗ ", Tone: x.Danger}, {Text: label}}
	return x.Content{activityRow(fitRunHints(row, hints, ctx.Width), "", ctx.Width)}
}

const thinkingSpinnerStep = 200 * time.Millisecond

func (m *Model) liveActivity(t *threadState, width int, phase livePhase, tool *x.Tool, count int) displayRow {
	if phase == liveIdle || width < 4 {
		return displayRow{}
	}
	if tool != nil || phase == liveFailed {
		ctx := m.renderContext(t, width, 1, true)
		ctx.MaxBytes = 4 << 10
		if t.liveOverride {
			ctx.Expanded = t.liveExpanded
		}
		var content x.Content
		var handled bool
		if tool != nil {
			content, handled = m.toolRenderer(ctx, *tool)
		} else {
			content, handled = m.liveFailureContent(ctx, t), true
		}
		if handled {
			if rows := m.layoutContent(ctx, renderKey{}, content); len(rows) > 0 {
				return rows[0]
			}
			return displayRow{}
		}
	}
	a := t.activity
	spinner := [...]string{"◌", "◔", "◑", "◕"}
	label, icon, tone := "", spinner[m.phase], x.Accent
	since := a.since
	switch phase {
	case liveCompacting:
		label = "Compacting context"
	case liveRetry:
		r := t.stream.Value.Retry
		left := max(0, int64(r.AtMS)-m.now.UnixMilli()+999) / 1000
		label = fmt.Sprintf("Retry %d · %ds", t.stream.Value.Attempt, left)
		icon, tone, since = "⟳", x.Warning, time.Time{}
	case liveToolRunning:
		label = a.intent
		if label == "" {
			action := toolActionLabel(*tool)
			if action == "bash" {
				action = "shell command"
			}
			label = "Running " + action
		}
		since = a.tools[tool.CallID]
	case liveToolParked:
		icon, tone, since = "◐", x.Warning, a.tools[tool.CallID]
		label = "Waiting on " + tool.Name
		if tool.Status == "sleeping" {
			label = "Sleeping"
		}
	case liveToolPending:
		label = "Waiting for " + tool.Name
	case liveThinking:
		label, since, icon = "Thinking", a.thinkAt, spinner[0]
		if !m.options.ReducedMotion && !since.IsZero() {
			step := max(0, m.now.Sub(since)) / thinkingSpinnerStep
			icon = spinner[step%time.Duration(len(spinner))]
		}
	case liveSending:
		label, since = "Sending", time.Time{}
	case liveStarting:
		label, since = "Starting", time.Time{}
	case liveWaiting:
		label = "Waiting for model"
	case liveResponding:
		label, since, icon = "Writing answer", time.Time{}, spinner[0]
	case liveWorking:
		label, since, icon = "Working", time.Time{}, spinner[0]
	}
	suffix := ""
	if count > 1 {
		suffix = fmt.Sprintf(" · +%d", count-1)
	}
	if elapsed := m.now.Sub(since); !since.IsZero() && elapsed >= time.Second {
		suffix += " · " + elapsedLabel(elapsed)
	}
	if stringWidth(suffix) > width-8 {
		suffix = ""
	}
	room := width - stringWidth(icon+" ") - stringWidth(suffix)
	label = strings.TrimSpace(oneLine(limitText(label, 512)))
	label = ansi.Truncate(label, room, "…")
	return displayRow{text: m.paint(tone, icon+" \x1b[3m"+label+"\x1b[23m"+suffix)}
}

func (m *Model) canHintFork() bool {
	_, bound := m.host.byID[forkPickerID]
	t := m.threads[m.active]
	return bound && m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching && !threadBusy(t) && m.connection == x.Connected
}
