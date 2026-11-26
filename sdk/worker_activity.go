package sdk

import (
	"context"
	"slices"
)

const WorkerActivityVersion = 1
const workerActivityCapability = "workerActivity"
const workerActivityWait = "ext.workerActivity.wait"
const workerActivityInspect = "ext.workerActivity.inspect"

type ReviewWorkerActivity struct {
	ReviewHandle
	State    string `json:"state"`
	Activity string `json:"activity"`
}

type ReviewActivityUpdate struct {
	Workers []ReviewWorkerActivity
	Err     error
}

type ReviewTranscriptEntry struct {
	ID   ID     `json:"id"`
	Kind string `json:"kind"` // reviewer text or public activity, never reasoning
	Text string `json:"text"`
}

type ReviewTranscript struct {
	Entries   []ReviewTranscriptEntry `json:"entries"`
	Truncated bool                    `json:"truncated"`
}

type ReviewInspection struct {
	Receipt    ReviewReceipt     `json:"receipt"`
	Worker     ReviewWorker      `json:"worker"`
	Brief      string            `json:"brief"`
	Scope      ReviewScope       `json:"scope"`
	Target     string            `json:"target"`
	Assessment *ReviewAssessment `json:"assessment,omitempty"`
	Transcript *ReviewTranscript `json:"transcript,omitempty"`
}

func validReviewTarget(s string) bool {
	return s == "current" || s == "stale" || s == "unavailable"
}

func reviewActivityState(s string) bool {
	switch s {
	case "reserved", "running", "stopping", "unresolved", "succeeded", "failed", "cancelled", "timed_out":
		return true
	}
	return false
}

// WatchReviewActivity follows one parent. Each value is a complete replacement
// roster. Retained rows are not notifications. On reconnect the watch baselines
// again; ordered, committed review notices belong to the native parent feed.
func (c *Client) WatchReviewActivity(ctx context.Context, thread ID) <-chan ReviewActivityUpdate {
	w := workspaceWatch{method: workerActivityWait, capability: workerActivityCapability, version: WorkerActivityVersion,
		base: delegationCapability, baseVersions: []uint64{DelegationVersion}}
	if thread.Valid() {
		w.params = map[string]any{"threadId": thread}
	}
	return watchWorkspace(ctx, c, w, func(raw any) (ReviewActivityUpdate, string, error) {
		v, err := decode[struct {
			ThreadID ID                     `json:"threadId"`
			Cursor   string                 `json:"cursor"`
			Workers  []ReviewWorkerActivity `json:"workers"`
		}](raw)
		if err != nil {
			return ReviewActivityUpdate{}, "", err
		}
		if v.ThreadID != thread || v.Workers == nil || len(v.Workers) > 128 {
			return ReviewActivityUpdate{}, "", ErrProtocol
		}
		seen := make(map[string]bool, len(v.Workers))
		for _, worker := range v.Workers {
			if !reviewHex(worker.Handle, 32) || seen[worker.Handle] || !reviewText(worker.Name, 64, false) ||
				!reviewActivityState(worker.State) || (worker.Activity != "" && (worker.State != "running" || !reviewText(worker.Activity, 384, false))) {
				return ReviewActivityUpdate{}, "", ErrProtocol
			}
			seen[worker.Handle] = true
		}
		return ReviewActivityUpdate{Workers: v.Workers}, v.Cursor, nil
	}, func(err error) ReviewActivityUpdate { return ReviewActivityUpdate{Err: err} })
}

// InspectReviewWorker reads one dossier for the named parent and handle. With
// transcript set it includes the latest bounded public child entries. It does
// not expose a child thread ID or permit entering ordinary child chat.
func (c *Client) InspectReviewWorker(ctx context.Context, thread ID, handle string, transcript bool) (ReviewInspection, error) {
	if !thread.Valid() || !reviewHex(handle, 32) {
		return ReviewInspection{}, ErrProtocol
	}
	v, err := optionalCall[ReviewInspection](ctx, c, workerActivityCapability, WorkerActivityVersion, workerActivityInspect,
		map[string]any{"threadId": thread, "handle": handle, "transcript": transcript})
	if err != nil {
		return v, err
	}
	if !token(v.Receipt.RequestID) || !v.Receipt.valid(thread, v.Receipt.RequestID) || !v.Worker.valid() || v.Worker.Handle != handle ||
		!slices.Contains(v.Receipt.Workers, v.Worker.ReviewHandle) || !reviewText(v.Brief, 2048, true) || !v.Scope.valid() ||
		!validReviewTarget(v.Target) || v.Assessment != nil && !v.Assessment.valid() || transcript != (v.Transcript != nil) {
		return ReviewInspection{}, ErrProtocol
	}
	if v.Transcript != nil {
		if v.Transcript.Entries == nil || len(v.Transcript.Entries) > 32 || v.Worker.State == "unresolved" {
			return ReviewInspection{}, ErrProtocol
		}
		var prior ID
		for _, e := range v.Transcript.Entries {
			if !e.ID.Valid() || (e.Kind != "reviewer" && e.Kind != "activity") || !reviewText(e.Text, 1600, true) ||
				len(prior) > len(e.ID) || len(prior) == len(e.ID) && prior >= e.ID {
				return ReviewInspection{}, ErrProtocol
			}
			prior = e.ID
		}
	}
	return v, nil
}
