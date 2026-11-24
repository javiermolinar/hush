package sdk

import (
	"context"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	WorkspaceSkillsVersion     = 1
	WorkspaceSkillDraftVersion = 2
	workspaceSkillsCapability  = "workspaceSkills"
	workspaceSkillsList        = "ext.workspaceSkills.list"
	workspaceSkillsSubmit      = "ext.workspaceSkills.submit"
)

var (
	workspaceSkillName  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	workspaceSkillToken = regexp.MustCompile(`^[A-Za-z0-9_-]{44,512}$`)
)

// WorkspaceSkill is an eligible text skill in a thread or draft scope. The
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
	return c.workspaceSkills(ctx, map[string]ID{"threadId": thread}, WorkspaceSkillsVersion, WorkspaceSkillDraftVersion)
}

// PreviewWorkspaceSkills lists skills for a prospective backend checkout
// without creating a thread. A preview token binds that exact checkout.
func (c *Client) PreviewWorkspaceSkills(ctx context.Context, cwd string) ([]WorkspaceSkill, error) {
	if !path.IsAbs(cwd) || len(cwd) > 4096 || !utf8.ValidString(cwd) || strings.IndexFunc(cwd, unicode.IsControl) >= 0 {
		return nil, ErrProtocol
	}
	return c.workspaceSkills(ctx, map[string]string{"cwd": cwd}, WorkspaceSkillDraftVersion)
}

func supportedWorkspaceSkills(version uint64) bool {
	return version == WorkspaceSkillsVersion || version == WorkspaceSkillDraftVersion
}

func (c *Client) workspaceSkills(ctx context.Context, scope any, versions ...uint64) ([]WorkspaceSkill, error) {
	out, err := waitOnCapability[struct {
		Skills []WorkspaceSkill `json:"skills"`
	}](ctx, c, workspaceSkillsList, scope, workspaceSkillsCapability, versions...)
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
