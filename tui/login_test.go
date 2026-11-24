package tui

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	x "github.com/javiermolinar/hush/tui/extension"
)

func loginFixture() *ProviderLoginOptions {
	return &ProviderLoginOptions{
		Providers: []x.Choice{
			{ID: "fixture", Label: "Fixture provider", Description: "Browser sign-in"},
			{ID: "device", Label: "Device provider", Description: "Device code"},
		},
		Command: func(context.Context, string) (*exec.Cmd, error) { return nil, nil },
	}
}

func TestProviderLoginIsHostGatedAndRequiresSelection(t *testing.T) {
	without := modelFor(t, Options{})
	without.editor.set("/login")
	if len(without.slashMatches()) != 0 {
		t.Fatal("attach-only shell offered login")
	}

	login := loginFixture()
	var selected []string
	login.Command = func(_ context.Context, id string) (*exec.Cmd, error) {
		selected = append(selected, id)
		return nil, nil
	}
	m := modelFor(t, Options{ProviderLogin: login})
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
	m.Update(tea.PasteMsg{Content: "device"})
	if frame := ansi.Strip(assertFrame(t, m)); !strings.Contains(frame, "Device provider") || strings.Contains(frame, "Fixture provider") {
		t.Fatal("provider picker did not filter to the matching provider")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(m.pending) != 1 || m.sheet != nil {
		t.Fatal("selection did not schedule local sign-in")
	}
	workflowWork(t, m, cmd)
	if !slices.Equal(selected, []string{"device"}) {
		t.Fatalf("prepared sign-in for %v, want device", selected)
	}
}
