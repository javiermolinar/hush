package tui

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const (
	planPreviewEntry               = "hush.plan.preview"
	planPreviewSaved               = "hush.plan.preview-saved"
	inspectPlanPreviewID           = activityID + ".plan-preview"
	savePlanPreviewID              = activityID + ".save-preview"
	inspectPlanPreviewOp x.LocalOp = "plan-preview.inspect"
	savePlanPreviewOp    x.LocalOp = "plan-preview.save"
)

type planDraftBackend interface {
	SavePlanDraft(context.Context, sdk.ID, sdk.ID) (sdk.PlanDraftReceipt, error)
}

type planDraftState struct {
	latest, boundary sdk.ID
	saving           bool
}

func (p *planDraftState) observe(e sdk.Entry) {
	if e.Kind == planPreviewEntry {
		if lessID(p.latest, e.ID) {
			p.latest = e.ID
		}
		return
	}
	if lessID(p.boundary, e.ID) && (e.Kind == "pi.reset" || e.Kind == planPreviewSaved ||
		len(e.Messages) > 0 && e.Messages[0].Role == "user") {
		p.boundary = e.ID
	}
}

func (p planDraftState) pending() sdk.ID {
	if lessID(p.boundary, p.latest) {
		return p.latest
	}
	return ""
}

func (m *Model) canSavePlanPreview(t *threadState, id sdk.ID) bool {
	return t != nil && m.planAvailable() && m.menuFeaturesFromBackend.Has(x.PlanDraft) && t.info.ID == m.active &&
		id.Valid() && t.planDraft.pending() == id && t.entries[id] != nil && !t.entries[id].entry.Truncated &&
		!t.planDraft.saving && !t.sending && !t.aborting && !threadBusy(t) && t.awaiting == "" &&
		len(t.queue) == 0 && len(t.admittedInputs) == 0 && t.gapUpper == ""
}

func planSaveConfirmation(text string) (match, namesPlan bool) {
	if len(text) <= 40 {
		switch strings.TrimRight(strings.ToLower(strings.TrimSpace(text)), ".!") {
		case "save it", "ok save it", "ok, save it", "okay save it", "okay, save it", "yes save it", "yes, save it":
			return true, false
		case "save to /plan", "save it to /plan":
			return true, true
		}
	}
	return false, false
}

func (m *Model) confirmPlanPreview(r ownerRoute, t *threadState, text string) (tea.Cmd, bool) {
	confirm, namesPlan := planSaveConfirmation(text)
	preview := t.planDraft.pending()
	if !confirm || !namesPlan && !preview.Valid() {
		return nil, false
	}
	if !m.menuFeaturesFromBackend.Has(x.PlanDraft) {
		msg := "This host doesn't support saving to /plan."
		if m.menuFeaturesFromBackend.Has(x.Plans) {
			msg = "Preview saving is unavailable. Use ‘Create a plan …’ or ‘Revise the plan …’."
		}
		return m.notifyRoute(r, msg, x.Warning), true
	}
	if !preview.Valid() {
		return m.notifyRoute(r, "No current plan preview. Ask to preview a plan first.", x.Warning), true
	}
	if m.canSavePlanPreview(t, preview) {
		m.editor.set("")
		m.editor.resetHistory()
		m.slashHidden = false
	}
	return m.savePlanPreview(r, preview), true
}

func (m *Model) planPreviewContent(ctx x.RenderContext, t *threadState, id sdk.ID, text string) x.Content {
	heading, _, _ := strings.Cut(text, "\n")
	out := x.Content{activityRow(x.Line{{Text: heading, Tone: x.Muted, Hint: "Inspect plan preview",
		Action: x.ActionRef{ID: inspectPlanPreviewID, Target: string(id)}}}, "", ctx.Width)}
	if m.canSavePlanPreview(t, id) {
		out = append(out, activityRow(x.Line{{Text: "Save to /plan", Tone: x.Muted, Hint: "Save this exact preview",
			Action: x.ActionRef{ID: savePlanPreviewID, Target: string(id)}}, {Text: " · ctrl+x i w", Tone: x.Muted}}, "", ctx.Width))
	}
	return out
}

func (m *Model) inspectPlanPreview(r ownerRoute, id sdk.ID) tea.Cmd {
	if !m.foreground(r) {
		return nil
	}
	rec := m.threads[r.owner].entries[id]
	if rec == nil || rec.entry.Record.Kind != planPreviewEntry {
		return nil
	}
	msgs := rec.entry.Record.Messages
	if len(msgs) != 1 || len(msgs[0].Blocks) != 1 {
		return nil
	}
	_, body, _ := strings.Cut(msgs[0].Blocks[0].Text, "\n")
	d := &dialog{route: r, token: m.token(), title: "Plan preview", output: newOutputViewer(body), review: true}
	d.output.reflowReview(m.pane().width)
	return m.pushDialog(d)
}

func (m *Model) savePlanPreview(r ownerRoute, id sdk.ID) tea.Cmd {
	b, ok := m.backend.(planDraftBackend)
	t := m.threads[r.owner]
	if !ok || !m.foreground(r) || !m.canSavePlanPreview(t, id) {
		return m.notifyRoute(r, "Wait for a current plan preview before saving", x.Warning)
	}
	t.planDraft.saving = true
	return m.startWork(r, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, menuTimeout)
		defer cancel()
		return b.SavePlanDraft(ctx, r.owner, id)
	}, func(value any, err error, _ *int) tea.Cmd {
		t.planDraft.saving = false
		if err != nil {
			if isIndeterminate(err) {
				return m.notifyRoute(r, "Plan save unconfirmed. Retry Save to /plan for the same preview.", x.Warning)
			}
			return m.notifyRoute(r, err.Error(), x.Danger)
		}
		if lessID(t.planDraft.boundary, id) {
			t.planDraft.boundary = id
		}
		return m.notifyRoute(r, fmt.Sprintf("Saved plan (r%d). Open /plan to review it.", value.(sdk.PlanDraftReceipt).Revision), x.Good)
	}, nil)
}
