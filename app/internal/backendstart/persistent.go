// Package backendstart resolves the persistent backend shared by both frontends.
// It has no terminal dependencies and never starts or replaces an explicitly selected socket.
package backendstart

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

const (
	helloTimeout = 500 * time.Millisecond
	startTimeout = 15 * time.Second
	pollInterval = 40 * time.Millisecond
)

type Profile string

const (
	Hush Profile = "hush"
	Pi   Profile = "pi"
)

func SelectProfile(value string, needPi bool) (Profile, error) {
	if value == "" {
		if needPi {
			return Pi, nil
		}
		value = os.Getenv("HUSH_PROFILE")
	}
	switch Profile(value) {
	case "", Hush:
		if needPi {
			return "", errors.New("Pi modes and options require the Pi profile")
		}
		return Hush, nil
	case Pi:
		return Pi, nil
	default:
		return "", errors.New("profile must be hush or pi")
	}
}

type Options struct {
	Profile    Profile
	Socket     string // requested external Unix socket, if any
	Bundle     string // trusted local Node entry point to use only on a cold start
	AttachOnly bool   // resolve the path without sending hello or starting a process (raw relay)
	// RequireWorkTrust keeps a new work-capable client off a running older
	// managed Hush daemon. Status, stop, and explicit attach stay compatible.
	RequireWorkTrust bool
}

type target struct {
	socket  string
	agent   string // empty for an explicit socket or attach-only mode
	profile Profile
}

func resolveTarget(opts Options) (target, error) {
	socket := opts.Socket
	if socket == "" {
		socket = os.Getenv("HUSH_SOCKET")
	}
	if socket != "" {
		if opts.Bundle != "" {
			return target{}, errors.New("--backend selects a local bundle; it cannot be combined with --socket or HUSH_SOCKET")
		}
		return target{socket: socket}, nil
	}
	profile, err := SelectProfile(string(opts.Profile), false)
	if err != nil {
		return target{}, err
	}
	agent, subdir, err := profile.directory()
	if err != nil {
		return target{}, fmt.Errorf("resolve default backend (or use --socket): %w", err)
	}
	socket = filepath.Join(agent, subdir, "hush.sock")
	if opts.AttachOnly {
		return target{socket: socket, profile: profile}, nil
	}
	return target{socket: socket, agent: agent, profile: profile}, nil
}

func (t target) clientOptions(opts sdk.ClientOptions) sdk.ClientOptions {
	if t.profile == Hush {
		opts.RequiredCapabilities = maps.Clone(opts.RequiredCapabilities)
		if opts.RequiredCapabilities == nil {
			opts.RequiredCapabilities = make(map[string]uint64, 1)
		}
		opts.RequiredCapabilities["hushProfile"] = 1
	}
	return opts
}

// Resolve returns the socket path. With default local configuration it waits
// for a compatible hello before returning. An explicit socket (including
// HUSH_SOCKET) is attach-only; its caller performs the application's handshake.
func Resolve(ctx context.Context, opts Options) (string, error) {
	t, err := resolveTarget(opts)
	if err == nil && t.agent != "" {
		err = t.ensure(ctx, opts.Bundle)
	}
	if err != nil {
		return "", err
	}
	return t.socket, nil
}

func (t target) needsStart(ctx context.Context) (bool, error) {
	socket := t.socket
	if err := t.hello(ctx); err == nil {
		return false, nil // Attaching doesn't require a bundle or Node.
	} else if errors.Is(err, syscall.ENOTSOCK) {
		return false, fmt.Errorf("backend path is not a Unix socket: %s", socket)
	} else if !unavailable(err) {
		return false, fmt.Errorf("backend at %s failed hello; not replacing it: %w", socket, err)
	}
	// Never remove even a stale socket ourselves. The backend checks ownership
	// and staleness under the same lock that guards its SQLite writer.
	if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket == 0 {
		return false, fmt.Errorf("backend path is not a Unix socket: %s", socket)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect backend socket: %w", err)
	}
	return true, nil
}

func (t target) ensure(ctx context.Context, bundlePath string) error {
	if needed, err := t.needsStart(ctx); err != nil || !needed {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if t.profile == Hush {
		if installed, err := serviceInstalled(t.agent); err != nil {
			return err
		} else if installed {
			return t.waitSupervisor(startup)
		}
	}
	node, bundle, err := LocalRuntime(startup, bundlePath)
	if err != nil {
		return err
	}
	err = launch(startup, node, bundle, t)
	if errors.Is(err, errSupervised) { // An install raced the pre-launch probe.
		return t.waitSupervisor(startup)
	}
	if err != nil && !errors.Is(err, errContended) {
		return err
	}
	// A competing launcher can lose the data-directory lock long before the
	// winner starts listening. Wait for that winner, never respawn or kill it.
	if err := t.waitHello(startup); err != nil {
		return fmt.Errorf("wait for persistent backend at %s: %w", t.socket, err)
	}
	return nil
}

func (profile Profile) directory() (path, subdir string, err error) {
	if profile == Hush {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		if !filepath.IsAbs(home) {
			return "", "", errors.New("HOME must be an absolute path")
		}
		return filepath.Join(home, ".hush"), "runtime", nil
	}
	dir := os.Getenv("PI_CODING_AGENT_DIR")
	if dir == "" || dir == "~" || strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", err
		}
		if dir == "" {
			dir = filepath.Join(home, ".pi", "agent")
		} else {
			dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
	}
	path, err = filepath.Abs(dir)
	return path, "hush", err
}

func (t target) hello(ctx context.Context) error {
	probe, stop := context.WithTimeout(ctx, helloTimeout)
	defer stop()
	client := sdk.NewClient(t.socket, t.clientOptions(sdk.ClientOptions{}))
	defer client.Close()
	_, err := client.Hello(probe)
	return err
}

func unavailable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func (t target) waitHello(ctx context.Context) error {
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		err := t.hello(ctx)
		if err == nil || !unavailable(err) {
			return err
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
