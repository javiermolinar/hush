package tui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

var gitTrustDenied = &sdk.ServerError{Code: "invalid_request",
	Message: "Workspace is not trusted by Hush. Run 'hush trust grant --cwd PATH' on the backend host."}

type gitTrustFixtureBackend struct {
	gitDiffFixtureBackend
	err error
}

func (b *gitTrustFixtureBackend) GitDiffSummary(_ context.Context, _ sdk.ID, name string) (sdk.GitDiffSummary, error) {
	if b.err != nil {
		return sdk.GitDiffSummary{}, b.err
	}
	return diffFixtureSummary(name), nil
}

func gitTrustStep(t testing.TB, m *Model, cmd tea.Cmd) tea.Cmd {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a Git review command")
	}
	_, next := m.Update(cmd())
	return next
}

func setupGitTrustFixture(m *Model, s *threadState) *gitTrustFixtureBackend {
	enableGitDiffFixture(m, s)
	b := &gitTrustFixtureBackend{err: gitTrustDenied}
	m.backend = b
	s.gitDiff = gitDiffSnapshot(sdk.GitDiffSummary{}, gitTrustDenied)
	return b
}

func gitTrustPreviewFixture(path string) func(context.Context, string) (GitTrustPreview, error) {
	return func(context.Context, string) (GitTrustPreview, error) {
		return GitTrustPreview{GitDirectory: path, Grant: func(context.Context) error { return nil }}, nil
	}
}

func openGitTrustFixture(t testing.TB, m *Model) {
	t.Helper()
	next := gitTrustStep(t, m, m.openGitDiff(m.route(m.active, coreID)))
	if m.options.PrepareGitTrust != nil {
		gitTrustStep(t, m, next)
	}
}

func TestGitTrustReviewFlow(t *testing.T) {
	var checks, grants int
	var backend *gitTrustFixtureBackend
	m, s := demo(t, Options{Fills: FillOff, PrepareGitTrust: func(_ context.Context, cwd string) (GitTrustPreview, error) {
		checks++
		if cwd != "/work/project/src" {
			return GitTrustPreview{}, errors.New("wrong review workspace")
		}
		return GitTrustPreview{GitDirectory: "/work/project/.git", Grant: func(context.Context) error {
			grants++
			backend.err = nil
			return nil
		}}, nil
	}})
	backend = setupGitTrustFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 85, Height: 22})
	m.editor.set("keep this draft")
	m.Update(gitDiffSummaryMsg{lease: m.gitDiffFeed.token, err: gitTrustDenied})
	if m.focusedDialog() != nil || checks != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "± needs trust") {
		t.Fatal("polling should only update the badge")
	}
	next := gitTrustStep(t, m, m.openGitDiff(m.route(m.active, coreID)))
	if !strings.Contains(ansi.Strip(assertFrame(t, m)), "Checking local repository") {
		t.Fatal("no local trust preview state")
	}
	gitTrustStep(t, m, next)
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Trust this repository?", `/work/project/.git`, "linked worktrees", "trust repository", "esc cancel"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in confirmation:\n%s", want, frame)
		}
	}
	if cmd := press(m, tea.KeyEnter, 0); cmd != nil || grants != 0 || m.focusedDialog() == nil {
		t.Fatal("Enter must not grant trust by default")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil || checks != 1 || grants != 0 || m.editor.value() != "keep this draft" {
		t.Fatal("dismissing the prompt changed trust or the draft")
	}

	openGitTrustFixture(t, m)
	assertFrame(t, m)
	row := slices.Index(m.frame.dialogChoices, gitTrustTarget)
	if row < 0 {
		t.Fatal("no rendered trust target")
	}
	_, confirm := m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: m.frame.dialogX + 2, Y: m.frame.dialogY + row}))
	if grants != 0 || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Saving trust") {
		t.Fatal("confirmation should schedule, not perform, the grant")
	}
	oldLease := m.gitDiffFeed.token
	m.gitDiffFeed.pending = true // a pre-grant reply can still be in flight
	gitTrustStep(t, m, gitTrustStep(t, m, confirm))
	m.Update(gitDiffSummaryMsg{lease: oldLease, err: gitTrustDenied})
	if grants != 1 || checks != 2 || m.focusedDialog().git.trust != nil ||
		!strings.Contains(ansi.Strip(assertFrame(t, m)), "src/auth/session.ts") || s.gitDiff.badge != gitDiffBadge(diffFixtureSummary(sdk.GitDiffHead)) {
		t.Fatal("confirmed grant did not reload the worktree review")
	}
}

