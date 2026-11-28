package pane

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/javiermolinar/hush/sdk"
)

func fixtureID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func fixtureBlock(n int) string { return "block:" + fixtureID(n) }

var fixtureWindow = "window:" + fixtureID(10)
var fixtureSession = "session:" + fixtureID(11)

type nativeFixture struct {
	Kind, Socket, Failure     string
	Tabs, Read, Stale, Remote bool
	Focused                   int
	Rows                      []fixtureRow
	Effects                   [][]string
}

type fixtureRow struct{ ID, Pane, Window int }

func saveFixture(path string, s nativeFixture) {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		panic(err)
	}
}

func loadFixture(path string) (s nativeFixture) {
	data, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(data, &s); err != nil {
		panic(err)
	}
	return s
}

func TestNativePaneCLIHelper(t *testing.T) {
	path := os.Getenv("HUSH_PANE_FIXTURE")
	if path == "" {
		return
	}
	args := os.Args[slices.Index(os.Args, "--")+1:]
	s := loadFixture(path)
	var output any
	var fail bool
	place := func(r fixtureRow) cmuxPlace {
		return cmuxPlace{Workspace: fixtureID(11), Window: fixtureID(r.Window), Pane: fixtureID(r.Pane), Surface: fixtureID(r.ID)}
	}
	create := func(tab bool) fixtureRow {
		s.Effects = append(s.Effects, args)
		r := fixtureRow{ID: len(s.Rows) + 1, Window: 10, Pane: len(s.Rows) + 21}
		if tab && s.Kind == "rex" {
			r.Window = 50 + r.ID
		}
		if tab && s.Kind == "cmux" {
			r.Pane = 21
		}
		if s.Failure != "unverified" {
			s.Rows = append(s.Rows, r)
		}
		saveFixture(path, s)
		return r
	}
	if s.Kind == "rex" {
		if !slices.Contains(args, "--autostart=false") {
			os.Exit(20)
		}
		i := 2
		if args[i] == "-S" {
			i += 2
		}
		rest := args[i:]
		switch rest[0] {
		case "whoami":
			origin := fixtureBlock(1)
			if slices.Contains(args, "-S") {
				origin = "" // Rex omits the inherited caller with an explicit server
			}
			transport := "unix"
			if s.Remote {
				transport = "tcp"
			}
			endpoint := (&url.URL{Scheme: "unix", Path: s.Socket}).String()
			output = map[string]any{"principal": map[string]string{"transport": transport}, "endpoint": endpoint,
				"instance_id": "fixture-instance", "host_id": "fixture-host", "session_id": fixtureSession, "window_id": fixtureWindow, "block_id": origin}
		case "block":
			if !slices.Contains(rest, fixtureSession) {
				os.Exit(21)
			}
			rows := make([]rexBlock, 0, len(s.Rows))
			for _, r := range s.Rows {
				if s.Stale && r.ID == 1 {
					continue
				}
				rows = append(rows, rexBlock{ID: fixtureBlock(r.ID), Window: "window:" + fixtureID(r.Window),
					Host: "fixture-host", Creator: "com.superlogical.terminal", Placement: "placed"})
			}
			output = map[string]any{"blocks": rows}
		case "split", "run":
			r := create(rest[0] == "run")
			ids := []string{fixtureBlock(r.ID)}
			switch s.Failure {
			case "missing":
				ids = nil
			case "ambiguous":
				ids = append(ids, fixtureBlock(2))
			case "existing":
				ids[0] = fixtureBlock(2)
			case "lost":
				fail = true
			}
			output = map[string]any{"block_ids": ids, "session_id": fixtureSession, "window_id": "window:" + fixtureID(r.Window),
				"anchor_id": fixtureBlock(1)}
		case "focus":
			s.Effects = append(s.Effects, args)
			for _, row := range s.Rows {
				if fixtureBlock(row.ID) == rest[len(rest)-1] {
					s.Focused = row.ID
				}
			}
			saveFixture(path, s)
			output = map[string]any{}
		case "session":
			r := s.Rows[s.Focused-1]
			wid := "window:" + fixtureID(r.Window)
			output = map[string]any{"session_id": fixtureSession, "active_window_id": wid, "windows": []any{map[string]string{"window_id": wid, "focused_block_id": fixtureBlock(r.ID)}}}
		case "capture":
			fmt.Print("one\ntwo\nthree\n")
			os.Exit(0)
		}
	} else {
		if !slices.Equal(args[:2], []string{"--socket", s.Socket}) {
			os.Exit(22)
		}
		origin := place(s.Rows[0])
		if args[len(args)-1] == "identify" {
			if s.Stale {
				origin = place(s.Rows[1])
			}
			output = map[string]any{"caller": origin, "focused": place(s.Rows[s.Focused-1]), "socket_path": s.Socket}
		} else if args[2] == "read-screen" {
			fmt.Print("one\ntwo\nthree\n")
			os.Exit(0)
		} else {
			var params map[string]any
			if args[3] != "rpc" || json.Unmarshal([]byte(args[5]), &params) != nil {
				os.Exit(23)
			}
			switch args[4] {
			case "system.capabilities":
				methods := []string{"surface.split", "surface.focus", "surface.list"}
				if s.Tabs {
					methods = append(methods, "surface.create")
				}
				if s.Read {
					methods = append(methods, "surface.read_text")
				}
				output = map[string]any{"methods": methods}
			case "workspace.list":
				output = map[string]any{"workspaces": []any{map[string]any{"id": origin.Workspace, "remote": map[string]bool{"enabled": s.Remote}}}}
			case "surface.list":
				rows := make([]cmuxSurface, 0, len(s.Rows))
				for _, r := range s.Rows {
					rows = append(rows, cmuxSurface{ID: fixtureID(r.ID), Pane: fixtureID(r.Pane), Type: "terminal", Focused: s.Focused == r.ID})
				}
				output = map[string]any{"window_id": origin.Window, "workspace_id": origin.Workspace, "surfaces": rows}
			case "surface.split", "surface.create":
				r := create(args[4] == "surface.create")
				p := place(r)
				switch s.Failure {
				case "missing":
					p.Surface = ""
				case "ambiguous":
					fmt.Printf(`{"surface_id":%q,"surface_id":%q,"pane_id":%q,"workspace_id":%q,"window_id":%q,"type":"terminal"}`,
						p.Surface, fixtureID(2), p.Pane, p.Workspace, p.Window)
					os.Exit(0)
				case "existing":
					p.Surface = fixtureID(2)
				case "lost":
					fail = true
				}
				output = map[string]any{"window_id": p.Window, "workspace_id": p.Workspace, "pane_id": p.Pane, "surface_id": p.Surface, "type": "terminal"}
			case "surface.focus":
				s.Effects = append(s.Effects, args)
				for _, row := range s.Rows {
					if fixtureID(row.ID) == params["surface_id"] {
						s.Focused = row.ID
					}
				}
				saveFixture(path, s)
				output = place(s.Rows[s.Focused-1])
			}
		}
	}
	if output == nil || fail {
		os.Exit(24)
	}
	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		os.Exit(25)
	}
	os.Exit(0)
}

