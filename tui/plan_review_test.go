package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func testPlanReviewFrameGeometry(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	enableGitDiffFixture(m, s)
	m.menuFeaturesFromBackend |= x.Plans | x.PlanWorkspace | x.PlanReview
	m.editor.set("keep this draft")
	m.Update(tea.WindowSizeMsg{Width: 104, Height: 24})
	assertFrame(t, m)
	rule := m.frame.ruleY
	m.openPlanReview(m.route(m.active, menusID), "fixture-plan", "Workspace profiles", 1)
	d := m.focusedDialog()
	p := d.planReview
	p.ready = true
	p.view = sdk.PlanReviewView{Title: p.title, Revision: 1, HeadRevision: 2, Source: "captured", Review: &sdk.PlanReviewRecord{
		ID: "30", ThreadID: m.active, ReviewerThreadID: "31", Model: sdk.Model{Provider: "fixture", ModelID: "reviewer"}, Thinking: "off", State: "complete",
		Report: &sdk.PlanReviewReport{Summary: "The saved requirements and checks disagree.",
			Checks:      sdk.PlanReviewChecks{Requirements: "concern", Acceptance: "addressed", Dependencies: "unverified"},
			Findings:    []sdk.PlanReviewFinding{{Severity: "blocking", Area: "requirements", StepID: "save", Message: strings.Repeat("a wide finding 界 ", 20) + "\x1b]52;c;clipboard\a"}},
			Limitations: []string{"No files inspected or tests run."}},
	}}
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Stale revision · head is r2", "Blocking · requirements · save", "keep this draft", "± 4 files"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if m.frame.ruleY != rule || m.frame.dialogY+m.frame.dialogHeight != rule || p.canStart() {
		t.Fatal("review did not dock above the input rule or offered Start on a stale revision")
	}
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {120, 30}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		p.offset = 0
		assertFrame(t, m)
		for range 12 {
			press(m, tea.KeyPgDown, 0)
		}
		body = assertFrame(t, m)
		if plain := ansi.Strip(body); !strings.Contains(plain, "No files inspected") || strings.Contains(body, "\x1b]52;") {
			t.Fatalf("review limitations did not fit or unsafe text reached the terminal at %v:\n%s", size, plain)
		}
	}
}
