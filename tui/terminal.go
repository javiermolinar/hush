package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
	"github.com/javiermolinar/hush/tui/extension"
)

// ProgramOptions applies the shell's frame rate and input filter. When using
// it directly, the caller must Close the model after the program stops.
func (m *Model) ProgramOptions() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithContext(m.ctx), tea.WithFPS(120), tea.WithFilter(motionFilter)}
}

// Run starts the shell and releases its local resources on exit. The caller
// retains ownership of backend. No app CLI or backend launch policy is implied.
func (m *Model) Run(options ...tea.ProgramOption) error {
	defer m.Close()
	// Embedded programs can override Tea's output and window size.
	fd := os.Stdout.Fd()
	ownsTTY := len(options) == 0 && term.IsTerminal(fd)
	if m.intro != nil && len(options) == 0 {
		m.intro.profile = colorprofile.Env(os.Environ())
		if ownsTTY {
			if w, h, err := term.GetSize(fd); err == nil && w > 0 && h > 0 {
				m.intro.tty, m.width, m.height = true, w, h
				m.resizeEditors()
			}
		}
	}
	if ownsTTY && m.images.mode == ImageKitty {
		out := &imageOutput{File: os.Stdout, events: make(chan imageScreen, 1)}
		if kittyTerminal() {
			out.painted = make(chan imagePainted, previewCacheSize)
		}
		m.images.output = out
		options = append(options, tea.WithOutput(out))
	}
	options = append(options, m.ProgramOptions()...)
	var size atomic.Pointer[tea.WindowSizeMsg]
	if ownsTTY {
		options = append(options, tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
			if v, ok := msg.(tea.WindowSizeMsg); ok {
				size.Store(&v)
			}
			return motionFilter(model, msg)
		}))
	}
	program := tea.NewProgram(m, options...)
	if ownsTTY {
		go watchTerminalSize(m.ctx, fd, &size, program.Send)
	}
	_, err := program.Run()
	if m.host.startupErr != nil {
		return m.host.startupErr
	}
	if m.closed && errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	return err
}

// A pane's PTY can change size without delivering SIGWINCH. Keep Tea's
// normal resize handling as the fast path and reconcile against the PTY off
// the UI loop. Send only on a mismatch so idle panes don't repaint.
func watchTerminalSize(ctx context.Context, fd uintptr, size *atomic.Pointer[tea.WindowSizeMsg], send func(tea.Msg)) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			w, h, err := term.GetSize(fd)
			if err != nil || w <= 0 || h <= 0 {
				continue
			}
			current := tea.WindowSizeMsg{Width: w, Height: h}
			if last := size.Load(); last == nil || *last != current {
				send(current)
			}
		}
	}
}

type interceptedKeys map[string]bool

func terminalKeymap(life context.Context) tea.Cmd {
	return func() tea.Msg {
		if os.Getenv("TERM_PROGRAM") != "rex" {
			if os.Getenv("ZELLIJ") == "" {
				return nil
			}
			return interceptedKeys{"ctrl+p": true, "ctrl+t": true, "ctrl+n": true, "ctrl+g": true, "ctrl+h": true, "ctrl+s": true, "ctrl+b": true, "ctrl+o": true, "alt+up": true}
		}
		ctx, cancel := context.WithTimeout(life, 2*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "rex", "keymap", "--json").Output()
		if err != nil || len(out) > 1<<20 {
			return nil
		}
		var keys struct {
			Bindings []struct {
				Keys []string
				Mode string
			}
		}
		if json.Unmarshal(out, &keys) != nil {
			return nil
		}
		result := make(interceptedKeys)
		for _, b := range keys.Bindings {
			if b.Mode == "" && len(b.Keys) > 0 {
				result[normalizeChord(b.Keys[0])] = true
			}
		}
		return result
	}
}

func parseHex(s string) (uint8, error) {
	v, err := strconv.ParseUint(s, 16, 8)
	return uint8(v), err
}

