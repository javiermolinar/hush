package headless

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePrivateAndDeferredFlagsBeforeWork(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--mode=rpc", "prompt"}, "unknown mode"},
		{[]string{"--mode=json", "--no-wait", "prompt"}, "unknown mode"},
		{[]string{"--session-dir=/tmp", "prompt"}, "unknown flag"},
		{[]string{"--profile=pi", "status"}, "only the Hush backend"},
		{[]string{"prompt", "--no-session", "--thread=7"}, "--no-session requires"},
		{[]string{"--no-session", "--no-wait", "prompt"}, "--no-session requires"},
		{[]string{"status", "--no-session"}, "--no-session requires"},
		{[]string{"--no-session", "--mode=protocol"}, "--no-session requires"},
		{[]string{"wait"}, "wait requires --thread"},
		{[]string{"thread"}, "use thread list"},
		{[]string{"thread", "list", "--limit", "0"}, "--limit must be"},
		{[]string{"thread", "new", "--thread=7"}, "--thread is for thread"},
		{[]string{"thread", "archive", "--worktree", "--thread=7"}, "--worktree is for thread new"},
		{[]string{"thread", "fork", "--thread=7"}, "requires --thread and --at-entry"},
		{[]string{"thread", "list", "--no-session"}, "--no-session requires"},
		{[]string{"--thread=", "prompt"}, "--thread must not be empty"},
		{[]string{"submission", "get", "--thread", "7", "--request-id", "key", "--submission-id", "1"}, "exactly one"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			if _, err := parse(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parse = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestInteractiveRouting(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		inTTY  bool
		outTTY bool
		want   bool
	}{
		{nil, true, true, true},
		{[]string{"--thread", "7", "--socket", "/tmp/sock", "--local=false", "--motion=false"}, true, true, true},
		{[]string{"--backend", "/tmp/bundle.mjs"}, true, true, true},
		{[]string{"--theme=violet"}, true, true, true},
		{[]string{"--theme", "terminal"}, false, true, false},
		{nil, false, true, false},
		{nil, true, false, false},
		{[]string{"status", "--json"}, true, true, false},
		{[]string{"stop", "--json"}, true, true, false},
		{[]string{"thread", "list", "--json"}, true, true, false},
		{[]string{"thread", "new", "--worktree", "--title=review"}, true, true, false},
		{[]string{"print a reply"}, true, true, false},
		{[]string{"--print"}, true, true, false},
		{[]string{"--mode=text"}, true, true, false},
		{[]string{"--mode=protocol"}, true, true, false},
		{[]string{"--no-session"}, true, true, false},
		{[]string{"--no-wait"}, true, true, false},
		{[]string{"--request-id=explicit"}, true, true, false},
		{[]string{"--help"}, true, true, false},
	} {
		o, err := parse(tc.args)
		if err != nil || o.interactive(tc.inTTY, tc.outTTY) != tc.want {
			t.Errorf("%v (tty %t/%t): interactive=%t error=%v, want %t", tc.args, tc.inTTY, tc.outTTY, o.interactive(tc.inTTY, tc.outTTY), err, tc.want)
		}
	}
	o, err := parse([]string{"--thread", "7", "--socket", "/tmp/sock", "--local=false", "--fills=off", "--images=none", "--motion=false", "--theme=violet"})
	if err != nil || o.ui.Thread != "7" || o.ui.Socket != "/tmp/sock" || o.ui.Local == nil || *o.ui.Local || o.ui.Motion || o.ui.Fills != "off" || o.ui.Images != "none" || o.ui.Theme != "violet" || !o.uiFlags {
		t.Fatalf("interactive options = %+v, %v", o.ui, err)
	}
	// Without --fills the TUI uses the choice saved in /theme.
	if o, err := parse(nil); err != nil || o.ui.Fills != "" || o.uiFlags {
		t.Fatalf("default fills = %q, flags %t, %v", o.ui.Fills, o.uiFlags, err)
	}
	if _, err := parse([]string{"--fills="}); err == nil {
		t.Fatal("accepted an empty --fills")
	}
}

func TestThemeFlagDoesNotApplyToHeadlessCommands(t *testing.T) {
	for _, args := range [][]string{
		{"--theme=violet", "--print", "test"}, {"status", "--theme=violet"},
		{"--theme=violet", "schedule", "list"}, {"--theme=violet", "--mode=protocol"},
		{"--theme=violet", "restart"}, {"--theme=violet", "--", "restart"},
	} {
		if _, err := parse(args); err == nil || !strings.Contains(err.Error(), "--theme requires the interactive TUI") {
			t.Errorf("parse(%v): %v", args, err)
		}
	}
	if _, err := parse([]string{"--theme="}); err == nil {
		t.Fatal("empty theme name accepted")
	}
	o, err := parse([]string{"restart", "--open", "--theme=terminal"})
	if err != nil || o.ui.Theme != "terminal" || !o.restartOpen {
		t.Fatalf("restart --open flags: %+v %v", o, err)
	}
}

func TestParseStop(t *testing.T) {
	o, err := parse([]string{"stop", "--socket", "/tmp/hush.sock", "--json"})
	if err != nil || o.command != stopBackend || !o.json || o.socket != "/tmp/hush.sock" || o.interactive(true, true) {
		t.Fatalf("stop options = %+v, %v", o, err)
	}
	o, err = parse([]string{"stop", "-q"})
	if err != nil || o.command != stopBackend || !o.quiet {
		t.Fatalf("quiet stop options = %+v, %v", o, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"stop", "extra"}, "unexpected positional"},
		{[]string{"stop", "--thread", "7"}, "does not accept thread"},
		{[]string{"stop", "--backend", "fixture.mjs"}, "attach-only"},
		{[]string{"stop", "--no-session"}, "--no-session requires"},
		{[]string{"stop", "--quiet", "--json"}, "cannot be combined"},
		{[]string{"status", "--quiet"}, "--quiet is for stop"},
	} {
		if _, err := parse(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parse(%v) = %v, want error containing %q", tc.args, err, tc.want)
		}
	}
	o, err = parse([]string{"--", "stop"})
	if err != nil || o.command != printAnswer || strings.Join(o.prompt, " ") != "stop" {
		t.Fatalf("literal stop prompt = %+v, %v", o, err)
	}
}

func TestParseRestart(t *testing.T) {
	o, err := parse([]string{"restart"})
	if err != nil || o.command != restartBackend || o.restartOpen || o.interactive(true, true) {
		t.Fatalf("restart options = %+v, %v", o, err)
	}
	o, err = parse([]string{"--backend", "fixture.mjs", "restart", "--open", "--thread", "7"})
	if err != nil || o.command != restartBackend || !o.restartOpen || o.ui.Thread != "7" || o.ui.Bundle != "fixture.mjs" || o.interactive(true, true) {
		t.Fatalf("restart --open options = %+v, %v", o, err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"restart", "extra"}, "unexpected positional"},
		{[]string{"restart", "--socket", "/tmp/hush.sock"}, "attach-only"},
		{[]string{"restart", "--thread", "7"}, "only accepts --thread with --open"},
		{[]string{"restart", "--open", "--thread", "invalid"}, "must be a valid durable ID"},
		{[]string{"restart", "--json"}, "not valid for restart"},
		{[]string{"restart", "--no-session"}, "--no-session requires"},
		{[]string{"stop", "--open"}, "--open is for restart"},
	} {
		if _, err := parse(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parse(%v) = %v, want error containing %q", tc.args, err, tc.want)
		}
	}
	for _, flag := range []string{"--", "--print"} {
		o, err := parse([]string{flag, "restart"})
		if err != nil || o.command != printAnswer || strings.Join(o.prompt, " ") != "restart" {
			t.Errorf("literal restart prompt with %s = %+v, %v", flag, o, err)
		}
	}
}

