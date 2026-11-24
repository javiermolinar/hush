package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

type recoveryTestBackend struct {
	introTestBackend
	retried, dismissed [2]sdk.ID
}

func (b *recoveryTestBackend) RetryFailure(_ context.Context, thread, task sdk.ID) (sdk.Receipt, error) {
	b.retried = [2]sdk.ID{thread, task}
	return sdk.Receipt{SubmissionID: "100"}, nil
}

func (b *recoveryTestBackend) DismissFailure(_ context.Context, thread, task sdk.ID) error {
	b.dismissed = [2]sdk.ID{thread, task}
	return nil
}

func TestRunRecoveryGolden(t *testing.T) {
	m, now := introFixture(t, Options{ReducedMotion: true, Fills: FillOff})
	backend := &recoveryTestBackend{}
	m.backend = backend
	m.connection = x.Connected
	m.menuFeaturesFromBackend = x.Catalog
	report := introFixtureReport(*now)
	issue := &sdk.IntroRunIssue{TaskID: "42", Reason: "model_error", Detail: "rate limited (429)", Retry: true, SwitchModel: true,
		EndedAtMS: ptr(now.Add(-14 * time.Minute).UnixMilli())}
	report.Attention = []IntroAttention{
		{Title: "Fix auth tests", ThreadID: "7", Status: x.Stopped, Issue: issue},
		{Title: "User cancelled", ThreadID: "8", Status: x.Stopped, Reason: "aborted"},
		{Title: "User reset", ThreadID: "9", Status: x.Stopped, Reason: "reset"},
	}
	m.Update(introReportMsg{rich: true, update: IntroUpdate{Report: report}})
	m.Update(tea.WindowSizeMsg{Width: 82, Height: 22})
	golden(t, "intro-recovery", m)
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Fix auth tests   rate limited (429) · 14m ago", "retry · switch model · dismiss", "9 loaded"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "User cancelled") || strings.Contains(body, "User reset") || m.eligible(m.host.actions[m.host.byID[retryFailureID]], m.active) {
		t.Fatal("ignored outcomes or a shortcut to another thread were visible")
	}
	click := func(id x.ActionID) tea.Cmd {
		t.Helper()
		assertFrame(t, m)
		for y, row := range m.frame.hits {
			for _, hit := range row {
				if hit.action.ID != id {
					continue
				}
				p := tea.Mouse{X: hit.first, Y: y, Button: tea.MouseLeft}
				m.Update(tea.MouseClickMsg(p))
				_, cmd := m.Update(tea.MouseReleaseMsg(p))
				return cmd
			}
		}
		return nil
	}
	m.editor.set("don't change my draft")
	cmd := click(retryFailureID)
	if cmd == nil {
		t.Fatal("no action on the startup report")
	}
	m.Update(cmd())
	if backend.retried != [2]sdk.ID{"7", "42"} || m.active != "1" || m.editor.value() != "don't change my draft" {
		t.Fatal("recovery did not target the displayed issue or changed the draft")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 25})
	assertFrame(t, m)
	for _, row := range m.frame.hits {
		for _, hit := range row {
			if hit.action.ID == dismissFailureID {
				t.Fatal("narrow report left a hit for an omitted hint")
			}
		}
	}
	m.freezeIntro(m.threads[m.active])
	press(m, tea.KeyHome, tea.ModCtrl)
	if click(retryFailureID) != nil {
		t.Fatal("frozen report should no longer offer run mutations")
	}
}

func TestRecoveryUsesMatchingLiveFailure(t *testing.T) {
	m, state := demo(t, Options{ReducedMotion: true})
	backend := &recoveryTestBackend{}
	m.backend = backend
	failure := sdk.Failure{TaskID: "15", Reason: "no_model"}
	state.activity.failure = failure
	state.info.Status = x.Failed
	m.applyPresentation(state, &sdk.IntroThread{ID: "1", Title: "Review tests", Issue: &sdk.IntroRunIssue{
		TaskID: "15", Reason: "no_model", Detail: "fixture/model is unavailable", SwitchModel: true,
	}})
	m.menuFeaturesFromBackend = x.Catalog
	assertFrame(t, m)
	y := m.frame.transcriptHeight
	if line := ansi.Strip(m.frame.rows[y]); !strings.Contains(line, "fixture/model is unavailable") || !strings.Contains(line, "/model") || strings.Contains(line, "/retry") {
		t.Fatalf("wrong active run diagnosis or recovery choices: %q", line)
	}
	press(m, 'x', tea.ModCtrl)
	cmd := press(m, 'x', 0)
	if cmd == nil {
		t.Fatal("leader form did not dismiss the matching failure")
	}
	m.Update(cmd())
	if backend.dismissed != [2]sdk.ID{"1", "15"} {
		t.Fatalf("wrong dismiss target: %v", backend.dismissed)
	}
	m.applyPresentation(state, &sdk.IntroThread{ID: "1", Title: "Review tests", DismissedFailureID: "15"})
	if state.info.Status != x.Idle || state.visibleFailure().TaskID != "" {
		t.Fatal("dismissed task is still reported")
	}
	state.activity.failure.TaskID = "16"
	if state.visibleFailure().TaskID != "16" || state.currentIssue() != nil {
		t.Fatal("dismissal or recovery controls leaked across task IDs")
	}
	for _, reason := range []string{"aborted", "reset", "nothing_to_answer"} {
		snapshot := sdk.State{Thread: sdk.Thread{ID: "1"}, Failure: &sdk.Failure{TaskID: "20", Reason: reason}}
		state.info = m.threadInfo(state, sdk.Snapshot{State: snapshot})
		m.applyActivity(state, snapshot, time.Unix(100, 0), nil)
		if phase, _, _ := state.liveStatus(); phase == liveFailed || state.info.Status == x.Failed {
			t.Fatalf("%s was shown as an actionable failure", reason)
		}
	}
}