func TestGitTrustAttachedHostInstructions(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, WorkspaceLocal: true})
	backend := setupGitTrustFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	// A review opened before the first poll completed should also explain the denial.
	s.gitDiff = gitDiffState{}
	m.openGitDiff(m.route(m.active, coreID))
	m.Update(gitDiffSummaryMsg{lease: m.gitDiffFeed.token, err: gitTrustDenied})
	frame := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"Workspace trust required", "On the backend host, run:", "hush trust grant --cwd PATH", "PATH = /work/project/src"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in host prompt:\n%s", want, frame)
		}
	}
	if slices.Contains(m.frame.dialogChoices, gitTrustTarget) || press(m, 't', 0) != nil {
		t.Fatal("attached client should not offer a grant")
	}
	backend.err = &sdk.ServerError{Code: "invalid_request", Message: "Managed checkout requires a Git working tree."}
	gitTrustStep(t, m, press(m, 'u', 0))
	if m.focusedDialog().git.trust != nil {
		t.Fatal("a different Git error was treated as a trust denial")
	}
}

func TestGitTrustRejectsStaleConfirmation(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, PrepareGitTrust: gitTrustPreviewFixture("/work/project/.git")})
	setupGitTrustFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 85, Height: 22})
	openGitTrustFixture(t, m)
	assertFrame(t, m)
	s.info.Workspace = "/work/another-repository"
	if press(m, 't', 0) != nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Workspace changed") {
		t.Fatal("confirmation used stale thread workspace")
	}
	preview := gitTrustStep(t, m, press(m, 'u', 0))
	press(m, tea.KeyEscape, 0)
	gitTrustStep(t, m, preview)
	if m.focusedDialog() != nil {
		t.Fatal("a late preview reopened the dismissed prompt")
	}
}

func TestGitTrustDoesNotRepromptOnDeniedRetry(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, PrepareGitTrust: gitTrustPreviewFixture("/work/project/.git")})
	setupGitTrustFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 85, Height: 22})
	openGitTrustFixture(t, m)
	assertFrame(t, m)
	// This fixture accepts the local grant but keeps denying the Git review.
	gitTrustStep(t, m, gitTrustStep(t, m, press(m, 't', 0)))
	m.Update(gitDiffSummaryMsg{lease: m.gitDiffFeed.token, err: gitTrustDenied})
	if m.focusedDialog().git.trust != nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Workspace trust required") {
		t.Fatal("a denied retry should stay in the review without reopening the prompt")
	}
}

func testGitTrustFrameGeometry(t *testing.T) {
	path := "/work/" + strings.Repeat("wide界", 18) + "/project/.git"
	m, s := demo(t, Options{Fills: FillOff, PrepareGitTrust: gitTrustPreviewFixture(path)})
	setupGitTrustFixture(m, s)
	m.editor.set(strings.Repeat("draft \n", 12) + "end of draft")
	openGitTrustFixture(t, m)
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {140, 40}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		frame := assertFrame(t, m)
		if frame != assertFrame(t, m) || !strings.Contains(ansi.Strip(frame), "end of draft") {
			t.Fatalf("trust prompt lost draft or changed frame at %v", size)
		}
		if size[0] == 40 && (slices.Contains(m.frame.dialogChoices, gitTrustTarget) || press(m, 't', 0) != nil) {
			t.Fatal("must not confirm a path that cannot fit in the prompt")
		}
		if size[0] >= 80 && !slices.Contains(m.frame.dialogChoices, gitTrustTarget) {
			t.Fatalf("no trust target at %v", size)
		}
	}
}
