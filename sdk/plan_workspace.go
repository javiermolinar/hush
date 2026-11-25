package sdk

import "context"

const PlanWorkspaceVersion = 1
const planWorkspace = "planWorkspace"
const planWorkspaceWait = "ext.planWorkspace.wait"
const planWorkspaceInspect = "ext.planWorkspace.inspect"
const planWorkspaceHistory = "ext.planWorkspace.history"
const planWorkspaceClose = "ext.planWorkspace.setClosed"
const planWorkspaceUse = "ext.planWorkspace.use"

// PlanSummary is a plan's live head, independently of a thread's binding.
type PlanSummary struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Revision uint64 `json:"revision"`
	Verified int    `json:"verified"`
	Total    int    `json:"total"`
	Status   string `json:"status"`
	Closed   bool   `json:"closed"`
}

type PlanWorkspaceUpdate struct {
	Plans []PlanSummary
	More  bool
	Err   error
}

type PlanWorkerRecord struct {
	StepID   string `json:"stepId"`
	ThreadID ID     `json:"threadId"`
	State    string `json:"state"`
}

type PlanTurnRecord struct {
	StepID  string `json:"stepId"`
	EntryID ID     `json:"entryId"`
	State   string `json:"state"`
}

type PlanRunRecord struct {
	ID                   ID                 `json:"id"`
	Revision             uint64             `json:"revision"`
	ThreadID             ID                 `json:"threadId"`
	StepIDs              []string           `json:"stepIds"`
	State                string             `json:"state"`
	Failure              string             `json:"failure,omitempty"`
	PublicationUncertain bool               `json:"publicationUncertain,omitempty"`
	Workers              []PlanWorkerRecord `json:"workers"`
	Turns                []PlanTurnRecord   `json:"turns,omitempty"`
}

type PlanHistoryPage struct {
	Runs []PlanRunRecord `json:"runs"`
	Next ID              `json:"next"`
}

func planWorkspaceWatch(method string) workspaceWatch {
	return workspaceWatch{method: method, capability: planWorkspace, version: PlanWorkspaceVersion, base: planCapability, baseVersions: []uint64{PlanVersion}}
}

// WatchWorkspacePlans publishes one page of plans in a thread's workspace.
func (c *Client) WatchWorkspacePlans(ctx context.Context, thread ID, page int) <-chan PlanWorkspaceUpdate {
	w := planWorkspaceWatch(planWorkspaceWait)
	if thread.Valid() && page >= 0 && page <= 1_000_000 {
		w.params = map[string]any{"threadId": thread, "page": page}
	}
	type report struct {
		Revision string        `json:"revision"`
		Plans    []PlanSummary `json:"plans"`
		More     bool          `json:"more"`
	}
	return watchWorkspace(ctx, c, w, func(raw any) (PlanWorkspaceUpdate, string, error) {
		v, err := decode[report](raw)
		if err != nil || v.Plans == nil || len(v.Plans) > 48 {
			return PlanWorkspaceUpdate{}, "", ErrProtocol
		}
		seen := make(map[string]bool, len(v.Plans))
		for _, p := range v.Plans {
			if !token(p.ID) || seen[p.ID] || !goodPlanText(p.Title, 160) || p.Revision < 1 || p.Revision > 1<<53-1 ||
				p.Total < 1 || p.Total > planStepLimit || p.Verified < 0 || p.Verified > p.Total {
				return PlanWorkspaceUpdate{}, "", ErrProtocol
			}
			switch p.Status {
			case "pending", "running", "attention", "complete":
			default:
				return PlanWorkspaceUpdate{}, "", ErrProtocol
			}
			seen[p.ID] = true
		}
		return PlanWorkspaceUpdate{Plans: v.Plans, More: v.More}, v.Revision, nil
	}, func(err error) PlanWorkspaceUpdate { return PlanWorkspaceUpdate{Err: err} })
}

// WatchNamedPlan observes a workspace plan without attaching the thread.
func (c *Client) WatchNamedPlan(ctx context.Context, thread ID, planID string) <-chan PlanUpdate {
	w := planWorkspaceWatch(planWorkspaceInspect)
	if thread.Valid() && token(planID) {
		w.params = map[string]any{"threadId": thread, "planId": planID}
	}
	return c.watchPlan(ctx, w)
}

// UsePlan explicitly changes the live plan binding. It does not admit a run.
func (c *Client) UsePlan(ctx context.Context, thread ID, planID string) error {
	if !thread.Valid() || !token(planID) {
		return ErrProtocol
	}
	_, err := optionalCall[struct{}](ctx, c, planWorkspace, PlanWorkspaceVersion, planWorkspaceUse,
		map[string]string{"threadId": string(thread), "planId": planID})
	return err
}

// SetPlanClosed changes lifecycle only. Reopening never resumes a Run.
func (c *Client) SetPlanClosed(ctx context.Context, thread ID, planID string, closed bool) error {
	if !thread.Valid() || !token(planID) {
		return ErrProtocol
	}
	_, err := optionalCall[struct{}](ctx, c, planWorkspace, PlanWorkspaceVersion, planWorkspaceClose,
		map[string]any{"threadId": thread, "planId": planID, "closed": closed})
	return err
}

func (c *Client) PlanHistory(ctx context.Context, thread ID, planID string, before ID) (PlanHistoryPage, error) {
	if !thread.Valid() || !token(planID) || before != "" && !before.Valid() {
		return PlanHistoryPage{}, ErrProtocol
	}
	params := map[string]any{"threadId": thread, "planId": planID}
	if before != "" {
		params["before"] = before
	}
	type historyWire struct {
		Runs []PlanRunRecord `json:"runs"`
		Next *ID             `json:"next"`
	}
	wire, err := metadataRead[historyWire](ctx, c, planWorkspaceHistory, params, planWorkspace, PlanWorkspaceVersion)
	if err != nil {
		return PlanHistoryPage{}, err
	}
	out := PlanHistoryPage{Runs: wire.Runs}
	if wire.Next != nil {
		out.Next = *wire.Next
	}
	if out.Runs == nil || len(out.Runs) > 12 || out.Next != "" && (!out.Next.Valid() || len(out.Runs) == 0 || out.Runs[len(out.Runs)-1].ID != out.Next) {
		return PlanHistoryPage{}, ErrProtocol
	}
	for _, run := range out.Runs {
		if !run.ID.Valid() || run.Revision < 1 || run.Revision > 1<<53-1 || !run.ThreadID.Valid() ||
			!planRunState(run.State) || run.Failure != "" && (run.State != "failed" || !goodPlanText(run.Failure, 240)) ||
			run.PublicationUncertain && run.Failure == "" || !validPlanIDs(run.StepIDs) || run.Workers == nil || len(run.Workers)+len(run.Turns) > len(run.StepIDs) || run.Turns != nil && len(run.Workers) != 0 {
			return PlanHistoryPage{}, ErrProtocol
		}
		for i, turn := range run.Turns {
			if turn.StepID != run.StepIDs[i] || !turn.EntryID.Valid() || !planState(turn.State) {
				return PlanHistoryPage{}, ErrProtocol
			}
		}
		for i, worker := range run.Workers {
			if worker.StepID != run.StepIDs[i] || !worker.ThreadID.Valid() || !planState(worker.State) {
				return PlanHistoryPage{}, ErrProtocol
			}
		}
	}
	return out, nil
}
