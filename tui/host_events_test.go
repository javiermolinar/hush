package tui

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/javiermolinar/hush/sdk"
	x "github.com/javiermolinar/hush/tui/extension"
)

// Count complete requests written by a production SDK client to the real
// backend socket. The seed client and trust CLI are not part of the count.
type workspaceRequests struct {
	sync.Mutex
	methods map[string]int
}

func (r *workspaceRequests) snapshot() map[string]int {
	r.Lock()
	defer r.Unlock()
	return maps.Clone(r.methods)
}

type countedWorkspaceConn struct {
	net.Conn
	requests *workspaceRequests
}

func (c countedWorkspaceConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && n == len(p) {
		var f struct{ Method string }
		_ = json.Unmarshal(p, &f)
		c.requests.Lock()
		c.requests.methods[f.Method]++
		c.requests.Unlock()
	}
	return n, err
}

func TestWorkspaceWatchesLive(t *testing.T) {
	if os.Getenv("HUSH_TEST_BACKEND") == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	helper, err := filepath.Abs("../bin/hush")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(helper); err != nil {
		t.Skip("build bin/hush for the native file helper")
	}
	root, cwd := standaloneHome(t)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
		cmd.Dir = cwd
		cmd.Env = append(standaloneFixtureEnv(root), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git: %v: %s", err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "--allow-empty", "-qm", "seed")
	ctx, socket := startSwitchFixture(t, root, cwd, "HUSH_FILE_LIST_HELPER="+helper)
	seed := switchClient(t, socket)
	thread, err := seed.NewThread(ctx, sdk.NewThread{CWD: cwd})
	if err != nil {
		t.Fatal(err)
	}
	caps := standaloneFixtureCapabilities()
	caps["gitDiff"], caps["gitDiffWatch"] = []uint64{sdk.GitDiffVersion}, []uint64{sdk.GitDiffWatchVersion}
	caps["workspaceFiles"], caps["workspaceScopeWatch"] = []uint64{sdk.WorkspaceFileProjectPathsVersion}, []uint64{sdk.WorkspaceScopeWatchVersion}
	requests := &workspaceRequests{methods: make(map[string]int)}
	c := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: caps,
		RequiredCapabilities: map[string]uint64{"hushProfile": 1},
		DialUnix: func(ctx context.Context, path string) (net.Conn, error) {
			raw, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
			if err != nil {
				return nil, err
			}
			return countedWorkspaceConn{Conn: raw, requests: requests}, nil
		}})
	defer c.Close()
	m, err := New(ctx, c, Options{ThreadID: thread.ID, NewThread: sdk.NewThread{CWD: cwd}, WorkspaceLocal: true, ReducedMotion: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	l := newProgramLoop(t, m)
	l.deliver(tea.WindowSizeMsg{Width: 140, Height: 42})
	l.run(tea.Batch(m.beginFeed(), m.menuProbe(), m.beginBackendReport()))
	l.until("Git badge and file hint", func(frame string) bool {
		return strings.Contains(frame, "± 0 files") && strings.Contains(frame, "@ files")
	})
	if !m.fileScope.pushed || !m.gitDiffFeed.pushed {
		t.Fatal("metadata push was not negotiated")
	}
	waitWindow := func(d time.Duration) {
		deadline := time.After(d)
		for {
			select {
			case msg := <-l.msgs:
				l.deliver(msg)
			case <-deadline:
				return
			}
		}
	}
	full := !testing.Short() && os.Getenv("HUSH_TEST_IDLE_60S") == "1"
	if full {
		waitWindow(350 * time.Millisecond) // allow the startup watches to arm their first conditional wait
		before := requests.snapshot()
		if before["ext.gitDiffWatch.wait"] != 2 || before["ext.workspaceScopeWatch.wait"] != 2 {
			t.Fatalf("expected one snapshot and one conditional wait per feed, got %v", before)
		}
		waitWindow(time.Minute)
		after := requests.snapshot()
		if !maps.Equal(before, after) {
			t.Fatalf("idle TUI wrote extra requests over 60s: before=%v after=%v", before, after)
		}
		t.Logf("idle TUI wrote 0 requests over 60s; setup requests by method: %v", before)
	}

	start := time.Now()
	if _, err := seed.Submit(ctx, sdk.Input{ThreadID: thread.ID, RequestID: "workspace-watch-write", Text: "!workspace-write", WhenBusy: sdk.RejectBusy}); err != nil {
		t.Fatal(err)
	}
	l.until("write tool updates the badge", func(frame string) bool { return strings.Contains(frame, "± 1 file") })
	t.Logf("submit to painted Git badge: %s", time.Since(start))

	if full {
		start = time.Now()
		if err := os.WriteFile(filepath.Join(cwd, "outside.txt"), []byte("external edit\n"), 0600); err != nil {
			t.Fatal(err)
		}
		l.untilTimeout(22*time.Second, "external edit updates the badge without Git metadata changes",
			func(frame string) bool { return strings.Contains(frame, "± 2 files") })
		t.Logf("external edit to painted Git badge (20s host fallback): %s", time.Since(start))
	}
	trust := func(action string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "node", os.Getenv("HUSH_TEST_BACKEND"), "trust", action, "--cwd", cwd)
		cmd.Dir, cmd.Env = cwd, standaloneFixtureEnv(root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("external trust %s: %v: %s", action, err, out)
		}
	}
	start = time.Now()
	trust("revoke")
	l.untilTimeout(4*time.Second, "revocation pushes both hints", func(frame string) bool {
		return strings.Contains(frame, "± needs trust") && !strings.Contains(frame, "@ files")
	})
	t.Logf("revoke command to painted hints: %s", time.Since(start))
	start = time.Now()
	trust("grant")
	l.untilTimeout(4*time.Second, "grant restores both hints", func(frame string) bool {
		return !strings.Contains(frame, "± needs trust") && strings.Contains(frame, "@ files")
	})
	t.Logf("grant command to painted hints: %s", time.Since(start))
	if got := requests.snapshot(); got["ext.gitDiff.summary"] != 0 || got["ext.workspaceFiles.scope"] != 0 {
		t.Fatalf("negotiated host got legacy requests: %v", got)
	}
}