func TestParseLocalAuth(t *testing.T) {
	for _, action := range []string{"login", "status", "logout"} {
		for _, provider := range []string{"", "openrouter", "openai-codex"} {
			args := []string{"--backend", "fixture.mjs", "auth", action, "--profile", "hush"}
			if provider != "" {
				args = append(args, provider)
			}
			o, err := parse(args)
			if err != nil || o.command != localAuth || o.authAction != action || o.authProvider != provider || o.bundle != "fixture.mjs" || o.interactive(true, true) {
				t.Errorf("auth %s %s: options=%+v error=%v", action, provider, o, err)
			}
		}
	}
	for _, args := range [][]string{
		{"auth"}, {"auth", "invalid"}, {"auth", "login", "secret"}, {"auth", "login", "--socket=/tmp/sock"},
		{"auth", "login", "openrouter", "secret"}, {"auth", "logout", "__proto__"}, {"auth", "status", "--provider=openai"},
		{"auth", "login", "--profile=pi"}, {"--model=x", "auth", "login"}, {"--mode=json", "auth", "login"},
		{"--print", "auth", "login"}, {"auth", "login", "--no-session"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("expected auth syntax to be rejected: %v", args)
		}
	}
	o, err := parse([]string{"--", "auth", "is a literal prompt"})
	if err != nil || o.command != printAnswer || strings.Join(o.prompt, " ") != "auth is a literal prompt" {
		t.Errorf("literal auth syntax: options=%+v error=%v", o, err)
	}
}

