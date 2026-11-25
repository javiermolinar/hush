//go:build darwin || linux

package tui

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Use a PTY without a controlling process so changing its size cannot send
// SIGWINCH. Check the rendered prompt and footer, not just the model's size.
const resizePTY = `
import fcntl, os, re, select, struct, subprocess, sys, termios, time

master, slave = os.openpty()
def resize(rows, cols):
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', rows, cols, 0, 0))

resize(22, 120)
child = subprocess.Popen([sys.argv[1], '-test.run=^TestTerminalResize$'],
                         stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
ansi = re.compile(rb'\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b\[[0-?]*[ -/]*[@-~]')

def expect_frame(height):
    # With an empty transcript the repaint jumps to the prompt rule, then
    # draws the draft and status on the final two rows.
    rule = b'\x1b[%d;3H' % (height - 2)
    output = b''
    deadline = time.monotonic() + 4
    while time.monotonic() < deadline and child.poll() is None:
        if select.select([master], [], [], .05)[0]:
            output += os.read(master, 8192)
            start = output.rfind(rule)
            if start != -1:
                rows = ansi.sub(b'', output[start+len(rule):]).split(b'\r\n')
                if len(rows) >= 3 and rows[1].strip() == 'you › resize-draft'.encode() and rows[2].startswith(b'  dir workspace'):
                    return
    raise AssertionError('prompt and footer not at the bottom of %d rows: %r' % (height, output[-800:]))

try:
    expect_frame(22)
    resize(44, 120)  # height only, no further input to wake the TUI
    expect_frame(44)
    resize(18, 72)
    expect_frame(18)
    os.write(master, b'\x03\x04')  # clear draft, then quit
    sys.exit(child.wait(timeout=4))
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
`

func TestTerminalResize(t *testing.T) {
	if os.Getenv("HUSH_TEST_RESIZE_PTY") == "child" {
		m := modelFor(t, Options{Fills: FillOff, Images: ImageKitty, ReducedMotion: true})
		m.editor.set("resize-draft")
		if err := m.Run(); err != nil {
			t.Fatal(err)
		}
		return
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required for the PTY fixture: ", err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", resizePTY, binary)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "TERM_PROGRAM=", "KITTY_WINDOW_ID=", "HUSH_TEST_RESIZE_PTY=child")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("resize PTY: %v\n%s", err, out)
	}
}
