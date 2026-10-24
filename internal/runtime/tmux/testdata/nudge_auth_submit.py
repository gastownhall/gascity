"""A Claude-shaped composer whose submits never draw a busy indicator."""

import os
import sys
import termios
import tty

mode = sys.argv[1]
draft = bytearray()
submitted = False


def render():
    rows = ["\x1b[2J\x1b[Hauth-submit-ready"]
    if submitted:
        rows.append("ENTER-RECEIVED")
        if mode == "auth":
            rows.append("● Login expired · Please run /login")
    rows += ["─" * 70, "❯ " + draft.decode("utf-8"), "─" * 70,
             "  ⏵⏵ bypass permissions on"]
    sys.stdout.write("\r\n".join(rows) + "\r\n")
    sys.stdout.flush()


fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
try:
    tty.setraw(fd)
    render()
    while True:
        ch = os.read(fd, 1)
        if not ch:
            break
        if ch in (b"\r", b"\n"):
            submitted = True
            if mode != "staged":
                draft.clear()
        elif ch in (b"\x15", b"\x0b"):
            draft.clear()
        elif ch[0] >= 32:
            draft.extend(ch)
        render()
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
