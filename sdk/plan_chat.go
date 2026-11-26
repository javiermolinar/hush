package sdk

import (
	"context"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	PlanChatVersion         = 1
	PlanChatActivityVersion = 1
	planChat                = "planChat"
	planChatRun             = "ext.planChat.run"
	planChatPlans           = "ext.planChat.workspace"
	planChatInspect         = "ext.planChat.inspect"
	planChatActivity        = "planChatActivity"
	planChatActivityWait    = "ext.planChatActivity.wait"
)

func validPlanReports(reports []PlanTaskReport, steps map[string]bool) bool {
	seen := make(map[string]bool, len(reports))
	for _, r := range reports {
		if !steps[r.StepID] || seen[r.StepID] || !goodPlanText(r.Detail, 180) || (strings.IndexFunc(r.Detail, unicode.IsControl) >= 0 || strings.ContainsAny(r.Detail, "\u2028\u2029")) {
			return false
		}
		switch r.State {
		case "done", "blocked", "incomplete":
		default:
			return false
		}
		seen[r.StepID] = true
	}
	return true
}

func planChatCWD(cwd string) bool {
	return path.IsAbs(cwd) && len(cwd) <= 4096 && utf8.ValidString(cwd) && strings.IndexFunc(cwd, unicode.IsControl) < 0
}

func planChatWatch(method string) workspaceWatch {
	return workspaceWatch{method: method, capability: planChat, version: PlanChatVersion, base: planWorkspace, baseVersions: []uint64{PlanWorkspaceVersion}}
}

// WatchWorkspacePlansAt opens a launch directory's plan list without a thread.
func (c *Client) WatchWorkspacePlansAt(ctx context.Context, cwd string, page int) <-chan PlanWorkspaceUpdate {
	w := planChatWatch(planChatPlans)
	if planChatCWD(cwd) && page >= 0 && page <= 1_000_000 {
		w.params = map[string]any{"cwd": cwd, "page": page}
	}
	return c.watchWorkspacePlans(ctx, w)
}

// WatchNamedPlanAt opens a live plan in a launch directory without a thread.
func (c *Client) WatchNamedPlanAt(ctx context.Context, cwd, planID string) <-chan PlanUpdate {
	w := planChatWatch(planChatInspect)
	if planChatCWD(cwd) && token(planID) {
		w.params = map[string]any{"cwd": cwd, "planId": planID}
	}
	return c.watchPlan(ctx, w)
}

// WatchChatPlanActivity follows Run here inputs admitted in this thread,
// independently of its plan binding. A fork does not inherit this activity.
func (c *Client) WatchChatPlanActivity(ctx context.Context, thread ID) <-chan PlanUpdate {
	w := workspaceWatch{method: planChatActivityWait, capability: planChatActivity, version: PlanChatActivityVersion,
		base: planChat, baseVersions: []uint64{PlanChatVersion}}
	if thread.Valid() {
		w.params = map[string]any{"threadId": thread}
	}
	return c.watchPlan(ctx, w)
}

// RunPlanInChat admits one native input using the target thread's normal tools.
// It neither binds the thread to the plan nor grants scoped plan tool access.
// Retry an indeterminate reply with the same request ID and exact selection.
func (c *Client) RunPlanInChat(ctx context.Context, selection PlanSelection) (ID, error) {
	return c.runPlan(ctx, selection, planChat, PlanChatVersion, planChatRun)
}
