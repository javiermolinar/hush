package sdk

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

const DelegationVersion = 1
const ReviewEvidenceVersion = 1
const reviewEvidenceCapability = "reviewEvidence"
const ReviewNoticeKind = "hush.worker.notice"
const delegationCapability = "delegation"
const reviewStart = "ext.delegation.start"
const reviewGet = "ext.delegation.get"
const reviewStop = "ext.delegation.stop"

// ReviewScope names exact files relative to the parent thread's backend cwd.
// Untracked files are included only when named separately.
type ReviewScope struct {
	Tracked   []string `json:"tracked"`
	Untracked []string `json:"untracked"`
}

type ReviewAssignment struct {
	Name  string `json:"name"`
	Brief string `json:"brief"`
}

type ReviewSelection struct {
	Model    Model  `json:"model"`
	Thinking string `json:"thinking"`
}

type ReviewCheck struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

type SuppliedReviewEvidence struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// ReviewAssessment pins acceptance checks and labeled excerpts supplied by the
// caller. They are input data, not verified results or executable instructions.
type ReviewAssessment struct {
	Checks   []ReviewCheck            `json:"checks,omitempty"`
	Evidence []SuppliedReviewEvidence `json:"evidence,omitempty"`
}

// ReviewLaunch is explicit code-review admission. An optional Prompt starts
// one native parent turn whose spawn tool can release only these assignments.
// Without Prompt the assignments run without waking the parent model.
type ReviewLaunch struct {
	ThreadID        ID                 `json:"threadId"`
	RequestID       string             `json:"requestId"`
	Scope           ReviewScope        `json:"scope"`
	Assignments     []ReviewAssignment `json:"assignments"`
	Mode            string             `json:"mode,omitempty"`
	Selection       *ReviewSelection   `json:"selection,omitempty"`
	Prompt          string             `json:"prompt,omitempty"`
	WorkerTimeoutMS int                `json:"workerTimeoutMs,omitempty"`
	Assessment      *ReviewAssessment  `json:"assessment,omitempty"`
}

type ReviewHandle struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
}

type ReviewReceipt struct {
	ThreadID     ID              `json:"threadId"`
	RequestID    string          `json:"requestId"`
	SnapshotID   string          `json:"snapshotId"`
	Base         string          `json:"base"`
	Mode         string          `json:"mode"`
	Selection    ReviewSelection `json:"selection"`
	Workers      []ReviewHandle  `json:"workers"`
	SubmissionID ID              `json:"submissionId,omitempty"`
}

type ReviewWorker struct {
	ReviewHandle
	State    string `json:"state"`
	Report   string `json:"report,omitempty"`
	Detail   string `json:"detail,omitempty"`
	NoticeID ID     `json:"noticeId,omitempty"`
}

type ReviewView struct {
	Receipt ReviewReceipt  `json:"receipt"`
	Workers []ReviewWorker `json:"workers"`
	Target  string         `json:"target"`
}

