package tui

import (
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func installPlanStripFixture(m *Model, t *threadState, total int) {
	m.menuFeaturesFromBackend |= x.Plans
	m.now = time.Unix(1000, 0)
	t.info.Status = x.Running
	t.activity.task, t.activity.since = "40", m.now.Add(-8*time.Second)
	start := entry(3, "assistant", "Plan run · r3 · Tooling and delegation · 3 steps")
	start.Kind = "hush.plan.run"
	t.merge([]sdk.Entry{start})
	titles := []string{"Enforce tool profiles", "Prove the Durable spawn and join contract", "Pin the read-only review target", "Document the assignment"}
	steps := make([]sdk.PlanStep, total)
	for i := range steps {
		title := fmt.Sprintf("Pending step %d", i+1)
		if i < len(titles) {
			title = titles[i]
		}
		steps[i] = sdk.PlanStep{ID: fmt.Sprintf("s%d", i), Title: title, Description: "Check the behavior against the reference."}
	}
	t.plan = planView{ready: true, snapshot: sdk.PlanSnapshot{
		Revision: strings.Repeat("a", 64), Plan: &sdk.PlanDefinition{ID: "tooling", Title: "Tooling and delegation", Revision: 3, Steps: steps},
		RunAvailability: sdk.PlanRunScoped,
		Run:             &sdk.PlanRun{ID: "41", StepIDs: []string{"s0", "s1", "s2"}, State: "running", OwnerThreadID: t.info.ID},
		Progress: []sdk.PlanProgress{
			{StepID: "s0", State: "verified", WorkerThreadID: t.info.ID, Evidence: []string{"Tool profile checks passed"}},
			{StepID: "s1", State: "running", WorkerThreadID: t.info.ID},
			{StepID: "s2", State: "running", WorkerThreadID: "2"},
		},
	}}
}

func planStripText(t testing.TB, m *Model) string {
	t.Helper()
	assertFrame(t, m)
	if m.frame.planStrip == "" || m.frame.ruleY < 1 {
		t.Fatal("no visible plan strip")
	}
	return strings.TrimSpace(ansi.Strip(m.frame.rows[m.frame.ruleY-1]))
}

func TestGoldenPlanStrip(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 116, Height: 24})
	installPlanStripFixture(m, s, 14)
	enableGitDiffFixture(m, s)
	golden(t, "plan-strip-running", m)
	v := m.View()
	ruleY := m.frame.ruleY
	press(m, tea.KeyUp, 0)
	golden(t, "plan-strip-peek", m)
	if peek := m.View(); peek.Cursor == nil || v.Cursor == nil || peek.Cursor.Position != v.Cursor.Position || m.frame.ruleY != ruleY {
		t.Fatal("peek moved the rule or prompt cursor")
	}
	press(m, tea.KeyEscape, 0)
	s.plan.snapshot.Progress[2].State = "failed"
	golden(t, "plan-strip-failed", m)
}

func TestGoldenPlanReportedDone(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	setTypographyPalette(m, typographyPalettes[1])
	installPlanStripFixture(m, s, 6)
	m.nativeChatPlanActivity = true
	s.chatPlan, s.plan = s.plan, planView{}
	v := &s.chatPlan.snapshot
	v.Chat, v.Progress, v.RunAvailability = true, nil, sdk.PlanRunDisabled
	v.Run.Chat, v.Run.State, v.Run.SubmissionID = true, "done", "39"
	for _, step := range v.Plan.Steps {
		v.Reports = append(v.Reports, sdk.PlanTaskReport{StepID: step.ID, State: "done", Detail: "Reported without verification"})
	}
	s.info.Status = x.Idle
	s.activity = activityState{}

	golden(t, "plan-strip-reported", m)
	row := m.frame.rows[m.frame.ruleY-1]
	if strings.Count(row, m.quiet("–")) != 6 || strings.Contains(row, "✓") || !strings.Contains(row, m.quiet("0/6")) {
		t.Fatalf("strip did not show six quiet claims and zero verified: %q", row)
	}
	m.openCurrentPlanRun(m.route(m.active, menusID), v.Run.ID)
	golden(t, "plan-reported", m)
	panel := strings.Join(m.frame.rows[m.frame.dialogY:m.frame.ruleY], "\n")
	if strings.Count(panel, m.quiet("–")) != 6 || !strings.Contains(ansi.Strip(panel), "0/6 verified · 6 reported done") {
		t.Fatalf("picker did not distinguish claims from verification: %q", panel)
	}
}

