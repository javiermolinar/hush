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
import unicodedata

binary, socket, parent, *options = sys.argv[1:]
stop_test = options == ["stop"]
height, width = 24, 60 if stop_test else 80
ansi = re.compile(rb"\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07]*?(?:\x07|\x1b\\)|[()][A-Za-z0-9]|[=>])")
terminal_tokens = re.compile(b"(" + ansi.pattern + rb"|[\x00-\x0d])")
cursor = re.compile(rb"^\x1b\[([0-9;]*)([ABCDEFGH`df])$")


def cell_of_hint(output, label):
    # Track cursor movement in Hush's xterm output, including partial redraws.
    # Return the last cell at which the complete ASCII hint was painted.
    def cells(text):
        return sum(0 if unicodedata.combining(c) else 2 if unicodedata.east_asian_width(c) in ("F", "W") else 1
                   for c in text.decode("utf-8", errors="replace"))

    col = row = 1
    found = None
    for part in terminal_tokens.split(output):
        if part == b"\r":
            col = 1
        elif part == b"\n":
            row += 1
        elif part == b"\b":
            col = max(1, col - 1)
        elif part == b"\t":
            col += 8 - (col - 1) % 8
        elif match := cursor.match(part):
            args, op = match.groups()
            first, second, *_ = [int(n or 1) or 1 for n in args.split(b";")] + [1]
            if op in (b"H", b"f"):
                row, col = first, second
            elif op == b"d":
                row = first
            elif op in (b"G", b"`"):
                col = first
            elif op in (b"A", b"B", b"E", b"F"):
                row += first if op in (b"B", b"E") else -first
                if op in (b"E", b"F"):
                    col = 1
            else:
                col += first if op == b"C" else -first
        elif part and part[0] >= 0x20:
            if label in part:
                found = (col + cells(part[:part.index(label)]), row)
            col += cells(part)
    assert found and 1 <= found[0] <= width and 1 <= found[1] <= height, (label, found)
    return found


def terminate(*_):
    raise SystemExit(143)


signal.signal(signal.SIGTERM, terminate)


class Hush:
    def __enter__(self):
        self.master, slave = os.openpty()
        fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", height, width, 0, 0))
        try:
            self.process = subprocess.Popen([binary, "--socket", socket, "--thread", parent, "--fills", "off"],
                                            stdin=slave, stdout=slave, stderr=slave)
        finally:
            os.close(slave)
        self.pending = bytearray()
        self.stop_output = bytearray()
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

    def click_hint(self, text):
        col, row = cell_of_hint(self.stop_output, text.encode())
        self.send(f"\x1b[<0;{col};{row}M\x1b[<0;{col};{row}m".encode())

    def see(self, *texts, timeout=5):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline and self.process.poll() is None:
            if not select.select([self.master], [], [], 0.05)[0]:
                continue
            try:
                data = os.read(self.master, 16384)
                self.pending.extend(data)
                if stop_test:
                    self.stop_output.extend(data)
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


if stop_test:
    with Hush() as ui:
        ui.see("ask anything", "2 workers")
        ui.send(b"\x18ij")
        ui.see("Review workers", "First worker", "Second worker", "Unresolved", "x stop worker")
        ui.send(b"x")
        ui.see("Stop was not confirmed")
        print("keyboard stop", flush=True)
        assert sys.stdin.readline() == "next\n"
        ui.send(b"j")
        ui.see("› ⏸ Second worker")
        ui.click_hint("x stop worker")
        print("mouse stop", flush=True)
        assert sys.stdin.readline() == "done\n"
    sys.exit(0)

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
