package tui

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func loginFixture() *ProviderLoginOptions {
	return &ProviderLoginOptions{
		Providers: []x.Choice{{ID: "fixture", Label: "Fixture provider", Description: "Browser sign-in"}},
		Command:   func(context.Context, string) (*exec.Cmd, error) { return nil, nil },
	}
}

func TestProviderLoginIsHostGatedAndRequiresSelection(t *testing.T) {
	without := modelFor(t, Options{})
	without.editor.set("/login")
	if len(without.slashMatches()) != 0 {
		t.Fatal("attach-only shell offered login")
	}

	m := modelFor(t, Options{ProviderLogin: loginFixture()})
	m.editor.set("/login")
	press(m, tea.KeyEnter, 0)
	if len(m.pending) != 0 || m.sheet == nil || m.editor.value() != "" || !strings.Contains(ansi.Strip(assertFrame(t, m)), "Fixture provider") {
		t.Fatal("/login did not open a local provider picker before preparing auth")
	}
	press(m, tea.KeyEscape, 0)
	if len(m.pending) != 0 || m.sheet != nil {
		t.Fatal("dismissing the picker prepared a command")
	}

	m.editor.set("/login untrusted-argument")
	press(m, tea.KeyEnter, 0)
	if len(m.pending) != 0 || m.sheet != nil || !strings.Contains(m.notice.text, "without arguments") {
		t.Fatal("/login accepted a free-form provider argument")
	}

	m.editor.set("/login")
	press(m, tea.KeyEnter, 0)
	assertFrame(t, m)
	press(m, tea.KeyEnter, 0)
	if len(m.pending) != 1 || m.sheet != nil {
		t.Fatal("selection did not schedule local sign-in")
	}
}
