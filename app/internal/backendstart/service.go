package backendstart

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/javiermolinar/hush/app/internal/filelist"
	"github.com/javiermolinar/hush/app/internal/keyring"
	"github.com/javiermolinar/hush/sdk"
	"golang.org/x/sys/unix"
)

const (
	ServiceRunnerArg = "--hush-internal-service"
	serviceClaim     = "supervisor-v1.json"
	serviceHold      = "supervisor-hold-v1"
)

func privateServiceFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ok || int(owner.Uid) != os.Getuid() || owner.Nlink != 1 {
		return false, fmt.Errorf("unsafe Hush service file: %s", path)
	}
	return true, nil
}

func serviceInstalled(agent string) (bool, error) {
	return privateServiceFile(filepath.Join(agent, serviceClaim))
}

func (t target) waitSupervisor(ctx context.Context) error {
	if held, err := privateServiceFile(filepath.Join(t.agent, serviceHold)); err != nil {
		return err
	} else if held {
		return errors.New("the Hush service is paused; run 'hush service status' on the backend host")
	}
	if err := t.waitHello(ctx); err != nil {
		return fmt.Errorf("wait for the Hush service (run 'hush service status' on the backend host): %w", err)
	}
	return nil
}

type loginService struct {
	home, agent, name, definition string
}

func servicePaths() (loginService, error) {
	agent, err := hushDirectory()
	if err != nil {
		return loginService{}, err
	}
	home, err := filepath.EvalSymlinks(filepath.Dir(agent))
	if err != nil || !servicePath(home) {
		return loginService{}, errors.New("the Hush service requires an existing absolute HOME directory")
	}
	agent = filepath.Join(home, ".hush")
	hash := sha256.Sum256([]byte(agent))
	name := fmt.Sprintf("io.github.javiermolinar.hush.%x", hash[:8])
	var definition string
	switch runtime.GOOS {
	case "darwin":
		definition = filepath.Join(home, "Library", "LaunchAgents", name+".plist")
	case "linux":
		name += ".service"
		definition = filepath.Join(agent, name)
	default:
		return loginService{}, errors.New("Hush user services require launchd or systemd")
	}
	return loginService{home, agent, name, definition}, nil
}

