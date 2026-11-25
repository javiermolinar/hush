package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type planPreviewBackend struct {
	inertBackend
	saves [][2]sdk.ID
}

func (b *planPreviewBackend) SavePlanDraft(_ context.Context, thread, preview sdk.ID) (sdk.PlanDraftReceipt, error) {
	b.saves = append(b.saves, [2]sdk.ID{thread, preview})
	return sdk.PlanDraftReceipt{PlanID: "plan-a", Revision: 1}, nil
}

func previewFixtureEntry(id int) sdk.Entry {
	p := entry(id, "assistant", "Plan preview · Worker reviews\nNew plan · not saved\nCheck: Worker results remain separate.\nScope: \x1b[2Jnot terminal control")
	p.Kind = planPreviewEntry
	return p
}

func previewFixture(m *Model, s *threadState) {
	m.menuFeaturesFromBackend |= x.Plans | x.PlanDraft
	s.merge([]sdk.Entry{previewFixtureEntry(3), entry(4, "assistant", "Preview shown; not saved.")})
}

func planPreviewMouse(m *Model, action x.ActionID) (tea.Mouse, bool) {
	for y, hits := range m.frame.hits {
		for _, hit := range hits {
			if hit.action.ID == action {
				return tea.Mouse{X: hit.first, Y: y, Button: tea.MouseLeft}, true
			}
		}
	}
	return tea.Mouse{}, false
}

func TestPlanPreviewActions(t *testing.T) {
	for _, method := range []string{"mouse", "keyboard"} {
		t.Run(method, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
			m.Update(tea.WindowSizeMsg{Width: 40, Height: 14})
			previewFixture(m, s)
			b := &planPreviewBackend{}
			m.backend = b
			assertFrame(t, m)
			var cmd tea.Cmd
			if method == "mouse" {
				pos, ok := planPreviewMouse(m, savePlanPreviewID)
				if !ok {
					t.Fatal("no visible Save to /plan target")
				}
				m.Update(tea.MouseClickMsg(pos))
				_, cmd = m.Update(tea.MouseReleaseMsg(pos))
			} else {
				press(m, 'x', tea.ModCtrl)
				press(m, 'i', 0)
				cmd = press(m, 'w', 0)
			}
			workflowWork(t, m, cmd)
			if len(b.saves) != 1 || b.saves[0] != [2]sdk.ID{"1", "3"} {
				t.Fatalf("saved wrong preview: %v", b.saves)
			}
			if body := ansi.Strip(assertFrame(t, m)); strings.Contains(body, "Save to /plan") || !strings.Contains(body, "Saved plan (r1)") {
				t.Fatal(body)
			}
		})
	}
}

func TestPlanPreviewStaleClickAndCapability(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	previewFixture(m, s)
	b := &planPreviewBackend{}
	m.backend = b
	assertFrame(t, m)
	pos, ok := planPreviewMouse(m, savePlanPreviewID)
	if !ok {
		t.Fatal("no Save hit")
	}
	s.merge([]sdk.Entry{entry(5, "user", "Different topic")})
	m.Update(tea.MouseClickMsg(pos))
	m.Update(tea.MouseReleaseMsg(pos))
	if len(m.pending) != 0 {
		t.Fatal("an old frame started a save for a stale preview")
	}
	if body := ansi.Strip(assertFrame(t, m)); strings.Contains(body, "Save to /plan") {
		t.Fatal(body)
	}
	p := entry(6, "assistant", "Plan preview · Another draft\nNew plan · not saved")
	p.Kind = planPreviewEntry
	s.merge([]sdk.Entry{p})
	m.menuFeaturesFromBackend &^= x.PlanDraft
	if body := ansi.Strip(assertFrame(t, m)); strings.Contains(body, "Save to /plan") {
		t.Fatal(body)
	}
}

func TestPlanPreviewConfirmationNeedsContext(t *testing.T) {
	m, s := demo(t, Options{})
	r := m.route(m.active, coreID)
	if _, handled := m.confirmPlanPreview(r, s, "ok save it"); handled {
		t.Fatal("ordinary confirmations must not be intercepted without a preview")
	}
	m.menuFeaturesFromBackend = x.Plans | x.PlanDraft
	if _, handled := m.confirmPlanPreview(r, s, "save to /plan"); !handled || !strings.Contains(m.notice.text, "No current plan preview") {
		t.Fatal("an explicit plan save must explain that a preview is missing")
	}
	previewFixture(m, s)
	m.menuFeaturesFromBackend &^= x.PlanDraft
	if _, handled := m.confirmPlanPreview(r, s, "ok save it"); !handled || !strings.Contains(m.notice.text, "Preview saving is unavailable") {
		t.Fatal("an older host must not send a plan confirmation as ordinary chat")
	}
	if len(m.pending) != 0 {
		t.Fatal("a missing preview or missing capability started work")
	}
}

func testPlanPreviewFrameGeometry(t *testing.T) {
	for _, width := range []int{40, 64, 100} {
		m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
		m.Update(tea.WindowSizeMsg{Width: width, Height: 16})
		previewFixture(m, s)
		body := ansi.Strip(assertFrame(t, m))
		if !strings.Contains(body, "Save to /plan") {
			t.Fatal(body)
		}
		press(m, 'x', tea.ModCtrl)
		press(m, 'i', 0)
		press(m, 'v', 0)
		body = ansi.Strip(assertFrame(t, m))
		if !strings.Contains(body, "New plan · not saved") || !strings.Contains(body, "␛[2Jnot terminal") {
			t.Fatal(body)
		}
	}
}