func TestPlanRunTranscriptIsOneRow(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 46, Height: 16})
	old := entry(3, "assistant", "Plan run · Legacy plan · r1\nSelected: Hidden old selection")
	old.Kind = "hush.plan.run"
	current := entry(4, "assistant", "Plan run · r3 · "+strings.Repeat("Wide 界 title ", 16)+" · 3 steps")
	current.Kind = "hush.plan.chat"
	s.merge([]sdk.Entry{old, current})
	body := ansi.Strip(assertFrame(t, m))
	var headings int
	for _, row := range strings.Split(body, "\n") {
		if strings.Contains(row, "Plan run · ") {
			headings++
			if strings.Contains(row, "r3") && !strings.Contains(row, " · 3 steps") {
				t.Fatal("the long title pushed the step count out of the heading: " + row)
			}
		}
	}
	if headings != 2 || strings.Contains(body, "Selected:") || strings.Contains(body, "Hidden old selection") {
		t.Fatal(body)
	}
}

func TestPlanStripPeekKeys(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	installPlanStripFixture(m, s, 6)
	m.planFeed = feedLease{token: m.token(), owner: m.active, ctx: m.ctx}
	s.history = []promptDraft{{text: "Earlier prompt"}}
	if got := planStripText(t, m); !strings.Contains(got, "✓●●○○○  1/6") || !strings.Contains(got, "↑ peek") {
		t.Fatal(got)
	}
	press(m, tea.KeyUp, 0)
	if got := ansi.Strip(assertFrame(t, m)); !m.frame.planPeek || m.focusedDialog() != nil ||
		!strings.Contains(got, "Waiting for model") || !strings.Contains(got, "Running in worker thread") {
		t.Fatal(got)
	}
	press(m, tea.KeyEscape, 0)
	if m.planPeek != "" || !m.stopAt.IsZero() || m.editor.value() != "" {
		t.Fatal("Esc from peek must not arm Stop or modify the draft")
	}
	planStripText(t, m)
	press(m, tea.KeyUp, 0)
	m.Update(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if m.planPeek != "" || m.editor.value() != "a" {
		t.Fatal("typing did not resume on the first key")
	}
	planStripText(t, m)
	press(m, tea.KeyUp, 0)
	if m.planPeek != "" || m.editor.value() != "Earlier prompt" {
		t.Fatal("Up stole history from the nonempty editor")
	}
	m.editor.set("")
	planStripText(t, m)
	press(m, tea.KeyUp, 0)
	m.Update(tea.PasteMsg{Content: "Pasted steer"})
	if m.planPeek != "" || m.editor.value() != "Pasted steer" {
		t.Fatal("paste did not dismiss the peek and reach the composer")
	}
	m.editor.set("")
	planStripText(t, m)
	press(m, tea.KeyUp, 0)
	press(m, tea.KeyEnter, 0)
	if d := m.focusedDialog(); d == nil || d.plan == nil || m.planViewFor(d) != &s.plan || m.planPeek != "" {
		t.Fatal("Enter from peek did not open the running plan")
	}
	assertFrame(t, m)
	if m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY {
		t.Fatal("the full plan no longer docks to the input rule")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil || m.editor.value() != "" || m.planPeek != "" {
		t.Fatal("Esc did not return to the composer")
	}
	m.editor.set("unfinished steer")
	planStripText(t, m)
	f := m.frame
	m.Update(tea.MouseClickMsg(tea.Mouse{X: m.pane().edge, Y: f.ruleY - 1, Button: tea.MouseLeft}))
	if d := m.focusedDialog(); d == nil || d.plan == nil || m.editor.value() != "unfinished steer" {
		t.Fatal("click did not open the plan while retaining the draft")
	}
}

func TestPlanStripEvents(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 112, Height: 24})
	installPlanStripFixture(m, s, 14)
	m.backend = planFixtureBackend{}
	m.planFeed = feedLease{token: m.token(), owner: m.active, ctx: m.ctx}
	update := func(next sdk.PlanSnapshot) {
		m.Update(planWatchMsg{token: m.planFeed.token, update: sdk.PlanUpdate{Snapshot: next}, ok: true})
	}
	initial := s.plan.snapshot
	s.plan.snapshot = sdk.PlanSnapshot{}
	update(initial)
	if strings.Contains(planStripText(t, m), "Enforce tool profiles verified") {
		t.Fatal("initial watch re-announced existing verification")
	}
	verified := initial
	verified.Progress = slices.Clone(verified.Progress)
	verified.Progress[1].State = "verified"
	update(verified)
	flash := s.plan.strip.flash
	if got := planStripText(t, m); !strings.Contains(got, "✓ Prove the Durable spawn and join contract verified") || flash == 0 {
		t.Fatal(got)
	}
	update(verified)
	if s.plan.strip.flash != flash {
		t.Fatal("a duplicate watch snapshot restarted the flash")
	}
	m.Update(planVerifiedExpired{m.active, flash})
	if got := planStripText(t, m); strings.Contains(got, "contract verified") || !strings.Contains(got, "Pin the read-only review target") {
		t.Fatal(got)
	}
	failed := verified
	failed.Progress = slices.Clone(failed.Progress)
	failed.Progress[2].State = "failed"
	failed.Run = &sdk.PlanRun{ID: "41", StepIDs: initial.Run.StepIDs, State: "failed", OwnerThreadID: m.active, Failure: "Isolated acceptance did not pass."}
	update(failed)
	m.Update(planVerifiedExpired{m.active, flash})
	if got := planStripText(t, m); !strings.Contains(got, "acceptance did not pass") || !strings.Contains(got, "↵ see why") {
		t.Fatal(got)
	}
	press(m, tea.KeyEnter, 0)
	d := m.focusedDialog()
	if d == nil || d.plan == nil || d.plan.focus != 2 || !strings.Contains(ansi.Strip(assertFrame(t, m)), failed.Run.Failure) {
		t.Fatal("the full plan did not focus the failure and show its reason")
	}
	press(m, tea.KeyEscape, 0)
	update(failed)
	if got := planStripText(t, m); strings.Contains(got, "↵ see why") || !strings.Contains(got, "✓✓✗") {
		t.Fatal("inspection should clear the alert, not the failed dot: " + got)
	}
	failed.Run = &sdk.PlanRun{ID: "42", StepIDs: initial.Run.StepIDs, State: "failed", OwnerThreadID: m.active, Failure: "Run stopped"}
	update(failed)
	if got := planStripText(t, m); !strings.Contains(got, "↵ see why") {
		t.Fatal("an older acknowledgement cleared a different run: " + got)
	}
}

