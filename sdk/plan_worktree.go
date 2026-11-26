package sdk

import (
	"context"
	"errors"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

const (
	PlanWorktreeVersion = 1
	planWorktreeCap     = "planWorktree"
	planWorktreePrepare = "ext.planWorktree.prepare"
	planWorktreeStart   = "ext.planWorktree.start"
)

var planWorktreeBranch = regexp.MustCompile(`^hush/([a-z0-9][a-z0-9-]{0,39}-)?[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// PlanWorktree is a prepared checkout and empty thread. It isn't a run
// receipt or a workspace grant.
type PlanWorktree struct {
	ThreadID ID     `json:"threadId"`
	Title    string `json:"title"`
	Checkout struct {
		CWD    string `json:"cwd"` // execution and trust directory, possibly inside Root
		Root   string `json:"root"`
		Branch string `json:"branch"`
	} `json:"checkout"`
}

func (w PlanWorktree) valid() bool {
	cwd, root := w.Checkout.CWD, w.Checkout.Root
	return w.ThreadID.Valid() && planChatCWD(cwd) && planChatCWD(root) && cwd == path.Clean(cwd) && root == path.Clean(root) &&
		(cwd == root || strings.HasPrefix(cwd, root+"/")) && planWorktreeBranch.MatchString(w.Checkout.Branch) &&
		path.Base(root) == strings.TrimPrefix(w.Checkout.Branch, "hush/") && goodPlanText(w.Title, 96) &&
		!strings.ContainsAny(w.Title, "\u2028\u2029") && strings.IndexFunc(w.Title, unicode.IsControl) < 0
}

type planWorktreePick struct {
	PlanID    string   `json:"planId"`
	Revision  uint64   `json:"revision"`
	StepIDs   []string `json:"stepIds"`
	RequestID string   `json:"requestId"`
}

func planWorktreeSelection(s PlanSelection) (planWorktreePick, bool) {
	return planWorktreePick{s.PlanID, s.Revision, s.StepIDs, s.RequestID}, token(s.PlanID) && token(s.RequestID) &&
		s.Revision >= 1 && s.Revision <= 1<<53-1 && validPlanIDs(s.StepIDs)
}

// PlanWorktreeSource describes a launch without an active thread. A staged
// model and thinking level are optional, but must be supplied together.
type PlanWorktreeSource struct {
	CWD           string `json:"cwd,omitempty"`
	Model         *Model `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
}

// PreparePlanWorktree allocates a checkout from committed HEAD without
// submitting implementation work. Specify a thread or a prospective source.
// Retry an uncertain result with the same request ID, source and selection.
func (c *Client) PreparePlanWorktree(ctx context.Context, selection PlanSelection, source PlanWorktreeSource) (PlanWorktree, error) {
	pick, ok := planWorktreeSelection(selection)
	if !ok || selection.ThreadID != "" && (source != (PlanWorktreeSource{}) || !selection.ThreadID.Valid()) ||
		selection.ThreadID == "" && (!planChatCWD(source.CWD) || (source.Model == nil) != (source.ThinkingLevel == "") ||
			source.Model != nil && (!validCatalogModel(*source.Model) || !slices.Contains(catalogThinking, source.ThinkingLevel))) {
		return PlanWorktree{}, ErrProtocol
	}
	out, err := optionalCall[PlanWorktree](ctx, c, planWorktreeCap, PlanWorktreeVersion, planWorktreePrepare, struct {
		planWorktreePick
		PlanWorktreeSource
		ThreadID ID `json:"threadId,omitempty"`
	}{pick, source, selection.ThreadID})
	return out, planWorktreeReply(out.valid(), err)
}

// StartPlanWorktree admits exactly the selection used to prepare the checkout.
// The checkout must have its own workspace grant. After an uncertain reply,
// retry the same checkout and selection, never allocate a new one to retry.
func (c *Client) StartPlanWorktree(ctx context.Context, w PlanWorktree, selection PlanSelection) (ID, error) {
	pick, ok := planWorktreeSelection(selection)
	if !ok || !w.valid() {
		return "", ErrProtocol
	}
	out, err := optionalCall[struct {
		RunID ID `json:"runId"`
	}](ctx, c, planWorktreeCap, PlanWorktreeVersion, planWorktreeStart, struct {
		planWorktreePick
		ThreadID ID     `json:"threadId"`
		Checkout string `json:"checkout"`
	}{pick, w.ThreadID, w.Checkout.CWD})
	return out.RunID, planWorktreeReply(out.RunID.Valid(), err)
}

func planWorktreeReply(valid bool, err error) error {
	if err == nil && !valid {
		err = ErrProtocol
	}
	var transport *TransportError
	if errors.Is(err, ErrProtocol) && !errors.As(err, &transport) {
		return &TransportError{Err: err, Indeterminate: true} // a malformed reply is not a rejection
	}
	return err
}
