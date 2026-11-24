package tui

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	x "github.com/javiermolinar/hush/tui/extension"
)

// ProviderLoginOptions is supplied by the local host, never the backend socket.
// Command prepares a trusted interactive command for a selected provider ID.
// It runs off the update loop and must leave terminal input and output to the
// shell. Bind the process to the supplied lifetime, not to preparation work.
type ProviderLoginOptions struct {
	Providers []x.Choice
	Command   func(context.Context, string) (*exec.Cmd, error)
}

const providerLoginOp x.LocalOp = "provider.login"

func providerLoginMenu(providers []x.Choice) func(x.Context, x.Invocation) x.Outcome {
	return func(_ x.Context, v x.Invocation) x.Outcome {
		if strings.TrimSpace(v.Args) != "" {
			return showNote("Use /login without arguments, then choose a provider", x.Warning)
		}
		return x.Outcome{x.SelectDialog{Placement: x.Sheet, Title: "Sign in to a provider", Choices: providers,
			Done: func(_ x.Context, reply x.Reply[string]) x.Outcome {
				if !reply.Accepted {
					return nil
				}
				return x.Outcome{x.Local{Op: providerLoginOp, Text: reply.Value}}
			}}}
	}
}

type loginFinished struct {
	route    ownerRoute
	provider string
	err      error
}

func (m *Model) startLogin(r ownerRoute, id string, budget *int) tea.Cmd {
	login := m.options.ProviderLogin
	if login == nil || login.Command == nil || !m.foreground(r) {
		return nil
	}
	label := ""
	for _, p := range login.Providers {
		if p.ID == id && !p.Disabled {
			label = limitText(p.Label, 256)
			break
		}
	}
	if label == "" {
		return nil
	}
	prepare, life := login.Command, m.ctx
	return m.startWork(r, func(context.Context) (any, error) {
		return prepare(life, id)
	}, func(value any, err error, _ *int) tea.Cmd {
		if !m.foreground(r) {
			return nil
		}
		if err != nil {
			return m.notifyRoute(r, err.Error(), x.Danger)
		}
		cmd, _ := value.(*exec.Cmd)
		if cmd == nil {
			return m.notifyRoute(r, "Provider sign-in unavailable", x.Warning)
		}
		if m.images.output != nil {
			cmd.Stdout = os.Stdout // Do not route the child through the image screen tracker.
		}
		// Bubble Tea releases and restores the terminal. Auth input never goes
		// through the prompt editor, a Tea message, or the SDK.
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return loginFinished{r, label, err} })
	}, budget)
}

func (m *Model) finishLogin(v loginFinished) tea.Cmd {
	if v.err == nil {
		return m.notifyRoute(v.route, "Signed in to "+v.provider, x.Good)
	}
	var exit *exec.ExitError
	if errors.As(v.err, &exit) && exit.ExitCode() == 130 {
		return m.notifyRoute(v.route, "Sign-in cancelled", x.Warning)
	}
	return m.notifyRoute(v.route, "Sign-in failed. Check the terminal output or retry from the shell.", x.Danger)
}
