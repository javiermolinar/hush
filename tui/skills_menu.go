package tui

import (
	"context"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

const skillsPickerID x.ActionID = menusID + ".skills"
const stageSkillID x.ActionID = menusID + ".stage-skill"
const stageSkillOp x.LocalOp = "workspaceSkills.stage"

type workspaceSkillsBackend interface {
	ListWorkspaceSkills(context.Context, sdk.ID) ([]sdk.WorkspaceSkill, error)
}

type skillsLease struct {
	owner                  sdk.ID
	navigation, generation uint64
	listed                 []sdk.WorkspaceSkill
	actions                []registeredAction
	slash, waiting         bool
	cancel                 context.CancelFunc
}

type skillsMsg struct {
	generation uint64
	listed     []sdk.WorkspaceSkill
}

type stagedSkill struct{ name, token string }

func skillOf(text string) string {
	tail, ok := strings.CutPrefix(text, "/skill:")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(tail, " \t\n"); i >= 0 {
		return tail[:i]
	}
	return tail
}

func (m *Model) stopSkills() {
	if m.skills.cancel != nil {
		m.skills.cancel()
	}
	m.skills = skillsLease{}
}

func (m *Model) syncSkills() tea.Cmd {
	t := m.threads[m.active]
	if t != nil && t.stagedSkill.token != "" && skillOf(m.editor.value()) != t.stagedSkill.name {
		t.stagedSkill = stagedSkill{}
	}
	if !m.nativeSkills || m.connection != x.Connected || !m.active.Valid() {
		if m.skills.generation != 0 {
			m.stopSkills()
		}
		return nil
	}
	text := m.editor.value()
	slash := strings.HasPrefix(text, "/") && !strings.ContainsAny(text, " \t\n")
	if m.skills.owner == m.active && m.skills.navigation == m.navigation {
		refresh := slash && !m.skills.slash && !m.skills.waiting
		m.skills.slash = slash
		if !refresh {
			return nil
		}
	} else {
		m.stopSkills()
	}
	backend := m.backend.(workspaceSkillsBackend)
	ctx, cancel := context.WithTimeout(m.ctx, menuTimeout+time.Second)
	s := &m.skills
	s.owner, s.navigation, s.generation, s.cancel, s.slash, s.waiting = m.active, m.navigation, m.token(), cancel, slash, true
	owner, generation := s.owner, s.generation
	return func() tea.Msg {
		defer cancel()
		listed, err := backend.ListWorkspaceSkills(ctx, owner)
		if err != nil {
			listed = nil
		}
		return skillsMsg{generation, listed}
	}
}

func (m *Model) receiveSkills(v skillsMsg) {
	s := &m.skills
	if s.generation == v.generation && s.owner == m.active && s.navigation == m.navigation && m.connection == x.Connected {
		s.listed, s.waiting, s.cancel = v.listed, false, nil
		if i := m.threads[m.active].intro; i != nil && !i.frozen && v.listed != nil {
			n := len(v.listed)
			i.standaloneSkillCount = &n
			i.refresh(i.motion.clock.now())
		}
		s.actions = make([]registeredAction, 0, len(v.listed))
		for _, skill := range v.listed {
			s.actions = append(s.actions, registeredAction{owner: menusID, action: x.Action{
				ID: stageSkillID, Title: "Stage " + skill.Name, Description: skill.Description, Slash: &x.Slash{Name: "skill:" + skill.Name},
			}, skill: stagedSkill{skill.Name, skill.Token}})
		}
		m.slashIndex = 0
	}
}

func (m *Model) currentSkills() []sdk.WorkspaceSkill {
	if m.nativeSkills && m.connection == x.Connected && m.skills.owner == m.active && m.skills.navigation == m.navigation {
		return m.skills.listed
	}
	return nil
}

func (m *Model) skillSlashMatches(prefix string, out []registeredAction) []registeredAction {
	if len(m.currentSkills()) == 0 {
		return out
	}
	var choices []registeredAction
	for _, a := range m.skills.actions {
		if strings.HasPrefix(a.action.Slash.Name, prefix) {
			choices = append(choices, a)
		}
	}
	if len(choices) == 0 {
		return out
	}
	return slices.Insert(out, min(3, len(out)), choices...)
}

func (m *Model) stageSkill(value stagedSkill) {
	if !m.nativeSkills || !m.active.Valid() || value.name == "" || value.token == "" {
		return
	}
	m.editor.set("/skill:" + value.name + " ")
	m.editor.resetHistory()
	m.slashIndex, m.slashHidden = 0, false
	m.threads[m.active].stagedSkill = value
}

func stageSkillChoice(result []sdk.WorkspaceSkill) x.Outcome {
	if len(result) == 0 {
		return showNote("No skills are loaded for this workspace", x.Muted)
	}
	choices := make([]x.Choice, 0, len(result))
	for _, s := range result {
		choices = append(choices, x.Choice{ID: s.Name, Label: s.Name, Description: s.Source + " · " + s.Description})
	}
	return x.Outcome{x.SelectDialog{Title: "Loaded skills · Enter inserts command", Placement: x.Sheet, Choices: choices,
		Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
			if reply.Accepted {
				if i := slices.IndexFunc(result, func(s sdk.WorkspaceSkill) bool { return s.Name == reply.Value }); i >= 0 {
					return x.Outcome{x.Local{Op: stageSkillOp, Text: result[i].Name + "\n" + result[i].Token}}
				}
			}
			return nil
		}}}
}
