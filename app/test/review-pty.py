"""Inspect one explicitly admitted review in Hush, then reattach during its join."""
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

binary, socket, parent = sys.argv[1:]
ansi = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*?(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>])")


def terminate(*_):
    raise SystemExit(143)


signal.signal(signal.SIGTERM, terminate)


class Hush:
    def __enter__(self):
        self.master, slave = os.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
        try:
            self.process = subprocess.Popen([binary, "--socket", socket, "--thread", parent, "--fills", "off"],
                                            stdin=slave, stdout=slave, stderr=slave)
        finally:
            os.close(slave)
        self.pending = bytearray()
        return self

    def __exit__(self, *_):
        self.process.terminate()
        try:
            self.process.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=3)
        finally:
            os.close(self.master)

    def send(self, keys):
        os.write(self.master, keys)

    def see(self, *texts, timeout=5):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline and self.process.poll() is None:
            if not select.select([self.master], [], [], 0.05)[0]:
                continue
            try:
                self.pending.extend(os.read(self.master, 16384))
            except OSError as error:
                if error.errno != errno.EIO:
                    raise
                break
            clean = ansi.sub(b"", self.pending)
            if all(text.encode() in clean for text in texts):
                self.pending.clear()
                return
            del self.pending[:-65536]
        raise AssertionError(f"did not see {texts!r}: {ansi.sub(b'', self.pending)[-2500:]!r}")

    def workers(self, *extra):
        self.send(b"\x18ij")
        self.see("Review workers", "Rules reader", "Slow reader", *extra)


with Hush() as ui:
    ui.see("ask anything", "2 workers", "Sleeping")
    ui.send(b"unsent parent draft")
    ui.see("unsent parent draft")
    ui.workers("Running", "Read pinned versions")
    ui.send(b"\r")
    ui.see("Review evidence", "Rules reader", "Current at inspection", "Review default branch")
    ui.send(b"\x1b[F")  # End
    ui.see("Supplied [local]", "Fixture test excerpt", "3 checks passed")
    ui.send(b"\x1b")
    ui.see("Review workers")
    ui.send(b"\x1b")
    print("inspected", flush=True)

with Hush() as ui:
    ui.see("2 workers", "Sleeping")
    ui.workers()
    print("reattached", flush=True)
    ui.see("Reported", "Timed out", timeout=15)
    ui.send(b"\r")
    ui.see("Review evidence", "Open transcript")
    ui.send(b"\x1b[6~")  # Page Down
    ui.see("Recorded default finding")
    ui.send(b"\x1b")
    ui.see("Review workers")
    ui.send(b"\x1b")
    time.sleep(0.12)  # Resolve Escape before the next xterm key sequence.
    ui.send(b"\x1b[1;5F")  # Ctrl+End: resume following the parent transcript.
    ui.see("Synthesis: default changed; slow review timed out.")
    print("settled", flush=True)
