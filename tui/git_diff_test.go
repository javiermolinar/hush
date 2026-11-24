package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const diffFixtureRevision = "129cb6d4-9d9b-4757-a711-b7b1fd839a5f"

func diffFixtureSummary(rangeName string) sdk.GitDiffSummary {
	add, remove := int64(41), int64(9)
	return sdk.GitDiffSummary{Revision: diffFixtureRevision, Range: rangeName, Checkpoint: true,
		Files: []sdk.GitDiffFile{{Path: "src/auth/session.ts", Status: "M", Added: &add, Removed: &remove, Staged: true, Text: true},
			{Path: "src/auth/lock.ts", Status: "A", Added: &add, Removed: new(int64), Text: true},
			{Path: "src/auth/legacy.ts", Status: "D", Added: new(int64), Removed: &remove, Text: true},
			{Path: "src/auth/fixture\x1b.png", Status: "?"}}}
}

func diffFixturePatch(index int) sdk.GitDiffPatch {
	return sdk.GitDiffPatch{Revision: diffFixtureRevision, File: index, Lines: []sdk.GitDiffLine{
		{Kind: "hunk", Text: "@@ -41,2 +41,2 @@ refreshSession()"},
		{Kind: "context", OldLine: 41, NewLine: 41, Text: "  const token = await refresh();"},
		{Kind: "remove", OldLine: 42, Text: "  if (!lock) lock = acquire();"},
		{Kind: "add", NewLine: 42, Text: "  const lock = await mutex.take(key);"},
		{Kind: "add", NewLine: 43, Text: "  // output with \x1b[31m and a long path " + strings.Repeat("界", 45)},
	}}
}

type gitDiffFixtureBackend struct{ inertBackend }

func (gitDiffFixtureBackend) GitDiffSummary(_ context.Context, _ sdk.ID, rangeName string) (sdk.GitDiffSummary, error) {
	return diffFixtureSummary(rangeName), nil
}
func (gitDiffFixtureBackend) GitDiffFile(_ context.Context, _ sdk.ID, _ string, i int) (sdk.GitDiffPatch, error) {
	return diffFixturePatch(i), nil
}

func enableGitDiffFixture(m *Model, s *threadState) {
	m.backend = gitDiffFixtureBackend{}
	m.menuFeaturesFromBackend |= x.GitDiff
	m.gitDiffFeed = gitDiffPoll{feedLease: feedLease{token: m.token(), owner: s.info.ID, ctx: m.ctx}, pending: true}
	s.gitDiff.summary = diffFixtureSummary(sdk.GitDiffHead)
	s.gitDiff.badge = gitDiffBadge(s.gitDiff.summary)
}

func TestGitDiffReviewFlow(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff})
	enableGitDiffFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.editor.set("unsent draft")
	assertFrame(t, m)
	var badgeFound bool
	for y, row := range m.frame.hits {
		for _, hit := range row {
			if hit.action.ID != gitDiffID {
				continue
			}
			badgeFound = true
			if !strings.Contains(ansi.Strip(ansi.Cut(m.frame.rows[y], hit.first, hit.last)), "± 4 files") {
				t.Fatalf("badge hit not over rendered label: %+v", hit)
			}
			m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: hit.first, Y: y}))
			break
		}
	}
	if !badgeFound || m.focusedDialog() == nil || m.focusedDialog().git == nil {
		t.Fatal("click did not open review")
	}
	m.View()
	d := m.focusedDialog()
	if got := m.frame.dialogY + m.frame.dialogHeight; got != m.frame.transcriptHeight {
		t.Fatalf("popover not anchored at bottom of transcript: %d != %d", got, m.frame.transcriptHeight)
	}
	cmd := press(m, tea.KeyTab, 0)
	if cmd == nil {
		t.Fatal("no comparison request")
	}
	m.Update(cmd())
	if d.git.list.Range != sdk.GitDiffCheckpoint {
		t.Fatalf("wrong range %+v", d.git.list)
	}
	assertFrame(t, m)
	cmd = nil
	for i, target := range m.frame.dialogChoices {
		if target != 0 {
			continue
		}
		_, cmd = m.Update(tea.MouseClickMsg(tea.Mouse{Button: tea.MouseLeft, X: m.frame.dialogX + 2, Y: m.frame.dialogY + i}))
		break
	}
	if cmd == nil {
		t.Fatal("file row did not open a patch")
	}
	m.Update(cmd())
	body := ansi.Strip(assertFrame(t, m))
	for _, want := range []string{"src/auth/session.ts · 1/4 · last checkpoint", "42 −   if (!lock)", "42 +   const lock", "␛[31m", "unsent draft", "± 4 files"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if m.frame.dialogHeight != m.frame.transcriptHeight || strings.Contains(body, "What changed in") {
		t.Fatal("patch did not cover the transcript")
	}
	cmd = press(m, 'n', 0)
	if cmd == nil {
		t.Fatal("next file did not start a load")
	}
	stale := cmd()
	press(m, tea.KeyEscape, 0)
	m.Update(stale)
	if d.git.patch || d.git.view != nil || m.focusedDialog() != d {
		t.Fatal("Escape did not return to the list or a late patch was accepted")
	}
	press(m, tea.KeyEscape, 0)
	if m.focusedDialog() != nil || m.editor.value() != "unsent draft" || !m.stopAt.IsZero() {
		t.Fatal("review changed chat state")
	}
	oldLease := m.gitDiffFeed.token
	m.switchThread("2")
	m.Update(gitDiffSummaryMsg{lease: oldLease, value: diffFixtureSummary(sdk.GitDiffHead)})
	if m.threads["2"].gitDiff.badge != "" || m.threads["2"].gitDiff.summary.Revision != "" {
		t.Fatal("late summary leaked across tabs")
	}
}

func TestGitDiffFrameGeometry(t *testing.T) {
	for _, size := range [][2]int{{40, 12}, {64, 16}, {80, 24}, {140, 40}, {200, 60}} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m, s := demo(t, Options{Fills: FillOff})
			enableGitDiffFixture(m, s)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			m.editor.set(strings.Repeat("draft\n", 16) + "visible end")
			m.openGitDiff(m.route(m.active, coreID))
			assertFrame(t, m)
			d := m.focusedDialog()
			d.git.patch, d.git.view = true, newGitPatchView(diffFixturePatch(0))
			v := assertFrame(t, m)
			if v != assertFrame(t, m) || !strings.Contains(ansi.Strip(v), "visible end") {
				t.Fatal("patch changed geometry or lost the prompt")
			}
			press(m, tea.KeyPgDown, 0)
			assertFrame(t, m)
		})
	}
}

func TestGitDiffGoldens(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	enableGitDiffFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 76, Height: 18})
	s.info.Status = x.Running
	m.editor.set("keep this draft")
	m.openGitDiff(m.route(m.active, coreID))
	golden(t, "git-diff-list", m)
	d := m.focusedDialog()
	d.git.patch, d.git.view = true, newGitPatchView(diffFixturePatch(0))
	golden(t, "git-diff-file", m)
}
