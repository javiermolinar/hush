package tui

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const (
	retryFailureID   x.ActionID = menusID + ".retry"
	dismissFailureID x.ActionID = menusID + ".dismiss"
	retryFailureOp   x.LocalOp  = "run.retry"
	dismissFailureOp x.LocalOp  = "run.dismiss"
)

type renameBackend interface {
	RenameThread(context.Context, sdk.ID, string) (string, error)
}

type recoveryBackend interface {
	RetryFailure(context.Context, sdk.ID, sdk.ID) (sdk.Receipt, error)
	DismissFailure(context.Context, sdk.ID, sdk.ID) error
}

// Presentation metadata is not evidence of admission or a replacement for
// the live feed. Only apply it to the same failure task seen on that feed.
func (t *threadState) currentIssue() *sdk.IntroRunIssue {
	if t.presentation != nil && !threadBusy(t) && !t.sending && t.awaiting == "" {
		if issue := t.presentation.Issue; issue != nil && issue.TaskID == t.activity.failure.TaskID {
			return issue
		}
	}
	return nil
}

func (t *threadState) dismissedFailure(task sdk.ID) bool {
	return task != "" && t.presentation != nil && t.presentation.DismissedFailureID == task
}

type runActions struct {
	thread, task       sdk.ID
	retry, switchModel bool
}

func issueActions(thread sdk.ID, issue *sdk.IntroRunIssue) runActions {
	if issue == nil || !thread.Valid() || !issue.TaskID.Valid() {
		return runActions{}
	}
	return runActions{thread, issue.TaskID, issue.Retry, issue.SwitchModel}
}

func (a runActions) matches(target string) bool {
	thread, task, ok := strings.Cut(target, "/")
	return ok && a.thread == sdk.ID(thread) && a.task == sdk.ID(task)
}

func (m *Model) recoveryFor(owner sdk.ID, target string) runActions {
	if _, ok := m.backend.(recoveryBackend); !ok || owner != m.active {
		return runActions{}
	}
	t := m.threads[owner]
	if m.connection == x.Connected {
		if action := issueActions(owner, t.currentIssue()); target == "" || action.matches(target) {
			return action
		}
	}
	// A startup report can refer to another thread. Only a drawn action may
	// name it; unqualified shortcuts always act on the active thread.
	if i := t.intro; target != "" && i != nil && !i.frozen && i.reportProblem == "" && !i.feedError {
		for _, label := range i.labels[introAttention : introAttentionThird+1] {
			if label.actions.matches(target) {
				return label.actions
			}
		}
	}
	return runActions{}
}

func runRecovery(op x.LocalOp) func(x.Context, x.Invocation) x.Outcome {
	return func(_ x.Context, v x.Invocation) x.Outcome {
		return x.Outcome{x.Local{Op: op, Text: v.Target}}
	}
}

func (m *Model) recoverRun(r ownerRoute, e x.Local, budget *int) tea.Cmd {
	backend, ok := m.backend.(recoveryBackend)
	a := m.recoveryFor(r.owner, e.Text)
	retry := e.Op == retryFailureOp
	if !ok || !m.foreground(r) || !a.task.Valid() || retry && !a.retry {
		return nil
	}
	return m.startWork(r, func(ctx context.Context) (any, error) {
		if retry {
			return backend.RetryFailure(ctx, a.thread, a.task)
		}
		return nil, backend.DismissFailure(ctx, a.thread, a.task)
	}, func(_ any, err error, _ *int) tea.Cmd {
		switch {
		case sdk.IsCode(err, "not_found"):
			return m.notifyRoute(r, "That failure is no longer current", x.Warning)
		case err != nil:
			return m.notifyRoute(r, err.Error(), x.Danger)
		case retry:
			return m.notifyRoute(r, "Retry requested", x.Muted)
		default:
			return nil // report and footer update from the host's committed dismissal
		}
	}, budget)
}

func (m *Model) recoveryHints(a runActions, slash bool) x.Line {
	if !a.task.Valid() {
		return nil
	}
	_, recover := m.backend.(recoveryBackend)
	target := string(a.thread) + "/" + string(a.task)
	var out x.Line
	add := func(label, command string, id x.ActionID, target string) {
		text := label
		if slash {
			text = command
		}
		out = append(out, x.Span{Text: text, Tone: x.Accent, Action: x.ActionRef{ID: id, Target: target}, Hint: label})
	}
	if recover && a.retry {
		add("retry", "/retry", retryFailureID, target)
	}
	if a.switchModel && m.menuFeatures().Has(x.Catalog) {
		add("switch model", "/model", modelPickerID, string(a.thread))
	}
	if recover {
		add("dismiss", "/dismiss", dismissFailureID, target)
	}
	return out
}

// The diagnosis or row label keeps its space. Recovery shortcuts either fit
// as complete units or are omitted from the right, never split across rows.
func fitRunHints(row, hints x.Line, width int) x.Line {
	used := activityWidth(row)
	for _, hint := range hints {
		sep := " · "
		if used == 0 {
			sep = ""
		}
		n := stringWidth(sep) + stringWidth(hint.Text)
		if used+n > width {
			break
		}
		if sep != "" {
			row = append(row, x.Span{Text: sep, Tone: x.Muted})
		}
		row = append(row, hint)
		used += n
	}
	return row
}

func failedRunCause(reason string) string {
	switch reason {
	case "model_error", "model error":
		return "model run failed"
	case "no_model", "no model":
		return "no model available"
	case "faulted":
		return "run task failed"
	case "orphaned":
		return "run task could not resume"
	case "interrupted":
		return "run was interrupted"
	case "restart_limit", "restart limit":
		return "restart limit reached"
	case "turn_limit", "turn limit":
		return "tool turn limit reached"
	}
	return ""
}

func runFailureDetail(reason, detail string) string {
	detail = oneLine(limitText(detail, 640))
	if detail != "" && (reason == "turn_limit" || reason == "turn limit") {
		detail = failedRunCause(reason) + " (" + detail + ")"
	}
	return detail
}

func runFailureText(reason, detail string) string {
	if label := runFailureDetail(reason, detail); label != "" {
		return "no answer · " + label
	}
	if cause := failedRunCause(reason); cause != "" {
		return "no answer · " + cause
	}
	return ""
}
