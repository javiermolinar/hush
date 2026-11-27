package pane

import (
	"context"
	"net/url"
	"strings"
)

type rexIdentity struct {
	Principal struct {
		Transport string `json:"transport"`
	} `json:"principal"`
	Endpoint string `json:"endpoint"`
	Instance string `json:"instance_id"`
	Host     string `json:"host_id"`
	Session  string `json:"session_id"`
	Window   string `json:"window_id"`
	Block    string `json:"block_id"`
}

// json/v2 matches native spelling exactly.
func (i rexIdentity) validCaller(block string) bool {
	return i.Principal.Transport == "unix" && i.Instance != "" && i.Host != "" && i.Block == block &&
		rexID(i.Session, "session") && rexID(i.Window, "window")
}

func rexID(id, kind string) bool {
	value, ok := strings.CutPrefix(id, kind+":")
	return ok && uuidText.MatchString(value)
}

type rexAdapter struct {
	cli    nativeCLI
	origin rexIdentity
	scope  Scope
}

func bindRex(ctx context.Context, path string, env []string, block string) (*rexAdapter, error) {
	if !rexID(block, "block") {
		return nil, errOrigin
	}
	cli := nativeCLI{path: path, env: env, prefix: []string{"--autostart=false", "--timeout=2s"}}
	var origin rexIdentity
	if _, err := cli.json(ctx, &origin, "whoami", "--json"); err != nil || !origin.validCaller(block) {
		return nil, errOrigin
	}
	u, err := url.Parse(origin.Endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errOrigin
	}
	cli.pin, err = pinSocket(u.Path)
	if err != nil {
		return nil, err
	}
	cli.prefix = append(cli.prefix, "-S", origin.Endpoint)
	a := &rexAdapter{cli: cli, origin: origin, scope: NewScope()}
	if _, err := a.snapshot(ctx); err != nil {
		return nil, err
	}
	return a, nil
}

func (*rexAdapter) Capabilities() Capabilities {
	return Capabilities{Left: true, Above: true, Tabs: true, Read: true}
}

func (a *rexAdapter) Verify(ctx context.Context) error {
	_, err := a.snapshot(ctx)
	return err
}

type rexBlock struct {
	ID        string `json:"block_id"`
	Window    string `json:"window_id"`
	Host      string `json:"host_id"`
	Creator   string `json:"creator_name"`
	Placement string `json:"placement"`
}

func (a *rexAdapter) terminal(b rexBlock) bool {
	return b.Host == a.origin.Host && b.Creator == "com.superlogical.terminal" && b.Placement == "placed"
}

func (a *rexAdapter) snapshot(ctx context.Context) (map[string]rexBlock, error) {
	var current rexIdentity
	// With an explicit -S Rex omits the inherited calling location. Verify
	// the instance here, then check the recorded location in the scoped list.
	if _, err := a.cli.json(ctx, &current, "whoami", "--json"); err != nil || current.Principal.Transport != "unix" ||
		current.Endpoint != a.origin.Endpoint || current.Instance != a.origin.Instance || current.Host != a.origin.Host {
		return nil, errOrigin
	}
	var list struct {
		Blocks []rexBlock `json:"blocks"`
	}
	if _, err := a.cli.json(ctx, &list, "block", "ls", "-s", a.origin.Session, "--short=false", "--json"); err != nil {
		return nil, err
	}
	seen := make(map[string]rexBlock, len(list.Blocks))
	for _, b := range list.Blocks {
		if _, repeated := seen[b.ID]; repeated || !rexID(b.ID, "block") || a.terminal(b) && !rexID(b.Window, "window") {
			return nil, errNative
		}
		seen[b.ID] = b
	}
	if b := seen[a.origin.Block]; b.Window != a.origin.Window || !a.terminal(b) {
		return nil, errOrigin
	}
	return seen, nil
}