func fakeAdapter(t *testing.T, kind string, extras bool) (Adapter, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hp-") // keep the Unix socket path short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	path := filepath.Join(dir, "fixture.json")
	saveFixture(path, nativeFixture{Kind: kind, Socket: socket, Tabs: extras, Read: extras, Focused: 2,
		Rows: []fixtureRow{{1, 21, 10}, {2, 22, 10}}})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(dir, kind)
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec \"$HUSH_PANE_TEST_EXE\" -test.run '^TestNativePaneCLIHelper$' -- \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HUSH_PANE_TEST_EXE="+exe, "HUSH_PANE_FIXTURE="+path, "GORACE=atexit_sleep_ms=0")
	var adapter Adapter
	if kind == "rex" {
		adapter, err = bindRex(t.Context(), cli, env, fixtureBlock(1))
	} else {
		adapter, err = bindCmux(t.Context(), cli, env, socket, fixtureID(11), fixtureID(1))
	}
	if err != nil {
		t.Fatal(err)
	}
	return adapter, path
}

func requireConfirmed[T any](t *testing.T, result Result[T]) T {
	t.Helper()
	out, ok := result.Value()
	if !ok {
		t.Fatalf("got %s: %v", result.Outcome(), result.Err())
	}
	return out
}

func TestNativePaneExactTargets(t *testing.T) {
	for _, kind := range []string{"rex", "cmux"} {
		t.Run(kind, func(t *testing.T) {
			a, path := fakeAdapter(t, kind, true)
			ctx := t.Context()
			want := []Placement{Right, Below, Left, Above, Tab}
			if !slices.Equal(a.Capabilities().Placements(), want) {
				t.Fatal(a.Capabilities())
			}
			dir := filepath.Join(t.TempDir(), "a worktree's dir")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"", "two words", "a'b", "$HOME", "*;&", "hi\nthere"}
			var created Ref
			for _, p := range want {
				req := OpenRequest{Placement: p, Dir: dir, Launch: Launch{Mode: LaunchArgv, Program: "/usr/bin/printf", Args: args}}
				created = requireConfirmed(t, a.Open(ctx, req))
				s := loadFixture(path)
				call := s.Effects[len(s.Effects)-1]
				if s.Focused != 2 {
					t.Fatal("open changed focus or test no longer has a competing focus")
				}
				if kind == "rex" {
					if !slices.Contains(call, fixtureSession) || !slices.Contains(call, dir) || !slices.Contains(call, "--shell=none") ||
						!slices.Contains(call, "--focus=false") || !slices.Equal(call[slices.Index(call, "--")+1:], append([]string{req.Launch.Program}, args...)) {
						t.Fatal(call)
					}
					if p == Tab && !slices.Contains(call, "run") || p != Tab && (!slices.Contains(call, "--split="+string(p)) || !slices.Contains(call, fixtureBlock(1))) {
						t.Fatal(call)
					}
				} else {
					var params map[string]any
					if err := json.Unmarshal([]byte(call[5]), &params); err != nil {
						t.Fatal(err)
					}
					if params["surface_id"] != fixtureID(1) || params["window_id"] != fixtureID(10) || params["workspace_id"] != fixtureID(11) || params["focus"] != false || params["remote_context"] != "local" {
						t.Fatal(params)
					}
					if p == Tab {
						if call[4] != "surface.create" || params["pane_id"] != fixtureID(21) {
							t.Fatal(call)
						}
					} else {
						direction := map[Placement]string{Right: "right", Below: "down", Left: "left", Above: "up"}[p]
						if call[4] != "surface.split" || params["direction"] != direction {
							t.Fatal(call)
						}
					}
					if params["initial_command"] != cmuxCommand(req) {
						t.Fatal("command changed")
					}
				}
			}
			panes := requireConfirmed(t, a.List(ctx, ListRequest{}))
			if len(panes) != 7 || !slices.Contains(panes, created) {
				t.Fatal("list omitted verified terminal")
			}
			if got := requireConfirmed(t, a.Focus(ctx, FocusRequest{Pane: created})); got != created {
				t.Fatal("focus changed target")
			}
			if s := loadFixture(path); s.Focused != 7 {
				t.Fatal("native focus selected another pane")
			}
			if got := requireConfirmed(t, a.Read(ctx, ReadRequest{Pane: created, Lines: 2})); got.Text != "two\nthree\n" || !got.Truncated {
				t.Fatalf("capture: %+v", got)
			}
			other := NewScope().Ref(created.id)
			if a.Focus(ctx, FocusRequest{Pane: other}).Outcome() != Rejected {
				t.Fatal("accepted foreign scope")
			}
		})
	}
}

