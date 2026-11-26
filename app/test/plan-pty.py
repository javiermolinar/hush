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


def open_plan(title, count):
    submit("/plan")
    wait_for("Workspace plans", title, count)
    os.write(master, b"\r")


try:
    if mode in ("startup", "chat"):
        wait_for("ask anything")
        os.write(master, b"keep this draft")
        wait_for("keep this draft")
        leader_plan()
        wait_for("Workspace plans", "Interface preferences")
        os.write(master, b"\r")
        wait_for("Select tasks to run here", "Restore preferences")
        if mode == "chat":
            os.write(master, b"\x1b[B ")
            wait_for("Run here Restore preferences")
            os.write(master, b"\r")
            wait_for("Plan run finished", "reported done", "plan r1")
            os.write(master, b"\x18ik")  # Inspect the unbound run without using the draft.
            wait_for("Interface preferences · Plan · r1", "Run here finished")
        else:
            os.write(master, b"\x1b")
            wait_for("Workspace plans")
    elif mode == "historical":
        wait_for("ask anything")
        submit("/plan")
        wait_for("Workspace plans", "Workspace profiles revised", "p pinned revision")
        os.write(master, b"p")
        wait_for("Workspace profiles · Plan · r1", "Pinned revision · cannot run")
        os.write(master, b" ws\r\x1b")
        wait_for("Workspace plans", "Workspace profiles revised")
    elif mode == "worktree":
        def pick_restore(count):
            os.write(master, b"/plan")
            wait_for("/plan", "Open plan")
            os.write(master, b"\r")
            wait_for("Workspace plans", "Interface preferences", count)
            os.write(master, b"\r")
            wait_for("Select tasks to run here")
            os.write(master, b"\x1b[B ")
            wait_for("Run here Restore preferences", "w Run in new worktree")

        wait_for("ask anything")
        pick_restore("0/2 verified · r1")
        os.write(master, b"\r")
        wait_for("Run here finished")
        pick_restore("1 reported done")
        os.write(master, b"w")
        wait_for("hush trust grant --cwd PATH", "y copy grant command")
        os.write(master, b"\x1b")
        wait_for("w review trust", "x keep idle")
        time.sleep(0.15)  # Let the terminal disambiguate the standalone Escape.
        os.write(master, b"x")
        wait_for("Run in new worktree")  # The renderer can reuse the leading key cell.
        os.write(master, b"w")
        wait_for("copy grant command", "start after granting")
        os.write(master, b"w")
        wait_for("Workspace is not trusted for tool execution", "review trust")
        os.write(master, b"w")
        wait_for("copy grant command", "start after granting")
        with open(draft + ".reviewed", "w", encoding="utf-8"):
            pass
        deadline = time.monotonic() + 5
        while not os.path.exists(draft) and time.monotonic() < deadline:
            time.sleep(0.02)
        assert os.path.exists(draft), "backend operator did not grant trust"
        os.write(master, b"w")
        wait_for("Plan run finished", "reported done")
    elif mode == "catalog":
        wait_for("ask anything")
        os.write(master, b"keep this draft")
        wait_for("keep this draft")
        leader_plan()
        wait_for("all workspaces", "Interface preferences", "Read-only", "source-", "checkout", "preview")
        os.write(master, b"\r")
        wait_for("Save preferences", "Read-only")
        os.write(master, b"ac ws\r\x1b")
        wait_for("all workspaces")
        os.write(master, b"\x1b")
    elif mode == "preview":
        wait_for("ask anything")
        submit(draft)
        wait_for("Plan preview · Subagents and independent reviews", "Save to /plan")
        submit("ok save it")
        wait_for("Saved Subagents and independent reviews (r1)")
        open_plan("Subagents and independent reviews", "0/1 verified · r1")
        # The terminal renderer may reuse 'Plan' cells from the inventory header.
        wait_for("Join worker results", "Select tasks to run here")
    elif mode == "draft":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Interface preferences (r1)")
        open_plan("Interface preferences", "0/2 verified · r1")
        wait_for("Interface preferences · Plan · r1", "Select tasks to run here", "Pending")
        os.write(master, b"\x1b[B")
        # The renderer can reuse the unchanged suffix " preferences." from the prior row.
        wait_for("Restore preferences", "Load the saved", "Round-trip preserves the selected theme")
        os.write(master, b"v")
        wait_for("Plan review · r1 · Interface preferences", "No review yet", "Start review")
        os.write(master, b"\r")
        wait_for("Blocking · acceptance · save", "Check how omitted values keep their defaults.")
        os.write(master, b"\x1b")
        wait_for("0/2 verified", "Select tasks to run here")
        os.write(master, b"\x1b")
        wait_for("Workspace plans", "0/2 verified · r1")
        os.write(master, b"\x1b")
        wait_for("Plan review · r1 · reviewer #", "Check how omitted values keep their defaults.")
    elif mode == "fileset":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Align saved profile themes (r1)")
        open_plan("Align saved profile themes", "0/2 verified · r1")
        wait_for("save.txt", "restore.txt", "reference.json")
        os.write(master, b" ")
        wait_for("Run here Align both profiles @ r1", "s scoped")
        os.write(master, b"s")
        wait_for("Isolated acceptance passed: Both profiles match the reference", "Plan run verified")
    elif mode == "scoped":
        wait_for("ask anything")
        submit(draft)
        wait_for("Saved Switch preference themes (r1)")
        open_plan("Switch preference themes", "0/3 verified · r1")
        wait_for("Switch preference themes · Plan · r1")
        os.write(master, b"\x1b[B ")
        wait_for("Run here Restore preferences")
        os.write(master, b"\x1b[A ")
        # The renderer reuses the unchanged "Run here " prefix.
        wait_for("2 @ r1", "s scoped")
        os.write(master, b"s")
        wait_for("Plan run verified · 2/2 verified", "Literal check passed: restore.txt", "1 proposal")
        open_plan("Switch preference themes", "2/3 verified · r1")
        wait_for("2/3 verified", "Update notes")
        os.write(master, b"h")
        wait_for("Run history · Switch preference themes", "Thread #", "restore · turn #")
        os.write(master, b"\x1b")
        wait_for("2/3 verified", "Update notes")
        os.write(master, b"\x1b")
        wait_for("Workspace plans", "2/3 verified · r1")
        os.write(master, b"c")
        wait_for("Closed plans", "reopen")
        os.write(master, b"c")
        wait_for("close plan")
    else:
        wait_for("ask anything")
        os.write(master, b"/plan")
        wait_for("/plan", "Open plan")
        os.write(master, b"\r")
        wait_for("Workspace plans", "Workspace profiles", "0/4 verified · r1")
        os.write(master, b"\r")
        wait_for("Workspace profiles · Plan · r1", "Select the steps to run")
        os.write(master, b"\x1b[B ")
        wait_for("Blocked · select Save a profile")
        os.write(master, b"\x1b[A ")
        wait_for("Run 2 @ r1")
        os.write(master, b"\r")
        # The repaint can reuse the existing “/2 verified” cells. Wait
        # for both committed task status glyphs rather than that byte run.
        wait_for("✓ Save a profile", "✓ Restore saved profiles")
    os.write(master, b"\x1b")
    child.send_signal(signal.SIGTERM)
    assert child.wait(timeout=5) in (0, 143)
finally:
    if child.poll() is None:
        child.kill()
        child.wait()
    os.close(master)
    os.close(slave)