func reviewText(s string, limit int, multiline bool) bool {
	return strings.TrimSpace(s) != "" && len(s) <= limit && utf8.ValidString(s) && strings.IndexFunc(s, func(r rune) bool {
		return (unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t'))) || r == '\u2028' || r == '\u2029' ||
			r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
	}) < 0
}

func reviewHex(s string, n int) bool {
	return len(s) == n && strings.IndexFunc(s, func(r rune) bool { return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') }) < 0
}

func (s ReviewSelection) valid() bool {
	return reviewText(s.Model.Provider, 256, false) && reviewText(s.Model.ModelID, 256, false) && planThinking(s.Thinking)
}

func reviewEvidenceID(s string) bool {
	return len(s) >= 1 && len(s) <= 32 && s[0] >= 'a' && s[0] <= 'z' && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) < 0
}

func (a ReviewAssessment) valid() bool {
	if len(a.Checks) > 6 || len(a.Evidence) > 4 {
		return false
	}
	checks, evidence := map[string]bool{}, map[string]bool{}
	for _, c := range a.Checks {
		if !reviewEvidenceID(c.ID) || checks[c.ID] || !reviewText(c.Description, 160, false) {
			return false
		}
		checks[c.ID] = true
	}
	for _, e := range a.Evidence {
		if !reviewEvidenceID(e.ID) || evidence[e.ID] || !reviewText(e.Label, 80, false) || !reviewText(e.Text, 640, true) {
			return false
		}
		evidence[e.ID] = true
	}
	return true
}

func (s ReviewScope) valid() bool {
	if n := len(s.Tracked) + len(s.Untracked); n < 1 || n > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, list := range [][]string{s.Tracked, s.Untracked} {
		for _, p := range list {
			if seen[p] || !reviewText(p, 1024, false) {
				return false
			}
			seen[p] = true
		}
	}
	return true
}

func (q ReviewLaunch) valid() bool {
	if !q.ThreadID.Valid() || !token(q.RequestID) || !q.Scope.valid() || len(q.Assignments) < 1 || len(q.Assignments) > 2 ||
		q.Mode != "" && q.Mode != "sequential" && q.Mode != "parallel" || q.Prompt != "" && !reviewText(q.Prompt, 2048, true) ||
		q.Selection != nil && !q.Selection.valid() || q.WorkerTimeoutMS != 0 && (q.WorkerTimeoutMS < 100 || q.WorkerTimeoutMS > 180000) ||
		q.Assessment != nil && !q.Assessment.valid() {
		return false
	}
	for i, a := range q.Assignments {
		if !reviewText(a.Name, 64, false) || !reviewText(a.Brief, 2048, true) || i > 0 && a.Name == q.Assignments[0].Name {
			return false
		}
	}
	return true
}

func (r ReviewReceipt) valid(thread ID, request string) bool {
	if r.ThreadID != thread || r.RequestID != request || !reviewHex(r.SnapshotID, 64) || (!reviewHex(r.Base, 40) && !reviewHex(r.Base, 64)) ||
		(r.Mode != "sequential" && r.Mode != "parallel") || !r.Selection.valid() || len(r.Workers) < 1 || len(r.Workers) > 2 ||
		r.SubmissionID != "" && !r.SubmissionID.Valid() {
		return false
	}
	for i, w := range r.Workers {
		if !reviewHex(w.Handle, 32) || !reviewText(w.Name, 64, false) || i > 0 && (w.Handle == r.Workers[0].Handle || w.Name == r.Workers[0].Name) {
			return false
		}
	}
	return true
}

func (w ReviewWorker) valid() bool {
	if !reviewHex(w.Handle, 32) || !reviewText(w.Name, 64, false) {
		return false
	}
	switch w.State {
	case "reserved", "running", "stopping":
		return w.NoticeID == "" && w.Report == "" && w.Detail == ""
	case "unresolved":
		return w.NoticeID == "" && w.Report == "" && reviewText(w.Detail, 4096, true)
	case "succeeded":
		return w.NoticeID.Valid() && reviewText(w.Report, 4096, true) && w.Detail == ""
	case "failed", "timed_out", "cancelled":
		return w.NoticeID.Valid() && w.Report == "" && reviewText(w.Detail, 4096, true)
	}
	return false
}

// StartCodeReview returns durable admission, not completion. Retry a lost
// reply using the same parent, request ID and exact launch payload.
func (c *Client) StartCodeReview(ctx context.Context, q ReviewLaunch) (ReviewReceipt, error) {
	if !q.valid() {
		return ReviewReceipt{}, ErrProtocol
	}
	if q.Scope.Tracked == nil {
		q.Scope.Tracked = []string{}
	}
	if q.Scope.Untracked == nil {
		q.Scope.Untracked = []string{}
	}
	capability, version := delegationCapability, uint64(DelegationVersion)
	if q.Assessment != nil {
		capability, version = reviewEvidenceCapability, ReviewEvidenceVersion
	}
	r, err := optionalCall[ReviewReceipt](ctx, c, capability, version, reviewStart, q)
	if err != nil {
		return r, err
	}
	mode := q.Mode
	if mode == "" {
		mode = "sequential"
	}
	if !r.valid(q.ThreadID, q.RequestID) || len(r.Workers) != len(q.Assignments) || r.Mode != mode ||
		(q.Prompt == "") != (r.SubmissionID == "") || (q.Selection != nil && r.Selection != *q.Selection) {
		return ReviewReceipt{}, ErrProtocol
	}
	for i, w := range r.Workers {
		if w.Name != q.Assignments[i].Name {
			return ReviewReceipt{}, ErrProtocol
		}
	}
	return r, nil
}

// CodeReview reads the parent-scoped receipt and current status. It does not
// launch, resume, or approve work. Use the parent's feed for committed notices.
func (c *Client) CodeReview(ctx context.Context, thread ID, requestID string) (ReviewView, error) {
	if !thread.Valid() || !token(requestID) {
		return ReviewView{}, ErrProtocol
	}
	v, err := optionalCall[ReviewView](ctx, c, delegationCapability, DelegationVersion, reviewGet,
		map[string]any{"threadId": thread, "requestId": requestID})
	if err != nil {
		return v, err
	}
	if !v.Receipt.valid(thread, requestID) || len(v.Workers) != len(v.Receipt.Workers) ||
		!validReviewTarget(v.Target) {
		return ReviewView{}, ErrProtocol
	}
	for i, w := range v.Workers {
		if !w.valid() || w.ReviewHandle != v.Receipt.Workers[i] {
			return ReviewView{}, ErrProtocol
		}
	}
	return v, nil
}

// StopReviewWorker addresses one worker. A stopping receipt is not evidence
// that the worker or its owned provider request has terminated.
func (c *Client) StopReviewWorker(ctx context.Context, thread ID, handle string) (ReviewWorker, error) {
	if !thread.Valid() || !reviewHex(handle, 32) {
		return ReviewWorker{}, ErrProtocol
	}
	v, err := optionalCall[struct {
		Worker ReviewWorker `json:"worker"`
	}](ctx, c, delegationCapability, DelegationVersion, reviewStop, map[string]any{"threadId": thread, "handle": handle})
	if err != nil {
		return ReviewWorker{}, err
	}
	if !v.Worker.valid() || v.Worker.Handle != handle {
		return ReviewWorker{}, ErrProtocol
	}
	return v.Worker, nil
}
