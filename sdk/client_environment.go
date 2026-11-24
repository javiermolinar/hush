package sdk

import "context"

const (
	ClientEnvironmentVersion    = 1
	clientEnvironmentCapability = "clientEnvironment"
	environmentSubmit           = "ext.clientEnvironment.submit"
)

// ClientEnvironment contains optional, normalized client facts for one input.
// The SDK omits any value outside the negotiated extension's vocabulary. A
// LoginShellHint is not the active shell and is ignored if Shell is known.
type ClientEnvironment struct {
	OS             string `json:"os,omitempty"`
	Terminal       string `json:"terminal,omitempty"`
	Multiplexer    string `json:"multiplexer,omitempty"`
	Shell          string `json:"shell,omitempty"`
	LoginShellHint string `json:"loginShellHint,omitempty"`
}

func allowedFact(value string, values ...string) string {
	for _, v := range values {
		if value == v {
			return v
		}
	}
	return ""
}

func knownShell(s string) string {
	return allowedFact(s, "bash", "zsh", "fish", "sh", "dash", "ash", "ksh", "csh", "tcsh", "nu", "pwsh")
}

func (v ClientEnvironment) normalized() ClientEnvironment {
	v.OS = allowedFact(v.OS, "macos", "linux")
	v.Terminal = allowedFact(v.Terminal, "apple-terminal", "iterm2", "ghostty", "kitty", "wezterm", "alacritty", "foot", "xterm", "vscode", "warp")
	v.Multiplexer = allowedFact(v.Multiplexer, "rex", "zellij", "cmux", "tmux")
	v.Shell = knownShell(v.Shell)
	if v.Shell != "" {
		v.LoginShellHint = ""
	} else {
		v.LoginShellHint = knownShell(v.LoginShellHint)
	}
	return v
}

type submissionRoute struct {
	method      string
	environment bool
}

func (c *Client) submitInput(ctx context.Context, input Input, route *submissionRoute) (Receipt, error) {
	s, err := c.connect(ctx)
	if err != nil {
		return Receipt{}, err
	}
	if route.method == "" {
		route.environment = input.Environment != (ClientEnvironment{}) && s.hello.Capabilities[clientEnvironmentCapability] == ClientEnvironmentVersion
		// Pin the method and payload for this attempt and any same-store retry.
		// Capability fallback is allowed only before the first write.
		switch {
		case input.SkillToken != "":
			route.method = workspaceSkillsSubmit
		case route.environment:
			route.method = environmentSubmit
		case len(input.Images) > 0:
			route.method = imageSubmit
		default:
			route.method = "submit"
		}
	}
	if len(input.Images) > 0 && s.hello.Capabilities["images"] != ImagesVersion {
		return Receipt{}, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: "images"}
	}
	var params any = input
	if route.method == workspaceSkillsSubmit {
		if !supportedWorkspaceSkills(s.hello.Capabilities[workspaceSkillsCapability]) {
			return Receipt{}, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: workspaceSkillsCapability}
		}
		v := struct {
			Input       Input              `json:"input"`
			Token       string             `json:"token"`
			Environment *ClientEnvironment `json:"environment,omitempty"`
		}{Input: input, Token: input.SkillToken}
		if route.environment {
			if s.hello.Capabilities[clientEnvironmentCapability] != ClientEnvironmentVersion {
				return Receipt{}, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: clientEnvironmentCapability}
			}
			v.Environment = &input.Environment
		}
		params = v
	} else if route.method == environmentSubmit {
		if s.hello.Capabilities[clientEnvironmentCapability] != ClientEnvironmentVersion {
			return Receipt{}, &ServerError{Code: "unsupported_capability", Message: "Capability was not negotiated", Capability: clientEnvironmentCapability}
		}
		params = struct {
			Input       Input             `json:"input"`
			Environment ClientEnvironment `json:"environment"`
		}{input, input.Environment}
	}
	v, err := s.call(ctx, route.method, params, nil)
	if err != nil {
		return Receipt{}, err
	}
	return decode[Receipt](v)
}
