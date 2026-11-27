package pane

import (
	"context"
	"encoding/json/v2"
	"os"
	"slices"
	"strconv"
	"strings"
)

type cmuxPlace struct {
	Window    string `json:"window_id"`
	Workspace string `json:"workspace_id"`
	Pane      string `json:"pane_id"`
	Surface   string `json:"surface_id"`
}

func (p *cmuxPlace) normalize() bool {
	for _, id := range []*string{&p.Window, &p.Workspace, &p.Pane, &p.Surface} {
		if !uuidText.MatchString(*id) {
			return false
		}
		*id = strings.ToLower(*id)
	}
	return true
}

type cmuxAdapter struct {
	cli    nativeCLI
	origin cmuxPlace
	caps   Capabilities
	scope  Scope
}

func bindCmux(ctx context.Context, path string, env []string, socket, workspace, surface string) (*cmuxAdapter, error) {
	pin, err := pinSocket(socket)
	if err != nil || !uuidText.MatchString(workspace) || !uuidText.MatchString(surface) {
		return nil, errOrigin
	}
	a := &cmuxAdapter{cli: nativeCLI{path: path, env: env, prefix: []string{"--socket", socket}, pin: pin}, scope: NewScope()}
	caller, err := a.identify(ctx)
	if err != nil || !strings.EqualFold(caller.Workspace, workspace) || !strings.EqualFold(caller.Surface, surface) {
		return nil, errOrigin
	}
	a.origin = caller
	if _, err := a.snapshot(ctx); err != nil {
		return nil, err
	}
	var caps struct {
		Methods []string `json:"methods"`
	}
	if _, err := a.rpc(ctx, &caps, "system.capabilities", struct{}{}); err != nil ||
		!slices.Contains(caps.Methods, "surface.split") || !slices.Contains(caps.Methods, "surface.focus") {
		return nil, errOrigin
	}
	a.caps = Capabilities{Left: true, Above: true, Tabs: slices.Contains(caps.Methods, "surface.create"), Read: slices.Contains(caps.Methods, "surface.read_text")}
	return a, nil
}

func (a *cmuxAdapter) identify(ctx context.Context) (cmuxPlace, error) {
	var info struct {
		Caller cmuxPlace `json:"caller"`
		Socket string    `json:"socket_path"`
	}
	if _, err := a.cli.json(ctx, &info, "--json", "--id-format", "uuids", "identify"); err != nil || !info.Caller.normalize() {
		return cmuxPlace{}, errOrigin
	}
	if stat, err := os.Stat(info.Socket); err != nil || !os.SameFile(stat, a.cli.pin.info) {
		return cmuxPlace{}, errOrigin
	}
	return info.Caller, nil
}

func (a *cmuxAdapter) rpc(ctx context.Context, result any, method string, payload any) (bool, error) {
	data, err := json.Marshal(payload)
	// Avoid a second command to recover an oversize creation request. cmux
	// builds with a small socket classification window need a compact frame.
	if err != nil || len(data) > 3000 {
		return false, errNative
	}
	return a.cli.json(ctx, result, "--json", "rpc", method, string(data))
}

func (a *cmuxAdapter) Capabilities() Capabilities { return a.caps }

func (a *cmuxAdapter) Verify(ctx context.Context) error {
	_, err := a.snapshot(ctx)
	return err
}

func (a *cmuxAdapter) workspaceArgs() map[string]any {
	return map[string]any{"window_id": a.origin.Window, "workspace_id": a.origin.Workspace}
}

func (a *cmuxAdapter) localWorkspace(ctx context.Context) bool {
	var list struct {
		Rows []struct {
			ID     string `json:"id"`
			Remote *struct {
				Enabled   *bool  `json:"enabled"`
				Connected bool   `json:"connected"`
				Transport string `json:"transport"`
				Backend   any    `json:"backend"`
			} `json:"remote"`
		} `json:"workspaces"`
	}
	if _, err := a.rpc(ctx, &list, "workspace.list", map[string]any{"window_id": a.origin.Window}); err != nil {
		return false
	}
	matches := 0
	for _, row := range list.Rows {
		if strings.EqualFold(row.ID, a.origin.Workspace) {
			r := row.Remote
			if r == nil || r.Enabled == nil || *r.Enabled || r.Connected || r.Backend != nil || r.Transport != "" && r.Transport != "local" {
				return false
			}
			matches++
		}
	}
	return matches == 1
}

type cmuxSurface struct {
	ID      string `json:"id"`
	Pane    string `json:"pane_id"`
	Type    string `json:"type"`
	Dock    string `json:"dock_scope"`
	Focused bool   `json:"focused"`
}

func (s cmuxSurface) terminal() bool {
	return s.Type == "terminal" && s.Dock == "" && uuidText.MatchString(s.Pane)
}

func (a *cmuxAdapter) snapshot(ctx context.Context) (map[string]cmuxSurface, error) {
	if caller, err := a.identify(ctx); err != nil || caller != a.origin || !a.localWorkspace(ctx) {
		return nil, errOrigin
	}
	var list struct {
		Window    string        `json:"window_id"`
		Workspace string        `json:"workspace_id"`
		Rows      []cmuxSurface `json:"surfaces"`
	}
	if _, err := a.rpc(ctx, &list, "surface.list", a.workspaceArgs()); err != nil {
		return nil, err
	}
	if !strings.EqualFold(list.Window, a.origin.Window) || !strings.EqualFold(list.Workspace, a.origin.Workspace) {
		return nil, errOrigin
	}
	seen := make(map[string]cmuxSurface, len(list.Rows))
	for _, row := range list.Rows {
		row.ID, row.Pane = strings.ToLower(row.ID), strings.ToLower(row.Pane)
		if _, repeated := seen[row.ID]; repeated || !uuidText.MatchString(row.ID) {
			return nil, errNative
		}
		seen[row.ID] = row
	}
	if origin := seen[a.origin.Surface]; !origin.terminal() || origin.Pane != a.origin.Pane {
		return nil, errOrigin
	}
	return seen, nil
}

