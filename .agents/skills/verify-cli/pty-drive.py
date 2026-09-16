#!/usr/bin/env python3
"""pty-drive — run a command under a real PTY, send keystrokes, print what the
screen received.

Why this exists: promptster's TUIs (brief_tui.go, decision_tui.go) check whether
stdout is a terminal and take a plain-text branch when it is not. Driving them
through a pipe verifies the fallback, not the TUI. `script(1)` is the usual
answer but BSD script calls tcgetattr on ITS OWN stdin, so it dies with
"Operation not supported on socket" the moment an agent runs it non-
interactively. Python's stdlib pty has no such requirement.

Raw PTY bytes go to stdout; diagnostics go to stderr. control-cli.mjs owns the
ANSI stripping and frame selection.

  pty-drive.py --keys q --settle 1200 --timeout 15000 -- ./promptster brief --here
"""
import argparse
import errno
import os
import pty
import select
import signal
import sys
import time

p = argparse.ArgumentParser()
p.add_argument("--keys", default="")            # comma-separated; \n and \r honoured
p.add_argument("--settle", type=int, default=1200)   # ms before the first keystroke
p.add_argument("--gap", type=int, default=300)       # ms between keystrokes
p.add_argument("--timeout", type=int, default=15000) # ms hard ceiling
p.add_argument("--cols", type=int, default=100)
p.add_argument("--rows", type=int, default=40)
p.add_argument("--cooked", action="store_true",
               help="leave the pty in canonical mode (default is raw, which is what a TUI expects)")
p.add_argument("cmd", nargs=argparse.REMAINDER)
a = p.parse_args()

cmd = a.cmd[1:] if a.cmd and a.cmd[0] == "--" else a.cmd
if not cmd:
    sys.stderr.write("pty-drive: no command given\n")
    sys.exit(2)

def unescape(k):
    # \n \r \t plus \xNN, so a caller can send control characters (ctrl+c is \x03).
    k = k.replace("\\n", "\n").replace("\\r", "\r").replace("\\t", "\t")
    out, i = [], 0
    while i < len(k):
        if k[i:i + 2] == "\\x" and len(k) >= i + 4:
            try:
                out.append(chr(int(k[i + 2:i + 4], 16))); i += 4; continue
            except ValueError:
                pass
        out.append(k[i]); i += 1
    return "".join(out)

keys = [unescape(k) for k in a.keys.split(",") if k != ""]

pid, fd = pty.fork()
if pid == 0:
    # Raw mode BEFORE exec. A bubbletea TUI expects to own the terminal; left
    # canonical, keystrokes sit in the line-discipline buffer and ctrl+c becomes
    # a signal instead of a KeyMsg, so the TUI renders but never sees a key.
    if not a.cooked:
        try:
            import tty
            tty.setraw(0)
        except Exception:
            pass
    # A TUI with no TERM renders nothing useful, so state one.
    os.environ.setdefault("TERM", "xterm-256color")
    os.environ["COLUMNS"] = str(a.cols)
    os.environ["LINES"] = str(a.rows)
    try:
        os.execvp(cmd[0], cmd)
    except OSError as e:
        sys.stderr.write("pty-drive: exec failed: %s\n" % e)
        os._exit(127)

try:
    import fcntl, struct, termios
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", a.rows, a.cols, 0, 0))
except Exception:
    pass

t0 = time.time()
deadline = t0 + a.timeout / 1000.0
send_at = t0 + a.settle / 1000.0
sent = 0
ended = "exit"
buf = bytearray()

while True:
    now = time.time()
    if now > deadline:
        ended = "timeout"
        break
    r, _, _ = select.select([fd], [], [], 0.05)
    if r:
        try:
            chunk = os.read(fd, 65536)
        except OSError as e:
            if e.errno == errno.EIO:   # the child closed the pty: normal exit
                break
            raise
        if not chunk:
            break
        buf += chunk
    if sent < len(keys) and now >= send_at:
        try:
            os.write(fd, keys[sent].encode())
        except OSError:
            break
        sent += 1
        send_at = now + a.gap / 1000.0

code = None
try:
    os.kill(pid, signal.SIGKILL if ended == "timeout" else 0)
except OSError:
    pass
try:
    _, status = os.waitpid(pid, 0)
    code = os.waitstatus_to_exitcode(status)
except Exception:
    pass
try:
    os.close(fd)
except Exception:
    pass

sys.stdout.buffer.write(bytes(buf))
sys.stdout.buffer.flush()
sys.stderr.write("pty-drive: ended=%s keysSent=%d exit=%s ms=%d\n"
                 % (ended, sent, code, int((time.time() - t0) * 1000)))
