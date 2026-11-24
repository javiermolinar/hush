package sdk

import (
	"context"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	WorkspaceSkillsVersion    = 1
	workspaceSkillsCapability = "workspaceSkills"
	workspaceSkillsList       = "ext.workspaceSkills.list"
	workspaceSkillsSubmit     = "ext.workspaceSkills.submit"
)

var (
	workspaceSkillName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	workspaceSkillToken = regexp.MustCompile(`^[A-Za-z0-9_-]{44,512}$`)
)

// WorkspaceSkill is an eligible text skill in the named thread's scope. The
// token pins the source and grant; keep it when staging a selected command.
type WorkspaceSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"`
	Token       string `json:"token"`
}

func (c *Client) ListWorkspaceSkills(ctx context.Context, thread ID) ([]WorkspaceSkill, error) {
	if !thread.Valid() {
		return nil, ErrProtocol
	}
	out, err := waitOnCapability[struct {
		Skills []WorkspaceSkill `json:"skills"`
	}](ctx, c, workspaceSkillsList,
		map[string]ID{"threadId": thread}, workspaceSkillsCapability, WorkspaceSkillsVersion)
	if err != nil {
		return nil, err
	}
	if out.Skills == nil || len(out.Skills) > 64 {
		return nil, ErrProtocol
	}
	seen := make(map[string]bool, len(out.Skills))
	for _, s := range out.Skills {
		if !workspaceSkillName.MatchString(s.Name) || !workspaceSkillToken.MatchString(s.Token) || seen[s.Name] ||
			s.Source != "workspace" && s.Source != "personal" || s.Description == "" || utf8.RuneCountInString(s.Description) > 160 ||
			strings.IndexFunc(s.Description, unicode.IsControl) >= 0 {
			return nil, ErrProtocol
		}
		seen[s.Name] = true
	}
	return out.Skills, nil
}
