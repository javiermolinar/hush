package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestLocalGitTrustOnlyOnManagedHushHost(t *testing.T) {
	t.Setenv("HUSH_SOCKET", "")
	local, remote := true, false
	for _, o := range []headless.InteractiveOptions{
		{Profile: "pi"},
		{Profile: "hush", Socket: "/remote.sock", Local: &local},
		{Profile: "hush", Local: &remote},
	} {
		if localGitTrust(o) != nil {
			t.Fatal("trust grant offered outside the managed local Hush profile")
		}
	}
	o := headless.InteractiveOptions{Profile: "hush"}
	if localGitTrust(o) == nil {
		t.Fatal("no host-local trust helper")
	}
	t.Setenv("HUSH_SOCKET", "/remote.sock")
	if localGitTrust(o) != nil {
		t.Fatal("explicit environment socket allowed a local grant")
	}
}

func TestLocalGitTrustHelper(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HUSH_SOCKET", "")
	t.Setenv("HUSH_PROFILE", "pi") // the explicit Hush profile takes precedence
	cwd := filepath.Join(home, "checkout 'with quotes'")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", cwd).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	prepare := localGitTrust(headless.InteractiveOptions{Profile: "hush", Bundle: bundle})
	review, err := prepare(t.Context(), cwd)
	want, _ := filepath.EvalSymlinks(filepath.Join(cwd, ".git"))
	if err != nil || review.Grant == nil || review.GitDirectory != want {
		t.Fatalf("Git trust review = %q, want %q (%v)", review.GitDirectory, want, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".hush")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparing a prompt must not write the profile: %v", err)
	}
	if err := review.Grant(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, err := runGitTrust(t.Context(), bundle, "status", "--cwd="+cwd, "--json")
	var status struct{ Trusted bool }
	if err != nil || json.Unmarshal(out, &status) != nil || !status.Trusted {
		t.Fatalf("host grant not visible: %s (%v)", out, err)
	}
}
