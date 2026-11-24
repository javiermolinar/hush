package main

import (
	"slices"
	"testing"

	"github.com/javiermolinar/hush/app/internal/headless"
)

func TestProviderLoginOnlyOnManagedHushHost(t *testing.T) {
	t.Setenv("HUSH_SOCKET", "")
	local := true
	if localProviderLogin(headless.InteractiveOptions{Profile: "pi"}) != nil ||
		localProviderLogin(headless.InteractiveOptions{Profile: "hush", Socket: "/remote.sock", Local: &local}) != nil {
		t.Fatal("provider login available in the Pi profile or attach-only mode")
	}
	o := headless.InteractiveOptions{Profile: "hush", Bundle: "/trusted/backend.mjs"}
	login := localProviderLogin(o)
	if login == nil || len(login.Providers) != 1 || login.Providers[0].ID != "openai-codex" {
		t.Fatal("the managed Hush profile did not offer its supported provider")
	}
	cmd, err := login.Command(t.Context(), login.Providers[0].ID)
	if err != nil || !slices.Equal(cmd.Args[1:], []string{"auth", "login", "--profile", "hush", "--backend", o.Bundle}) {
		t.Fatalf("expected local auth with the same backend override, got %v (%v)", cmd, err)
	}
	if _, err := login.Command(t.Context(), "unknown-provider"); err == nil {
		t.Fatal("prepared a sign-in command for an unsupported provider")
	}
	t.Setenv("HUSH_SOCKET", "/remote.sock")
	if localProviderLogin(o) != nil {
		t.Fatal("provider login available with HUSH_SOCKET set")
	}
}
