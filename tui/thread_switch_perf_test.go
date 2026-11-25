package tui

import (
	"bufio"
	"context"
	"fmt"
	"net"
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

// programLoop runs the model's commands concurrently and delivers their
// messages one at a time, rendering after each Update, as Bubble Tea does.
type programLoop struct {
	t    *testing.T
	m    *Model
	msgs chan tea.Msg
}

func newProgramLoop(t *testing.T, m *Model) *programLoop {
	return &programLoop{t: t, m: m, msgs: make(chan tea.Msg, 4096)}
}

func (l *programLoop) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		switch msg := cmd().(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range msg {
				l.run(c)
			}
		default:
			select {
			case l.msgs <- msg:
			case <-l.m.ctx.Done():
			}
		}
	}()
}

// deliver updates the model, starts the commands it returns and renders.
func (l *programLoop) deliver(msg tea.Msg) string {
	_, cmd := l.m.Update(msg)
	l.run(cmd)
	return ansi.Strip(l.m.View().Content)
}

// until delivers messages until a frame satisfies done.
func (l *programLoop) until(what string, done func(frame string) bool) {
	l.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case msg := <-l.msgs:
			if done(l.deliver(msg)) {
				return
			}
		case <-deadline:
			l.t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// time delivers a key press and returns the time to the first frame that satisfies done.
func (l *programLoop) time(key tea.KeyPressMsg, what string, done func(frame string) bool) time.Duration {
	l.t.Helper()
	start := time.Now()
	if done(l.deliver(key)) {
		return time.Since(start)
	}
	l.until(what, done)
	return time.Since(start)
}

// delayedSocket forwards a Unix socket and holds every chunk for oneWay in
// each direction, simulating a remote host's round trip.
func delayedSocket(t *testing.T, target string, oneWay time.Duration) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hush-rtt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "hush.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("unix", target)
			if err != nil {
				_ = client.Close()
				continue
			}
			go delayCopy(server, client, oneWay)
			go delayCopy(client, server, oneWay)
		}
	}()
	return path
}

func delayCopy(dst, src net.Conn, d time.Duration) {
	type chunk struct {
		due  time.Time
		data []byte
	}
	queue := make(chan chunk, 1024)
	go func() {
		defer close(queue)
		for {
			buf := make([]byte, 64<<10)
			n, err := src.Read(buf)
			if n > 0 {
				queue <- chunk{time.Now().Add(d), buf[:n]}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		time.Sleep(time.Until(c.due))
		if _, err := dst.Write(c.data); err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

// startSwitchFixture starts the native faux backend for the whole test; the
// shared fixture's 15 s deadline is too short for two timed scenarios.
func startSwitchFixture(t *testing.T, root, cwd string) (context.Context, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	grantStandaloneFixture(t, root, cwd)
	cmd := exec.CommandContext(ctx, "node", os.Getenv("HUSH_TEST_BACKEND"), "--faux")
	cmd.Dir, cmd.Env, cmd.Stderr = cwd, standaloneFixtureEnv(root), os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Signal(os.Interrupt); _ = cmd.Wait() })
	if scan := bufio.NewScanner(stdout); !scan.Scan() || !strings.Contains(scan.Text(), `"ready":true`) {
		t.Fatalf("standalone backend not ready: %q %v", scan.Text(), scan.Err())
	}
	return ctx, filepath.Join(root, ".hush", "runtime", "hush.sock")
}

func switchClient(t *testing.T, socket string) *sdk.Client {
	client := sdk.NewClient(socket, sdk.ClientOptions{Capabilities: standaloneFixtureCapabilities(),
		RequiredCapabilities: map[string]uint64{"hushProfile": 1}})
	t.Cleanup(client.Close)
	return client
}

// TestThreadSwitchLatency measures a key press to the first frame that shows
// the target thread's transcript: Enter in the thread picker for a thread
// this TUI has not opened, and the previous-tab key for an open one. It runs
// on the local socket and with a simulated 50 ms round trip. There's no
// target yet; it records the baseline.
func TestThreadSwitchLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("timing is not measured in short/race runs")
	}
	if os.Getenv("HUSH_TEST_BACKEND") == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	root, cwd := standaloneHome(t)
	ctx, socket := startSwitchFixture(t, root, cwd)
	seed := switchClient(t, socket)
	const opened = 8 // threads opened for the first time in each scenario
	ids := make([]sdk.ID, opened+1)
	for i := range ids {
		thread, err := seed.NewThread(ctx, sdk.NewThread{CWD: cwd})
		if err != nil {
			t.Fatal(err)
		}
		receipt, err := seed.Submit(ctx, sdk.Input{ThreadID: thread.ID, RequestID: fmt.Sprint("switch-", i),
			Text: fmt.Sprintf("switch marker %d", i), WhenBusy: sdk.RejectBusy})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := seed.WaitSubmission(ctx, thread.ID, receipt.SubmissionID); err != nil {
			t.Fatal(err)
		}
		ids[i] = thread.ID
	}
	shows := func(m *Model, i int) func(string) bool {
		return func(frame string) bool {
			return m.active == ids[i] && m.sheet == nil && strings.Contains(frame, fmt.Sprintf("Faux: switch marker %d", i))
		}
	}
	for _, scenario := range []struct {
		name   string
		oneWay time.Duration
	}{{"local", 0}, {"rtt-50ms", 25 * time.Millisecond}} {
		t.Run(scenario.name, func(t *testing.T) {
			path := socket
			if scenario.oneWay > 0 {
				path = delayedSocket(t, socket, scenario.oneWay)
			}
			m, err := New(ctx, switchClient(t, path), Options{ThreadID: ids[0], NewThread: sdk.NewThread{CWD: cwd},
				WorkspaceLocal: true, ReducedMotion: true})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(m.Close)
			l := newProgramLoop(t, m)
			l.deliver(tea.WindowSizeMsg{Width: 140, Height: 42})
			l.run(tea.Batch(m.beginFeed(), m.menuProbe(), m.beginBackendReport()))
			l.until("the first thread", shows(m, 0))
			l.until("the thread directory", func(string) bool { return m.directoryEnabled })
			var first, open []time.Duration
			for i := 1; i <= opened; i++ {
				l.deliver(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
				l.until("the picker row", func(string) bool {
					if d := m.sheet; d != nil && d.threadPicker != nil {
						for n, c := range d.filtered {
							if d.choices[c].ID == string(ids[i]) {
								d.selected = n
								return true
							}
						}
					}
					return false
				})
				first = append(first, l.time(tea.KeyPressMsg{Code: tea.KeyEnter}, "a first-time thread", shows(m, i)))
				for _, back := range []int{i - 1, i} { // previous tab, then back again
					l.deliver(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl})
					open = append(open, l.time(tea.KeyPressMsg{Code: '[', Text: "["}, "an open thread", shows(m, back)))
				}
			}
			for _, r := range []struct {
				name string
				ds   []time.Duration
			}{{"first-time", first}, {"already-open", open}} {
				slices.Sort(r.ds)
				t.Logf("%s thread: %d switches, p50 %s, p95 %s, max %s", r.name, len(r.ds),
					r.ds[len(r.ds)/2], r.ds[len(r.ds)*95/100], r.ds[len(r.ds)-1])
			}
		})
	}
}