func TestChatPlanStripTracksOnlyItsSubmission(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.Update(tea.WindowSizeMsg{Width: 98, Height: 24})
	installPlanStripFixture(m, s, 6)
	m.nativeChatPlanActivity = true
	m.chatPlanFeed = feedLease{token: m.token(), owner: m.active, ctx: m.ctx}
	chat := s.plan.snapshot
	chat.Run = &sdk.PlanRun{ID: "45", StepIDs: []string{"s1", "s2"}, State: "running", Chat: true, OwnerThreadID: s.info.ID, SubmissionID: "44"}
	chat.Chat, chat.Progress, chat.RunAvailability = true, nil, sdk.PlanRunDisabled
	chat.Reports = []sdk.PlanTaskReport{{StepID: "s3", State: "done", Detail: "Reported but not verified"}}
	s.plan.snapshot = sdk.PlanSnapshot{}
	m.Update(planWatchMsg{token: m.chatPlanFeed.token, update: sdk.PlanUpdate{Snapshot: chat}, ok: true})
	s.runInputs = []sdk.ID{"43"}
	s.queue = []sdk.QueueItem{{SubmissionID: "44", Mode: "followUp", Text: "Plan run · r3 · Tooling and delegation · 2 steps"}}
	s.tools = []x.Tool{{Name: "edit", Live: true, ArgumentsJSON: `{"path":"other-turn.go"}`}}
	if got := planStripText(t, m); !strings.Contains(got, "○○○–○○  0/6") || !strings.Contains(got, "Run queued") {
		t.Fatal(got)
	}
	press(m, tea.KeyUp, 0)
	assertFrame(t, m)
	if row := ansi.Strip(m.frame.rows[m.frame.ruleY-3]); !strings.Contains(row, "Run queued") || strings.Contains(row, "other-turn.go") {
		t.Fatal("peek attributed an unrelated tool call to a queued plan: " + row)
	}
	s.runInputs, s.queue = []sdk.ID{"44"}, nil
	s.tools[0].ArgumentsJSON = `{"path":"tools.go"}`
	if content := ansi.Strip(assertFrame(t, m)); !strings.Contains(content, "Shared turn · Editing tools.go") || !strings.Contains(content, "○●●–○○") {
		t.Fatal(content)
	}
	press(m, tea.KeyEnter, 0)
	if d := m.focusedDialog(); d == nil || d.plan == nil || d.plan.activityRun != "45" || s.plan.snapshot.Plan != nil {
		t.Fatal("peek tried to bind the thread instead of inspecting the chat run")
	}
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Run here in progress") {
		t.Fatal("the panel didn't show the live submission: " + frame)
	}
	if cmd := press(m, tea.KeyEnter, 0); cmd != nil || s.plan.chatRequest != nil || s.plan.request != nil {
		t.Fatal("inspecting chat activity allowed admission")
	}
	press(m, tea.KeyEscape, 0)
	s.chatPlan.snapshot.Run = &sdk.PlanRun{ID: "46", StepIDs: chat.Run.StepIDs, State: "running", Chat: true, OwnerThreadID: s.info.ID, SubmissionID: "45"}
	// The pre-dialog frame has no strip; an undrawn action must not take keys.
	press(m, tea.KeyUp, 0)
	if m.planPeek != "" {
		t.Fatal("Up focused an undrawn plan run")
	}
}

