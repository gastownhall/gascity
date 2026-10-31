#!/usr/bin/env python3
"""Minimal Claude-Code-shaped TUI for nudge-confirmation experiments.

Accepts pasted text + a submit Enter exactly like an agent TUI, logs every
SUBMITTED line with a monotonic timestamp, and only renders a busy indicator
after a configurable delay -- modelling the render lag between "Enter landed"
and "the spinner is on screen".
"""
import sys, os, time, threading, termios, tty

logpath = sys.argv[1]
busy_delay = float(sys.argv[2])   # seconds after submit before the spinner shows
busy_hold = float(sys.argv[3]) if len(sys.argv) > 3 else 30.0

state = {"draft": [], "busy_at": None, "done_at": None}
lock = threading.Lock()

def log(msg):
    with open(logpath, "a") as f:
        f.write("%.4f\t%s\n" % (time.monotonic(), msg))

def render():
    while True:
        with lock:
            now = time.monotonic()
            ba, da = state["busy_at"], state["done_at"]
            busy = ba is not None and now >= ba and (da is None or now < da)
            draft = "".join(state["draft"])
        out = ["\x1b[2J\x1b[H", "fake-claude-tui\r\n", "\r\n"]
        if busy:
            el = int(now - ba) + 1
            out.append("✻ Thinking… (%ds · ↑ 0 tokens · esc to interrupt)\r\n" % el)
        else:
            out.append("\r\n")
        out.append("❯ %s\r\n" % draft)
        out.append("  bypass permissions on\r\n")
        sys.stdout.write("".join(out)); sys.stdout.flush()
        time.sleep(0.05)

threading.Thread(target=render, daemon=True).start()
log("TUI-START busy_delay=%s" % busy_delay)

fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
try:
    tty.setraw(fd)
    while True:
        ch = os.read(fd, 1)
        if not ch:
            break
        b = ch[0]
        with lock:
            if b == 0x15:                      # C-u: clear composer
                state["draft"] = []
            elif b in (0x0d, 0x0a):            # CR / LF: submit
                text = "".join(state["draft"])
                if text.strip():
                    log("SUBMIT\t%s" % text.replace("\t", " "))
                    state["draft"] = []
                    state["busy_at"] = time.monotonic() + busy_delay
                    state["done_at"] = state["busy_at"] + busy_hold
                else:
                    log("EMPTY-ENTER")
            elif b == 0x1b:                    # ESC / CSI prefix: ignore
                pass
            elif 0x20 <= b < 0x7f or b >= 0x80:
                state["draft"].append(chr(b) if b < 0x80 else ch.decode("utf-8", "ignore"))
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
