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

type workspaceMetadataFixture struct {
	inertBackend
	caps    map[string]uint64
	context func(context.Context, sdk.ID) ([]sdk.InstructionSource, error)
	skills  func(sdk.ID) []sdk.WorkspaceSkill
	submit  func(sdk.Input) (sdk.Receipt, error)
}

func (f workspaceMetadataFixture) Hello(context.Context) (sdk.Hello, error) {
	return sdk.Hello{Capabilities: f.caps}, nil
}
func (f workspaceMetadataFixture) LoadedContext(ctx context.Context, id sdk.ID) ([]sdk.InstructionSource, error) {
	return f.context(ctx, id)
}
func (f workspaceMetadataFixture) ListWorkspaceSkills(_ context.Context, id sdk.ID) ([]sdk.WorkspaceSkill, error) {
	if f.skills == nil {
		return nil, nil
	}
	return f.skills(id), nil
}
func (f workspaceMetadataFixture) Submit(_ context.Context, input sdk.Input) (sdk.Receipt, error) {
	return f.submit(input)
}

func testLoadedContextFrameGeometry(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true, Fills: FillOff})
	m.nativeContext = true
	d := &dialog{route: m.route(m.active, menusID), token: m.token(), title: "Loaded context", loadedContext: &contextDialog{ready: true}}
	m.setContextOutput(d, "Personal · APPEND_SYSTEM.md\n  /backend/home/"+strings.Repeat("界 long", 24)+"/APPEND_SYSTEM.md")
	m.pushDialog(d)
	for _, size := range [][2]int{{40, 12}, {64, 16}, {100, 24}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		assertFrame(t, m)
	}
}

func TestGoldenLoadedContextFromPalette(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		local, skills bool
	}{
		{"normal", 96, 24, true, true}, {"narrow", 40, 12, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, s := demo(t, Options{ReducedMotion: true, Fills: FillOff, WorkspaceLocal: tc.local})
			m.Update(tea.WindowSizeMsg{Width: tc.width, Height: tc.height})
			caps := map[string]uint64{"loadedContext": sdk.LoadedContextVersion}
			if tc.skills {
				caps["workspaceSkills"] = 1
				caps["loadedContext"] = sdk.LoadedContextAgentsVersion
			}
			m.backend = workspaceMetadataFixture{caps: caps,
				context: func(_ context.Context, id sdk.ID) ([]sdk.InstructionSource, error) {
					if id != "1" {
						t.Fatal("context used the wrong scope")
					}
					return []sdk.InstructionSource{{Kind: "personal", Path: "/backend/home/.hush/APPEND_SYSTEM.md"},
						{Kind: "workspace", Path: "/backend/workspace/AGENTS.md"}, {Kind: "workspace", Path: "/backend/workspace/nested folder/AGENTS.md"}}, nil
				}, skills: func(sdk.ID) []sdk.WorkspaceSkill {
					return []sdk.WorkspaceSkill{{Name: "local-review", Source: "workspace", Description: "A text skill", Token: strings.Repeat("a", 90)}}
				}}
			_, initial := m.Update(m.menuProbe()())
			if tc.skills {
				m.Update(initial().(skillsMsg))
			}
			press(m, '?', 0)
			d := m.focusedDialog()
			if d == nil || !d.palette {
				t.Fatal("? did not open the command palette")
			}
			m.changeDialogQuery(d, "show loaded context")
			frame := ansi.Strip(assertFrame(t, m))
			if c, ok := d.choice(); !ok || c.ID != string(loadedContextID) || !strings.Contains(frame, "Show loaded") {
				t.Fatalf("action not discoverable: %v\n%s", c, frame)
			}
			workflowWork(t, m, press(m, tea.KeyEnter, 0))
			if m.focusedDialog() == nil || m.focusedDialog().loadedContext == nil {
				t.Fatal("missing inspector")
			}
			if frame := ansi.Strip(assertFrame(t, m)); strings.Contains(frame, "s skills") != tc.skills || !strings.Contains(frame, "Personal") {
				t.Fatal(frame)
			}
			golden(t, "loaded-context-"+tc.name, m)
			if tc.skills {
				workflowWork(t, m, press(m, 's', 0))
				if m.sheet == nil || !strings.Contains(ansi.Strip(assertFrame(t, m)), "local-review") {
					t.Fatal("missing skills picker")
				}
				press(m, tea.KeyEnter, 0)
				if m.editor.value() != "/skill:local-review " || threadBusy(s) || s.stagedSkill.token == "" {
					t.Fatalf("selection did more or less than stage the skill: %q (busy=%t staged=%t)", m.editor.value(), threadBusy(s), s.stagedSkill.token != "")
				}
			} else {
				press(m, tea.KeyEscape, 0)
				if m.editor.value() != "" || threadBusy(s) {
					t.Fatal("inspection changed the draft or run state")
				}
			}
		})
	}
}

