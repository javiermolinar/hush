package backendstart

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/javiermolinar/hush/app/internal/keyring"
	"syscall"
	"time"

	"github.com/javiermolinar/hush/sdk"
)

// Only explicit overrides can supersede the embedded release backend.
// The sentinel cannot collide with an override, which is made absolute.
const embeddedBundle = ":embedded:"

func findBundle(path string) (string, error) {
	if path == "" {
		path = os.Getenv("HUSH_BACKEND")
	}
	if path != "" {
		entry, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve backend bundle: %w", err)
		}
		info, err := os.Stat(entry)
		if err != nil {
			return "", fmt.Errorf("read backend bundle: %w", err)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("backend bundle is not a regular file: %s", entry)
		}
		return entry, nil
	}
	if len(releaseBackend) != 0 {
		return embeddedBundle, nil
	}
	if executable, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(executable); err == nil {
			bin := filepath.Dir(real)
			paths := []string{filepath.Join(bin, "hush-backend.mjs")}
			if filepath.Base(bin) == "bin" {
				paths = append(paths, filepath.Join(filepath.Dir(bin), "dist", "hush-backend.mjs"))
			}
			for _, entry := range paths {
				if info, err := os.Stat(entry); err == nil && info.Mode().IsRegular() {
					return entry, nil
				}
			}
		}
	}
	// The working directory is a project to work on, not an installation to trust.
	return "", errors.New("no local backend bundle found; build it with npm run backend:bundle and pass --backend PATH or set HUSH_BACKEND")
}