func editArgs(value string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	present, escaped := false, false
	for _, c := range value {
		switch {
		case escaped:
			word.WriteRune(c)
			escaped = false
		case c == '\\' && quote != '\'':
			escaped, present = true, true
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, present = c, true
		case unicode.IsSpace(c):
			if present {
				args = append(args, word.String())
				word.Reset()
				present = false
			}
		default:
			word.WriteRune(c)
			present = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unclosed quote in editor setting")
	}
	if present {
		args = append(args, word.String())
	}
	if len(args) == 0 || args[0] == "" {
		return nil, errors.New("no editor configured")
	}
	return args, nil
}

func configuredEditor() ([]string, error) {
	v := os.Getenv("VISUAL")
	if v == "" {
		v = os.Getenv("EDITOR")
	}
	if v == "" {
		return nil, nil
	}
	return editArgs(v)
}

func editorCommand(ctx context.Context, args []string, path string, line int) (*exec.Cmd, bool) {
	argv := append([]string(nil), args[1:]...)
	name := filepath.Base(args[0])
	terminal := false
	switch name {
	case "code", "code-insiders", "codium", "cursor":
		if line > 0 {
			return exec.CommandContext(ctx, args[0], append(argv, "--goto", fmt.Sprintf("%s:%d", path, line))...), false
		}
	case "hx", "helix":
		terminal = true
		if line > 0 {
			path = fmt.Sprintf("%s:%d", path, line)
		}
	case "vi", "vim", "nvim", "nano", "emacs", "micro", "kak", "ne", "joe":
		terminal = true
		if line > 0 {
			argv = append(argv, fmt.Sprintf("+%d", line))
		}
	}
	return exec.CommandContext(ctx, args[0], append(argv, path)...), terminal
}

func opener(ctx context.Context, target string) *exec.Cmd {
	if runtime.GOOS == "darwin" {
		return exec.CommandContext(ctx, "open", target)
	}
	return exec.CommandContext(ctx, "xdg-open", target)
}

type openPrepared struct {
	cmd      *exec.Cmd
	terminal bool
}
type openFinished struct {
	route ownerRoute
	err   error
}

func (m *Model) openTarget(r ownerRoute, target linkTarget, budget *int) tea.Cmd {
	if target.path != "" && !m.options.WorkspaceLocal {
		return m.copyText(r, target.path, budget)
	}
	urlHook, fileHook := m.options.OpenURL, m.options.OpenFile
	life := m.ctx
	cmd := m.startWork(r, func(ctx context.Context) (any, error) {
		if target.path == "" {
			if urlHook != nil {
				return nil, urlHook(ctx, target.url)
			}
			return nil, opener(ctx, target.url).Run()
		}
		if fileHook != nil {
			return nil, fileHook(ctx, target.path, target.line)
		}
		args, err := configuredEditor()
		if err != nil {
			return nil, err
		}
		if args == nil {
			return nil, opener(ctx, target.path).Run()
		}
		cmd, terminal := editorCommand(life, args, target.path, target.line)
		if terminal {
			return openPrepared{cmd, terminal}, nil
		}
		// The preparation command's context is canceled on reply.
		cmd, _ = editorCommand(ctx, args, target.path, target.line)
		return nil, cmd.Run()
	}, func(v any, err error, _ *int) tea.Cmd {
		if err != nil {
			return m.notifyRoute(r, err.Error(), extension.Danger)
		}
		if p, ok := v.(openPrepared); ok && p.terminal && m.foreground(r) {
			if m.images.output != nil {
				p.cmd.Stdout = os.Stdout // the editor's alt-screen writes aren't Hush's epochs
			}
			return tea.ExecProcess(p.cmd, func(err error) tea.Msg { return openFinished{r, err} })
		}
		return nil
	}, budget)
	return cmd
}

type promptEditPrepared struct {
	cmd       *exec.Cmd
	dir, path string
}
type promptEditFinished struct {
	route     ownerRoute
	rev       uint64
	dir, path string
	err       error
}

func (m *Model) editPrompt(r ownerRoute, text string, rev uint64, budget *int) tea.Cmd {
	life := m.ctx
	return m.startWork(r, func(ctx context.Context) (any, error) {
		args, err := configuredEditor()
		if err != nil {
			return nil, err
		}
		if len(args) == 0 {
			return nil, errors.New("set VISUAL or EDITOR to edit the prompt")
		}
		dir, err := os.MkdirTemp("", "hush-prompt-")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, "prompt.txt")
		if err = os.WriteFile(path, []byte(text), 0600); err != nil {
			_ = os.RemoveAll(dir)
			return nil, err
		}
		cmd, _ := editorCommand(life, args, path, 0)
		if strings.Contains(filepath.Base(args[0]), "code") || filepath.Base(args[0]) == "cursor" {
			cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "--wait", path)
		}
		return promptEditPrepared{cmd, dir, path}, nil
	}, func(v any, err error, _ *int) tea.Cmd {
		if err != nil {
			return m.notifyRoute(r, err.Error(), extension.Danger)
		}
		p, ok := v.(promptEditPrepared)
		if !ok {
			return nil
		}
		if !m.foreground(r) || rev != m.editor.revision {
			return func() tea.Msg { _ = os.RemoveAll(p.dir); return nil }
		}
		m.editorTemp = p.dir
		if m.images.output != nil {
			p.cmd.Stdout = os.Stdout
		}
		return tea.ExecProcess(p.cmd, func(err error) tea.Msg {
			return promptEditFinished{r, rev, p.dir, p.path, err}
		})
	}, budget)
}

func (m *Model) finishPromptEdit(msg promptEditFinished) tea.Cmd {
	b := maxEffects
	r := msg.route
	return m.startWork(r, func(context.Context) (any, error) {
		defer os.RemoveAll(msg.dir)
		if msg.err != nil {
			return nil, msg.err
		}
		f, err := os.Open(msg.path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, maxPromptBytes+1)
		n, err := readLimited(f, buf)
		if err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	}, func(v any, err error, _ *int) tea.Cmd {
		if m.editorTemp == msg.dir {
			m.editorTemp = ""
		}
		if err != nil {
			return m.notifyRoute(r, err.Error(), extension.Danger)
		}
		if m.foreground(r) && m.editor.revision == msg.rev {
			if edited := v.(string); edited != m.editor.expanded() {
				m.editor.set(edited)
				m.slashHidden = false
			}
		}
		return nil
	}, &b)
}

func readLimited(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		k, err := f.Read(buf[n:])
		n += k
		if err != nil {
			if n >= len(buf) {
				break
			}
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return 0, err
		}
	}
	return 0, errors.New("editor result exceeds the prompt limit")
}
