package headless

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/javiermolinar/hush/app/internal/backendstart"
)

type privateBackend struct {
	socket string
	dir    string
	life   *os.File
	cmd    *exec.Cmd
	done   chan struct{}
}

const (
	privateReadyTimeout = 15 * time.Second
	privateCloseGrace   = 2 * time.Second
)

func startPrivate(ctx context.Context, node, entry string, profile backendstart.Profile, piEvents bool) (*privateBackend, error) {
	base, err := filepath.Abs(os.TempDir())
	if err != nil {
		return nil, fmt.Errorf("resolve temporary directory: %w", err)
	}
	// Leave enough space for MkdirTemp's random suffix and the socket
	// terminator. macOS sun_path is shorter than Linux's.
	if len(filepath.Join(base, "hush-0123456789", "hush.sock")) >= 104 {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "hush-")
	if err != nil {
		return nil, fmt.Errorf("create private backend directory: %w", err)
	}
	b := &privateBackend{dir: dir, socket: filepath.Join(dir, "hush.sock"), done: make(chan struct{})}
	fail := func(err error) (*privateBackend, error) {
		return nil, errors.Join(err, b.Close())
	}
	parentRead, parentWrite, err := os.Pipe()
	if err != nil {
		return fail(fmt.Errorf("create private backend lifetime pipe: %w", err))
	}
	defer parentRead.Close()
	b.life = parentWrite // Only this invocation holds the write end.
	ready := readyLine{socket: b.socket, result: make(chan error, 1)}
	args := []string{entry, "--no-session", "--socket", b.socket, "--parent-stdin"}
	if piEvents {
		args = append(args, "--pi-events")
	} else {
		args = append(args, "--profile", string(profile))
	}
	cmd := exec.Command(node, args...)
	cmd.Env = backendstart.RuntimeEnv(profile)
	cmd.Stdin, cmd.Stdout = parentRead, &ready
	// Child diagnostics aren't part of native CLI stdout/stderr. In particular,
	// do not surface a provider's output from a private invocation.
	cmd.WaitDelay = time.Second
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("start private backend: %w", err))
	}
	b.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(b.done)
	}()
	timer := time.NewTimer(privateReadyTimeout)
	defer timer.Stop()
	select {
	case err := <-ready.result:
		if err != nil {
			return fail(err)
		}
		select {
		case <-b.done:
			return fail(errors.New("private backend exited during startup"))
		default:
			return b, nil
		}
	case <-b.done:
		return fail(errors.New("private backend exited before readiness"))
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-timer.C:
		return fail(errors.New("private backend did not become ready"))
	}
}

// Close ends the lifetime pipe even if no SDK connection was ever opened,
// and reaps the child before removing its directory.
func (b *privateBackend) Close() error {
	var err error
	if b.life != nil {
		err = b.life.Close()
	}
	if b.cmd != nil {
		t := time.NewTimer(privateCloseGrace)
		defer t.Stop()
		select {
		case <-b.done:
		case <-t.C:
			if killErr := b.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				return fmt.Errorf("stop private backend: %w", killErr)
			}
			<-b.done
		}
	}
	if cleanErr := errors.Join(err, os.RemoveAll(b.dir)); cleanErr != nil {
		return fmt.Errorf("clean up private backend: %w", cleanErr)
	}
	return nil
}

// exec drains the child's stdout throughout its lifetime. Buffer only the
// first, bounded readiness record and discard all subsequent bytes.
type readyLine struct {
	socket   string
	result   chan error
	line     []byte
	finished bool
}

func (r *readyLine) Write(p []byte) (int, error) {
	if r.finished {
		return len(p), nil
	}
	part, _, found := bytes.Cut(p, []byte{'\n'})
	if len(r.line)+len(part) > 4096 {
		r.finished = true
		r.result <- errors.New("oversized private backend readiness record")
		return len(p), nil
	}
	r.line = append(r.line, part...)
	if found {
		r.finished = true
		var record struct {
			Ready  bool   `json:"ready"`
			Socket string `json:"socket"`
		}
		if err := json.Unmarshal(r.line, &record); err != nil || !record.Ready || record.Socket != r.socket {
			r.result <- errors.New("invalid private backend readiness record")
		} else {
			r.result <- nil
		}
	}
	return len(p), nil
}
