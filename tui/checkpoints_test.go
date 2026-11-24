package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

type checkpointFixture struct {
	menuBackend
	preview   sdk.RewindPreview
	decisions []sdk.RewindDecision
}

func (f *checkpointFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: map[string]uint64{"checkpoints": sdk.CheckpointsVersion}}, nil
}

func (f *checkpointFixture) ListCheckpoints(context.Context, sdk.ID) ([]sdk.Checkpoint, error) {
	return []sdk.Checkpoint{{ID: "77397caf-ceff-47c1-bab3-59b820b87eb5", Kind: "files", Label: "Fixture checkpoint", AtEntryID: "2"}}, nil
}
func (f *checkpointFixture) RecordCheckpoint(context.Context, sdk.CheckpointInput) (sdk.Checkpoint, error) {
	return sdk.Checkpoint{}, sdk.ErrProtocol
}
func (f *checkpointFixture) RecordKnownEffect(context.Context, sdk.ID, string) (sdk.ID, error) {
	return "", sdk.ErrProtocol
}
func (f *checkpointFixture) PreviewRewind(context.Context, sdk.RewindInput) (sdk.RewindPreview, error) {
	return f.preview, nil
}
func (f *checkpointFixture) ConfirmRewind(_ context.Context, d sdk.RewindDecision) (sdk.RewindResult, error) {
	f.decisions = append(f.decisions, d)
	return sdk.RewindResult{ID: d.ID, Mode: d.Mode, State: "failed", ThreadID: d.ThreadID, AtEntryID: f.preview.AtEntryID,
		Reason: "Staging failed verification"}, nil
}
func (f *checkpointFixture) RewindStatus(context.Context, sdk.ID, string) (sdk.RewindResult, error) {
	return sdk.RewindResult{}, sdk.ErrProtocol
}

func TestRewindReviewShowsExactBranchAndRecordedEffectsBeforeConfirmation(t *testing.T) {
	m, _ := demo(t, Options{Fills: FillOff, ReducedMotion: true})
	p := sdk.RewindPreview{ID: "3df259fa-f60e-49d6-b09d-7937e2fedd3e", ThreadID: "1", AtEntryID: "2", Mode: sdk.RewindFiles,
		Files: &sdk.FileRestore{Commit: strings.Repeat("a", 40), Repository: "/workspace/.git",
			Ref: "refs/hush/checkpoints/77397caf-ceff-47c1-bab3-59b820b87eb5",
			CWD: "/private/runtime/rewind-worktrees-v1/3df259fa-f60e-49d6-b09d-7937e2fedd3e", Branch: "hush/rewind/3df259fa-f60e-49d6-b09d-7937e2fedd3e"},
		Effects: []sdk.KnownEffect{{EntryID: "9", Text: "Published fixture release"}}}
	f := &checkpointFixture{preview: p}
	m.backend = f
	m.Update(m.menuProbe()())
	m.Update(tea.WindowSizeMsg{Width: 144, Height: 28})
	frame := func(text string) {
		t.Helper()
		if got := ansi.Strip(assertFrame(t, m)); !strings.Contains(got, text) {
			t.Fatalf("want %q\n%s", text, got)
		}
	}
	done := func(cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("expected async backend work")
		}
		switch msg := cmd().(type) {
		case workMsg:
			m.finishWork(msg)
		case tea.BatchMsg:
			for _, f := range msg {
				if w, ok := f().(workMsg); ok {
					m.finishWork(w)
				}
			}
		default:
			t.Fatalf("expected work, got %T", msg)
		}
	}
	done(m.runAction(m.host.byID[menusID+".rewind"], ""))
	frame("Fixture checkpoint")
	press(m, tea.KeyEnter, 0)
	frame("Transcript and committed files")
	press(m, tea.KeyDown, 0)
	done(press(m, tea.KeyEnter, 0))
	frame("Review rewind")
	frame(p.Files.Commit)
	frame(p.Files.Branch)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	assertFrame(t, m)
	m.Update(tea.WindowSizeMsg{Width: 144, Height: 28})
	assertFrame(t, m)
	press(m, tea.KeyEnd, 0)
	frame("Published fixture release")
	frame("Other effects may not be recorded")
	press(m, tea.KeyEnter, 0)
	frame("Restore into the new worktree and fork?")
	if len(f.decisions) != 0 {
		t.Fatal("preview or review started restore")
	}
	press(m, tea.KeyDown, 0)
	done(press(m, tea.KeyEnter, 0))
	frame("Preview transcript-only fork")
	if len(f.decisions) != 1 || f.decisions[0] != (sdk.RewindDecision{ThreadID: "1", ID: p.ID, Mode: sdk.RewindFiles}) {
		t.Fatalf("incorrect confirmation: %#v", f.decisions)
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	assertFrame(t, m)
	if m.active != "1" {
		t.Fatal("failed staging changed the active thread")
	}
}
