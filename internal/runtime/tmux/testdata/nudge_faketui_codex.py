#!/usr/bin/env python3
"""Minimal codex-shaped TUI for the staged-draft submit experiments.

Like codex, it collapses a large paste into a "[Pasted Content N chars]"
placeholder in its composer. It models a TUI still ingesting that paste: the
first `swallow` submits that arrive while a draft is staged are eaten (logged
as SWALLOWED) and the draft stays put. The next one submits (logged as SUBMIT),
clears the composer, and renders codex's busy footer after `busy_delay` for
`busy_hold` seconds.
"""
import sys, os, time, threading, termios, tty

logpath = sys.argv[1]
swallow = int(sys.argv[2])
busy_delay = float(sys.argv[3])
busy_hold = float(sys.argv[4]) if len(sys.argv) > 4 else 30.0
PASTE_PLACEHOLDER_OVER = 200  # chars; longer drafts render as a placeholder

state = {"draft": [], "swallowed": 0, "busy_at": None, "done_at": None}
lock = threading.Lock()

def log(msg):
    with open(logpath, "a") as f:
        f.write("%.4f\t%s\n" % (time.monotonic(), msg))

def composer_text(draft):
    if len(draft) > PASTE_PLACEHOLDER_OVER:
        return "[Pasted Content %d chars]" % len(draft)
    return draft

def render():
    while True:
        with lock:
            now = time.monotonic()
            ba, da = state["busy_at"], state["done_at"]
            busy = ba is not None and now >= ba and (da is None or now < da)
            draft = "".join(state["draft"])
        out = ["\x1b[2J\x1b[H", "fake-codex-tui\r\n", "\r\n"]
        if busy:
            out.append("• Working (%ds • esc to interrupt)\r\n" % (int(now - ba) + 1))
        else:
            out.append("\r\n")
        out.append("› %s\r\n" % (composer_text(draft) or "Explain this codebase"))
        out.append("  gpt-5.5 low · /tmp/probe\r\n")
        sys.stdout.write("".join(out)); sys.stdout.flush()
        time.sleep(0.05)

threading.Thread(target=render, daemon=True).start()
log("TUI-START swallow=%d busy_delay=%s" % (swallow, busy_delay))

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
                if not text.strip():
                    log("EMPTY-ENTER")
                elif state["swallowed"] < swallow:
                    state["swallowed"] += 1
                    log("SWALLOWED\t%d" % len(text))
                else:
                    log("SUBMIT\t%d" % len(text))
                    state["draft"] = []
                    state["busy_at"] = time.monotonic() + busy_delay
                    state["done_at"] = state["busy_at"] + busy_hold
            elif b == 0x1b:                    # ESC (codex's pre-submit key): ignore
                pass
            elif 0x20 <= b < 0x7f or b >= 0x80:
                state["draft"].append(chr(b) if b < 0x80 else ch.decode("utf-8", "ignore"))
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
