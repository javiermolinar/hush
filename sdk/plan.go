package sdk

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	PlanVersion       = 2
	planCapability    = "plan"
	planList          = "ext.plan.list"
	planGet           = "ext.plan.get"
	planHistory       = "ext.plan.history"
	planClose         = "ext.plan.close"
	planActivity      = "ext.plan.activity"
	planRun           = "ext.plan.run"
	planRunInWorktree = "ext.plan.runInWorktree"
	planStepLimit     = 24
	planListPageSize  = 48
	planPromptLimit   = 4096
)

var planMethods = []string{planList, planGet, planHistory, planClose, planActivity, planRun, planRunInWorktree}

// PlanDelegate is a step's subagent specification. The run's model passes
// only the step ID; the backend fills these fields from the pinned revision.
type PlanDelegate struct {
	ReadOnly *bool  `json:"read_only,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	PWD      string `json:"pwd,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// Writes reports whether a subagent for this step may edit files.
func (d *PlanDelegate) Writes() bool { return d != nil && d.ReadOnly != nil && !*d.ReadOnly }

// PlanStep is a step of the current revision with its status. Done is final;
// blocked carries a reason and can be selected again.
type PlanStep struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	Prompt   string        `json:"prompt"`
	Requires []string      `json:"requires,omitempty"`
	Delegate *PlanDelegate `json:"delegate,omitempty"`
	Status   string        `json:"status"`
	Reason   string        `json:"reason,omitempty"`
}

// PlanActiveRun is a run whose thread turn has not settled. A step is in at
// most one active run.
type PlanActiveRun struct {
	ID       ID       `json:"id"`
	ThreadID ID       `json:"threadId"`
	StepIDs  []string `json:"stepIds"`
}

type Plan struct {
	ID        string          `json:"id"`
	Revision  uint64          `json:"revision"`
	Title     string          `json:"title"`
	Workspace string          `json:"workspace"`
	Steps     []PlanStep      `json:"steps"`
	Runs      []PlanActiveRun `json:"runs"`
	Closed    bool            `json:"closed,omitempty"`
	Complete  bool            `json:"complete,omitempty"`
}

// Running reports the active run working on a step, if any.
func (p *Plan) Running(step string) *PlanActiveRun {
	for i := range p.Runs {
		if slices.Contains(p.Runs[i].StepIDs, step) {
			return &p.Runs[i]
		}
	}
	return nil
}

// PlanSnapshot is a committed plan. Revision is an opaque watch cursor,
// distinct from the plan's numbered revision.
type PlanSnapshot struct {
	Revision string `json:"revision"`
	Plan     Plan   `json:"plan"`
}

type PlanUpdate struct {
	Snapshot PlanSnapshot
	Err      error
}

// PlanSummary is one row of the plan list, across workspaces.
type PlanSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Revision  uint64 `json:"revision"`
	Workspace string `json:"workspace"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Blocked   int    `json:"blocked"`
	Running   bool   `json:"running"`
	Closed    bool   `json:"closed"`
}

type PlanListUpdate struct {
	Plans []PlanSummary
	More  bool
	Err   error
}

// PlanOutcome is what a run's turn reported for one selected step.
type PlanOutcome struct {
	StepID string `json:"stepId"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type PlanRunRecord struct {
	ID           ID            `json:"id"`
	Revision     uint64        `json:"revision"`
	ThreadID     ID            `json:"threadId"`
	StepIDs      []string      `json:"stepIds"`
	State        string        `json:"state"`
	Outcomes     []PlanOutcome `json:"outcomes"`
	SubmissionID ID            `json:"submissionId,omitempty"`
	EntryID      ID            `json:"entryId,omitempty"`
	Failure      string        `json:"failure,omitempty"`
	Worktree     bool          `json:"worktree,omitempty"`
}

type PlanHistoryPage struct {
	Runs []PlanRunRecord `json:"runs"`
	Next ID              `json:"next"`
}

type PlanActivityStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// PlanActivityRun is a thread's oldest active run, or else its last one.
type PlanActivityRun struct {
	PlanRunRecord
	PlanID string             `json:"planId"`
	Title  string             `json:"title"`
	Steps  []PlanActivityStep `json:"steps"`
}

type PlanActivityUpdate struct {
	Run *PlanActivityRun
	Err error
}

// PlanSelection names steps of one plan revision. A retry after an
// indeterminate reply reuses the request ID with the same selection.
type PlanSelection struct {
	PlanID    string   `json:"planId"`
	Revision  uint64   `json:"revision"`
	StepIDs   []string `json:"stepIds"`
	RequestID string   `json:"requestId"`
}

