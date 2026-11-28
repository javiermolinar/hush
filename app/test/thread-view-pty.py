"""Open an existing thread in a plain Hush TUI and an attach-only child."""
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

binary, socket, origin, target, epoch, *live = sys.argv[1:]
ansi = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*?(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>])")


def terminate(*_):
    raise SystemExit(143)


signal.signal(signal.SIGTERM, terminate)


class Hush:
    def __init__(self, thread, *extra):
        self.args = [binary, "--socket", socket, "-t", thread, "--fills", "off", "--motion=false", *extra]

    def __enter__(self):
        self.master, slave = os.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
        try:
            self.process = subprocess.Popen(self.args, stdin=slave, stdout=slave, stderr=slave)
        finally:
            os.close(slave)
        self.pending = bytearray()
        return self

    def __exit__(self, *_):
        self.process.terminate()
        try:
            self.process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=2)
        finally:
            os.close(self.master)

    def send(self, keys):
        os.write(self.master, keys)

    def see(self, *texts):
        until = time.monotonic() + 5
        while time.monotonic() < until:
            if not select.select([self.master], [], [], 0.05)[0]:
                continue
            try:
                self.pending.extend(os.read(self.master, 16384))
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                break
            plain = ansi.sub(b"", self.pending)
            if all(text in plain for text in texts):
                self.last = plain
                self.pending.clear()
                return
            del self.pending[:-65536]
        raise AssertionError(f"did not see {texts!r}: {ansi.sub(b'', self.pending)[-2500:]!r}")


with Hush(origin) as ui:
    ui.see(b"READY-ORIGIN-THREAD")
    ui.send(f"Open thread {target} here as a tab".encode() + b"\r")
    ui.see(b"TAB-REGISTERED-HERE")
    ui.send(b"\t")
    if live:
        ui.see(b"Always keep this target active")
        ui.send(b"/close\r")
        ui.see(b"READY-ORIGIN-THREAD")
        ui.send(f"Reopen thread {target} here as a tab".encode() + b"\r")
        ui.see(b"TAB-REREGISTERED-HERE")
        ui.send(b"\t")
        ui.see(b"Always keep this target active")
        with open(live[0], "w", encoding="utf-8"):
            pass
        ui.see(b"LATE-TARGET-ANSWER")
        ui.send(b"\t")
        ui.see(b"TAB-REREGISTERED-HERE")
        assert b"LATE-TARGET-ANSWER" not in ui.last
    else:
        ui.see(b"EXACT-TARGET-TRANSCRIPT")

if not live:
    with Hush(target, "--pane-epoch", epoch) as child:
        child.see(b"EXACT-TARGET-TRANSCRIPT")
