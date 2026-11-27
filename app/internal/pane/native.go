package pane

import (
	"context"
	"encoding/json/v2"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

var (
	errOrigin = errors.New("unverified pane origin")
	errTarget = errors.New("unverified pane target")
	errNative = errors.New("could not verify multiplexer response")
	uuidText  = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
)

// Detect only inspects the advertised local origin. A CLI on PATH or a
// focused terminal is not evidence. The caller must be an interactive client.
func Detect(ctx context.Context) Adapter {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	get := os.Getenv
	rex, cmux := get("REX_BLOCK"), get("CMUX_SURFACE_ID")
	term := strings.ToLower(get("TERM_PROGRAM"))
	if get("TMUX") != "" || get("ZELLIJ") != "" || rex != "" && cmux != "" {
		return nil
	}
	if rex != "" && (term == "" || term == "rex") {
		path, err := exec.LookPath("rex")
		if err != nil && filepath.IsAbs(get("REX_BIN_DIR")) {
			path, err = exec.LookPath(filepath.Join(get("REX_BIN_DIR"), "rex"))
		}
		if err == nil {
			adapter, err := bindRex(ctx, path, os.Environ(), rex)
			if err == nil {
				return adapter
			}
		}
	}
	if cmux != "" {
		if path, err := exec.LookPath("cmux"); err == nil {
			adapter, err := bindCmux(ctx, path, os.Environ(), get("CMUX_SOCKET_PATH"), get("CMUX_WORKSPACE_ID"), cmux)
			if err == nil {
				return adapter
			}
		}
	}
	return nil
}

type socketPin struct {
	path string
	info os.FileInfo
}

func pinSocket(path string) (socketPin, error) {
	if !filepath.IsAbs(path) {
		return socketPin{}, errOrigin
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return socketPin{}, errOrigin
	}
	return socketPin{path, info}, nil
}

func (p socketPin) valid() bool {
	if p.info == nil {
		return false
	}
	info, err := os.Stat(p.path)
	return err == nil && info.Mode()&os.ModeSocket != 0 && os.SameFile(p.info, info)
}

type nativeCLI struct {
	path   string
	env    []string
	prefix []string
	pin    socketPin
}

// started is an uncertainty boundary, not proof that a write reached the mux.
func (c nativeCLI) run(ctx context.Context, out *nativeOutput, args ...string) (started bool, err error) {
	if c.pin.info != nil && !c.pin.valid() {
		return false, errOrigin
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	all := make([]string, 0, len(c.prefix)+len(args))
	all = append(append(all, c.prefix...), args...)
	cmd := exec.CommandContext(ctx, c.path, all...)
	cmd.Env, cmd.Stdout, cmd.WaitDelay = c.env, out, 100*time.Millisecond
	if err = cmd.Start(); err != nil {
		return false, errNative
	}
	if cmd.Wait() != nil || ctx.Err() != nil || c.pin.info != nil && !c.pin.valid() {
		return true, errNative
	}
	return true, nil
}

type nativeOutput struct {
	buf       []byte
	limit     int
	tail      bool
	truncated bool
}

func (w *nativeOutput) Write(p []byte) (int, error) {
	n := len(p)
	if excess := len(w.buf) + n - w.limit; excess > 0 {
		w.truncated = true
		switch {
		case !w.tail:
			p = p[:n-excess]
		case excess < len(w.buf):
			w.buf = w.buf[:copy(w.buf, w.buf[excess:])]
		default:
			w.buf = w.buf[:0]
			p = p[n-w.limit:]
		}
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

func (c nativeCLI) json(ctx context.Context, result any, args ...string) (bool, error) {
	out := nativeOutput{limit: 1 << 20}
	started, err := c.run(ctx, &out, args...)
	if err != nil {
		return started, err
	}
	if out.truncated || json.Unmarshal(out.buf, result) != nil {
		return started, errNative
	}
	return started, nil
}

func (c nativeCLI) capture(ctx context.Context, lines int, args ...string) Result[ReadSnapshot] {
	// Keep one extra byte to detect a byte-limited tail; Rex can return all
	// scrollback. Native plain-text mode doesn't need an ANSI renderer.
	out := nativeOutput{limit: MaxReadBytes + 1, tail: true}
	started, err := c.run(ctx, &out, args...)
	if err != nil {
		return nativeFailure[ReadSnapshot](started, err)
	}
	text := strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, string(out.buf))
	// The trailing newline terminates a line; it doesn't count as an extra one.
	left := lines
	for i := len(text) - 2; i >= 0; i-- {
		if text[i] == '\n' {
			left--
			if left == 0 {
				text, out.truncated = text[i+1:], true
				break
			}
		}
	}
	if len(text) > MaxReadBytes {
		i := len(text) - MaxReadBytes
		for text[i]&0xc0 == 0x80 {
			i++
		}
		text, out.truncated = text[i:], true
	}
	return Confirm(ReadSnapshot{Text: text, Truncated: out.truncated})
}

func nativeFailure[T any](started bool, err error) Result[T] {
	if started {
		return Uncertain[T](err)
	}
	return Reject[T](err)
}

func verifyOpenDirectory(req OpenRequest) error {
	if req.Directory != nil {
		return checkDirectory(req.Dir, *req.Directory)
	}
	if info, err := os.Stat(req.Dir); err != nil || !info.IsDir() {
		return errors.New("client working directory is unavailable")
	}
	return nil
}

func scopedRefs(scope Scope, ids []string) []Ref {
	slices.Sort(ids)
	refs := make([]Ref, len(ids))
	for i, id := range ids {
		refs[i] = scope.Ref(id)
	}
	return refs
}
