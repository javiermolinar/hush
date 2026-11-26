package sdk

import (
	"context"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	PlanVersion    = 1
	planCapability = "plan"
	planWait       = "ext.plan.wait"
	planAttach     = "ext.plan.attach"
	planRun        = "ext.plan.run"
	planStepLimit  = 24
)

// PlanStep is the authored contract, not a run or its status.
type PlanStep struct {
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Checks      []string       `json:"checks"`
	Requires    []string       `json:"requires,omitempty"`
	After       string         `json:"after,omitempty"`
	Blocker     string         `json:"blocker,omitempty"`
	Thinking    string         `json:"thinking,omitempty"`
	Edit        *PlanFileEdit  `json:"edit,omitempty"`
	Scope       *PlanFileScope `json:"scope,omitempty"`
}

type PlanFileEdit struct {
	Path     string `json:"path"`
	Contains string `json:"contains"`
}

type PlanFileScope struct {
	Read       []string       `json:"read,omitempty"`
	Write      []string       `json:"write"`
	Acceptance PlanAcceptance `json:"acceptance"`
}

type PlanAcceptance struct {
	Label  string `json:"label"`
	Script string `json:"script"`
}

type PlanDefinition struct {
	ID       string     `json:"id"`
	Revision uint64     `json:"revision"`
	Title    string     `json:"title"`
	Steps    []PlanStep `json:"steps"`
}

type PlanProgress struct {
	StepID            string   `json:"stepId"`
	State             string   `json:"state"`
	EffectiveThinking string   `json:"effectiveThinking"`
	WorkerThreadID    ID       `json:"workerThreadId"` // Legacy name; same as initiating thread for a native turn.
	EntryID           ID       `json:"entryId,omitempty"`
	Evidence          []string `json:"evidence,omitempty"`
}

// PlanTaskReport records the agent's account of one task, not verified evidence.
type PlanTaskReport struct {
	StepID string `json:"stepId"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

type PlanRun struct {
	ID                   ID       `json:"id"`
	StepIDs              []string `json:"stepIds"`
	State                string   `json:"state"`
	OwnerThreadID        ID       `json:"ownerThreadId,omitempty"`
	Failure              string   `json:"failure,omitempty"`
	PublicationUncertain bool     `json:"publicationUncertain,omitempty"`
	Chat                 bool     `json:"chat,omitempty"`
	SubmissionID         ID       `json:"submissionId,omitempty"`
}

type PlanRunAvailability string

const (
	PlanRunDisabled PlanRunAvailability = "disabled"
	PlanRunFixture  PlanRunAvailability = "fixture"
	PlanRunScoped   PlanRunAvailability = "scoped"
	PlanRunPinned   PlanRunAvailability = "pinned"
	PlanRunUnbound  PlanRunAvailability = "unbound"
)

// PlanSnapshot is a complete committed projection. Revision is an opaque
// watch cursor, distinct from the plan definition's numbered revision.
type PlanSnapshot struct {
	Revision        string              `json:"revision"`
	Plan            *PlanDefinition     `json:"plan"`
	Progress        []PlanProgress      `json:"progress"`
	Run             *PlanRun            `json:"run"`
	RunAvailability PlanRunAvailability `json:"runAvailability"`
	Closed          bool                `json:"closed,omitempty"`
	Complete        bool                `json:"complete,omitempty"`
	Settling        bool                `json:"settling,omitempty"`
	Chat            bool                `json:"chat,omitempty"`
	Reports         []PlanTaskReport    `json:"reports,omitempty"`
}

type PlanSelection struct {
	ThreadID  ID       `json:"threadId"`
	PlanID    string   `json:"planId"`
	Revision  uint64   `json:"revision"`
	StepIDs   []string `json:"stepIds"`
	RequestID string   `json:"requestId"`
}

type PlanUpdate struct {
	Snapshot PlanSnapshot
	Err      error
}

var planFilePath = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*){0,15}$`)