func TestParseStoreCutover(t *testing.T) {
	for _, action := range []string{"inspect", "prepare", "activate", "rollback"} {
		args := []string{"store", action, "--backend", "fixture.mjs"}
		if action == "prepare" {
			args = append(args, "--from", "pinned-hush")
		}
		o, err := parse(args)
		if err != nil || o.command != localStore || o.storeAction != action || o.bundle != "fixture.mjs" || o.interactive(true, true) {
			t.Errorf("store %s: options=%+v error=%v", action, o, err)
		}
	}
	for _, args := range [][]string{
		{"store", "upgrade"}, {"store", "prepare"}, {"store", "prepare", "--from="},
		{"store", "rollback", "--from=other"}, {"store", "activate", "--socket=/tmp/peer"},
		{"store", "inspect", "--profile=pi"}, {"--mode=rpc", "store", "inspect"}, {"--from=other", "status"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("expected store syntax to be rejected: %v", args)
		}
	}
}

// Fixture-only PTY driver. It sends input only after the matching prompt and
// checks both hidden input and restoration of the original terminal flags.
const codexPTY = `
import json, os, select, subprocess, sys, termios, time
binary, script = sys.argv[1:]
steps = json.loads(script)
master, slave = os.openpty()
initial = termios.tcgetattr(slave)
child = subprocess.Popen([binary, 'auth', 'login'], stdin=slave, stdout=slave, stderr=slave)
output = b''
try:
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if select.select([master], [], [], 0.1)[0]:
            chunk = os.read(master, 4096)
            output += chunk
            sys.stdout.buffer.write(chunk)
            sys.stdout.buffer.flush()
            if steps and steps[0][0].encode() in output:
                assert not termios.tcgetattr(slave)[3] & termios.ECHO
                os.write(master, steps.pop(0)[1].encode())
        elif child.poll() is not None:
            break
    assert not steps, 'auth prompt not reached'
    result = child.wait(timeout=2)
    mask = termios.ECHO | termios.ICANON
    assert (termios.tcgetattr(slave)[3] & mask) == (initial[3] & mask), 'tty flags not restored'
    sys.exit(result)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
`

