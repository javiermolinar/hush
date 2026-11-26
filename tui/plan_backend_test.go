package tui

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/javiermolinar/hush/sdk"
)

// TestPlanPickerE2E drives the real TUI input/render loop, Go SDK, Unix socket,
// SQLite and a Durable task-owned conversation per selected step. The faux
// workers only run fixture checks; they never write project files.
func TestPlanPickerE2E(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	bundle, err := filepath.Abs(bundle)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	root, err := os.MkdirTemp("/tmp", "hush-plan-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	workspace, dir := filepath.Join(root, "project"), filepath.Join(root, "data")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + root, "PATH=" + os.Getenv("PATH"), "HUSH_PROFILE=hush", "NODE_NO_WARNINGS=1", "LANG=C"}
	grant := exec.CommandContext(ctx, "node", bundle, "trust", "grant", "--cwd", workspace)
	grant.Env = env
	if out, err := grant.CombinedOutput(); err != nil {
		t.Fatalf("grant fixture directory: %v: %s", err, out)
	}
	proc := exec.CommandContext(ctx, "node", bundle, "--profile", "hush", "--faux", "--cwd", workspace, "--data-dir", dir)
	proc.Dir, proc.Env, proc.Stderr = workspace, env, os.Stderr
	stdout, err := proc.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = proc.Wait() }()
	scan := bufio.NewScanner(stdout)
	if !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("backend did not start: %s %v", scan.Text(), scan.Err())
	}
	client := sdk.NewClient(filepath.Join(dir, "hush.sock"), sdk.ClientOptions{
		Capabilities: map[string][]uint64{"plan": {sdk.PlanVersion}, "storeIdentity": {sdk.StoreIdentityVersion}},
	})
	defer client.Close()
	thread, err := client.NewThread(ctx, sdk.NewThread{CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(ctx, client, Options{ThreadID: thread.ID, ReducedMotion: true, Fills: FillOff})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	defer func() {
		if t.Failed() {
			t.Logf("last frame:\n%s", ansi.Strip(m.View().Content))
		}
	}()
	loop := newProgramLoop(t, m)
	loop.deliver(tea.WindowSizeMsg{Width: 124, Height: 26})
	loop.run(m.Init())
	key := func(code rune) string { return loop.deliver(tea.KeyPressMsg{Code: code}) }
	input := func(text string) {
		loop.deliver(tea.PasteMsg{Content: text})
		key(tea.KeyEnter)
	}
	await := func(want string) {
		t.Helper()
		contains := func(frame string) bool { return strings.Contains(frame, want) }
		if !contains(ansi.Strip(m.View().Content)) {
			loop.until(want, contains)
		}
	}
	state := func() sdk.PlanSnapshot {
		t.Helper()
		snapshot, err := client.Plan(ctx, thread.ID)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}

	loop.until("plan capability and thread readiness", func(string) bool { return m.planAvailable() })
	input("/plan")
	await("No plan yet")
	key(tea.KeyEscape)
	input("!plan-fixture")
	await("Saved Workspace profiles (r1)")
	input("/plan")
	await("Workspace profiles · Plan · r1")
	await("Needs 1")
	if before := state(); before.Run != nil || len(before.Progress) != 0 {
		t.Fatalf("drafting or opening admitted work: %+v", before)
	}

	key(tea.KeyDown)
	await("Needs     Save a profile")
	key(tea.KeySpace)
	await("Blocked · select Save a profile")
	key(tea.KeyEnter)
	if state().Run != nil {
		t.Fatal("Enter admitted a selection missing its prerequisite")
	}
	key(tea.KeySpace) // clear the blocked selection
	key(tea.KeyUp)
	key(tea.KeySpace)
	await("Run Save a profile @ r1")
	await("Restore saved profiles stays pending")
	key(tea.KeyDown)
	key(tea.KeySpace)
	key(tea.KeyUp)
	await("Run 2 @ r1")
	await("Save a profile → Restore saved profiles")
	if state().Run != nil {
		t.Fatal("selecting tasks admitted a run before Enter")
	}
	key(tea.KeyEnter)
	await("Running")
	await("Thinking  high → off")
	key(tea.KeyEscape)
	if strings.Contains(ansi.Strip(assertFrame(t, m)), "Workspace profiles · Plan · r1") {
		t.Fatal("Esc left the picker visible")
	}
	loop.until("completed progress summary", func(frame string) bool {
		return strings.Contains(frame, "2/2 verified") || strings.Contains(frame, "✓✓○○  2/4")
	})
	input("/plan")
	await("2/4 verified")
	await("Fixture passed: Saved profile has the expected name")
	finished := state()
	if finished.Run == nil || finished.Run.State != "verified" || !slices.Equal(finished.Run.StepIDs, []string{"save", "restore"}) || len(finished.Progress) != 2 {
		t.Fatalf("unexpected settled scope: %+v", finished)
	}
	a, b := finished.Progress[0], finished.Progress[1]
	if a.StepID != "save" || b.StepID != "restore" || a.State != "verified" || b.State != "verified" || a.WorkerThreadID == b.WorkerThreadID {
		t.Fatalf("worker sequence or evidence: %+v, %+v", a, b)
	}
	// Committed transcript IDs prove B was submitted after A's final answer,
	// not merely that A happened to finish before B.
	first, err := client.History(ctx, a.WorkerThreadID, sdk.Before(nil), 16)
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.History(ctx, b.WorkerThreadID, sdk.Before(nil), 16)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Entries) < 3 || len(second.Entries) < 3 || !lessID(first.Entries[len(first.Entries)-1].ID, second.Entries[0].ID) {
		t.Fatalf("overlapping or empty worker histories: %+v / %+v", first, second)
	}

	key(tea.KeyEnd)
	await("After     Restore saved profiles")
	key(tea.KeySpace)
	await("Run Verify the report @ r1")
	key(tea.KeyEnter)
	await("Not verified")
	failed := state()
	if failed.Run == nil || failed.Run.State != "failed" || !slices.Equal(failed.Run.StepIDs, []string{"report-only"}) ||
		len(failed.Progress) != 3 || len(failed.Progress[2].Evidence) != 0 {
		t.Fatalf("model prose was mistaken for evidence: %+v", failed)
	}

	peer, err := client.NewThread(ctx, sdk.NewThread{CWD: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AttachPlan(ctx, peer.ID, failed.Plan.ID); err != nil {
		t.Fatal(err)
	}
	shared, err := client.Plan(ctx, peer.ID)
	if err != nil || shared.Revision != failed.Revision || shared.Run == nil || shared.Run.ID != failed.Run.ID {
		t.Fatalf("attached thread did not see the same committed plan: %+v, %v", shared, err)
	}
}
