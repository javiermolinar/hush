package backendstart

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

func TestManagedWaitersShareBootWithoutOwningIt(t *testing.T) {
	life, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	started, release := make(chan struct{}, 1), make(chan struct{})
	bootFailure := errors.New("fixture startup failed")
	var calls atomic.Int32
	m := &managedSocket{life: life, start: func(ctx context.Context) error {
		calls.Add(1)
		started <- struct{}{}
		select {
		case <-release:
			return bootFailure
		case <-ctx.Done():
			return ctx.Err()
		}
	}}

	probe, cancel := context.WithCancel(life)
	defer cancel()
	probed := make(chan error, 1)
	go func() { probed <- m.waitReady(probe) }()
	select {
	case <-started:
	case <-life.Done():
		t.Fatal("startup did not begin")
	}
	cancel()
	if err := <-probed; !errors.Is(err, context.Canceled) {
		t.Fatalf("probe error = %v", err)
	}

	prompt := make(chan error, 1)
	go func() { prompt <- m.waitReady(life) }()
	close(release)
	for _, err := range []error{<-prompt, m.waitReady(life)} {
		if !errors.Is(err, bootFailure) {
			t.Fatalf("waiter lost the shared startup result: %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("started %d times instead of once", got)
	}
}

func TestManagedClientIsLazyAndExplicitSocketDoesNotStart(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "hush-managed-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	agent, external := filepath.Join(root, "agent"), filepath.Join(root, "remote.sock")
	t.Setenv("HOME", root)
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	t.Setenv("HUSH_PROFILE", "")
	t.Setenv("HUSH_SOCKET", "")
	t.Setenv("HUSH_BACKEND", filepath.Join(root, "missing.mjs"))
	stopped, err := Stop(t.Context(), Options{})
	if err != nil || stopped {
		t.Fatalf("stopping an absent backend = %t, %v", stopped, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".hush")); !os.IsNotExist(err) {
		t.Fatalf("stopping an absent backend touched the data directory: %v", err)
	}
	c, err := NewClient(t.Context(), Options{}, sdk.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := os.Stat(filepath.Join(root, ".hush")); !os.IsNotExist(err) {
		t.Fatalf("constructing the client touched the data directory: %v", err)
	}
	if _, err := c.Hello(t.Context()); !errors.Is(err, os.ErrNotExist) || !containsStartError(err) {
		t.Fatalf("local dial should attempt startup and report missing bundle, got %v", err)
	}
	for _, name := range []string{"option", "environment"} {
		opt := Options{Socket: external}
		if name == "environment" {
			opt = Options{}
			t.Setenv("HUSH_SOCKET", external)
		}
		remote, err := NewClient(t.Context(), opt, sdk.ClientOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, err = remote.Hello(t.Context())
		remote.Close()
		var op *net.OpError
		if !errors.Is(err, syscall.ENOENT) || !errors.As(err, &op) || containsStartError(err) {
			t.Fatalf("%s socket should be attach-only, got %v", name, err)
		}
	}
}

func TestManagedClientColdStartsAndReattaches(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	root, err := os.MkdirTemp("/tmp", "hush-managed-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	agent, pidFile := filepath.Join(root, "agent"), filepath.Join(root, "pids")
	t.Setenv("HOME", root)
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	t.Setenv("HUSH_PROFILE", "")
	t.Setenv("HUSH_SOCKET", "")
	source, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(root, "fixture.mjs")
	bootstrap := fmt.Sprintf("import { appendFileSync as hushManagedFixturePid } from 'node:fs'; hushManagedFixturePid(%q, process.pid + '\\n'); process.argv.push('--faux');\n", pidFile)
	if err := os.WriteFile(entry, append([]byte(bootstrap), source...), 0600); err != nil {
		t.Fatal(err)
	}
	pack, err := os.ReadFile(bundle + ".wasm-pack")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry+".wasm-pack", pack, 0o600); err != nil {
		t.Fatal(err)
	}
	pids := func() []int {
		contents, _ := os.ReadFile(pidFile)
		var out []int
		for _, field := range strings.Fields(string(contents)) {
			if pid, err := strconv.Atoi(field); err == nil && pid > 0 {
				out = append(out, pid)
			}
		}
		return out
	}
	defer func() {
		for _, pid := range pids() {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	c, err := NewClient(ctx, Options{Bundle: entry}, sdk.ClientOptions{Capabilities: map[string][]uint64{"intro": {sdk.IntroVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	updates := c.WatchIntro(ctx, sdk.IntroScope{CWD: root})
	thread, err := c.NewThread(ctx, sdk.NewThread{CWD: root})
	if err != nil {
		t.Fatalf("cold prompt could not create a thread: %v", err)
	}
	first, err := c.Hello(ctx)
	if err != nil || first.Capabilities["hushProfile"] != 1 || first.Capabilities["storeIdentity"] != sdk.StoreIdentityVersion || first.Capabilities["piEvents"] != 0 {
		t.Fatalf("not a standalone hello: %+v %v", first, err)
	}
	if _, err := os.Stat(agent); !os.IsNotExist(err) {
		t.Fatalf("cold start touched the Pi directory: %v", err)
	}
	for u := range updates {
		if u.Err == nil {
			break
		}
	}
	started := pids()
	if len(started) != 1 {
		t.Fatalf("one launch expected for competing dials: %v", started)
	}
	if err := syscall.Kill(started[0], syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for u := range updates {
		if u.Err != nil {
			continue
		}
		hello, err := c.Hello(ctx)
		if err == nil && hello.Epoch != first.Epoch {
			page, err := c.ListThreads(ctx, 20, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range page.Threads {
				if got.ID == thread.ID {
					if len(pids()) != 2 {
						t.Fatal("reconnect did not start exactly one replacement")
					}
					return
				}
			}
			t.Fatal("reconnect lost the durable thread")
		}
	}
	t.Fatalf("intro did not reconnect after local shutdown: %v", ctx.Err())
}

func containsStartError(err error) bool {
	var path *os.PathError
	return errors.As(err, &path) && path.Op == "stat"
}

// Test-only child processes are isolated from the actual HOME and use the
// built bundle and the local, deterministic faux model (no provider traffic).
type storeFixture struct {
	ctx                context.Context
	bundle, root, node string
}

func newStoreFixture(t *testing.T) storeFixture {
	t.Helper()
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built backend bundle")
	}
	bundle, err := filepath.Abs(bundle)
	checkStoreFixture(t, err)
	node, err := exec.LookPath("node")
	checkStoreFixture(t, err)
	root, err := os.MkdirTemp("/tmp", "hush-store-")
	checkStoreFixture(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	t.Cleanup(cancel)
	return storeFixture{ctx, bundle, root, node}
}

func (f storeFixture) command(dir string) *exec.Cmd {
	cmd := exec.CommandContext(f.ctx, f.node, f.bundle, "--profile", "hush", "--faux", "--data-dir", dir, "--cwd", f.root)
	cmd.Env = append(RuntimeEnv(), "HOME="+f.root)
	return cmd
}

func (f storeFixture) start(t *testing.T, dir string) func(crash bool) {
	t.Helper()
	cmd := f.command(dir)
	stdout, err := cmd.StdoutPipe()
	checkStoreFixture(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	checkStoreFixture(t, cmd.Start())
	stopped := false
	stop := func(crash bool) {
		if stopped {
			return
		}
		stopped = true
		if crash {
			_ = cmd.Process.Kill()
		} else {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		if err := cmd.Wait(); err != nil && !crash {
			t.Errorf("shutdown: %v: %s", err, stderr.String())
		}
	}
	t.Cleanup(func() { stop(true) })
	line, err := bufio.NewReader(stdout).ReadBytes('\n')
	if err != nil || readStartup(bytes.NewReader(line), filepath.Join(dir, "hush.sock")) != nil {
		stop(true)
		t.Fatalf("boot: %s %v: %s", line, err, stderr.String())
	}
	return stop
}

var storeOpts = sdk.ClientOptions{RequiredCapabilities: map[string]uint64{"hushProfile": 1, "storeIdentity": 1}}

func (f storeFixture) seed(t *testing.T, dir string) sdk.ID {
	t.Helper()
	stop := f.start(t, dir)
	c := sdk.NewClient(filepath.Join(dir, "hush.sock"), storeOpts)
	defer c.Close()
	thread, err := c.NewThread(f.ctx, sdk.NewThread{CWD: f.root})
	checkStoreFixture(t, err)
	stop(false)
	return thread.ID
}

func checkStoreFixture(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func readStoreFixture(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	checkStoreFixture(t, err)
	return data
}

// Follow the offline restore contract: bind a checkpoint copy in a staged
// directory, keep its logical store ID, assign a new generation.
func restoreStoreFixture(t *testing.T, original, stage string) {
	t.Helper()
	var marker map[string]any
	checkStoreFixture(t, json.Unmarshal(readStoreFixture(t, filepath.Join(original, "store-profile")), &marker))
	checkStoreFixture(t, os.Mkdir(stage, 0700))
	file := filepath.Join(stage, "history.sqlite")
	checkStoreFixture(t, os.WriteFile(file, readStoreFixture(t, filepath.Join(original, "history.sqlite")), 0600))
	checkStoreFixture(t, os.Link(file, filepath.Join(stage, "history.identity")))
	fi, err := os.Stat(file)
	checkStoreFixture(t, err)
	st := fi.Sys().(*syscall.Stat_t)
	marker["device"], marker["inode"] = fmt.Sprint(st.Dev), fmt.Sprint(st.Ino)
	const generation = "50585983-c167-459d-9c4b-492d3da95285"
	if marker["generation"] == generation {
		t.Fatal("fixture generation must differ")
	}
	marker["generation"] = generation
	data, err := json.Marshal(marker)
	checkStoreFixture(t, err)
	checkStoreFixture(t, os.WriteFile(filepath.Join(stage, "store-profile"), data, 0600))
}

// Hold a real admission response until the original process is gone and the
// next one is listening, then deliver EOF without any bytes from that record.
type lostStoreReply struct {
	net.Conn
	r        *bufio.Reader
	pending  []byte
	observed chan<- sdk.Receipt
	release  <-chan struct{}
	life     context.Context
}

func (c *lostStoreReply) Read(dst []byte) (int, error) {
	if len(c.pending) == 0 {
		line, err := c.r.ReadBytes('\n')
		if err != nil {
			return 0, err
		}
		var frame struct {
			Method string
			OK     bool
			Result sdk.Receipt
		}
		if json.Unmarshal(line, &frame) == nil && frame.Method == "submit" && frame.OK {
			c.observed <- frame.Result
			select {
			case <-c.release:
			case <-c.life.Done():
			}
			return 0, io.EOF
		}
		c.pending = line
	}
	n := copy(dst, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func TestBackendStoreRetryAcrossProcessLoss(t *testing.T) {
	for _, mode := range []string{"same-store", "replacement", "restore"} {
		t.Run(mode, func(t *testing.T) {
			f := newStoreFixture(t)
			grant := exec.CommandContext(f.ctx, f.node, f.bundle, "trust", "grant", "--scope=work", "--cwd="+f.root)
			grant.Env = append(RuntimeEnv(), "HOME="+f.root)
			if out, err := grant.CombinedOutput(); err != nil {
				t.Fatalf("fixture workspace grant: %v: %s", err, out)
			}
			active, stage := filepath.Join(f.root, "active"), filepath.Join(f.root, "stage")
			path := filepath.Join(active, "hush.sock")
			thread := f.seed(t, active)
			switch mode {
			case "replacement":
				if got := f.seed(t, stage); got != thread {
					t.Fatalf("fixture needs overlapping thread IDs: %s != %s", got, thread)
				}
			case "restore":
				restoreStoreFixture(t, active, stage)
			}
			stop := f.start(t, active)
			observed, release := make(chan sdk.Receipt, 1), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var dials atomic.Int32
			opts := storeOpts
			opts.DialUnix = func(ctx context.Context, path string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
				if err == nil && dials.Add(1) == 1 {
					return &lostStoreReply{Conn: conn, r: bufio.NewReader(conn), observed: observed, release: release, life: f.ctx}, nil
				}
				return conn, err
			}
			client := sdk.NewClient(path, opts)
			defer client.Close()
			first, err := client.Hello(f.ctx)
			checkStoreFixture(t, err)
			type outcome struct {
				receipt sdk.Receipt
				err     error
			}
			result := make(chan outcome, 1)
			const key = "lost-key"
			go func() {
				receipt, err := client.Submit(f.ctx, sdk.Input{ThreadID: thread, RequestID: key, Text: "answer once"})
				result <- outcome{receipt, err}
			}()
			var lost sdk.Receipt
			select {
			case lost = <-observed:
			case out := <-result:
				t.Fatalf("submit ended before real admission: %+v", out)
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			stop(true)
			if mode != "same-store" {
				checkStoreFixture(t, os.Rename(active, filepath.Join(f.root, "old")))
				checkStoreFixture(t, os.Rename(stage, active))
			}
			f.start(t, active)
			close(release)
			out := <-result
			if mode == "same-store" {
				if out.err != nil || lost.SubmissionID != out.receipt.SubmissionID || dials.Load() != 2 {
					t.Fatalf("lost=%+v retry=%+v, dials=%d", lost, out, dials.Load())
				}
				h, err := client.Hello(f.ctx)
				if err != nil || h.Epoch == first.Epoch {
					t.Fatalf("retry did not cross a boot: %+v %v", h, err)
				}
				stored, err := client.LookupSubmission(f.ctx, thread, key)
				if err != nil || stored.SubmissionID != lost.SubmissionID {
					t.Fatalf("durable admission changed: %+v %v", stored, err)
				}
				return
			}
			var tx *sdk.TransportError
			if !errors.Is(out.err, sdk.ErrStoreChanged) || !errors.As(out.err, &tx) || !tx.Indeterminate {
				t.Fatalf("retry accepted changed store: %+v", out)
			}
			if _, err := client.History(f.ctx, thread, sdk.Before(nil), 10); !errors.Is(err, sdk.ErrStoreChanged) {
				t.Fatalf("history crossed the identity fence: %v", err)
			}
			fresh := sdk.NewClient(path, storeOpts)
			defer fresh.Close()
			if _, err := fresh.History(f.ctx, thread, sdk.Before(nil), 10); err != nil {
				t.Fatalf("fresh attach cannot see overlapping thread: %v", err)
			}
			if _, err := fresh.LookupSubmission(f.ctx, thread, key); !sdk.IsCode(err, "not_found") {
				t.Fatalf("replacement/restore admitted stale retry: %v", err)
			}
		})
	}
}

func TestBackendRefusesIncompatibleHistoryAndLeavesSocket(t *testing.T) {
	for _, mode := range []string{"unversioned", "unsupported-format", "swapped-file", "nonregular-history"} {
		t.Run(mode, func(t *testing.T) {
			f := newStoreFixture(t)
			dir := filepath.Join(f.root, "data")
			f.seed(t, dir)
			history, marker, sock := filepath.Join(dir, "history.sqlite"), filepath.Join(dir, "store-profile"), filepath.Join(dir, "hush.sock")
			switch mode {
			case "unversioned":
				checkStoreFixture(t, os.Remove(marker))
				checkStoreFixture(t, os.Remove(filepath.Join(dir, "history.identity")))
			case "unsupported-format":
				var m map[string]any
				checkStoreFixture(t, json.Unmarshal(readStoreFixture(t, marker), &m))
				m["format"] = 999
				data, err := json.Marshal(m)
				checkStoreFixture(t, err)
				checkStoreFixture(t, os.WriteFile(marker, data, 0600))
			case "swapped-file":
				checkStoreFixture(t, os.WriteFile(history+".swap", readStoreFixture(t, history), 0600))
				checkStoreFixture(t, os.Rename(history+".swap", history))
			case "nonregular-history":
				checkStoreFixture(t, os.Remove(history))
				checkStoreFixture(t, syscall.Mkfifo(history, 0600)) // opening for reading can block
			}
			var before []byte
			if mode != "nonregular-history" {
				before = readStoreFixture(t, history)
			}
			l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
			checkStoreFixture(t, err)
			l.SetUnlinkOnClose(false)
			checkStoreFixture(t, l.Close())
			socketBefore, err := os.Lstat(sock)
			checkStoreFixture(t, err)
			out, err := f.command(dir).CombinedOutput()
			if err == nil || !bytes.Contains(out, []byte("hush-backend: store_profile\n")) {
				t.Fatalf("expected profile refusal before attempting storage open: %v %s", err, out)
			}
			socketAfter, err := os.Lstat(sock)
			if err != nil || !os.SameFile(socketBefore, socketAfter) {
				t.Fatalf("incompatible launch removed or replaced socket: %v", err)
			}
			if before != nil && !bytes.Equal(before, readStoreFixture(t, history)) {
				t.Fatal("incompatible launch changed history bytes")
			}
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				if _, err := os.Lstat(history + suffix); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("history sidecar %s exists after refusal: %v", suffix, err)
				}
			}
		})
	}
}