func quotePOSIX(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// cmux accepts an initial command string, not argv. The only shell code in
// argv mode is a fixed directory guard; all caller values travel as positional
// parameters and exec expands each one exactly once. No interactive input.
func cmuxCommand(r OpenRequest) string {
	parts := []string{"/bin/sh", "-c", `cd -P "$1" || exit 125; shift; exec -- "$@"`, "hush-pane", r.Dir}
	parts = append(parts, launchArgv(r.Launch)...)
	for i := range parts {
		parts[i] = quotePOSIX(parts[i])
	}
	return strings.Join(parts, " ")
}

func (a *cmuxAdapter) Open(ctx context.Context, req OpenRequest) Result[Ref] {
	if err := req.Validate(a.caps); err != nil {
		return Reject[Ref](err)
	}
	before, err := a.snapshot(ctx)
	if err != nil {
		return Reject[Ref](err)
	}
	launch, err := guardedLaunch(req)
	if err != nil {
		return Reject[Ref](err)
	}
	req.Launch = launch
	params := a.workspaceArgs()
	params["type"], params["focus"], params["remote_context"] = "terminal", false, "local"
	params["surface_id"], params["initial_command"] = a.origin.Surface, cmuxCommand(req)
	method := "surface.split"
	if req.Placement == Tab {
		method, params["pane_id"] = "surface.create", a.origin.Pane
	} else {
		direction := string(req.Placement)
		switch req.Placement {
		case Above:
			direction = "up"
		case Below:
			direction = "down"
		}
		params["direction"] = direction
	}
	if err := verifyOpenDirectory(req); err != nil {
		return Reject[Ref](err)
	}
	var reply struct {
		cmuxPlace `json:",inline"`
		Type      string `json:"type"`
	}
	started, err := a.rpc(ctx, &reply, method, params)
	if err != nil {
		return nativeFailure[Ref](started, err)
	}
	if !reply.cmuxPlace.normalize() || reply.Workspace != a.origin.Workspace || reply.Window != a.origin.Window || reply.Type != "terminal" ||
		(req.Placement == Tab) != (reply.Pane == a.origin.Pane) {
		return Uncertain[Ref](errNative)
	}
	if _, exists := before[reply.Surface]; exists {
		return Uncertain[Ref](errNative)
	}
	if req.Placement != Tab {
		for _, row := range before {
			if row.Pane == reply.Pane {
				return Uncertain[Ref](errNative)
			}
		}
	}
	rows, err := a.snapshot(ctx)
	if row := rows[reply.Surface]; err != nil || !row.terminal() || row.Pane != reply.Pane {
		return Uncertain[Ref](errNative)
	}
	return Confirm(a.scope.Ref(reply.Surface))
}

func (a *cmuxAdapter) List(ctx context.Context, _ ListRequest) Result[[]Ref] {
	rows, err := a.snapshot(ctx)
	if err == errOrigin {
		return Reject[[]Ref](err)
	}
	if err != nil {
		return Uncertain[[]Ref](err)
	}
	ids := make([]string, 0, len(rows))
	for id, row := range rows {
		if row.terminal() {
			ids = append(ids, id)
		}
	}
	return Confirm(scopedRefs(a.scope, ids))
}

func (a *cmuxAdapter) target(ctx context.Context, ref Ref) (cmuxSurface, error) {
	id, ok := a.scope.Resolve(ref)
	if !ok {
		return cmuxSurface{}, errTarget
	}
	rows, err := a.snapshot(ctx)
	if err != nil {
		return cmuxSurface{}, err
	}
	if row := rows[id]; row.terminal() {
		return row, nil
	}
	return cmuxSurface{}, errTarget
}

func (a *cmuxAdapter) Focus(ctx context.Context, req FocusRequest) Result[Ref] {
	row, err := a.target(ctx, req.Pane)
	if err != nil {
		return Reject[Ref](err)
	}
	params := a.workspaceArgs()
	params["surface_id"] = row.ID
	var reply cmuxPlace
	started, err := a.rpc(ctx, &reply, "surface.focus", params)
	if err != nil {
		return nativeFailure[Ref](started, err)
	}
	if !strings.EqualFold(reply.Surface, row.ID) || !strings.EqualFold(reply.Workspace, a.origin.Workspace) ||
		!strings.EqualFold(reply.Window, a.origin.Window) {
		return Uncertain[Ref](errNative)
	}
	rows, err := a.snapshot(ctx)
	if target := rows[row.ID]; err != nil || !target.terminal() || !target.Focused {
		return Uncertain[Ref](errNative)
	}
	return Confirm(req.Pane)
}

func (a *cmuxAdapter) Read(ctx context.Context, req ReadRequest) Result[ReadSnapshot] {
	if err := req.Validate(a.caps); err != nil {
		return Reject[ReadSnapshot](err)
	}
	row, err := a.target(ctx, req.Pane)
	if err != nil {
		return Reject[ReadSnapshot](err)
	}
	return a.cli.capture(ctx, req.Lines, "read-screen", "--window", a.origin.Window, "--workspace", a.origin.Workspace,
		"--surface", row.ID, "--lines", strconv.Itoa(req.Lines+1))
}
