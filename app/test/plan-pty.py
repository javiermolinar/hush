"""Drive the Hush plan picker in a PTY without provider or terminal dependencies."""
import errno
import fcntl
import os
import re
import select
import signal
import struct
import subprocess
import sys
import termios
import time

binary, socket, thread, mode, draft = sys.argv[1:]
master, slave = os.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 26, 124, 0, 0))
args = [binary, "--socket", socket, "--fills", "off"]
args += ["--local"] if mode in ("faux", "catalog") else ["--thread", thread]
child = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave)
ansi = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*?(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>])")
pending = bytearray()


def wait_for(*texts):
    expected = [text.encode("utf-8") for text in texts]
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline and child.poll() is None:
        if not select.select([master], [], [], 0.05)[0]:
            continue
        try:
            pending.extend(os.read(master, 16384))
        except OSError as error:
            if error.errno != errno.EIO:
                raise
            break
        clean = ansi.sub(b"", pending)
        if all(text in clean for text in expected):
            pending.clear()
            return
        del pending[:-131072]
    raise AssertionError(f"did not see {texts!r}: {ansi.sub(b'', pending)[-1500:]!r}")


def submit(text):
    os.write(master, text.encode("utf-8"))
    wait_for(text)
    os.write(master, b"\r")


def open_plan(title, count):
    submit("/plan")
    wait_for("Workspace plans", title, count)
    os.write(master, b"\r")


try:
    if mode == "catalog":
        wait_for("ask anything")
        os.write(master, b"/plan")
        wait_for("/plan", "Open plan")
        os.write(master, b"\r")
        wait_for("all workspaces", "Workspace profiles", "source-checkout", "preview")
        os.write(master, b"\r")
        wait_for("Save a profile", "Read-only", "source-checkout")
        os.write(master, b"a c\r")
    elif mode == "preview":
        wait_for("ask anything")
        submit(draft)
        wait_for("Plan preview · Subagents and independent reviews", "Save to /plan")
        submit("ok save it")
        wait_for("Saved Subagents and independent reviews (r1)")
        open_plan("Subagents and independent reviews", "0/1 · r1")
        # The terminal renderer may reuse 'Plan' cells from the inventory header.
        wait_for("Join worker results", "Draft only · execution unavailable")
    elif mode == "draft":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Interface preferences (r1)")
        open_plan("Interface preferences", "0/2 · r1")
        wait_for("Plan · r1 · Interface preferences", "Draft only · execution unavailable", "Pending")
        os.write(master, b" \r\x1b[B")
        wait_for("› ○ Restore preferences")
        os.write(master, b"v")
        wait_for("Plan review · r1 · Interface preferences", "No review yet", "start review")
        os.write(master, b"\r")
        wait_for("Blocking · acceptance · save", "Check how omitted values keep their defaults.")
        os.write(master, b"\x1b")
        wait_for("0/2 verified", "Draft only")
        os.write(master, b"\x1b")
        wait_for("Workspace plans", "0/2 · r1")
        os.write(master, b"\x1b")
        wait_for("Plan review · r1 · reviewer #", "Check how omitted values keep their defaults.")
    elif mode == "fileset":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Align saved profile themes (r1)")
        open_plan("Align saved profile themes", "0/2 · r1")
        wait_for("save.txt", "restore.txt", "reference.json")
        os.write(master, b" ")
        wait_for("Run Align both profiles @ r1", "1 unselected")
        os.write(master, b"\r")
        wait_for("Isolated acceptance passed: Both profiles match the reference", "Plan run verified")
    elif mode == "scoped":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Switch preference themes (r1)")
        open_plan("Switch preference themes", "0/3 · r1")
        wait_for("Plan · r1 · Switch preference themes")
        os.write(master, b"\x1b[B ")
        wait_for("Blocked · select Save preferences")
        os.write(master, b"\x1b[A ")
        wait_for("Run 2 @ r1", "1 unselected")
        os.write(master, b"\r")
        wait_for("Plan run verified · 2/2 verified", "Literal check passed: restore.txt", "1 proposal")
        open_plan("Switch preference themes", "2/3 · r1")
        wait_for("2/3 verified", "Update notes")
        os.write(master, b"h")
        wait_for("Run history · Switch preference themes", "Thread #", "restore · turn #")
        os.write(master, b"\x1b")
        wait_for("2/3 verified", "Update notes")
        os.write(master, b"\x1b")
        wait_for("Workspace plans", "2/3 · r1")
        os.write(master, b"c")
        wait_for("Closed plans", "reopen")
        os.write(master, b"c")
        wait_for("Pending", "close plan")
    else:
        wait_for("ask anything")
        os.write(master, b"/plan")
        wait_for("/plan", "Open plan")
        os.write(master, b"\r")
        wait_for("Workspace plans", "Workspace profiles", "0/4 · r1")
        os.write(master, b"\r")
        wait_for("Plan · r1 · Workspace profiles", "a Use in this thread")
        os.write(master, b"a")
        wait_for("Select the steps to run")
        os.write(master, b"\x1b[B ")
        wait_for("Blocked · select Save a profile")
        os.write(master, b"\x1b[A ")
        wait_for("Run 2 @ r1")
        os.write(master, b"\r")
        wait_for("2/2 verified")
    os.write(master, b"\x1b")
    child.send_signal(signal.SIGTERM)
    assert child.wait(timeout=5) in (0, 143)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
