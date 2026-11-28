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
args += ["--local"] if mode in ("startup", "catalog") else ["--thread", thread]
child = subprocess.Popen(args, stdin=slave, stdout=slave, stderr=slave)
ansi = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*?(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>])")
pending = bytearray()


def wait_for(*texts, timeout=5):
    expected = [text.encode("utf-8") for text in texts]
    deadline = time.monotonic() + timeout
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


def leader_plan():
    # A transcript can paint before capabilities arrive. Probe the existing
    # chord without submitting the draft, and retry only from an idle leader.
    for _ in range(4):
        os.write(master, b"\x18")
        wait_for("Leader")
        os.write(master, b"i")
        try:
            wait_for("Open plan", timeout=0.35)
        except AssertionError:
            os.write(master, b"\x1b")
            time.sleep(0.12)  # Escape disambiguation
            pending.clear()
            continue
        os.write(master, b"n")
        return
    raise AssertionError("plan capability did not appear in the leader menu")


def open_plan(title):
    os.write(master, b"/plan")
    wait_for("/plan", "Open plan")
    os.write(master, b"\r")
    wait_for("Plans", title)
    os.write(master, b"\r")


try:
    if mode == "save":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved the plan.")
        open_plan("Interface preferences")
        wait_for("Interface preferences · r1", "Run 1 here @ r1", "w new worktree")
        os.write(master, b"\x1b[B\x1b[B\t")
        # The step's delegate specification is part of its detail.
        wait_for("Review the round trip", "read-only subagent")
        os.write(master, b"\x1b")
        wait_for("Plans", "Interface preferences")
        os.write(master, b"\x1b")
    elif mode == "startup":
        wait_for("ask anything")
        os.write(master, b"keep this draft")
        wait_for("keep this draft")
        leader_plan()
        wait_for("Plans", "Interface preferences")
        os.write(master, b"\r")
        wait_for("Interface preferences · r1", "Save preferences")
        os.write(master, b"\x1b")
        wait_for("Plans")
        os.write(master, b"\x1b")
    elif mode == "chat":
        wait_for("ask anything")
        os.write(master, b"keep this draft")
        wait_for("keep this draft")
        leader_plan()
        wait_for("Plans", "Interface preferences")
        os.write(master, b"\r")
        wait_for("Interface preferences · r1", "Run 1 here @ r1")
        os.write(master, b"\x1b[B ")
        wait_for("Restore preferences")
        os.write(master, b"\r")
        wait_for("Plan run finished", "plan r1")
        leader_plan()  # the composer still holds the draft
        wait_for("Plans", "Interface preferences")
        os.write(master, b"\r")
        wait_for("1/3 done")
        os.write(master, b"h")
        wait_for("Run history", "Restore preferences: done")
        os.write(master, b"\x1b")
        wait_for("1/3 done")
        os.write(master, b"\x1b")
        wait_for("Plans")
        os.write(master, b"\x1b")
    elif mode == "worktree":
        wait_for("ask anything")
        open_plan("Interface preferences")
        wait_for("Interface preferences · r1", "Run 1 here @ r1")
        os.write(master, b"\r")
        # The strip is the run's state; the transcript may not follow new output.
        wait_for("plan r1", "Run finished")
        open_plan("Interface preferences")
        wait_for("1/3 done")
        os.write(master, b"\x1b[B")
        wait_for("Restore preferences")
        # The new checkout inherits the source's grant, so the run starts at once.
        os.write(master, b"w")
        wait_for("Preference round", "Plan run finished · 1 done", timeout=10)
    elif mode == "catalog":
        wait_for("ask anything")
        os.write(master, b"keep this draft")
        wait_for("keep this draft")
        leader_plan()
        wait_for("all workspaces", "Interface preferences", "Workspace")
        os.write(master, b"\r")
        wait_for("Interface preferences · r1", "Runs from work")
        os.write(master, b" \rw\x1b")
        wait_for("all workspaces")
        os.write(master, b"\x1b")
    os.write(master, b"\x1b")
    child.send_signal(signal.SIGTERM)
    assert child.wait(timeout=5) in (0, 143)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