func TestNativePaneDoesNotGuessOrReplay(t *testing.T) {
	for _, kind := range []string{"rex", "cmux"} {
		t.Run(kind, func(t *testing.T) {
			a, path := fakeAdapter(t, kind, false)
			ctx := t.Context()
			req := OpenRequest{Placement: Tab, Dir: t.TempDir(), Launch: Launch{Mode: LaunchArgv, Program: "example"}}
			if kind == "cmux" && (a.Open(ctx, req).Outcome() != Rejected || len(loadFixture(path).Effects) != 0 || a.Capabilities().Read) {
				t.Fatal("unsupported capability exposed or dispatched")
			}
			req.Placement = Right
			for _, failure := range []string{"missing", "ambiguous", "existing", "unverified", "lost"} {
				s := loadFixture(path)
				s.Failure = failure
				saveFixture(path, s)
				result := a.Open(ctx, req)
				if ref, ok := result.Value(); ok || ref != (Ref{}) || result.Outcome() != Unknown {
					t.Fatalf("%s: got %s", failure, result.Outcome())
				}
				if len(loadFixture(path).Effects) != len(s.Effects)+1 {
					t.Fatalf("%s: repeated dispatch", failure)
				}
			}
			s := loadFixture(path)
			s.Stale = true
			saveFixture(path, s)
			if got := a.Open(ctx, req).Outcome(); got != Rejected || len(loadFixture(path).Effects) != len(s.Effects) {
				t.Fatalf("stale: %s", got)
			}
			s.Stale, s.Remote = false, true
			saveFixture(path, s)
			if got := a.Open(ctx, req).Outcome(); got != Rejected || len(loadFixture(path).Effects) != len(s.Effects) {
				t.Fatalf("remote: %s", got)
			}
		})
	}
}

func TestCmuxLiteralCommandAndWorkingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "checkout ' with spaces")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"", "with spaces", "a'b\"c", "$HOME", "$(not a command)", "line one\nline two"}
	r := OpenRequest{Dir: dir, Launch: Launch{Mode: LaunchArgv, Program: "/bin/sh", Args: append([]string{"-c", `printf '%s\0' "$PWD" "$@"`, "probe"}, args...)}}
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", cmuxCommand(r)).CombinedOutput()
	if err != nil || string(out) != strings.Join(append([]string{canonical}, args...), "\x00")+"\x00" {
		t.Fatalf("literal launch: %q, %v", out, err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if out, err = exec.CommandContext(t.Context(), "/bin/sh", "-c", cmuxCommand(r)).Output(); err == nil || len(out) != 0 {
		t.Fatalf("unavailable cwd ran command: %q, %v", out, err)
	}
}

func guardArgs(t *testing.T, id sdk.PaneFileIdentity, argv ...string) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{exe, LaunchGuardArg, id.Device, id.Inode, id.Birth}, argv...)
}

func TestPaneLaunchGuardHelper(t *testing.T) {
	if i := slices.Index(os.Args, LaunchGuardArg); i >= 0 {
		os.Exit(RunLaunchGuard(os.Args[i+1:], os.Stderr))
	}
}