func servicePath(s string) bool {
	return filepath.IsAbs(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

func servicePin(path string) (string, error) {
	if !servicePath(path) {
		return "", errors.New("service executable and bundle paths must be absolute")
	}
	pin, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(pin)
	if err != nil {
		return "", err
	}
	if !servicePath(pin) || !info.Mode().IsRegular() {
		return "", errors.New("invalid service executable or bundle path")
	}
	return pin, nil
}

func (s loginService) target() string { return "gui/" + fmt.Sprint(os.Getuid()) }

func (s loginService) control(ctx context.Context, manager string, args ...string) error {
	var cmd *exec.Cmd
	if manager == "launchd" {
		cmd = exec.CommandContext(ctx, "/bin/launchctl", args...)
		cmd.Env = []string{"HOME=" + s.home, "LC_ALL=C"}
	} else {
		cmd = exec.CommandContext(ctx, "/usr/bin/systemctl", append([]string{"--user"}, args...)...)
		dir := fmt.Sprintf("/run/user/%d", os.Getuid())
		cmd.Env = []string{"HOME=" + s.home, "LC_ALL=C", "XDG_RUNTIME_DIR=" + dir, "DBUS_SESSION_BUS_ADDRESS=unix:path=" + dir + "/bus"}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", manager, args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (s loginService) load(ctx context.Context, install bool) error {
	if runtime.GOOS == "darwin" {
		job := s.target() + "/" + s.name
		if !install && s.control(ctx, "launchd", "print", job) == nil {
			return s.control(ctx, "launchd", "kickstart", job)
		}
		return s.control(ctx, "launchd", "bootstrap", s.target(), s.definition)
	}
	if install {
		return s.control(ctx, "systemd", "enable", "--now", s.definition)
	}
	return s.control(ctx, "systemd", "start", s.name)
}

func (s loginService) unload(ctx context.Context, remove bool) error {
	if runtime.GOOS == "darwin" {
		job := s.target() + "/" + s.name
		err := s.control(ctx, "launchd", "bootout", "--wait", job)
		if err != nil {
			// A failed bootout is not proof of absence. Check this exact
			// GUI domain and job, never fall back to a system domain.
			if s.control(ctx, "launchd", "print", s.target()) == nil {
				if absent := s.control(ctx, "launchd", "print", job); absent != nil && strings.Contains(absent.Error(), "Could not find service") {
					return nil
				}
			}
		}
		return err
	}
	if remove {
		return s.control(ctx, "systemd", "disable", "--now", s.name)
	}
	return s.control(ctx, "systemd", "stop", s.name)
}

func serviceDefinition(goos string, s loginService, args []string) string {
	quoteXML := func(value string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(value))
		return "<string>" + b.String() + "</string>"
	}
	if goos == "darwin" {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key>` + quoteXML(s.name) + `<key>ProgramArguments</key><array>`)
		for _, arg := range args {
			b.WriteString(quoteXML(arg))
		}
		return b.String() + `</array><key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>` +
			`<key>ThrottleInterval</key><integer>30</integer><key>ExitTimeOut</key><integer>20</integer>` +
			`<key>WorkingDirectory</key>` + quoteXML(s.home) + `<key>EnvironmentVariables</key><dict><key>HOME</key>` +
			quoteXML(s.home) + `</dict></dict></plist>` + "\n"
	}
	q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
	for i, value := range args {
		args[i] = `"` + q.Replace(value) + `"`
	}
	return "[Unit]\nDescription=Hush user backend\nStartLimitIntervalSec=120\nStartLimitBurst=4\n" +
		"[Service]\nType=simple\nExecStart=:" + strings.Join(args, " ") + "\n" +
		`Environment="HOME=` + q.Replace(s.home) + "\"\n" + `Environment="DBUS_SESSION_BUS_ADDRESS=unix:path=%t/bus"` + "\n" +
		"Restart=on-failure\nRestartSec=30\nKillMode=mixed\nTimeoutStopSec=20\n" +
		"[Install]\nWantedBy=default.target\n"
}

func writeServiceFile(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	return errors.Join(err, f.Sync(), f.Close(), syncServiceDirectory(filepath.Dir(path)))
}

func syncServiceDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func removeServiceFile(path string) error {
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return syncServiceDirectory(filepath.Dir(path))
}

func (s loginService) lock() (*os.File, error) {
	if err := os.Mkdir(s.agent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	st, err := os.Lstat(s.agent)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 || int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return nil, errors.New("Hush service requires a private, user-owned ~/.hush directory")
	}
	name := filepath.Join(s.agent, "supervisor-v1.lock")
	fd, err := unix.Open(name, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if _, err := privateServiceFile(name); err != nil {
		f.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another Hush service command is changing this profile")
	}
	return f, nil
}

var serviceToken = regexp.MustCompile(`^[a-f0-9-]{36}$`)

func (s loginService) install(ctx context.Context, bundle string, stderr io.Writer) error {
	if _, err := os.Lstat(s.definition); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("service definition already exists or is inaccessible: %s", s.definition)
	}
	node, entry, err := LocalRuntime(ctx, bundle)
	if err != nil {
		return err
	}
	node, err = servicePin(node)
	if err != nil {
		return err
	}
	if entry != embeddedBundle {
		entry, err = servicePin(entry)
		if err != nil {
			return err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = servicePin(exe)
	if err != nil {
		return err
	}
	helper := exe
	if configured := os.Getenv(keyring.HelperEnv); configured != "" {
		helper, err = servicePin(configured)
		if err != nil {
			return fmt.Errorf("pin keyring helper: %w", err)
		}
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	nonce[6], nonce[8] = nonce[6]&0x0f|0x40, nonce[8]&0x3f|0x80
	token := fmt.Sprintf("%x-%x-%x-%x-%x", nonce[:4], nonce[4:6], nonce[6:8], nonce[8:10], nonce[10:])
	materialized, cleanup, err := materialize(entry)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, node, materialized, "--hush-internal-service-claim", token)
	cmd.Dir = s.home
	cmd.Env = []string{"HOME=" + s.home}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claim Hush service ownership (stop any managed backend first): %w", err)
	}
	// From this point, an interrupted install leaves a reversible ownership
	// claim. Never remove it on an ambiguous supervisor result.
	if err := os.MkdirAll(filepath.Dir(s.definition), 0o700); err != nil {
		return err
	}
	args := []string{exe, ServiceRunnerArg, token, node, entry, helper}
	if err := writeServiceFile(s.definition, serviceDefinition(runtime.GOOS, s, args)); err != nil {
		return err
	}
	if err := s.load(ctx, true); err != nil {
		return fmt.Errorf("service was claimed but startup was not confirmed; inspect with 'hush service status' or uninstall: %w", err)
	}
	return nil
}

// Service changes only the default Hush profile on the backend host.
func Service(ctx context.Context, action, bundle string, out, stderr io.Writer) error {
	s, err := servicePaths()
	if err != nil {
		return err
	}
	if action == "status" {
		installed, err := serviceInstalled(s.agent)
		if err != nil {
			return err
		}
		if !installed {
			_, err = fmt.Fprintln(out, "Hush user service is not installed")
			return err
		}
		if held, err := privateServiceFile(filepath.Join(s.agent, serviceHold)); err != nil {
			return err
		} else if held {
			_, err = fmt.Fprintln(out, "Hush user service is paused. When the store is ready, run 'hush service resume'.")
			return err
		}
		if !s.report(ctx, out) {
			return fmt.Errorf("user service is installed but not ready; see %s, check 'hush store inspect', then try 'hush service resume'", filepath.Join(s.agent, "supervisor.log"))
		}
		fmt.Fprintln(out, "Service diagnostics:", filepath.Join(s.agent, "supervisor.log"))
		return nil
	}
	if os.Getuid() == 0 || os.Getuid() != os.Geteuid() {
		return errors.New("manage the Hush service as the login user, not with sudo")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	lock, err := s.lock()
	if err != nil {
		return err
	}
	defer lock.Close()
	installed, err := serviceInstalled(s.agent)
	if err != nil {
		return err
	}
	hold := filepath.Join(s.agent, serviceHold)
	if action == "install" {
		if installed {
			return errors.New("a Hush service already owns this profile; run 'hush service status'")
		}
		err = s.install(ctx, bundle, stderr)
	} else if !installed {
		return errors.New("no Hush user service is installed")
	} else {
		switch action {
		case "pause", "uninstall":
			if err := writeServiceFile(hold, "paused\n"); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if exists, err := privateServiceFile(s.definition); err != nil {
				return err
			} else if exists {
				if err := s.unload(ctx, action == "uninstall"); err != nil {
					return err
				}
			}
			if action == "uninstall" {
				if err := removeServiceFile(s.definition); err != nil {
					return err
				}
				if err := removeServiceFile(hold); err != nil {
					return err
				}
				err = removeServiceFile(filepath.Join(s.agent, serviceClaim))
			}
		case "resume", "restart":
			if exists, err := privateServiceFile(s.definition); err != nil || !exists {
				return errors.New("service definition unavailable; uninstall the incomplete service before reinstalling")
			}
			if action == "restart" {
				if held, err := privateServiceFile(hold); err != nil {
					return err
				} else if held {
					return errors.New("service is paused; only 'hush service resume' releases the hold")
				}
				if err := s.unload(ctx, false); err != nil {
					return err
				}
			}
			if err := removeServiceFile(hold); err != nil {
				return err
			}
			err = s.load(ctx, false)
		default:
			return errors.New("invalid service action")
		}
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, map[string]string{"install": "Hush user service installed; run 'hush service status' to check readiness.",
		"uninstall": "Hush user service removed; your threads and history were kept.",
		"pause":     "Hush user service paused.", "resume": "Hush user service start requested.", "restart": "Hush user service restart requested."}[action])
	return err
}

// intro reads the running service's summary. It is attach-only.
func (s loginService) intro(ctx context.Context) (sdk.IntroReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	client := sdk.NewClient(filepath.Join(s.agent, "runtime", "hush.sock"), sdk.ClientOptions{
		Capabilities: map[string][]uint64{"intro": {sdk.IntroActionsVersion}}, RequiredCapabilities: map[string]uint64{"hushProfile": 1},
	})
	defer client.Close()
	return client.Intro(ctx, sdk.IntroScope{})
}

// ServiceStatus is the machine-readable form of 'hush service status' for
// status displays such as the macOS menu bar app.
type ServiceStatus struct {
	// State is not_installed, paused, running or unavailable (installed but
	// not answering; see Log).
	State         string               `json:"state"`
	Sessions      *sdk.IntroSessions   `json:"sessions,omitempty"`
	Attention     []sdk.IntroAttention `json:"attention"`
	MoreAttention int                  `json:"moreAttention"`
	StartedAtMS   int64                `json:"startedAtMs,omitempty"`
	Log           string               `json:"log,omitempty"`
}

// ServiceStatusJSON reports the service state as one JSON object. Unlike the
// text form, an unavailable service is a state, not an error.
func ServiceStatusJSON(ctx context.Context, out io.Writer) error {
	s, err := servicePaths()
	if err != nil {
		return err
	}
	status := ServiceStatus{State: "not_installed", Attention: []sdk.IntroAttention{}}
	if installed, err := serviceInstalled(s.agent); err != nil {
		return err
	} else if installed {
		status.Log = filepath.Join(s.agent, "supervisor.log")
		if held, err := privateServiceFile(filepath.Join(s.agent, serviceHold)); err != nil {
			return err
		} else if held {
			status.State = "paused"
		} else if report, err := s.intro(ctx); err != nil {
			status.State = "unavailable"
		} else {
			status.State, status.Sessions, status.MoreAttention, status.StartedAtMS = "running", &report.Sessions, report.MoreAttention, report.StartedAtMS
			status.Attention = append(status.Attention, report.Attention...)
		}
	}
	return json.NewEncoder(out).Encode(status)
}

func (s loginService) report(ctx context.Context, out io.Writer) bool {
	report, err := s.intro(ctx)
	if err != nil {
		return false
	}
	fmt.Fprintf(out, "Hush service is running: %d active, %d need attention%s.\n", report.Sessions.Running,
		len(report.Attention)+report.MoreAttention, map[bool]string{true: " (partial inventory)"}[report.Sessions.Partial])
	for _, row := range report.Attention {
		if row.ThreadID != "" {
			fmt.Fprintf(out, "  Thread %s (%s): hush --thread %s\n", row.ThreadID, row.Status, row.ThreadID)
			if row.Issue != nil && (row.Issue.Reason == "no_model" || row.Issue.Reason == "model_error") {
				fmt.Fprintln(out, "    Check the thread model and run 'hush auth status' on the backend host.")
			}
		}
	}
	fmt.Fprintln(out, "Reattach with 'hush'; use 'hush thread list' to find a thread ID.")
	return true
}

// ReportServiceAfterExit is attach-only and never invokes the OS supervisor.
func ReportServiceAfterExit(ctx context.Context, out io.Writer) {
	s, err := servicePaths()
	if err != nil {
		return
	}
	if installed, _ := serviceInstalled(s.agent); installed {
		if !s.report(ctx, out) {
			fmt.Fprintln(out, "Hush user service is installed. Run 'hush service status' or reattach with 'hush'.")
		}
	}
}

// RunSupervised is the pinned foreground child of a native login supervisor.
// It is not a detached client launch and never accepts profile/socket overrides.
func RunSupervised(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 4 || !serviceToken.MatchString(args[0]) || !servicePath(args[1]) ||
		args[2] != embeddedBundle && !servicePath(args[2]) || !servicePath(args[3]) {
		return 2
	}
	token, node, bundle, helper := args[0], args[1], args[2], args[3]
	s, err := servicePaths()
	if err != nil {
		return 0
	}
	marker := filepath.Join(s.agent, serviceClaim)
	if exists, err := privateServiceFile(marker); err != nil || !exists {
		return 0
	}
	var claim struct{ Token string }
	if b, err := os.ReadFile(marker); err != nil || len(b) > 4096 || json.Unmarshal(b, &claim) != nil || claim.Token != token {
		return 0
	}
	if held, err := privateServiceFile(filepath.Join(s.agent, serviceHold)); err != nil || held {
		return 0
	}
	name := filepath.Join(s.agent, "supervisor.log")
	if fd, err := unix.Open(name, unix.O_CREAT|unix.O_APPEND|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o600); err == nil {
		f := os.NewFile(uintptr(fd), name)
		defer f.Close()
		if exists, err := privateServiceFile(name); err == nil && exists {
			stderr = f
		}
	}
	entry, cleanup, err := materialize(bundle)
	if err != nil {
		fmt.Fprintf(stderr, "Hush service: %s\n", err)
		return 0 // Local configuration; do not loop on failure.
	}
	defer cleanup()
	cmd := exec.CommandContext(ctx, node, entry, "--profile", "hush", "--supervisor", token)
	cmd.Dir, cmd.Stderr = s.home, stderr
	cmd.Env = []string{"HOME=" + s.home, "PATH=/usr/bin:/bin", keyring.HelperEnv + "=" + helper}
	if exe, err := os.Executable(); err == nil {
		cmd.Env = append(cmd.Env, filelist.HelperEnv+"="+exe)
	}
	if runtime.GOOS == "linux" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", os.Getuid()),
			fmt.Sprintf("DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/%d/bus", os.Getuid()))
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	err = cmd.Run()
	var exited *exec.ExitError
	if err == nil || ctx.Err() != nil || errors.As(err, &exited) && exited.ExitCode() == 78 {
		return 0
	}
	fmt.Fprintf(stderr, "Hush service backend exited: %s\n", err)
	if cmd.Process == nil {
		return 0 // Executable removed/unavailable. Repair and resume explicitly.
	}
	return 1
}
