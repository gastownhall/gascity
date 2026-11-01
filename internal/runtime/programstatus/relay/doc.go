// Package relay is the probe-answering pty relay behind `gc-osc7501 relay`
// (design §4.3): it runs an agent program on a pseudo-terminal of its own and
// copies bytes verbatim between that pty and the relay's terminal (the tmux
// pane), so a program that gates its OSC 7501 reports on the terminal
// answering the support probe sees an answer even though tmux drops the
// sequence.
//
// The relay sits in the agent's data path: if it dies, the agent dies. It
// therefore does as little as possible:
//
//   - Output (program to pane) is forwarded verbatim and in order, probes
//     included. A [programstatus.ProbeMatcher] sees every chunk; when a chunk
//     completes the probe ESC ] 7501 ; ? (BEL or ST terminated), the reply
//     ESC ] 7501 ; ? ESC \ is queued into the program's input before the chunk
//     is forwarded. Every later byte the program receives, the pane's DA1
//     answer included, travels the same input queue behind it, so the
//     program reads the 7501 reply before the DA1 reply it asked for in the
//     same breath.
//   - Input (pane to program) is forwarded verbatim. The queue writes each
//     input read and each reply whole, so a reply never lands inside a read.
//   - The relay never parses or publishes reports; capture is the tap's job.
//
// [Run] owns the process side: the pty pair, the program as its session
// leader with the pty as controlling terminal, raw mode on the outer
// terminal, window-size and signal forwarding, and the exit status. [Exit]
// ends the relay with the program's status, re-raising a fatal signal so
// #{pane_dead_status} and #{pane_dead_signal} report the program's death.
//
// # Death semantics
//
// What the relay does when one side of it ends, and what the agent sees:
//
//   - The program exits: the relay drains the pty for at most [DrainLimit]
//     (background descendants may hold it open), restores the terminal and
//     ends with the program's exit code or fatal signal.
//   - The pane goes (stdin ends, or writing the pane fails): the relay closes
//     the pty master, which hangs the program up (SIGHUP to its session), and
//     waits for it, then ends as above. It never kills on a timer of its own:
//     gc's stop already kills the whole process tree.
//   - The relay receives SIGTERM, SIGHUP, SIGINT or SIGQUIT: it forwards the
//     signal to the program's process group and keeps relaying until the
//     program exits. SIGWINCH copies the pane's size to the pty.
//   - The relay itself crashes or is SIGKILLed: the pty master closes, so
//     the program is hung up and in practice dies. The pane shows the
//     relay's death, and the existing crash accounting restarts the seat.
//   - Before the program starts (pty, raw mode or exec fails) the relay
//     prints one line to stderr and exits 127; it never writes to the pane
//     otherwise.
//
// Typed ^C, ^Z and ^\ reach the program as bytes; its own pty's line
// discipline turns them into signals, as the pane's would have.
package relay