func TestNativePaneLaunchChecksEnteredDirectory(t *testing.T) {
	for _, kind := range []string{"rex", "cmux"} {
		t.Run(kind, func(t *testing.T) {
			a, path := fakeAdapter(t, kind, true)
			dir := filepath.Join(t.TempDir(), "checkout ' space")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			stamp, ok := localIdentity(dir, os.ModeDir)
			if !ok {
				t.Fatal("missing original identity")
			}
			literal := []string{"", "space and '$HOME'", "line one\nline two"}
			r := OpenRequest{Placement: Right, Dir: dir, Directory: &stamp,
				Launch: Launch{Mode: LaunchArgv, Program: "/usr/bin/printf", Args: append([]string{"<%s>\n"}, literal...)}}
			requireConfirmed(t, a.Open(t.Context(), r))
			wire := loadFixture(path).Effects[0]
			var cmd func() *exec.Cmd
			if kind == "rex" {
				i := slices.Index(wire, "--")
				argv := wire[i+1:]
				if !slices.Equal(argv, guardArgs(t, stamp, launchArgv(r.Launch)...)) || wire[i-1] != dir {
					t.Fatalf("Rex omitted the pinned launch: %v", wire)
				}
				cmd = func() *exec.Cmd {
					c := exec.CommandContext(t.Context(), argv[0], append([]string{"-test.run=^TestPaneLaunchGuardHelper$", "--"}, argv[1:]...)...)
					c.Dir = dir // Rex enters the requested pathname before launching.
					return c
				}
			} else {
				params := map[string]any{}
				if err := json.Unmarshal([]byte(wire[5]), &params); err != nil {
					t.Fatal(err)
				}
				argv := guardArgs(t, stamp, launchArgv(r.Launch)...)
				text := params["initial_command"].(string)
				needle := quotePOSIX(argv[0]) + " " + quotePOSIX(LaunchGuardArg)
				if !strings.Contains(text, needle) {
					t.Fatalf("cmux omitted the pinned launch: %s", text)
				}
				text = strings.Replace(text, needle, quotePOSIX(argv[0])+" -test.run=^TestPaneLaunchGuardHelper$ -- "+quotePOSIX(LaunchGuardArg), 1)
				cmd = func() *exec.Cmd { return exec.CommandContext(t.Context(), "/bin/sh", "-c", text) }
			}
			if out, err := cmd().CombinedOutput(); err != nil || string(out) != "<>\n<space and '$HOME'>\n<line one\nline two>\n" {
				t.Fatalf("original directory or literal arguments lost: %q %v", out, err)
			}
			if err := os.Rename(dir, dir+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if out, err := cmd().CombinedOutput(); err == nil || !strings.Contains(string(out), "working directory changed") || strings.Contains(string(out), "line two") {
				t.Fatalf("replacement directory executed the requested command: %q %v", out, err)
			}
		})
	}
}

func TestPaneHandlerReadRequiresSameConnectionCommandOpen(t *testing.T) {
	a, _ := fakeAdapter(t, "rex", true)
	h := NewHandler(a, "")
	ctx := t.Context()
	listed := h.Handle(ctx, sdk.PaneRequest{Op: "list"})
	for _, ref := range listed.Panes {
		if got := h.Handle(ctx, sdk.PaneRequest{Op: "read", Pane: ref, Lines: 1}); got.Outcome != "rejected" {
			t.Fatal("listing granted read")
		}
	}
	dir := t.TempDir()
	stamp, ok := localIdentity(dir, os.ModeDir)
	if !ok {
		t.Fatal("missing directory identity")
	}
	opened := h.Handle(ctx, sdk.PaneRequest{Op: "open", Placement: "right", Dir: dir, Directory: stamp,
		Launch: sdk.PaneLaunch{Mode: "argv", Program: "example"}})
	if opened.Outcome != "confirmed" || !strings.HasPrefix(opened.Pane, "hp_") {
		t.Fatal(opened)
	}
	if got := h.Handle(ctx, sdk.PaneRequest{Op: "read", Pane: opened.Pane, Lines: 1}); got.Outcome != "confirmed" || got.Text != "three\n" {
		t.Fatal(got)
	}
	other := NewHandler(a, "")
	if got := other.Handle(ctx, sdk.PaneRequest{Op: "read", Pane: opened.Pane, Lines: 1}); got.Outcome != "rejected" {
		t.Fatal("new connection reused handle")
	}
	for _, ref := range other.Handle(ctx, sdk.PaneRequest{Op: "list"}).Panes {
		if got := other.Handle(ctx, sdk.PaneRequest{Op: "read", Pane: ref, Lines: 1}); got.Outcome != "rejected" {
			t.Fatal("new connection widened read provenance")
		}
	}
}

func TestPaneHandlerTypedViewsNeverGrantRead(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	a, path := fakeAdapter(t, "rex", true)
	h := NewHandler(a, "/local/hush with spaces")
	dir := t.TempDir()
	stamp, ok := localIdentity(dir, os.ModeDir)
	if !ok || !h.Capabilities().Views {
		t.Fatal("view fixture missing a local directory or executable")
	}
	open := sdk.PaneRequest{Op: "open", Placement: "right", Dir: dir, Directory: stamp,
		Launch: sdk.PaneLaunch{Mode: "hush"}}
	if r := h.Handle(t.Context(), open); r.Outcome != "rejected" || len(loadFixture(path).Effects) != 0 {
		t.Fatal("view without local attachment dispatches", r)
	}
	open.Attachment = sdk.PaneAttachment{Socket: "/local/backend sock", Epoch: "current-epoch"}
	for i, tc := range []struct {
		launch sdk.PaneLaunch
		argv   []string
	}{
		{sdk.PaneLaunch{Mode: "terminal"}, []string{"/bin/sh", "-i"}},
		{sdk.PaneLaunch{Mode: "hush"}, []string{"/local/hush with spaces", "--local", "--socket", open.Attachment.Socket, "--pane-epoch", open.Attachment.Epoch}},
	} {
		open.Launch = tc.launch
		r := h.Handle(t.Context(), open)
		if r.Outcome != "confirmed" || !strings.HasPrefix(r.Pane, "hp_") {
			t.Fatal(r)
		}
		effects := loadFixture(path).Effects
		if len(effects) != i+1 || !slices.Equal(effects[i][slices.Index(effects[i], "--")+1:], guardArgs(t, stamp, tc.argv...)) {
			t.Fatalf("wrong argv for %s: %v", tc.launch.Mode, effects)
		}
		if got := h.Handle(t.Context(), sdk.PaneRequest{Op: "read", Pane: r.Pane, Lines: 1}); got.Outcome != "rejected" {
			t.Fatal("typed view granted read", got)
		}
	}
}

func TestExistingThreadTabAndSplitDispatchSeparateEffects(t *testing.T) {
	a, path := fakeAdapter(t, "rex", true)
	var tab sdk.ID
	var focused bool
	h := NewThreadHandler(a, "/local/hush with spaces", func(_ context.Context, id sdk.ID, focus bool) Outcome {
		tab, focused = id, focus
		return Confirmed
	})
	if r := h.Handle(t.Context(), sdk.PaneRequest{Op: "thread_open", ThreadID: "8", Where: "focus"}); r.Outcome != "confirmed" || r.Where != "focus" ||
		tab != "8" || !focused || len(loadFixture(path).Effects) != 0 {
		t.Fatal("focus did not open and switch to the TUI tab", r, tab, focused)
	}
	if r := h.Handle(t.Context(), sdk.PaneRequest{Op: "thread_open", ThreadID: "7", Where: "tab"}); r.Outcome != "confirmed" || tab != "7" || focused ||
		len(loadFixture(path).Effects) != 0 {
		t.Fatal("tab open attempted a native launch or took focus", r, tab)
	}
	dir := t.TempDir()
	stamp, ok := localIdentity(dir, os.ModeDir)
	if !ok {
		t.Fatal("missing directory identity")
	}
	open := sdk.PaneRequest{Op: "thread_open", ThreadID: "9", Where: "split", Dir: dir, Directory: stamp,
		Attachment: sdk.PaneAttachment{Socket: "/local/backend sock", Epoch: "current-epoch"}}
	if r := h.Handle(t.Context(), open); r.Outcome != "confirmed" || r.Where != "split" || r.ThreadID != "9" || tab != "7" {
		t.Fatal("split did not open the expected view", r, tab)
	}
	effects := loadFixture(path).Effects
	want := []string{"/local/hush with spaces", "--local", "--thread", "9", "--socket", open.Attachment.Socket, "--pane-epoch", open.Attachment.Epoch}
	if len(effects) != 1 || !slices.Equal(effects[0][slices.Index(effects[0], "--")+1:], guardArgs(t, stamp, want...)) {
		t.Fatalf("wrong attach-only argv: %v", effects)
	}
}