func TestWorkspaceWatchesDowngradeToPolling(t *testing.T) {
	m, _ := demo(t, Options{ReducedMotion: true})
	m.backend = legacyWorkspaceMetadata{fileScopeFixture{capable: true, scope: sdk.WorkspaceFileScope{Workspace: true}}}
	m.menuFeaturesFromBackend |= x.GitDiff
	m.nativeFiles, m.gitDiffPush, m.fileScopePush = true, true, true
	m.syncGitDiff()
	m.syncFileScope()
	if !m.fileScope.pushed || !m.gitDiffFeed.pushed {
		t.Fatal("missing initial push leases")
	}
	gitLease, scopeLease := m.gitDiffFeed, m.fileScope
	unsupported := &sdk.ServerError{Code: "unsupported_capability"}
	toGitPoll := m.receiveGitDiffWatch(gitDiffWatchMsg{lease: gitLease.token, ok: true, update: sdk.GitDiffUpdate{Err: unsupported}})
	toScopePoll := m.receiveFileScopeWatch(fileScopeWatchMsg{generation: scopeLease.generation, ok: true, update: sdk.WorkspaceFileScopeUpdate{Err: unsupported}})
	if toGitPoll == nil || toScopePoll == nil || gitLease.ctx.Err() == nil || scopeLease.ctx.Err() == nil {
		t.Fatal("downgrade did not cancel the old watches and start legacy reads")
	}
	if cmd := m.receiveGitSummary(toGitPoll().(gitDiffSummaryMsg)); cmd == nil || m.gitDiffFeed.tick == 0 {
		t.Fatal("no legacy Git timer")
	}
	if cmd := m.receiveFileScope(toScopePoll().(fileScopeMsg)); cmd == nil || !m.currentFileScope().Workspace {
		t.Fatal("no legacy scope or renewal")
	}
	// An already queued update from the old watch cannot overwrite the scope.
	_ = m.receiveFileScopeWatch(fileScopeWatchMsg{generation: scopeLease.generation, ok: true})
	if !m.currentFileScope().Workspace {
		t.Fatal("stale scope update survived downgrade")
	}
}

type legacyWorkspaceMetadata struct{ fileScopeFixture }

func (legacyWorkspaceMetadata) GitDiffSummary(ctx context.Context, id sdk.ID, baseline string) (sdk.GitDiffSummary, error) {
	return gitDiffFixtureBackend{}.GitDiffSummary(ctx, id, baseline)
}

func (legacyWorkspaceMetadata) GitDiffFile(ctx context.Context, id sdk.ID, rev string, i int) (sdk.GitDiffPatch, error) {
	return gitDiffFixtureBackend{}.GitDiffFile(ctx, id, rev, i)
}

func (legacyWorkspaceMetadata) WatchGitDiff(context.Context, sdk.ID) <-chan sdk.GitDiffUpdate {
	panic("fixture delivers push results directly")
}

func (legacyWorkspaceMetadata) WatchWorkspaceFileScope(context.Context, sdk.WorkspaceFileTarget) <-chan sdk.WorkspaceFileScopeUpdate {
	panic("fixture delivers push results directly")
}
