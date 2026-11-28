package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestGitDiffActionBeforeFirstThread(t *testing.T) {
	var cwds []string
	m := modelFor(t, Options{ReducedMotion: true, Fills: FillOff, WorkspaceLocal: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.backend = gitDiffLaunchFixture{cwds: &cwds}
	m.menuFeaturesFromBackend |= x.GitDiff
	m.gitDiffLaunchReady = true
	m.Update(m.syncGitDiff()())
	if m.threads[""].gitDiff.badge == "" {
		t.Fatal("fixture did not get the gitDiff:2 launch badge")
	}
	m.editor.set("/diff")
	if len(m.slashMatches()) == 0 {
		t.Error("/diff is missing from available slash actions")
	}
	m.runAction(m.host.byID[gitDiffID], "")
	if d := m.focusedDialog(); d == nil || d.git == nil {
		t.Error("dispatching the registered Git action does not open review")
	}
}

func TestGitDiffLaunchFilesNotCarriedIntoThread(t *testing.T) {
	var cwds []string
	m := modelFor(t, Options{ReducedMotion: true, Fills: FillOff, WorkspaceLocal: true})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.backend = gitDiffLaunchFixture{cwds: &cwds}
	m.menuFeaturesFromBackend |= x.GitDiff
	m.gitDiffLaunchReady = true
	m.Update(m.syncGitDiff()())
	startup := m.threads[""].gitDiff
	if startup.summary.Revision == "" {
		t.Fatal("missing launch summary")
	}
	s := m.prepareDraftThread("7", sdk.Model{}, "")
	if s.gitDiff.badge != startup.badge {
		t.Fatal("lost startup badge")
	}
	m.switchThread("7") // Do not let the new thread's scan settle before opening the panel.
	m.openGitDiff(m.route("7", coreID))
	if d := m.focusedDialog(); d != nil && d.git != nil && d.git.list.Revision == startup.summary.Revision {
		t.Fatal("thread review uses the launch directory's file locator")
	}
}