func (a *rexAdapter) Open(ctx context.Context, req OpenRequest) Result[Ref] {
	if err := req.Validate(a.Capabilities()); err != nil {
		return Reject[Ref](err)
	}
	before, err := a.snapshot(ctx)
	if err != nil {
		return Reject[Ref](err)
	}
	args := []string{"run", "-s", a.origin.Session}
	if req.Placement != Tab {
		args[0] = "split"
		args = append(args, "-w", a.origin.Window, "-b", a.origin.Block, "--split="+string(req.Placement))
	}
	args = append(args, "--json", "--focus=false", "--keep-open", "--shell=none", "-c", req.Dir, "--")
	if err := verifyOpenDirectory(req); err != nil {
		return Reject[Ref](err)
	}
	launch, err := guardedLaunch(req)
	if err != nil {
		return Reject[Ref](err)
	}
	args = append(args, launchArgv(launch)...)
	var created struct {
		Blocks  []string `json:"block_ids"`
		Anchor  string   `json:"anchor_id"`
		Session string   `json:"session_id"`
		Window  string   `json:"window_id"`
	}
	started, err := a.cli.json(ctx, &created, args...)
	if err != nil {
		return nativeFailure[Ref](started, err)
	}
	if len(created.Blocks) != 1 || !rexID(created.Blocks[0], "block") ||
		created.Session != "" && created.Session != a.origin.Session {
		return Uncertain[Ref](errNative)
	}
	id, window := created.Blocks[0], a.origin.Window
	if _, exists := before[id]; exists {
		return Uncertain[Ref](errNative)
	}
	if req.Placement == Tab {
		window = created.Window
		if !rexID(window, "window") || window == a.origin.Window {
			return Uncertain[Ref](errNative)
		}
	} else if created.Anchor != a.origin.Block || created.Window != "" && created.Window != window {
		return Uncertain[Ref](errNative)
	}
	rows, err := a.snapshot(ctx)
	if err != nil || rows[id].Window != window || !a.terminal(rows[id]) {
		return Uncertain[Ref](errNative)
	}
	return Confirm(a.scope.Ref(id))
}

func (a *rexAdapter) List(ctx context.Context, _ ListRequest) Result[[]Ref] {
	rows, err := a.snapshot(ctx)
	if err == errOrigin {
		return Reject[[]Ref](err)
	}
	if err != nil {
		return Uncertain[[]Ref](err)
	}
	ids := make([]string, 0, len(rows))
	for id, row := range rows {
		if a.terminal(row) {
			ids = append(ids, id)
		}
	}
	return Confirm(scopedRefs(a.scope, ids))
}

func (a *rexAdapter) target(ctx context.Context, ref Ref) (rexBlock, error) {
	id, ok := a.scope.Resolve(ref)
	if !ok {
		return rexBlock{}, errTarget
	}
	rows, err := a.snapshot(ctx)
	if err != nil {
		return rexBlock{}, err
	}
	if b := rows[id]; a.terminal(b) && rexID(b.Window, "window") {
		return b, nil
	}
	return rexBlock{}, errTarget
}

func (a *rexAdapter) Focus(ctx context.Context, req FocusRequest) Result[Ref] {
	b, err := a.target(ctx, req.Pane)
	if err != nil {
		return Reject[Ref](err)
	}
	out := nativeOutput{limit: 1024}
	started, err := a.cli.run(ctx, &out, "focus", "-s", a.origin.Session, "-w", b.Window, b.ID)
	if err != nil {
		return nativeFailure[Ref](started, err)
	}
	var info struct {
		Session string `json:"session_id"`
		Active  string `json:"active_window_id"`
		Windows []struct {
			ID      string `json:"window_id"`
			Focused string `json:"focused_block_id"`
		} `json:"windows"`
	}
	if _, err = a.cli.json(ctx, &info, "session", "inspect", a.origin.Session, "--json"); err != nil ||
		info.Session != a.origin.Session || info.Active != b.Window {
		return Uncertain[Ref](errNative)
	}
	matches := 0
	for _, w := range info.Windows {
		if w.ID == b.Window {
			if w.Focused != b.ID {
				return Uncertain[Ref](errNative)
			}
			matches++
		}
	}
	if matches != 1 {
		return Uncertain[Ref](errNative)
	}
	return Confirm(req.Pane)
}

func (a *rexAdapter) Read(ctx context.Context, req ReadRequest) Result[ReadSnapshot] {
	if err := req.Validate(a.Capabilities()); err != nil {
		return Reject[ReadSnapshot](err)
	}
	b, err := a.target(ctx, req.Pane)
	if err != nil {
		return Reject[ReadSnapshot](err)
	}
	return a.cli.capture(ctx, req.Lines, "capture", "-s", a.origin.Session, "-w", b.Window, "-b", b.ID, "--format=text", "--trim")
}
