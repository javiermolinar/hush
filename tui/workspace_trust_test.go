package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

var gitTrustDenied = &sdk.ServerError{Code: "invalid_request",
	Message: "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host."}

type deniedGitDiffFixture struct{ gitDiffFixtureBackend }

func (deniedGitDiffFixture) GitDiffSummary(context.Context, sdk.ID, string) (sdk.GitDiffSummary, error) {
	return sdk.GitDiffSummary{}, gitTrustDenied
}

func workspaceTrustStep(t testing.TB, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a trust command")
	}
	_, next := m.Update(cmd())
	return next
}

func workspaceTrustFixture(path string) *WorkspaceTrustOptions {
	return &WorkspaceTrustOptions{Review: func(context.Context, string) (WorkspaceTrustPreview, error) {
		return WorkspaceTrustPreview{Path: path, Decide: func(context.Context, bool) error { return nil }}, nil
	}}
}

func openWorkspaceTrustFixture(t testing.TB, m *Model) {
	t.Helper()
	workspaceTrustStep(t, m, m.openWorkspaceTrust(m.route(m.active, coreID)))
}

func TestGoldenWorkspaceTrustOnFirstEntry(t *testing.T) {
	var checks, grants, dismissals int
	opt := &WorkspaceTrustOptions{Review: func(_ context.Context, cwd string) (WorkspaceTrustPreview, error) {
		checks++
		if cwd != "/work/a folder" {
			t.Fatalf("checked wrong workspace: %s", cwd)
		}
		return WorkspaceTrustPreview{Path: cwd, Trusted: grants > 0, Dismissed: dismissals > 0,
			Decide: func(_ context.Context, grant bool) error {
				if grant {
					grants++
				} else {
					dismissals++
				}
				return nil
			}}, nil
	}}
	opts := Options{Fills: FillOff, NewThread: sdk.NewThread{CWD: "/work/a folder"}, WorkspaceTrust: opt}
	m := modelFor(t, opts) // a fresh local TUI without an existing thread
	workspaceTrustStep(t, m, m.syncWorkspaceTrust())
	m.editor.set("keep this draft")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	golden(t, "menu-workspace-trust", m)
	frame := ansi.Strip(m.View().Content)
	for _, want := range []string{"Trust this workspace?", "Tools, file browsing, and Git (whole checkout)", `"/work/a folder"`, "t trust", "n not now"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in confirmation:\n%s", want, frame)
		}
	}
	if cmd := press(m, tea.KeyEnter, 0); cmd != nil || grants != 0 {
		t.Fatal("Enter must not grant trust by default")
	}
	workspaceTrustStep(t, m, press(m, 'n', 0))
	if m.focusedDialog() != nil || m.syncWorkspaceTrust() != nil || checks != 1 || dismissals != 1 || m.editor.value() != "keep this draft" {
		t.Fatal("dismissing changed the draft or prompted again")
	}

	m = modelFor(t, opts)
	workspaceTrustStep(t, m, m.syncWorkspaceTrust())
	if m.focusedDialog() != nil {
		t.Fatal("a remembered dismissal was prompted again")
	}
	if _, ok := m.host.slash["trust"]; !ok {
		t.Fatal("no manual /trust action")
	}
	openWorkspaceTrustFixture(t, m)
	assertFrame(t, m)
	row := slices.Index(m.frame.dialogChoices, workspaceTrustGrantTarget)
	if row < 0 {
		t.Fatal("no rendered trust target")
	}
	_, confirm := m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: m.frame.dialogX + 2, Y: m.frame.dialogY + row}))
	if grants != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Saving workspace trust") {
		t.Fatal("confirmation should schedule, not perform, the grant")
	}
	workspaceTrustStep(t, m, confirm)
	if grants != 1 || m.focusedDialog() != nil || m.syncWorkspaceTrust() != nil {
		t.Fatal("grant did not settle the dialog")
	}
	m = modelFor(t, opts)
	workspaceTrustStep(t, m, m.syncWorkspaceTrust())
	if m.focusedDialog() != nil {
		t.Fatal("an already authorized workspace was prompted again")
	}
}

