package main

import (
	"encoding/json"
	"errors"
	"os"
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

func TestLocalWorkspaceTrustOnlyOnManagedHushHost(t *testing.T) {
	t.Setenv("HUSH_SOCKET", "")
	local, remote := true, false
	if workspaceTrustOptions(headless.InteractiveOptions{Profile: "pi"}) != nil {
		t.Fatal("native trust advertised for the Pi profile")
	}
	for _, o := range []headless.InteractiveOptions{
		{Profile: "hush", Socket: "/remote.sock", Local: &local},
		{Profile: "hush", Local: &remote},
	} {
		if opt := workspaceTrustOptions(o); opt == nil || opt.Review != nil {
			t.Fatal("attached client should get instructions, not a trust helper")
		}
	}
	o := headless.InteractiveOptions{Profile: "hush"}
	if opt := workspaceTrustOptions(o); opt == nil || opt.Review == nil {
		t.Fatal("no host-local trust helper")
	}
	t.Setenv("HUSH_SOCKET", "/remote.sock")
	if opt := workspaceTrustOptions(o); opt == nil || opt.Review != nil {
		t.Fatal("explicit environment socket allowed a local grant")
	}
}

func TestLocalWorkspaceTrustHelper(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HUSH_SOCKET", "")
	t.Setenv("HUSH_PROFILE", "pi") // the explicit Hush profile takes precedence
	cwd := filepath.Join(home, "plain folder 'with quotes'")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	prepare := workspaceTrustOptions(headless.InteractiveOptions{Profile: "hush", Bundle: bundle}).Review
	review, err := prepare(t.Context(), cwd)
	want, _ := filepath.EvalSymlinks(cwd)
	if err != nil || review.Decide == nil || review.Path != want || review.Trusted || review.Dismissed {
		t.Fatalf("workspace trust review = %+v, want fresh %q (%v)", review, want, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".hush")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparing a prompt must not write the profile: %v", err)
	}
	if err := review.Decide(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	review, err = prepare(t.Context(), cwd)
	if err != nil || review.Trusted || !review.Dismissed {
		t.Fatalf("dismissal was not saved separately: %+v (%v)", review, err)
	}
	alias := filepath.Join(home, "alias")
	if err := os.Symlink(cwd, alias); err != nil {
		t.Fatal(err)
	}
	bound, err := prepare(t.Context(), alias)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if err := bound.Decide(t.Context(), true); err == nil {
		t.Fatal("changed alias granted trust without a new review")
	}
	if err := review.Decide(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	out, err := runWorkspaceTrust(t.Context(), bundle, "status", "--scope=work", "--cwd="+cwd, "--json")
	var status struct{ Trusted bool }
	if err != nil || json.Unmarshal(out, &status) != nil || !status.Trusted {
		t.Fatalf("host grant not visible: %s (%v)", out, err)
	}
}