// PlanWorktreeSource is a thread, or a launch directory with an optional
// staged model and thinking level (supplied together).
type PlanWorktreeSource struct {
	Thread        ID
	CWD           string
	Model         *Model
	ThinkingLevel string
}

// PlanWorktreeRun is the run and the new thread and checkout that hold it.
type PlanWorktreeRun struct {
	RunID    ID     `json:"runId"`
	ThreadID ID     `json:"threadId"`
	Title    string `json:"title"`
	Checkout struct {
		CWD    string `json:"cwd"` // execution and trust directory, possibly inside Root
		Root   string `json:"root"`
		Branch string `json:"branch"`
	} `json:"checkout"`
}

var planWorktreeBranch = regexp.MustCompile(`^hush/([a-z0-9][a-z0-9-]{0,39}-)?[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func (w PlanWorktreeRun) valid() bool {
	cwd, root := w.Checkout.CWD, w.Checkout.Root
	return w.RunID.Valid() && w.ThreadID.Valid() && planPath(cwd) && planPath(root) && cwd == path.Clean(cwd) && root == path.Clean(root) &&
		(cwd == root || strings.HasPrefix(cwd, root+"/")) && planWorktreeBranch.MatchString(w.Checkout.Branch) &&
		path.Base(root) == strings.TrimPrefix(w.Checkout.Branch, "hush/") && singleLine(w.Title, 96)
}

func goodPlanText(s string, max int) bool {
	return len(s) <= 4*max && utf8.RuneCountInString(s) <= max && strings.TrimSpace(s) != ""
}

func singleLine(s string, max int) bool {
	return goodPlanText(s, max) && !strings.ContainsAny(s, "\u2028\u2029") && strings.IndexFunc(s, unicode.IsControl) < 0
}

func planPath(p string) bool {
	return path.IsAbs(p) && len(p) <= 4096 && utf8.ValidString(p) && strings.IndexFunc(p, unicode.IsControl) < 0
}

func planStepID(s string) bool { return len(s) <= 64 && token(s) }

func validPlanIDs(ids []string) bool {
	if len(ids) < 1 || len(ids) > planStepLimit {
		return false
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !planStepID(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func planStatus(s string) bool { return s == "pending" || s == "done" || s == "blocked" }

func validOutcomes(outcomes []PlanOutcome, selected []string) bool {
	seen := make(map[string]bool, len(outcomes))
	for _, o := range outcomes {
		if !slices.Contains(selected, o.StepID) || seen[o.StepID] || o.Status != "done" && o.Status != "blocked" || len(o.Detail) > 4*180 {
			return false
		}
		seen[o.StepID] = true
	}
	return outcomes != nil
}

func (r PlanRunRecord) valid() bool {
	return r.ID.Valid() && r.Revision >= 1 && r.ThreadID.Valid() && validPlanIDs(r.StepIDs) && validOutcomes(r.Outcomes, r.StepIDs) &&
		(r.State == "running" || r.State == "finished" || r.State == "failed") && (r.SubmissionID == "" || r.SubmissionID.Valid()) &&
		(r.EntryID == "" || r.EntryID.Valid()) && (r.Failure == "" || r.State == "failed" && goodPlanText(r.Failure, 240))
}

func (p Plan) valid() bool {
	if !token(p.ID) || p.Revision < 1 || p.Revision > 1<<53-1 || !goodPlanText(p.Title, 160) || !planPath(p.Workspace) ||
		len(p.Steps) < 1 || len(p.Steps) > planStepLimit || p.Runs == nil {
		return false
	}
	steps := make(map[string]bool, len(p.Steps))
	done := 0
	for _, s := range p.Steps {
		if !planStepID(s.ID) || steps[s.ID] || !goodPlanText(s.Title, 160) || !goodPlanText(s.Prompt, planPromptLimit) || !planStatus(s.Status) ||
			s.Reason != "" && (s.Status != "blocked" || !goodPlanText(s.Reason, 180)) {
			return false
		}
		for _, dep := range s.Requires {
			if !steps[dep] {
				return false
			}
		}
		if d := s.Delegate; d != nil && (len(d.Worktree) > 255 || len(d.PWD) > 1024 || d.Thinking != "" && !slices.Contains(catalogThinking, d.Thinking)) {
			return false
		}
		if s.Status == "done" {
			done++
		}
		steps[s.ID] = true
	}
	claimed := make(map[string]bool)
	for _, r := range p.Runs {
		if !r.ID.Valid() || !r.ThreadID.Valid() || !validPlanIDs(r.StepIDs) {
			return false
		}
		for _, id := range r.StepIDs {
			if claimed[id] {
				return false // a step is in at most one active run
			}
			claimed[id] = true
		}
	}
	return p.Complete == (done == len(p.Steps))
}

func validSummary(p PlanSummary) bool {
	return token(p.ID) && goodPlanText(p.Title, 160) && p.Revision >= 1 && p.Revision <= 1<<53-1 && planPath(p.Workspace) &&
		p.Total >= 1 && p.Total <= planStepLimit && p.Done >= 0 && p.Blocked >= 0 && p.Done+p.Blocked <= p.Total
}

func (r *PlanActivityRun) valid() bool {
	if !r.PlanRunRecord.valid() || !token(r.PlanID) || !goodPlanText(r.Title, 160) || len(r.Steps) > len(r.StepIDs) {
		return false
	}
	for _, s := range r.Steps {
		if !slices.Contains(r.StepIDs, s.ID) || !goodPlanText(s.Title, 160) || !planStatus(s.Status) {
			return false
		}
	}
	return true
}

func planWatch(method string, params map[string]any) workspaceWatch {
	return workspaceWatch{method: method, capability: planCapability, version: PlanVersion, base: planCapability,
		baseVersions: []uint64{PlanVersion}, params: params}
}

// WatchPlans publishes one page of plans across workspaces, the trusted
// directory's own plans first. Open plans come before closed ones.
func (c *Client) WatchPlans(ctx context.Context, cwd string, page int) <-chan PlanListUpdate {
	var params map[string]any
	if planPath(cwd) && page >= 0 && page <= 1_000_000 {
		params = map[string]any{"cwd": cwd, "page": page}
	}
	type report struct {
		Revision string        `json:"revision"`
		Plans    []PlanSummary `json:"plans"`
		More     bool          `json:"more"`
	}
	return watchWorkspace(ctx, c, planWatch(planList, params), func(raw any) (PlanListUpdate, string, error) {
		v, err := decode[report](raw)
		if err != nil || !workspaceRevision.MatchString(v.Revision) || v.Plans == nil || len(v.Plans) > planListPageSize {
			return PlanListUpdate{}, "", ErrProtocol
		}
		seen := make(map[string]bool, len(v.Plans))
		for _, p := range v.Plans {
			if !validSummary(p) || seen[p.ID] {
				return PlanListUpdate{}, "", ErrProtocol
			}
			seen[p.ID] = true
		}
		return PlanListUpdate{Plans: v.Plans, More: v.More}, v.Revision, nil
	}, func(err error) PlanListUpdate { return PlanListUpdate{Err: err} })
}

// WatchPlan observes one plan, which may belong to another workspace. The
// directory only authorizes the read. Canceling the watch never stops a run.
func (c *Client) WatchPlan(ctx context.Context, cwd, planID string) <-chan PlanUpdate {
	var params map[string]any
	if planPath(cwd) && token(planID) {
		params = map[string]any{"cwd": cwd, "planId": planID}
	}
	return watchWorkspace(ctx, c, planWatch(planGet, params), func(raw any) (PlanUpdate, string, error) {
		v, err := decode[PlanSnapshot](raw)
		if err != nil || !workspaceRevision.MatchString(v.Revision) || v.Plan.ID != planID || !v.Plan.valid() {
			return PlanUpdate{}, "", ErrProtocol
		}
		return PlanUpdate{Snapshot: v}, v.Revision, nil
	}, func(err error) PlanUpdate { return PlanUpdate{Err: err} })
}

// WatchPlanActivity follows the thread's oldest active run, or else its
// last one. A fork starts with none.
func (c *Client) WatchPlanActivity(ctx context.Context, thread ID) <-chan PlanActivityUpdate {
	var params map[string]any
	if thread.Valid() {
		params = map[string]any{"threadId": thread}
	}
	type report struct {
		Revision string           `json:"revision"`
		Run      *PlanActivityRun `json:"run"`
	}
	return watchWorkspace(ctx, c, planWatch(planActivity, params), func(raw any) (PlanActivityUpdate, string, error) {
		v, err := decode[report](raw)
		if err != nil || !workspaceRevision.MatchString(v.Revision) || v.Run != nil && (!v.Run.valid() || v.Run.ThreadID != thread) {
			return PlanActivityUpdate{}, "", ErrProtocol
		}
		return PlanActivityUpdate{Run: v.Run}, v.Revision, nil
	}, func(err error) PlanActivityUpdate { return PlanActivityUpdate{Err: err} })
}

// PlanHistory pages a plan's runs, newest first.
func (c *Client) PlanHistory(ctx context.Context, cwd, planID string, before ID) (PlanHistoryPage, error) {
	if !planPath(cwd) || !token(planID) || before != "" && !before.Valid() {
		return PlanHistoryPage{}, ErrProtocol
	}
	params := map[string]any{"cwd": cwd, "planId": planID}
	if before != "" {
		params["before"] = before
	}
	// ID rejects null, so the page cursor decodes through a pointer.
	wire, err := optionalCall[struct {
		Runs []PlanRunRecord `json:"runs"`
		Next *ID             `json:"next"`
	}](ctx, c, planCapability, PlanVersion, planHistory, params)
	if err != nil {
		return PlanHistoryPage{}, err
	}
	out := PlanHistoryPage{Runs: wire.Runs}
	if wire.Next != nil {
		out.Next = *wire.Next
	}
	if out.Runs == nil || len(out.Runs) > 12 || out.Next != "" && (len(out.Runs) == 0 || out.Runs[len(out.Runs)-1].ID != out.Next) {
		return PlanHistoryPage{}, ErrProtocol
	}
	for _, r := range out.Runs {
		if !r.valid() {
			return PlanHistoryPage{}, ErrProtocol
		}
	}
	return out, nil
}

// SetPlanClosed closes or reopens a plan of the directory's workspace. A plan
// with an active run cannot close.
func (c *Client) SetPlanClosed(ctx context.Context, cwd, planID string, closed bool) error {
	if !planPath(cwd) || !token(planID) {
		return ErrProtocol
	}
	_, err := optionalCall[struct{}](ctx, c, planCapability, PlanVersion, planClose, map[string]any{"cwd": cwd, "planId": planID, "closed": closed})
	return err
}

func (s PlanSelection) valid() bool {
	return token(s.PlanID) && token(s.RequestID) && s.Revision >= 1 && s.Revision <= 1<<53-1 && validPlanIDs(s.StepIDs)
}

// RunPlan submits one turn for the selected steps in a thread of the plan's
// workspace. It queues behind a busy turn.
func (c *Client) RunPlan(ctx context.Context, thread ID, selection PlanSelection) (ID, error) {
	if !thread.Valid() || !selection.valid() {
		return "", ErrProtocol
	}
	out, err := optionalCall[struct {
		RunID ID `json:"runId"`
	}](ctx, c, planCapability, PlanVersion, planRun, struct {
		ThreadID ID `json:"threadId"`
		PlanSelection
	}{thread, selection})
	if err == nil && !out.RunID.Valid() {
		return "", ErrProtocol
	}
	return out.RunID, err
}

// RunPlanInWorktree creates a checkout from committed HEAD, a thread in it
// and the run, in one call. Retry an uncertain reply with the same request.
func (c *Client) RunPlanInWorktree(ctx context.Context, source PlanWorktreeSource, selection PlanSelection) (PlanWorktreeRun, error) {
	if !selection.valid() || source.Thread != "" && (!source.Thread.Valid() || source.CWD != "" || source.Model != nil) ||
		source.Thread == "" && (!planPath(source.CWD) || (source.Model == nil) != (source.ThinkingLevel == "") ||
			source.Model != nil && (!validCatalogModel(*source.Model) || !slices.Contains(catalogThinking, source.ThinkingLevel))) {
		return PlanWorktreeRun{}, ErrProtocol
	}
	type params struct {
		ThreadID      ID     `json:"threadId,omitempty"`
		CWD           string `json:"cwd,omitempty"`
		Model         *Model `json:"model,omitempty"`
		ThinkingLevel string `json:"thinkingLevel,omitempty"`
		PlanSelection
	}
	out, err := optionalCall[PlanWorktreeRun](ctx, c, planCapability, PlanVersion, planRunInWorktree,
		params{source.Thread, source.CWD, source.Model, source.ThinkingLevel, selection})
	if err == nil && !out.valid() {
		err = ErrProtocol
	}
	var transport *TransportError
	if errors.Is(err, ErrProtocol) && !errors.As(err, &transport) {
		return out, &TransportError{Err: err, Indeterminate: true} // a malformed reply is not a rejection
	}
	return out, err
}
