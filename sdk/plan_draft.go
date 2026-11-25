package sdk

import "context"

const PlanDraftVersion = 1
const planDraftCapability = "planDraft"
const planDraftSave = "ext.planDraft.save"

// PlanDraftReceipt identifies the revision saved for a preview.
type PlanDraftReceipt struct {
	PlanID   string `json:"planId"`
	Revision uint64 `json:"revision"`
}

// SavePlanDraft saves exactly the preview entry from the owning thread. The
// same thread and entry IDs can be retried after an indeterminate reply.
func (c *Client) SavePlanDraft(ctx context.Context, thread, preview ID) (PlanDraftReceipt, error) {
	if !thread.Valid() || !preview.Valid() {
		return PlanDraftReceipt{}, ErrProtocol
	}
	out, err := optionalCall[PlanDraftReceipt](ctx, c, planDraftCapability, PlanDraftVersion, planDraftSave,
		map[string]ID{"threadId": thread, "previewId": preview})
	if err == nil && (!token(out.PlanID) || out.Revision < 1 || out.Revision > 1<<53-1) {
		return PlanDraftReceipt{}, &TransportError{Err: ErrProtocol, Indeterminate: true}
	}
	return out, err
}
