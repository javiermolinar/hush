package tui

import (
	"slices"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

func TestSlashSearchRanksCommandsAndSkills(t *testing.T) {
	ext := x.Registration{ID: "fixture/slash", APIVersion: x.APIVersion}
	for _, name := range []string{"theme-alt", "model-alt"} {
		ext.Actions = append(ext.Actions, x.Action{
			ID: x.ActionID(ext.ID + "." + name), Title: name,
			Binding: x.Binding{Leader: "7 " + name[:1]}, Slash: &x.Slash{Name: name},
			Run: func(x.Context, x.Invocation) x.Outcome { return nil },
		})
	}
	m, _ := demo(t, Options{Extensions: []x.Registration{ext}})
	m.backend = workspaceMetadataFixture{
		caps: map[string]uint64{"workspaceSkills": sdk.WorkspaceSkillsVersion},
		skills: func(sdk.ID) []sdk.WorkspaceSkill {
			return []sdk.WorkspaceSkill{{Name: "code-review"}, {Name: "model"}, {Name: "review"}, {Name: "review-draft"}, {Name: "skills"}, {Name: "theme"}}
		},
	}
	_, initial := m.Update(m.menuProbe()())
	m.Update(initial().(skillsMsg))
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"/theme", []string{"theme", "theme-alt", "skill:theme"}},
		{"/skills", []string{"skills", "skill:skills"}},
		{"/sk", []string{"skills", "skill:skills", "skill:code-review", "skill:model", "skill:review", "skill:review-draft", "skill:theme"}},
		{"/model", nil}, // An unavailable exact command must not run another action.
		{"/rev", []string{"skill:review", "skill:review-draft", "skill:code-review"}},
		{"/skill:RVW", []string{"skill:code-review", "skill:review", "skill:review-draft"}},
		{"/skill:theme", []string{"skill:theme"}},
		{"/klv", nil}, // A fuzzy match cannot borrow letters from "skill:".
		{"/rvw argument", nil},
		{"/review/notes", nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			m.editor.set(tc.query)
			var got []string
			for _, a := range m.slashMatches() {
				got = append(got, a.action.Slash.Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("matches = %v, want %v", got, tc.want)
			}
		})
	}
	m.editor.set("/thds")
	press(m, tea.KeyTab, 0)
	if got := m.editor.value(); got != "/threads " {
		t.Fatalf("Tab on a fuzzy command completed to %q", got)
	}
}
