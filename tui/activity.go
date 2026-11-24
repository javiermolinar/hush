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
	}}}
}

func (m *Model) activityEntry(ctx x.RenderContext, t *threadState, r *record) x.Content {
	var out x.Content
	e := r.entry
	if e.Record.Kind == "pi.compaction" {
		out = append(out, x.Block{Text: "─  context compacted", Tone: x.Muted})
		if !ctx.Expanded {
			return out
		}
	} else if len(e.Record.Messages) == 0 && e.Record.Kind != "model" {
		label := "─  " + e.Record.Kind + " · transcript fork"
		if f := m.options.RestorableEntry; f != nil && f(ctx.Thread.ID, e.Record.ID) {
			label = "─  " + e.Record.Kind + " · file snapshot"
		}
		if m.canHintFork() {
			label += " · esc esc"
		}
		return x.Content{{Text: label, Tone: x.Muted}}
	}
	for i, msg := range e.Record.Messages {
		if msg.Role == "toolResult" {
			if burst := r.bursts[i]; burst != nil {
				out = append(out, m.burstContent(ctx, t, burst)...)
			}
			continue
		}
		shownThought := false
		afterBurst := r.afterBurst&(1<<i) != 0
		for _, b := range msg.Blocks {
			switch b.Type {
			case "thinking":
				if !ctx.Expanded || strings.TrimSpace(b.Text) == "" {
					continue
				}
				if !shownThought {
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
				if afterBurst {
					out = append(out, x.Block{Lines: []x.Line{nil}})
					afterBurst = false
				}
			}
			out = append(out, messageBlocks(ctx, msg.Role, []sdk.Block{b})...)
		}
		if msg.Error != "" {
			out = append(out, x.Block{Text: msg.Error, Tone: x.Danger})
		}
	}
	return out
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
	if s.Streaming != nil {
		v.attempt = s.Streaming.Attempt
		for _, b := range s.Streaming.Blocks {
			if isReasoning(b) {
				v.thinking = true
			}
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				v.responding = true
				v.thinking = false
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
	case a.thinking:
		return liveThinking, nil, 0
	case t.sending && a.task == "" && !t.stream.Present:
		return liveSending, nil, 0
	case t.awaiting != "" && a.task == "" && !t.stream.Present:
		return liveStarting, nil, 0
	case t.waitingForModel() || outstanding > 0:
		return liveWaiting, nil, 0
	case t.visibleFailure().TaskID != "" && !t.activeRun:
		return liveFailed, nil, 0
	}
	return liveIdle, nil, 0
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
		hints = x.Line{{Text: "/model to switch", Tone: x.Accent, Action: x.ActionRef{ID: modelPickerID}, Hint: "choose a model"}}
	}
	label := runFailureText(f.Reason, f.Detail)
	if f == t.activity.compactionFailure {
		label = "compaction failed"
	}
	row := x.Line{{Text: "✗ ", Tone: x.Danger}, {Text: label}}
	return x.Content{activityRow(fitRunHints(row, hints, ctx.Width), "", ctx.Width)}
}

const thinkingSpinnerStep = 200 * time.Millisecond

func (m *Model) liveActivity(ctx x.RenderContext, t *threadState) x.Content {
	phase, tool, count := t.liveStatus()
	if tool != nil {
		if content, handled := m.toolRenderer(ctx, *tool); handled {
			return content
		}
	}
	a := t.activity
	spinner := [...]string{"◌", "◔", "◑", "◕"}
	label, preview, icon, tone := "", "", spinner[m.phase], x.Accent
	since := a.since
	switch phase {
	case liveCompacting:
		label = "compacting the context"
	case liveRetry:
		r := t.stream.Value.Retry
		left := max(0, int64(r.AtMS)-m.now.UnixMilli()+999) / 1000
		label = fmt.Sprintf("retry %d · %ds", t.stream.Value.Attempt, left)
		icon, tone = "⟳", x.Warning
		since = time.Time{}
	case liveToolRunning, liveToolParked:
		label = "running " + toolActionLabel(*tool)
		if count > 1 {
			label = fmt.Sprintf("%s · +%d", label, count-1)
		}
		if phase == liveToolParked {
			icon, tone = "◐", x.Warning
			label = tool.Name + " " + tool.Status
		} else if output := strings.TrimRight(tool.Output, " \t\r\n"); output != "" {
			preview = output[strings.LastIndexByte(output, '\n')+1:]
		}
		since = a.tools[tool.CallID]
	case liveToolPending:
		label = "waiting for " + tool.Name
	case liveThinking:
		label, since, icon = "thinking", a.thinkAt, spinner[0]
		if !m.options.ReducedMotion && !since.IsZero() {
			step := max(0, m.now.Sub(since)) / thinkingSpinnerStep
			icon = spinner[step%time.Duration(len(spinner))]
		}
	case liveSending:
		label, since = "sending", time.Time{}
	case liveStarting:
		label, since = "starting", time.Time{}
	case liveWaiting:
		label = "waiting for model"
	case liveFailed:
		return m.liveFailureContent(ctx, t)
	default:
		return nil
	}
	suffix := ""
	if elapsed := m.now.Sub(since); !since.IsZero() && elapsed >= time.Second {
		suffix = " · " + elapsedLabel(elapsed)
	}
	row := x.Line{{Text: icon + " ", Tone: tone}, {Text: label, Tone: x.Plain}}
	if suffix != "" || preview != "" {
		room := max(1, ctx.Width-stringWidth(icon+" ")-stringWidth(suffix))
		label = ansi.Truncate(oneLine(limitText(label, 4096)), room, "…")
		if spare := room - stringWidth(label); preview != "" && spare >= 5 {
			label += ansi.Truncate(" · "+oneLine(limitText(preview, 1024)), spare, "…")
		}
		row[1].Text = label + suffix
	}
	return x.Content{activityRow(row, "", ctx.Width)}
}

func (m *Model) canHintFork() bool {
	_, bound := m.host.byID[forkPickerID]
	t := m.threads[m.active]
	return bound && m.editor.value() == "" && len(t.attachments) == 0 && !t.attaching && !threadBusy(t) && m.connection == x.Connected
}