func TestWorkspaceTrustWaitsForActiveWorkspaceAndOverlay(t *testing.T) {
	m, s := demo(t, Options{WorkspaceTrust: workspaceTrustFixture("/workspace")})
	if m.syncWorkspaceTrust() != nil {
		t.Fatal("must wait for actual thread cwd, not initial client cwd")
	}
	s.observed = true
	cmd := m.syncWorkspaceTrust()
	m.pushDialog(&dialog{token: m.token(), route: m.route(m.active, coreID), title: "Other action"})
	workspaceTrustStep(t, m, cmd)
	if top := m.focusedDialog(); top == nil || top.workTrust != nil {
		t.Fatal("automatic trust stole focus from an open dialog")
	}
	press(m, tea.KeyEscape, 0)
	if top := m.focusedDialog(); top == nil || top.workTrust == nil || top.workTrust.cwd != s.info.Workspace {
		t.Fatal("active workspace wasn't offered after closing the overlay")
	}
	assertFrame(t, m)
	s.info.Workspace = "/other"
	if press(m, 't', 0) != nil || m.focusedDialog().workTrust.notice == "" {
		t.Fatal("stale dialog allowed trust or didn't explain changed workspace")
	}
}

func TestWorkspaceTrustAttachedHostInstructions(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, WorkspaceLocal: true, WorkspaceTrust: &WorkspaceTrustOptions{}})
	s.observed = true
	if m.syncWorkspaceTrust() != nil {
		t.Fatal("attached client attempted a local review")
	}
	m.openWorkspaceTrust(m.route(m.active, coreID))
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"On the backend host, run:", "hush trust grant --cwd PATH", "PATH = /workspace"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in instructions:\n%s", want, frame)
		}
	}
	if slices.Contains(m.frame.dialogChoices, workspaceTrustGrantTarget) || press(m, 't', 0) != nil {
		t.Fatal("attached client should not offer a grant")
	}
}

func TestGitDiffTrustDenialIsReadOnly(t *testing.T) {
	checks := 0
	opt := &WorkspaceTrustOptions{Review: func(context.Context, string) (WorkspaceTrustPreview, error) {
		checks++
		return WorkspaceTrustPreview{}, nil
	}}
	m, s := demo(t, Options{Fills: FillOff, WorkspaceTrust: opt})
	s.observed = true
	enableGitDiffFixture(m, s)
	m.backend = deniedGitDiffFixture{}
	s.gitDiff = gitDiffSnapshot(sdk.GitDiffSummary{}, gitTrustDenied)
	workspaceTrustStep(t, m, m.openGitDiff(m.route(m.active, coreID)))
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "Workspace trust required for Git review") || !strings.Contains(frame, "hush trust grant --cwd PATH") ||
		!strings.Contains(frame, "± needs trust") || press(m, 't', 0) != nil || checks != 0 {
		t.Fatalf("Git review must only explain access restrictions:\n%s", frame)
	}
	workspaceTrustStep(t, m, press(m, 'u', 0))
	if top := m.focusedDialog(); top == nil || top.git == nil || top.workTrust != nil || checks != 0 {
		t.Fatal("refresh attempted to prompt for workspace trust")
	}
}

func testWorkspaceTrustFrameGeometry(t *testing.T) {
	path := "/work/" + strings.Repeat("wide界", 18) + "/project\u202e"
	m, s := demo(t, Options{Fills: FillOff, WorkspaceTrust: workspaceTrustFixture(path)})
	s.observed = true
	m.editor.set(strings.Repeat("draft \n", 12) + "end of draft")
	openWorkspaceTrustFixture(t, m)
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {140, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		frame := assertFrame(t, m)
		if frame != assertFrame(t, m) || !strings.Contains(ansi.Strip(frame), "end of draft") {
			t.Fatalf("trust prompt lost draft or changed frame at %v", size)
		}
		if size[0] == 40 && (slices.Contains(m.frame.dialogChoices, workspaceTrustGrantTarget) || press(m, 't', 0) != nil) {
			t.Fatal("must not confirm a path that cannot fit in the prompt")
		}
		if size[0] >= 80 && (!slices.Contains(m.frame.dialogChoices, workspaceTrustGrantTarget) || !strings.Contains(frame, `\u202e`)) {
			t.Fatalf("no trust target or escaped path at %v", size)
		}
	}
}