func goodPlanText(s string, max int) bool {
	return len(s) <= 4*max && utf8.RuneCountInString(s) <= max && strings.TrimSpace(s) != ""
}
func planStepID(s string) bool { return len(s) <= 64 && token(s) }
func planState(s string) bool  { return s == "running" || s == "verified" || s == "failed" }
func planRunState(s string, chat bool) bool {
	if chat {
		return s == "running" || s == "done" || s == "failed" || s == "closed"
	}
	return planState(s) || s == "closed"
}
func planThinking(s string) bool {
	switch s {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}

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

func (s PlanSnapshot) valid() bool {
	if !workspaceRevision.MatchString(s.Revision) || s.Progress == nil || len(s.Progress) > planStepLimit {
		return false
	}
	p := s.Plan
	switch s.RunAvailability {
	case PlanRunDisabled, PlanRunPinned:
		if !s.Chat && (s.Run != nil || len(s.Progress) != 0) || s.RunAvailability == PlanRunPinned && (p == nil || s.Chat) {
			return false
		}
	case PlanRunFixture, PlanRunScoped, PlanRunUnbound:
	default:
		return false
	}
	if s.Chat != (s.Reports != nil) || s.Chat && s.RunAvailability == PlanRunFixture {
		return false
	}
	if p == nil {
		return len(s.Progress) == 0 && s.Run == nil && !s.Closed && !s.Complete && !s.Settling && !s.Chat
	}
	if !token(p.ID) || p.Revision < 1 || p.Revision > 1<<53-1 || !goodPlanText(p.Title, 160) || len(p.Steps) < 1 || len(p.Steps) > planStepLimit {
		return false
	}
	steps := make(map[string]bool, len(p.Steps))
	for _, step := range p.Steps {
		if !planStepID(step.ID) || steps[step.ID] || !goodPlanText(step.Title, 160) || !goodPlanText(step.Description, 1024) ||
			len(step.Checks) < 1 || len(step.Checks) > 6 || step.After != "" && !steps[step.After] ||
			step.Thinking != "" && !planThinking(step.Thinking) || step.Blocker != "" && !goodPlanText(step.Blocker, 160) ||
			len(step.Requires) != 0 && !validPlanIDs(step.Requires) {
			return false
		}
		if edit := step.Edit; edit != nil && (len(edit.Path) > 160 || !planFilePath.MatchString(edit.Path) ||
			!goodPlanText(edit.Contains, 160) || strings.ContainsRune(edit.Contains, 0)) {
			return false
		}
		if scope := step.Scope; scope != nil {
			if step.Edit != nil || len(scope.Write) < 1 || len(scope.Write) > 4 || len(scope.Read)+len(scope.Write) > 8 ||
				!goodPlanText(scope.Acceptance.Label, 120) || strings.ContainsAny(scope.Acceptance.Label, "\n\r\t") ||
				len(scope.Acceptance.Script) > 4096 || strings.TrimSpace(scope.Acceptance.Script) == "" || strings.ContainsRune(scope.Acceptance.Script, 0) {
				return false
			}
			paths := make(map[string]bool, len(scope.Read)+len(scope.Write))
			for _, set := range [][]string{scope.Read, scope.Write} {
				for _, path := range set {
					if paths[path] || len(path) > 160 || !planFilePath.MatchString(path) {
						return false
					}
					paths[path] = true
				}
			}
		}
		for _, dep := range step.Requires {
			if !steps[dep] {
				return false
			}
		}
		for _, check := range step.Checks {
			if !goodPlanText(check, 160) {
				return false
			}
		}
		steps[step.ID] = true
	}
	progress := make(map[string]bool, len(s.Progress))
	for _, pr := range s.Progress {
		if !steps[pr.StepID] || progress[pr.StepID] || !planState(pr.State) || !pr.WorkerThreadID.Valid() || pr.EntryID != "" && !pr.EntryID.Valid() || !planThinking(pr.EffectiveThinking) ||
			len(pr.Evidence) > 6 || (pr.State == "verified") != (len(pr.Evidence) > 0) {
			return false
		}
		for _, evidence := range pr.Evidence {
			if !goodPlanText(evidence, 180) {
				return false
			}
		}
		progress[pr.StepID] = true
	}
	if !validPlanReports(s.Reports, steps) {
		return false
	}
	if r := s.Run; r != nil {
		if !r.ID.Valid() || !planRunState(r.State, r.Chat) || !validPlanIDs(r.StepIDs) ||
			r.Chat && (!s.Chat || !r.SubmissionID.Valid() || r.PublicationUncertain) || !r.Chat && r.SubmissionID != "" ||
			(r.Chat || s.RunAvailability == PlanRunScoped) && !r.OwnerThreadID.Valid() ||
			r.PublicationUncertain && (r.State != "failed" || r.Failure == "") ||
			r.Failure != "" && (r.State != "failed" || !goodPlanText(r.Failure, 240)) {
			return false
		}
		for _, id := range r.StepIDs {
			if !steps[id] {
				return false
			}
		}
	}
	return true
}

// Plan reads the current plan without starting or accepting a run.
func (c *Client) Plan(ctx context.Context, thread ID) (PlanSnapshot, error) {
	if !thread.Valid() {
		return PlanSnapshot{}, ErrProtocol
	}
	out, err := metadataRead[PlanSnapshot](ctx, c, planWait, map[string]ID{"threadId": thread}, planCapability, PlanVersion)
	if err == nil && !out.valid() {
		err = ErrProtocol
	}
	return out, err
}

// WatchPlan observes committed snapshots on its own cancelable connection.
// Canceling the watch never cancels a run. Intermediate snapshots may coalesce.
func (c *Client) WatchPlan(ctx context.Context, thread ID) <-chan PlanUpdate {
	w := workspaceWatch{method: planWait, capability: planCapability, version: PlanVersion, base: planCapability, baseVersions: []uint64{PlanVersion}}
	if thread.Valid() {
		w.params = map[string]any{"threadId": thread}
	}
	return c.watchPlan(ctx, w)
}

func (c *Client) watchPlan(ctx context.Context, w workspaceWatch) <-chan PlanUpdate {
	return watchWorkspace(ctx, c, w, func(raw any) (PlanUpdate, string, error) {
		v, err := decode[PlanSnapshot](raw)
		if err != nil || !v.valid() {
			return PlanUpdate{}, "", ErrProtocol
		}
		return PlanUpdate{Snapshot: v}, v.Revision, nil
	}, func(err error) PlanUpdate { return PlanUpdate{Err: err} })
}

// AttachPlan explicitly binds a thread to a live plan in the same workspace.
// It does not admit a run or transfer a receipt from the source thread.
func (c *Client) AttachPlan(ctx context.Context, thread ID, planID string) error {
	if !thread.Valid() || !token(planID) {
		return ErrProtocol
	}
	_, err := optionalCall[struct{}](ctx, c, planCapability, PlanVersion, planAttach, map[string]string{
		"threadId": string(thread), "planId": planID,
	})
	return err
}

// RunPlan requests exactly the selected scope. After an indeterminate reply,
// retry with the same request ID and identical selection, never a new key.
func (c *Client) RunPlan(ctx context.Context, selection PlanSelection) (ID, error) {
	return c.runPlan(ctx, selection, planCapability, PlanVersion, planRun)
}

func (c *Client) runPlan(ctx context.Context, selection PlanSelection, capability string, version uint64, method string) (ID, error) {
	if !selection.ThreadID.Valid() || !token(selection.PlanID) || !token(selection.RequestID) ||
		selection.Revision < 1 || selection.Revision > 1<<53-1 || !validPlanIDs(selection.StepIDs) {
		return "", ErrProtocol
	}
	out, err := optionalCall[struct {
		RunID ID `json:"runId"`
	}](ctx, c, capability, version, method, selection)
	if err == nil && !out.RunID.Valid() {
		return "", ErrProtocol
	}
	return out.RunID, err
}
