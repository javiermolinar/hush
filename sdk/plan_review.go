package sdk

import "context"

const PlanReviewVersion = 1
const planReviewCapability = "planReview"
const planReviewWait = "ext.planReview.wait"
const planReviewStart = "ext.planReview.start"
const planReviewCancel = "ext.planReview.cancel"

// PlanReviewTarget identifies an immutable revision in the thread's workspace.
type PlanReviewTarget struct {
	ThreadID ID     `json:"threadId"`
	PlanID   string `json:"planId"`
	Revision uint64 `json:"revision"`
}

type PlanReviewChecks struct {
	Requirements string `json:"requirements"`
	Acceptance   string `json:"acceptance"`
	Dependencies string `json:"dependencies"`
}

type PlanReviewFinding struct {
	Severity string `json:"severity"`
	Area     string `json:"area"`
	StepID   string `json:"stepId,omitempty"`
	Message  string `json:"message"`
}

type PlanReviewReport struct {
	Summary     string              `json:"summary"`
	Checks      PlanReviewChecks    `json:"checks"`
	Findings    []PlanReviewFinding `json:"findings"`
	Limitations []string            `json:"limitations"`
}

type PlanReviewRecord struct {
	ID               ID                `json:"id"`
	ThreadID         ID                `json:"threadId"`
	ReviewerThreadID ID                `json:"reviewerThreadId,omitempty"`
	Model            Model             `json:"model"`
	Thinking         string            `json:"thinking"`
	State            string            `json:"state"`
	Report           *PlanReviewReport `json:"report,omitempty"`
	Failure          string            `json:"failure,omitempty"`
}

type PlanReviewView struct {
	Title        string            `json:"title"`
	Revision     uint64            `json:"revision"`
	HeadRevision uint64            `json:"headRevision"`
	Source       string            `json:"source"`
	CanStart     bool              `json:"canStart"`
	Review       *PlanReviewRecord `json:"review"`
}

type PlanReviewUpdate struct {
	View PlanReviewView
	Err  error
}

func (q PlanReviewTarget) valid() bool {
	return q.ThreadID.Valid() && token(q.PlanID) && q.Revision > 0 && q.Revision <= 1<<53-1
}

func (v PlanReviewView) valid(revision uint64) bool {
	if !goodPlanText(v.Title, 160) || v.Revision != revision || v.HeadRevision < v.Revision || v.HeadRevision > 1<<53-1 ||
		v.Source != "captured" && v.Source != "partial" && v.Source != "missing" {
		return false
	}
	r := v.Review
	if v.CanStart && (v.HeadRevision != v.Revision || r != nil && r.State != "failed") {
		return false
	}
	if r == nil {
		return true
	}
	if !r.ID.Valid() || !r.ThreadID.Valid() || r.ReviewerThreadID != "" && (!r.ReviewerThreadID.Valid() || r.ReviewerThreadID == r.ThreadID) ||
		!goodPlanText(r.Model.Provider, 256) || !goodPlanText(r.Model.ModelID, 256) || !planThinking(r.Thinking) {
		return false
	}
	switch r.State {
	case "running":
		return r.Report == nil && r.Failure == ""
	case "failed":
		return r.Report == nil && goodPlanText(r.Failure, 240)
	case "complete":
		return r.ReviewerThreadID.Valid() && r.Failure == "" && r.Report != nil && r.Report.valid(v.Source)
	}
	return false
}

func (r PlanReviewReport) valid(source string) bool {
	if !goodPlanText(r.Summary, 400) || r.Findings == nil || len(r.Findings) > 8 || len(r.Limitations) < 1 || len(r.Limitations) > 6 ||
		source != "captured" && r.Checks.Requirements != "unverified" {
		return false
	}
	for _, check := range [...]string{r.Checks.Requirements, r.Checks.Acceptance, r.Checks.Dependencies} {
		if check != "addressed" && check != "concern" && check != "unverified" {
			return false
		}
	}
	for _, f := range r.Findings {
		if f.Severity != "blocking" && f.Severity != "advisory" ||
			f.Area != "requirements" && f.Area != "acceptance" && f.Area != "dependencies" ||
			f.StepID != "" && !planStepID(f.StepID) || !goodPlanText(f.Message, 400) {
			return false
		}
	}
	for _, limit := range r.Limitations {
		if !goodPlanText(limit, 400) {
			return false
		}
	}
	return true
}

// WatchPlanReview observes one revision's latest review without admitting work.
func (c *Client) WatchPlanReview(ctx context.Context, q PlanReviewTarget) <-chan PlanReviewUpdate {
	w := workspaceWatch{method: planReviewWait, capability: planReviewCapability, version: PlanReviewVersion,
		base: planWorkspace, baseVersions: []uint64{PlanWorkspaceVersion}}
	if q.valid() {
		w.params = map[string]any{"threadId": q.ThreadID, "planId": q.PlanID, "revision": q.Revision}
	}
	return watchWorkspace(ctx, c, w, func(raw any) (PlanReviewUpdate, string, error) {
		v, err := decode[struct {
			PlanReviewView
			Cursor string `json:"cursor"`
		}](raw)
		if err != nil || !v.valid(q.Revision) {
			return PlanReviewUpdate{}, "", ErrProtocol
		}
		return PlanReviewUpdate{View: v.PlanReviewView}, v.Cursor, nil
	}, func(err error) PlanReviewUpdate { return PlanReviewUpdate{Err: err} })
}

// StartPlanReview explicitly requests an advisory review of the current head.
// Retry a lost response with the same target and request ID.
func (c *Client) StartPlanReview(ctx context.Context, q PlanReviewTarget, requestID string) (ID, error) {
	if !q.valid() || !token(requestID) {
		return "", ErrProtocol
	}
	v, err := optionalCall[struct {
		ReviewID ID `json:"reviewId"`
	}](ctx, c, planReviewCapability, PlanReviewVersion, planReviewStart, struct {
		PlanReviewTarget
		RequestID string `json:"requestId"`
	}{q, requestID})
	if err == nil && !v.ReviewID.Valid() {
		err = ErrProtocol
	}
	return v.ReviewID, err
}

// CancelPlanReview stops only the named review started by q.ThreadID.
func (c *Client) CancelPlanReview(ctx context.Context, q PlanReviewTarget, reviewID ID) error {
	if !q.valid() || !reviewID.Valid() {
		return ErrProtocol
	}
	_, err := optionalCall[struct{}](ctx, c, planReviewCapability, PlanReviewVersion, planReviewCancel, struct {
		PlanReviewTarget
		ReviewID ID `json:"reviewId"`
	}{q, reviewID})
	return err
}