func TestLoadedContextDiscardsLateRepliesAfterThreadSwitch(t *testing.T) {
	m, s := demo(t, Options{ReducedMotion: true})
	f := workspaceMetadataFixture{caps: map[string]uint64{"loadedContext": 1}, context: func(_ context.Context, id sdk.ID) ([]sdk.InstructionSource, error) {
		return []sdk.InstructionSource{{Kind: "workspace", Path: "/work/" + string(id) + "/AGENTS.md"}}, nil
	}}
	m.backend = f
	m.Update(m.menuProbe()())
	m.editor.set("keep this draft")
	cmd := m.runAction(m.host.byID[loadedContextID], "")
	late := cmd().(workMsg)
	_ = m.switchThread("2")
	m.connection = x.Connected
	workflowWork(t, m, m.runAction(m.host.byID[loadedContextID], ""))
	m.finishWork(late)
	frame := ansi.Strip(assertFrame(t, m))
	if !strings.Contains(frame, "/work/2/AGENTS.md") || strings.Contains(frame, "/work/1/AGENTS.md") {
		t.Fatal(frame)
	}
	if s.draft.text != "keep this draft" || threadBusy(s) || threadBusy(m.threads["2"]) {
		t.Fatal("inspection changed the draft or run")
	}
	press(m, tea.KeyEscape, 0)
	f.caps = nil
	m.backend = f
	m.Update(m.menuProbe()())
	press(m, '?', 0)
	for _, item := range m.focusedDialog().paletteItems {
		if item.ID == string(loadedContextID) {
			t.Fatal("older host exposed the inspector")
		}
	}
}

func TestStandaloneSkillSuggestionsStageAndSendExactSource(t *testing.T) {
	var got []sdk.Input
	token := strings.Repeat("A", 80)
	m, s := demo(t, Options{ReducedMotion: true})
	m.backend = workspaceMetadataFixture{caps: map[string]uint64{"workspaceSkills": 1}, skills: func(id sdk.ID) []sdk.WorkspaceSkill {
		if id != "1" {
			return nil
		}
		return []sdk.WorkspaceSkill{{Name: "review", Description: "A scoped skill", Source: "workspace", Token: token}}
	}, submit: func(input sdk.Input) (sdk.Receipt, error) {
		got = append(got, input)
		return sdk.Receipt{}, &sdk.ServerError{Code: "invalid_request", Message: "stale"}
	}}
	_, initial := m.Update(m.menuProbe()())
	m.Update(initial().(skillsMsg))
	m.editor.set("/")
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "/skill:review") {
		t.Fatal(frame)
	}
	m.editor.set("/skill:rev")
	press(m, tea.KeyEnter, 0)
	if len(got) != 0 || m.editor.value() != "/skill:review " || s.stagedSkill.token != token {
		t.Fatal("picker submitted or lost the source pin")
	}
	m.editor.set("/skill:review keep these arguments")
	m.finishWork(press(m, tea.KeyEnter, 0)().(workMsg))
	if len(got) != 1 || got[0].SkillToken != token || s.stagedSkill.token != token || m.editor.value() != got[0].Text {
		t.Fatal("the rejected draft lost its pin")
	}
	m.editor.set("")
	m.Update(tea.FocusMsg{})
	m.editor.set("/skill:review typed explicitly")
	m.finishWork(press(m, tea.KeyEnter, 0)().(workMsg))
	if len(got) != 2 || got[1].SkillToken != "" {
		t.Fatal("typed command inherited a stale source pin")
	}
	m.editor.set("/skill:rev")
	m.View()
	var drawn x.ActionRef
	for _, row := range m.frame.hits {
		for _, h := range row {
			if h.action.ID == stageSkillID {
				drawn = h.action
			}
		}
	}
	if drawn.ID != stageSkillID {
		t.Fatal("skill row had no click action")
	}
	changed := append([]sdk.WorkspaceSkill(nil), m.skills.listed...)
	changed[0].Token = strings.Repeat("B", 80)
	m.receiveSkills(skillsMsg{m.skills.generation, changed})
	budget := maxEffects
	m.invoke(m.route(m.active, menusID), drawn, &budget)
	if s.stagedSkill.token != token {
		t.Fatal("click on a stale row selected the replacement source")
	}
}