func testPlanStripFrameGeometry(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	installPlanStripFixture(m, s, 24)
	s.plan.snapshot.Plan.Steps[1].Title = strings.Repeat("Wide 界 ", 15) + "\x1b]52;c;not-clipboard\a"
	s.plan.snapshot.Progress[2].State = "failed"
	for _, size := range [][2]int{{12, 3}, {12, 4}, {20, 11}, {40, 12}, {80, 24}, {116, 24}, {180, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFrame(t, m)
		if m.frame.planStrip == "" && size[1] > 3 {
			t.Fatalf("strip did not fit at %v", size)
		}
		press(m, tea.KeyUp, 0)
		frame := assertFrame(t, m)
		if strings.Contains(frame, "\x1b]52;") || strings.Contains(frame, "\a") {
			t.Fatal("plan text emitted terminal controls")
		}
		press(m, tea.KeyEscape, 0)
	}
	if got := planStripText(t, m); !strings.Contains(got, "1 running · 1 verified · 1 failed · 21 pending") || strings.Contains(got, "○○") {
		t.Fatal("long plans should collapse to counts: " + got)
	}
}

func TestPlanStripThemeFrames(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	for _, theme := range []struct {
		name   string
		bg, fg color.Color
	}{
		{"latte", themeRGB(0xeff1f5), themeRGB(0x4c4f69)},
		{"mocha", themeRGB(0x1e1e2e), themeRGB(0xcdd6f4)},
	} {
		t.Run(theme.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOn})
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			m.Update(tea.BackgroundColorMsg{Color: theme.bg})
			m.Update(tea.ForegroundColorMsg{Color: theme.fg})
			m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			preset, _ := presetTheme("panther")
			m.applyTheme(preset)
			installPlanStripFixture(m, s, 14)
			s.plan.snapshot.Chat = true
			s.plan.snapshot.Reports = []sdk.PlanTaskReport{{StepID: "s3", State: "done", Detail: "Reported but not verified"}}
			for _, state := range []string{"running", "peek", "failed"} {
				if state == "peek" {
					press(m, tea.KeyUp, 0)
				} else if state == "failed" {
					press(m, tea.KeyEscape, 0)
					s.plan.snapshot.Progress[2].State = "failed"
				}
				frame := assertFrame(t, m)
				row := m.frame.rows[m.frame.ruleY-1]
				for _, color := range []struct {
					tone x.Tone
					mark string
				}{{x.Good, "✓"}, {x.Warning, "●"}, {x.Muted, "–"}, {x.Muted, "○"}} {
					if !strings.Contains(row, m.paint(color.tone, color.mark)) {
						t.Errorf("%s frame lost %s tone: %q", state, color.mark, row)
					}
				}
				if state == "failed" && !strings.Contains(row, m.paint(x.Danger, "✗")) {
					t.Errorf("failed dot did not use the error role: %q", row)
				}
				if dir := os.Getenv("HUSH_RELEASE_FRAMES"); dir != "" {
					path := filepath.Join(dir, theme.name+"-plan-strip-"+state+".ansi")
					if err := os.WriteFile(path, []byte(frame+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			m.openCurrentPlanRun(m.route(m.active, menusID), s.plan.snapshot.Run.ID)
			for _, mode := range []surfaceMode{surfaceTransparent, surfaceTinted, surfaceSolid} {
				preset.surface = mode
				m.applyTheme(preset)
				assertFrame(t, m)
				panel := strings.Join(m.frame.rows[m.frame.dialogY:m.frame.ruleY], "\n")
				for _, want := range []string{m.paint(x.Good, "✓"), m.paint(x.Warning, "◐"), m.paint(x.Danger, "✗"), m.quiet("–"), m.quiet("○")} {
					if !strings.Contains(panel, want) {
						t.Errorf("%s picker is missing %q: %q", mode, want, panel)
					}
				}
			}
		})
	}
}