func TestCodexAuthLoopback(t *testing.T) {
	bundle := os.Getenv("HUSH_TEST_BACKEND")
	if bundle == "" {
		t.Skip("set HUSH_TEST_BACKEND to the built bundle to run the Codex/PTY fixture")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the PTY fixture: ", err)
	}
	// Keep the managed socket below the short macOS Unix-socket path limit.
	home, err := os.MkdirTemp("/tmp", "hush-auth-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	dir := filepath.Join(home, ".hush")
	authFile := filepath.Join(dir, "auth.json")
	runtimeDir := filepath.Join(dir, "runtime")
	socket := filepath.Join(runtimeDir, "hush.sock")
	entry := filepath.Join(home, "fixture.mjs")
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	const account = "test-account-marker"
	const refresh = "test-refresh-marker"
	const rotated = "test-rotated-marker"
	const manual = "test-manual-input-marker"
	const deviceCode = "TEST-9ABC"
	const deviceID = "test-device-id-marker"
	const diagnostic = "test-provider-error-marker"
	const altKey = "test-alternative-key-marker"
	jwt := func(n int) string {
		payload := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q},"n":%d}`, account, n)
		return "fixture." + base64.StdEncoding.EncodeToString([]byte(payload)) + ".fixture"
	}
	// The bundled decoder accepts base64 (kept padded for Node's atob).
	original, fresh := jwt(1), jwt(2)
	var mu sync.Mutex
	var refreshes, exchanges, bearers, requests []string
	var devices int
	var failGeneration bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		payload, _ := io.ReadAll(r.Body)
		writeJSON := func(status int, value map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(value)
		}
		switch r.URL.Path {
		case "/oauth/token":
			form, _ := url.ParseQuery(string(payload))
			switch form.Get("grant_type") {
			case "authorization_code":
				exchanges = append(exchanges, form.Get("code"))
				if form.Get("code") == manual && form.Get("code_verifier") != "" {
					writeJSON(200, map[string]any{"access_token": original, "refresh_token": refresh, "expires_in": 3600})
					return
				}
			case "refresh_token":
				refreshes = append(refreshes, form.Get("refresh_token"))
				if form.Get("refresh_token") == refresh {
					writeJSON(200, map[string]any{"access_token": fresh, "refresh_token": rotated, "expires_in": 3600})
					return
				}
			}
		case "/api/accounts/deviceauth/usercode":
			devices++
			writeJSON(200, map[string]any{"device_auth_id": deviceID, "user_code": deviceCode, "interval": 1})
			return
		case "/api/accounts/deviceauth/token":
			writeJSON(403, map[string]any{"error": "deviceauth_authorization_pending"})
			return
		case "/fixture/responses":
			bearers = append(bearers, r.Header.Get("Authorization"))
			requests = append(requests, string(payload))
			if !json.Valid(payload) {
				writeJSON(400, map[string]any{"error": "expected plaintext JSON in fixture"})
				return
			}
			if failGeneration || r.Header.Get("Chatgpt-Account-Id") != account {
				writeJSON(401, map[string]any{"error": map[string]string{"message": diagnostic + " " + r.Header.Get("Authorization")}})
				return
			}
			codexFixtureReply(w)
			return
		}
		writeJSON(400, map[string]any{"error": diagnostic})
	}))
	defer provider.Close()
	// This prefix exists only in the temporary copy of the Node bundle. It
	// blocks unexpected destinations and disallows HTTP redirects/websockets.
	prefix := fmt.Sprintf(`{
  const fetchFixture = globalThis.fetch;
  // Keep the fixture's request bodies inspectable on Node versions with zstd.
  const zlib = process.getBuiltinModule('node:zlib');
  if (zlib) zlib.zstdCompressSync = undefined;
  globalThis.WebSocket = undefined;
  globalThis.fetch = (input, init) => {
    const url = new URL(input instanceof Request ? input.url : input);
    const auth = url.origin === 'https://auth.openai.com';
    const codex = ['https://chatgpt.com', 'https://api.openai.com'].includes(url.origin) && url.pathname.endsWith('/responses');
    if (!auth && !codex) throw new Error('Outside the loopback fixture');
    const local = new URL(%q);
    local.pathname = auth ? url.pathname : '/fixture/responses';
    return fetchFixture(input instanceof Request ? new Request(local, input) : local, { ...init, redirect: 'error' });
  };
}
`, provider.URL)
	production, err := os.ReadFile(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(production, []byte("#!")) {
		_, production, _ = bytes.Cut(production, []byte("\n"))
	}
	if err := os.WriteFile(entry, append([]byte(prefix), production...), 0o600); err != nil {
		t.Fatal(err)
	}
	pack, err := os.ReadFile(bundle + ".wasm-pack")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry+".wasm-pack", pack, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(home, "hush")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/hush")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v; %s", err, output)
	}
	env := []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "LANG=C", "NODE_NO_WARNINGS=1", "HUSH_BACKEND=" + entry}
	var ordinary strings.Builder
	command := func(exe string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Env, cmd.Dir = env, home
		return cmd
	}
	auth := func(action string, want int, marker string) {
		cmd := command(binary, "auth", action)
		out, err := cmd.CombinedOutput()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want || !strings.Contains(string(out), marker) {
			t.Fatalf("auth %s: %v; %s", action, err, out)
		}
		ordinary.Write(out)
	}
	interactive := func(steps [][2]string, want int, marker string) string {
		script, _ := json.Marshal(steps)
		cmd := command(python, "-c", codexPTY, binary, string(script))
		out, err := cmd.CombinedOutput()
		if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want || !strings.Contains(string(out), marker) {
			t.Fatalf("interactive auth: %v; %s", err, out)
		}
		return string(out)
	}
	// An environment key for the OpenAI API is not a Codex OAuth login.
	env = append(env, "OPENAI_API_KEY="+altKey)
	auth("status", 0, "no Hush-managed credential")
	env = env[:len(env)-1]
	auth("login", 1, "requires an interactive terminal")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("noninteractive auth created the profile: %v", err)
	}

	boot := func() (func(string, string) map[string]any, func()) {
		t.Helper()
		cmd := command(node, entry, "--profile", "hush", "--cwd", home)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		var conn net.Conn
		var stop sync.Once
		closeHost := func() {
			stop.Do(func() {
				if conn != nil {
					_ = conn.Close()
				}
				_ = cmd.Process.Signal(os.Interrupt)
				if err := cmd.Wait(); err != nil {
					t.Errorf("stop auth fixture host: %v; %s", err, &stderr)
				}
				ordinary.WriteString(stderr.String())
			})
		}
		t.Cleanup(closeHost)
		ready, err := bufio.NewReader(out).ReadBytes('\n')
		if err != nil || !bytes.Contains(ready, []byte(`"ready":true`)) {
			t.Fatalf("backend readiness: %v; %s", err, ready)
		}
		ordinary.Write(ready)
		conn, err = (&net.Dialer{}).DialContext(ctx, "unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		read := bufio.NewReader(conn)
		seq := 0
		call := func(method, params string) map[string]any {
			t.Helper()
			seq++
			if _, err := fmt.Fprintf(conn, "{\"type\":\"request\",\"id\":\"r%d\",\"method\":%q,\"params\":%s}\n", seq, method, params); err != nil {
				t.Fatal(err)
			}
			line, err := read.ReadBytes('\n')
			var reply map[string]any
			if err != nil || json.Unmarshal(line, &reply) != nil || reply["id"] != fmt.Sprintf("r%d", seq) {
				t.Fatalf("%s reply: %v; %s", method, err, line)
			}
			ordinary.Write(line)
			return reply
		}
		return call, closeHost
	}
	result := func(frame map[string]any) map[string]any {
		t.Helper()
		row, ok := frame["result"].(map[string]any)
		if frame["ok"] != true || !ok {
			t.Fatalf("expected success: %v", frame)
		}
		return row
	}
	failure := func(frame map[string]any, code, marker string) {
		t.Helper()
		row, ok := frame["error"].(map[string]any)
		if frame["ok"] != false || !ok || row["code"] != code || !strings.Contains(fmt.Sprint(row["message"]), marker) {
			t.Fatalf("expected failure %s: %v", code, frame)
		}
	}
	readFile := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if out, err := command(binary, "trust", "grant", "--scope=work").CombinedOutput(); err != nil {
		t.Fatalf("fixture workspace grant: %v: %s", err, out)
	}
	call, stop := boot()
	hello := func() any {
		reply := result(call("hello", `{"versions":[1],"capabilities":{"hushProfile":[1],"intro":[2],"modelCatalog":[1]}}`))
		if caps, ok := reply["capabilities"].(map[string]any); !ok || caps["modelCatalog"] != float64(1) || caps["piEvents"] != nil {
			t.Fatalf("expected native catalog without Pi: %v", reply)
		}
		return reply["epoch"]
	}
	failure(call("ext.modelCatalog.list", `{"threadId":"1"}`), "not_negotiated", "")
	epoch := hello()
	thread := result(call("thread.new", `{}`))["id"].(string)
	scope := fmt.Sprintf(`"threadId":%q`, thread)
	catalog := func(available bool) {
		t.Helper()
		report := result(call("ext.modelCatalog.list", "{"+scope+"}"))
		current, _ := report["current"].(map[string]any)
		models, _ := report["models"].([]any)
		if current["provider"] != "openai-codex" || len(models) == 0 || len(models) > 64 {
			t.Fatal("missing selected Codex model in bounded native catalog")
		}
		row, _ := models[0].(map[string]any)
		if row["provider"] != current["provider"] || row["modelId"] != current["modelId"] || row["available"] != available ||
			(!available && row["reason"] != "credentials_required") || (available && row["reason"] != nil) {
			t.Fatal("catalog did not reflect live Codex auth for the saved selection")
		}
	}
	failure(call("ext.modelCatalog.list", `{"threadId":"01"}`), "invalid_request", "")
	failure(call("ext.modelCatalog.list", "{"+scope+`,"query":"x"}`), "invalid_request", "")
	failure(call("ext.modelCatalog.unknown", "{"+scope+"}"), "unknown_method", "")
	failure(call("ext.modelCatalog.list", `{"threadId":"9007199254740991"}`), "not_found", "")
	catalog(false)
	submit := func(key string) map[string]any {
		return call("submit", fmt.Sprintf(`{%s,"requestId":%q,"text":"Explain the auth fixture","whenBusy":"reject"}`, scope, key))
	}
	missing := func(key string) {
		failure(submit(key), "invalid_request", "hush auth login")
		failure(call("submission.get", fmt.Sprintf(`{%s,"requestId":%q}`, scope, key)), "not_found", "")
	}
	ask := func(key, status string) string {
		id := result(submit(key))["submissionId"].(string)
		settled := result(call("submission.wait", fmt.Sprintf(`{%s,"submissionId":%q}`, scope, id)))
		if settled["status"] != status {
			t.Fatalf("settlement: %v", settled)
		}
		return id
	}
	missing("before-login")
	failure(call("auth.login", `{}`), "unknown_method", "")
	signedIn := interactive([][2]string{{"[1] Browser", "1\n"}, {"(hidden)", manual + "\n"}}, 0, "signed in with OAuth")
	auth("status", 0, "OAuth configured")
	for path, mode := range map[string]os.FileMode{dir: 0o700, authFile: 0o600} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Fatalf("wrong private profile mode: %v; %v", st, err)
		}
	}
	catalog(true)
	kept := ask("active-login", "done")
	other := result(call("thread.new", `{"model":{"provider":"openai","modelId":"gpt-4.1"}}`))["id"]
	failure(call("submit", fmt.Sprintf(`{"threadId":%q,"requestId":"no-fallback","text":"No fallback","whenBusy":"reject"}`, other)),
		"invalid_request", "Selected model unavailable")
	saved := readFile(authFile)
	cancelled := interactive([][2]string{{"[1] Browser", "2\n"}, {"Waiting for browser approval", "\x03"}}, 130, "sign-in cancelled")
	if !bytes.Equal(saved, readFile(authFile)) {
		t.Fatal("cancelled login replaced the prior credential")
	}
	history := func() []byte {
		t.Helper()
		page := result(call("history", fmt.Sprintf(`{%s,"range":{"kind":"before","beforeEntryId":null},"limit":50}`, scope)))
		entries, err := json.Marshal(page["entries"])
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	ask("after-cancel", "done")
	priorHistory := history()
	if !bytes.Contains(priorHistory, []byte("Explain the auth fixture")) || !bytes.Contains(priorHistory, []byte("Loopback Codex reply")) {
		t.Fatal("expected committed user and assistant text before restart")
	}
	stop()
	mu.Lock()
	beforeOK := len(exchanges) == 1 && exchanges[0] == manual && devices == 1 && len(refreshes) == 0 &&
		len(bearers) == 2 && bearers[0] == "Bearer "+original && bearers[1] == "Bearer "+original
	mu.Unlock()
	if !beforeOK {
		t.Fatal("unexpected login exchanges or generation credentials")
	}

	// Age the fixture credential on disk only while the host is stopped.
	var credentials map[string]map[string]any
	if err := json.Unmarshal(saved, &credentials); err != nil {
		t.Fatal(err)
	}
	credentials["openai-codex"]["expires"] = 1
	credentials["other-fixture-provider"] = map[string]any{"type": "api_key", "key": altKey}
	aged, _ := json.Marshal(credentials)
	if err := os.WriteFile(authFile, aged, 0o600); err != nil {
		t.Fatal(err)
	}
	private := command(binary, "--no-session", "--print", "private invocation with stale OAuth")
	if output, err := private.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("do not refresh OAuth")) ||
		!bytes.Equal(readFile(authFile), aged) {
		t.Fatal("private invocation did not reject stale OAuth without a write")
	}
	call, stop = boot()
	if hello() == epoch {
		t.Fatal("backend did not restart")
	}
	if !bytes.Equal(history(), priorHistory) {
		t.Fatal("reopened thread did not return its original committed history before the new prompt")
	}
	ask("after-restart", "done")
	mu.Lock()
	refreshOK := len(refreshes) == 1 && refreshes[0] == refresh && len(bearers) == 3 && bearers[2] == "Bearer "+fresh
	failGeneration = true
	mu.Unlock()
	var persisted map[string]map[string]any
	if err := json.Unmarshal(readFile(authFile), &persisted); err != nil {
		t.Fatal("invalid persisted credential file")
	}
	codex := persisted["openai-codex"]
	expires, _ := codex["expires"].(float64)
	if !refreshOK || codex["type"] != "oauth" || codex["access"] != fresh || codex["refresh"] != rotated ||
		expires <= float64(time.Now().UnixMilli()) || persisted["other-fixture-provider"]["key"] != altKey {
		t.Fatal("expected one refresh, a generation using the new bearer, and persisted rotation without changing the other entry")
	}
	if page := history(); bytes.Equal(page, priorHistory) || !bytes.Contains(page, []byte("Loopback Codex reply")) {
		t.Fatal("expected an updated history after the reopened thread accepted a new prompt")
	}
	t.Log("loopback: CLI login, original history after restart, one expired-token refresh, new bearer on a completed prompt, rotated record on disk")
	ask("provider-error", "unanswered")
	var issue map[string]any
	for i := 0; i < 50; i++ {
		intro := result(call("ext.intro.get", "{"+scope+"}"))
		threadReport, _ := intro["thread"].(map[string]any)
		issue, _ = threadReport["issue"].(map[string]any)
		if issue["retry"] == true {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if issue["retry"] != true {
		t.Fatalf("no retry for the failed generation: %v", issue)
	}
	mu.Lock()
	beforeLogout := len(bearers)
	mu.Unlock()
	auth("logout", 0, "no Hush-managed credential")
	catalog(false)
	failure(call("thread.configure", "{"+scope+`,"change":{"thinkingLevel":"off"}}`), "invalid_request", "hush auth login")
	missing("after-logout")
	print := command(binary, "--print", "--thread", thread, "print after logout")
	var diagnosticOutput bytes.Buffer
	print.Stderr = &diagnosticOutput
	if output, err := print.Output(); err == nil || len(output) != 0 || !strings.Contains(diagnosticOutput.String(), "hush auth login") {
		t.Fatalf("native print after logout: %s %v", output, err)
	}
	ordinary.WriteString(diagnosticOutput.String())
	failure(call("continue", "{"+scope+"}"), "invalid_request", "hush auth login")
	failure(call("ext.intro.retry", fmt.Sprintf(`{%s,"taskId":%q}`, scope, issue["taskId"])), "invalid_request", "hush auth login")
	if result(submit("active-login"))["submissionId"] != kept {
		t.Fatal("logout hid a keyed receipt")
	}
	stop()
	mu.Lock()
	afterLogout := len(bearers)
	ordinary.WriteString(strings.Join(requests, "\n"))
	mu.Unlock()
	if afterLogout != beforeLogout {
		t.Fatal("provider request after logout")
	}

	models := filepath.Join(dir, "models.json")
	config := fmt.Sprintf(`{"providers":{"openai-codex":{"apiKey":%q,"baseUrl":"https://chatgpt.com/backend-api"}}}`, altKey)
	if err := os.WriteFile(models, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	auth("logout", 0, "configured alternative remains")
	remaining := map[string]map[string]any{}
	if err := json.Unmarshal(readFile(authFile), &remaining); err != nil {
		t.Fatal(err)
	}
	if string(readFile(models)) != config || remaining["openai-codex"] != nil || remaining["other-fixture-provider"]["key"] != altKey {
		t.Fatal("logout changed unrelated auth or kept the managed credential")
	}
	ordinary.Write(readFile(filepath.Join(runtimeDir, "hush.log")))
	for _, secret := range []string{account, original, fresh, refresh, rotated, manual, deviceCode, deviceID, diagnostic, altKey} {
		if strings.Contains(ordinary.String(), secret) || strings.Contains(signedIn, secret) || secret != deviceCode && strings.Contains(cancelled, secret) {
			t.Fatal("found credential material outside the intended transport or device-code display")
		}
	}
}

func codexFixtureReply(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	event := func(kind string, fields string) {
		fmt.Fprintf(w, "data: {\"type\":%q,%s}\n\n", kind, fields)
	}
	event("response.created", `"response":{"id":"fixture","status":"in_progress","output":[]}`)
	event("response.output_item.added", `"output_index":0,"item":{"id":"message","type":"message","role":"assistant","status":"in_progress","content":[]}`)
	event("response.content_part.added", `"output_index":0,"item_id":"message","content_index":0,"part":{"type":"output_text","text":"","annotations":[]}`)
	event("response.output_text.delta", `"output_index":0,"item_id":"message","content_index":0,"delta":"Loopback Codex reply"`)
	event("response.output_text.done", `"output_index":0,"item_id":"message","content_index":0,"text":"Loopback Codex reply"`)
	event("response.content_part.done", `"output_index":0,"item_id":"message","content_index":0,"part":{"type":"output_text","text":"Loopback Codex reply","annotations":[]}`)
	event("response.output_item.done", `"output_index":0,"item":{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Loopback Codex reply","annotations":[]}]}`)
	event("response.completed", `"response":{"id":"fixture","status":"completed","usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestParseLiteralPromptAndTrailingFlags(t *testing.T) {
	o, err := parse([]string{"--", "status", "--no-session"})
	if err != nil || o.command != printAnswer || o.ephemeral || strings.Join(o.prompt, " ") != "status --no-session" {
		t.Fatalf("literal prompt = %+v, %v", o, err)
	}
	o, err = parse([]string{"status", "--socket", "/tmp/hush.sock", "--thread=42", "--json"})
	if err != nil || o.command != backendStatus || o.socket != "/tmp/hush.sock" || o.thread != "42" || !o.json {
		t.Fatalf("status = %+v, %v", o, err)
	}
	o, err = parse([]string{"prompt", "--no-session", "--backend", "bundle.mjs"})
	if err != nil || o.command != printAnswer || !o.ephemeral || o.bundle != "bundle.mjs" {
		t.Fatalf("private print = %+v, %v", o, err)
	}
}

func TestLiteralAtReferenceInNativePrint(t *testing.T) {
	o, err := parse([]string{"--print", "Read", "@notes.txt"})
	if err != nil || o.command != printAnswer || strings.Join(o.prompt, " ") != "Read @notes.txt" {
		t.Fatalf("literal @ reference = %+v, %v", o, err)
	}
}

func TestReadPromptUsesInputOnlyWithoutArgs(t *testing.T) {
	input := strings.NewReader("piped\n")
	if text, err := readPrompt([]string{"from", "args"}, input, false); err != nil || text != "from args" {
		t.Fatalf("arguments = %q, %v", text, err)
	}
	if text, err := readPrompt(nil, input, false); err != nil || text != "piped\n" {
		t.Fatalf("stdin = %q, %v", text, err)
	}
	if _, err := readPrompt(nil, strings.NewReader("not read"), true); err == nil {
		t.Fatal("no arguments and terminal stdin must not read")
	}
}

func TestReadyLine(t *testing.T) {
	for _, tc := range []struct {
		fragments []string
		ok        bool
	}{
		{[]string{"{\"ready\":", "true,\"socket\":\"/test/hush.sock\"}\nignored"}, true},
		{[]string{"{\"ready\":true,\"socket\":\"/different.sock\"}\n"}, false},
		{[]string{"unframed log\n"}, false},
		{[]string{strings.Repeat("x", 4097), "\n"}, false},
	} {
		r := readyLine{socket: "/test/hush.sock", result: make(chan error, 1)}
		for _, part := range tc.fragments {
			n, err := r.Write([]byte(part))
			if n != len(part) || err != nil {
				t.Fatalf("write readiness bytes = %d, %v", n, err)
			}
		}
		select {
		case err := <-r.result:
			if (err == nil) != tc.ok {
				t.Fatalf("ready = %v, want ok=%v", err, tc.ok)
			}
		default:
			t.Fatal("missing readiness result")
		}
	}
}

func TestParseServiceStatusJSON(t *testing.T) {
	o, err := parse([]string{"service", "status", "--json"})
	if err != nil || o.command != localService || o.serviceAction != "status" || !o.json {
		t.Fatalf("service status --json options = %+v, %v", o, err)
	}
	for _, action := range []string{"install", "pause", "resume", "restart", "uninstall"} {
		if _, err := parse([]string{"service", action, "--json"}); err == nil || !strings.Contains(err.Error(), "only status accepts --json") {
			t.Errorf("service %s --json = %v, want a status-only error", action, err)
		}
	}
}
