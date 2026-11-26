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

type draftSkillFixture struct {
	workspaceMetadataFixture
	preview func(string) []sdk.WorkspaceSkill
	create  func(sdk.NewThread) (sdk.Thread, error)
}

func (f draftSkillFixture) PreviewWorkspaceSkills(_ context.Context, cwd string) ([]sdk.WorkspaceSkill, error) {
	return f.preview(cwd), nil
}
func (f draftSkillFixture) NewThread(_ context.Context, input sdk.NewThread) (sdk.Thread, error) {
	return f.create(input)
}

func TestGoldenWorkspaceSkillsOnFreshLocalPrompt(t *testing.T) {
	const cwd = "/workspace with spaces"
	pin := strings.Repeat("A", 80)
	m := modelFor(t, Options{NewThread: sdk.NewThread{CWD: cwd}, WorkspaceLocal: true, ReducedMotion: true, Intro: &IntroOptions{}})
	var previewed, created, submitted int
	m.backend = draftSkillFixture{
		workspaceMetadataFixture: workspaceMetadataFixture{
			caps: map[string]uint64{"workspaceSkills": sdk.WorkspaceSkillDraftVersion},
			submit: func(input sdk.Input) (sdk.Receipt, error) {
				submitted++
				if input.ThreadID != "2" || input.Text != "/skill:review " || input.SkillToken != pin {
					t.Fatalf("new-thread input lost draft scope: %+v", input)
				}
				return sdk.Receipt{SubmissionID: "3"}, nil
			},
		},
		preview: func(path string) []sdk.WorkspaceSkill {
			previewed++
			if path != cwd {
				t.Fatalf("previewed %q, want %q", path, cwd)
			}
			return []sdk.WorkspaceSkill{{Name: "review", Description: "Read the draft", Source: "workspace", Token: pin}}
		},
		create: func(input sdk.NewThread) (sdk.Thread, error) {
			created++
			if input.CWD != cwd {
				t.Fatalf("creating in %q, want %q", input.CWD, cwd)
			}
			return sdk.Thread{ID: "2"}, nil
		},
	}
	_, initial := m.Update(m.menuProbe()())
	if initial == nil || m.active != "" || m.connection == x.Connected {
		t.Fatal("expected preview on an uncreated draft")
	}
	msg, ok := initial().(skillsMsg)
	if !ok {
		t.Fatal("expected a passive draft lookup")
	}
	m.Update(msg)
	if m.intro.standaloneSkillCount == nil || *m.intro.standaloneSkillCount != 1 {
		t.Fatal("preview did not feed the intro count")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.editor.set("/skills")
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	golden(t, "menu-skills", m)
	if frame := ansi.Strip(m.View().Content); !strings.Contains(frame, "Loaded skills") || !strings.Contains(frame, "review") {
		t.Fatal(frame)
	}
	press(m, tea.KeyEnter, 0)
	if m.active != "" || m.editor.value() != "/skill:review " || previewed != 2 || created != 0 || submitted != 0 {
		t.Fatal("selecting a draft skill created a thread or submitted")
	}
	workflowWork(t, m, press(m, tea.KeyEnter, 0))
	if m.active != "2" || created != 1 || submitted != 1 {
		t.Fatal("explicit submission did not create exactly one thread")
	}
}

func TestWorkspaceSkillsDoNotPreviewRemoteOrOlderDrafts(t *testing.T) {
	for _, local := range []bool{true, false} {
		m := modelFor(t, Options{WorkspaceLocal: local, NewThread: sdk.NewThread{CWD: "/remote path"}, ReducedMotion: true})
		version := uint64(sdk.WorkspaceSkillDraftVersion)
		if local {
			version = sdk.WorkspaceSkillsVersion
		}
		m.backend = draftSkillFixture{workspaceMetadataFixture: workspaceMetadataFixture{caps: map[string]uint64{"workspaceSkills": version}}}
		_, cmd := m.Update(m.menuProbe()())
		if cmd != nil || m.menuFeatures().Has(x.Skills) {
			t.Fatal("unsupported or remote draft advertised a local skill preview")
		}
	}
}
