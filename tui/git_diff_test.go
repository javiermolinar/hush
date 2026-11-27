package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	return sdk.GitDiffSummary{Revision: diffFixtureRevision, Range: rangeName, Checkpoint: true, Branch: "feature/review",
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

func longDiffFixturePatch() sdk.GitDiffPatch {
	patch := diffFixturePatch(0)
	for i := 44; i <= 80; i++ {
		text := fmt.Sprintf("  // fixture line %d", i)
		if i == 75 {
			text += strings.Repeat(" ../wide/界/fixture", 25)
		}
		patch.Lines = append(patch.Lines, sdk.GitDiffLine{Kind: "add", NewLine: i, Text: text})
	}
	patch.Truncated = true
	return patch
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
	s.info.Workspace = "/work/project/src"
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
	if got := m.frame.dialogY + m.frame.dialogHeight; got != m.frame.ruleY {
		t.Fatalf("changes list not docked at the input rule: %d != %d", got, m.frame.ruleY)
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
	for _, want := range []string{"src/auth/session.ts · last checkpoint", "1/4", "/work/project/src  ⎇ feature/review", "42 −   if (!lock)", "42 +   const lock", "␛[31m", "unsent draft", "± 4 files"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if m.frame.dialogY+m.frame.dialogHeight != m.frame.ruleY || m.frame.dialogHeight > m.height*7/10 {
		t.Fatal("patch is not docked and capped")
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

func TestGitDiffScopeWithoutBranch(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff})
	enableGitDiffFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.openGitDiff(m.route(m.active, coreID))
	d := m.focusedDialog()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "No changes"},
		{errors.New("This directory is not a Git worktree."), "This directory is not a Git worktree."},
	} {
		d.git.apply(sdk.GitDiffSummary{Revision: diffFixtureRevision}, tc.err)
		rows := m.gitReviewRows(d, 70, 6).rows
		text := ansi.Strip(strings.Join(rows, "\n"))
		if !strings.Contains(text, "/work/project/src\n"+tc.want) || strings.Contains(text, "⎇") {
			t.Fatalf("unexpected review scope:\n%s", text)
		}
	}
	const width = 28
	scope := gitReviewScope("/work/"+strings.Repeat("界", 20)+"/src", "main", width)
	if !strings.HasPrefix(scope, "…") || !strings.HasSuffix(scope, "/src  ⎇ main") || stringWidth(scope) > width {
		t.Fatalf("narrow review scope: %q", scope)
	}
}

func TestGitDiffPatchScrollKeepsPanelFixed(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	enableGitDiffFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("keep this draft")
	m.openGitDiff(m.route(m.active, coreID))
	d := m.focusedDialog()
	d.git.patch, d.git.view = true, newGitPatchView(longDiffFixturePatch())
	assertFrame(t, m)
	first := *m.frame
	body := func(f *frameMemo) []string {
		start := f.dialogY + f.dialogBodyStart
		return f.rows[start+1 : start+f.dialogBodyHeight] // exclude the workspace context
	}
	start := first.dialogY + first.dialogBodyStart + 1
	end := first.dialogY + first.dialogBodyStart + first.dialogBodyHeight
	check := func() []string {
		t.Helper()
		assertFrame(t, m)
		f := m.frame
		if m.focusedDialog() != d || f.dialogY != first.dialogY || f.dialogHeight != first.dialogHeight || f.ruleY != first.ruleY ||
			!slices.Equal(f.rows[:start], first.rows[:start]) || !slices.Equal(f.rows[end:], first.rows[end:]) {
			t.Fatalf("scroll changed rows outside the diff body: panel top %d → %d, height %d → %d", first.dialogY, f.dialogY, first.dialogHeight, f.dialogHeight)
		}
		return body(f)
	}
	wheel := func(button tea.MouseButton, y int) {
		m.Update(tea.MouseWheelMsg(tea.Mouse{Button: button, X: first.dialogX + 2, Y: y}))
	}
	press(m, tea.KeyDown, 0)
	afterDown := check()
	if !slices.Equal(body(&first)[1:], afterDown[:len(afterDown)-1]) {
		t.Fatal("Down did not scroll one rendered row")
	}
	wheel(tea.MouseWheelDown, start)
	if afterWheel := check(); !slices.Equal(afterDown[3:], afterWheel[:len(afterWheel)-3]) {
		t.Fatal("wheel did not scroll three rendered rows")
	}
	for range 8 {
		press(m, tea.KeyPgDown, 0)
		check()
	}
	bottom := check()
	if got := ansi.Strip(bottom[len(bottom)-1]); !strings.Contains(got, "fixture line 80") {
		t.Fatalf("last row of the patch not visible: %q", got)
	}
	press(m, tea.KeyDown, 0)
	wheel(tea.MouseWheelDown, start)
	if !slices.Equal(bottom, check()) {
		t.Fatal("scrolled past the last full page")
	}
	wheel(tea.MouseWheelUp, first.ruleY)
	if !slices.Equal(bottom, check()) {
		t.Fatal("wheel outside the panel scrolled the diff")
	}
	wheel(tea.MouseWheelUp, start)
	if afterWheel := check(); !slices.Equal(bottom[:len(bottom)-3], afterWheel[3:]) {
		t.Fatal("wheel did not scroll back three rendered rows")
	}
	press(m, tea.KeyPgDown, 0)
	if !slices.Equal(bottom, check()) {
		t.Fatal("Page Down did not return to the last page")
	}

	// A larger viewport must fill from above the old top near the end of the diff.
	m.Update(tea.WindowSizeMsg{Width: 112, Height: 36})
	assertFrame(t, m)
	resized := body(m.frame)
	if m.frame.dialogHeight != m.height*7/10 || !strings.Contains(ansi.Strip(resized[len(resized)-1]), "fixture line 80") {
		t.Fatal("resizing at the end did not fill the viewport")
	}
	press(m, tea.KeyUp, 0)
	assertFrame(t, m)
	if !slices.Equal(resized[:len(resized)-1], body(m.frame)[1:]) {
		t.Fatal("Up did not scroll from the top of the resized viewport")
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
			if after := assertFrame(t, m); size[1] >= 24 && after != v {
				t.Fatal("a patch that fits scrolled")
			}
		})
	}
}

func TestGitDiffGoldens(t *testing.T) {
	m, s := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	enableGitDiffFixture(m, s)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	s.info.Status = x.Running
	m.editor.set("keep this draft")
	m.openGitDiff(m.route(m.active, coreID))
	golden(t, "git-diff-list", m)
	d := m.focusedDialog()
	d.git.patch, d.git.view = true, newGitPatchView(diffFixturePatch(0))
	golden(t, "git-diff-file", m)
	d.git.view = newGitPatchView(longDiffFixturePatch())
	assertFrame(t, m)
	for range 8 {
		press(m, tea.KeyPgDown, 0)
		assertFrame(t, m)
	}
	golden(t, "git-diff-file-bottom", m)
}