var nodeVersion = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.[0-9]+$`)

func findNode(ctx context.Context) (string, error) {
	path, err := exec.LookPath("node")
	if err != nil {
		return "", errors.New("starting a local backend requires Node.js 22.19.0 or newer on PATH")
	}
	check, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(check, path, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("check Node.js version: %w", err)
	}
	version := strings.TrimSpace(string(out))
	match := nodeVersion.FindStringSubmatch(version)
	if match == nil {
		return "", errors.New("could not read the Node.js version (requires 22.19.0 or newer)")
	}
	major, majorErr := strconv.Atoi(match[1])
	minor, minorErr := strconv.Atoi(match[2])
	if majorErr != nil || minorErr != nil {
		return "", errors.New("could not read the Node.js version (requires 22.19.0 or newer)")
	}
	if major < 22 || major == 22 && minor < 19 {
		return "", fmt.Errorf("starting a local backend requires Node.js 22.19.0 or newer; found %s", version)
	}
	return path, nil
}

var errContended = errors.New("another backend holds the data-directory lock")

type startupRecord struct {
	Ready  bool   `json:"ready"`
	Socket string `json:"socket"`
	Error  string `json:"error"`
}

// LocalRuntime selects the installed backend and checks the minimum Node version.
func LocalRuntime(ctx context.Context, bundle string) (node, entry string, err error) {
	entry, err = findBundle(bundle)
	if err == nil {
		node, err = findNode(ctx)
	}
	return
}

// LocalInvocation resolves an entry for a foreground child. The caller must
// clean up after the child exits.
func LocalInvocation(ctx context.Context, bundle string) (node, entry string, cleanup func() error, err error) {
	node, entry, err = LocalRuntime(ctx, bundle)
	if err == nil {
		entry, cleanup, err = materialize(entry)
	}
	return
}

// Each launch gets a fresh private directory. No existing executable content
// is reused and the entry isn't passed to Node until the write has closed.
func materialize(entry string) (string, func() error, error) {
	if entry != embeddedBundle {
		return entry, func() error { return nil }, nil
	}
	// Use the system sticky directory, not a project-controlled TMPDIR.
	// On macOS /tmp is an alias for /private/tmp; use the real location.
	base := "/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	st, err := os.Lstat(base)
	if err != nil {
		return "", nil, fmt.Errorf("inspect backend temporary directory: %w", err)
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode()&os.ModeSticky == 0 || !ok || owner.Uid != 0 {
		return "", nil, errors.New("backend temporary base is not a root-owned sticky directory")
	}
	dir, err := os.MkdirTemp(base, "hush-bundle-")
	if err != nil {
		return "", nil, fmt.Errorf("create backend bundle directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(dir) }
	entry = filepath.Join(dir, "hush-backend.mjs")
	// O_EXCL with O_CREATE refuses even a dangling symlink.
	file, err := os.OpenFile(entry, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err == nil {
		_, err = file.Write(releaseBackend)
		err = errors.Join(err, file.Close())
	}
	if err != nil {
		return "", nil, fmt.Errorf("extract embedded backend: %w", errors.Join(err, cleanup()))
	}
	return entry, cleanup, nil
}

// RuntimeEnv removes inherited Pi session/settings selectors for the standalone
// child. Provider-specific environment credentials remain an explicit alternative.
func RuntimeEnv(profile Profile) []string {
	env := os.Environ()
	if profile == Hush {
		out := env[:0]
		for _, item := range env {
			if !strings.HasPrefix(item, "PI_") && !strings.HasPrefix(item, keyring.HelperEnv+"=") {
				out = append(out, item)
			}
		}
		// The trusted backend can call the installed binary without PATH
		// lookup. No provider credential is passed in argv or the environment.
		if exe, err := os.Executable(); err == nil {
			out = append(out, keyring.HelperEnv+"="+exe)
		}
		env = out
	}
	return env
}

func launch(ctx context.Context, node, bundle string, t target) (resultErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry, cleanup, err := materialize(bundle)
	if err != nil {
		return err
	}
	// The startup record (or EOF) follows evaluation of the single-file
	// module. There are no later loads of this entry or adjacent assets.
	defer func() {
		if err := cleanup(); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create backend startup pipe: %w", err)
	}
	defer reader.Close()
	defer writer.Close()
	args := []string{entry, "--data-dir", filepath.Dir(t.socket), "--startup-fd", "3"}
	if t.profile == Pi {
		args = append(args, "--agent-dir", t.agent, "--pi-events")
	} else {
		args = append(args, "--profile", string(Hush))
	}
	cmd := exec.Command(node, args...)
	cmd.Env = RuntimeEnv(t.profile)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.ExtraFiles = []*os.File{writer}
	// Deliberately leave all three standard streams at /dev/null. The backend
	// reports its startup on fd 3 and logs to its private data directory.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch backend: %w", err)
	}
	_ = writer.Close()
	go func() { _ = cmd.Wait() }()
	result := make(chan error, 1)
	go func() { result <- readStartup(reader, t.socket) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		// The invoking CLI doesn't own the durable daemon's lifetime. If
		// Node has already loaded the entry it can finish booting; otherwise
		// removing a transient entry can cancel this launch before it boots.
		return fmt.Errorf("wait for backend startup: %w", ctx.Err())
	}
}

func readStartup(r io.Reader, socket string) error {
	line, err := bufio.NewReaderSize(r, 4096).ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("backend did not report startup (rebuild the bundle or check its logs): %w", err)
	}
	var status startupRecord
	if json.Unmarshal(line, &status) != nil {
		return errors.New("invalid backend startup record")
	}
	if status.Ready && status.Socket == socket && status.Error == "" {
		return nil
	}
	if !status.Ready && status.Socket == "" {
		switch status.Error {
		case "data_locked":
			return errContended
		case "store_profile", "store_cutover":
			return fmt.Errorf("could not start backend (%s); run 'hush store inspect' on the backend host: %w", status.Error, sdk.ErrStoreIncompatible)
		case "credential_configuration":
			return errors.New("set ~/.hush/credential-store to file or keyring (private, mode 600), then restart the Hush backend")
		case "configuration", "socket_directory", "socket_unavailable", "startup_failed", "shutdown_failed":
			return fmt.Errorf("could not start backend (%s)", status.Error)
		}
	}
	return errors.New("invalid backend startup record")
}
