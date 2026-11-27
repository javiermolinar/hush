package pane

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// Uses a disposable local session, never a session selected in the UI.
func TestRexAdapterLive(t *testing.T) {
	path := os.Getenv("HUSH_TEST_REX")
	if path == "" {
		t.Skip("set HUSH_TEST_REX to an installed Rex CLI with a running local server")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cli := nativeCLI{path: path, prefix: []string{"--autostart=false", "--timeout=2s"}, env: os.Environ()}
	var who rexIdentity
	if _, err := cli.json(ctx, &who, "whoami", "--json"); err != nil || who.Principal.Transport != "unix" {
		t.Fatal("no verified local Rex server")
	}
	cli.prefix = append(cli.prefix, "-S", who.Endpoint)
	var session struct {
		ID      string `json:"session_id"`
		Windows []struct {
			Blocks []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if _, err := cli.json(ctx, &session, "new", "hush-adapter-smoke", "--keep-open", "--shell=none", "--json", "--", "/usr/bin/true"); err != nil || !rexID(session.ID, "session") {
		t.Fatal("could not create a disposable Rex session")
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		out := nativeOutput{limit: 1024}
		if _, err := cli.run(cleanup, &out, "kill", session.ID); err != nil {
			t.Error("could not close disposable Rex session")
		}
	})
	if len(session.Windows) != 1 || len(session.Windows[0].Blocks) != 1 {
		t.Fatal("ambiguous disposable origin")
	}
	origin := session.Windows[0].Blocks[0]
	a, err := bindRex(ctx, path, append(os.Environ(), "REX_BLOCK="+origin), origin)
	if err != nil {
		t.Fatal(err)
	}
	if a.origin.Instance != who.Instance || a.origin.Session != session.ID {
		t.Fatal("resolved a different server or session")
	}
	dir := t.TempDir()
	var opened []Ref
	for _, p := range a.Capabilities().Placements() {
		ref := requireConfirmed(t, a.Open(ctx, OpenRequest{Placement: p, Dir: dir, Launch: Launch{Mode: LaunchArgv, Program: "/usr/bin/printf", Args: []string{"%s\n", "fixture $literal'a", "last line"}}}))
		opened = append(opened, ref)
		if p == Right {
			requireConfirmed(t, a.Focus(ctx, FocusRequest{Pane: ref}))
		}
	}
	list := requireConfirmed(t, a.List(ctx, ListRequest{}))
	for _, ref := range opened {
		if !slices.Contains(list, ref) {
			t.Fatal("open returned a pane not in the verified session")
		}
	}
	tab := opened[len(opened)-1]
	requireConfirmed(t, a.Focus(ctx, FocusRequest{Pane: tab}))
	requireConfirmed(t, a.Focus(ctx, FocusRequest{Pane: a.scope.Ref(origin)}))
	snapshot := requireConfirmed(t, a.Read(ctx, ReadRequest{Pane: tab, Lines: 1}))
	if strings.TrimSpace(snapshot.Text) != "last line" || !snapshot.Truncated {
		t.Fatalf("wrong readback: %q, truncated=%t", snapshot.Text, snapshot.Truncated)
	}
}
