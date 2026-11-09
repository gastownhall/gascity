//go:build linux || darwin

package relay

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// DrainLimit bounds how long the relay keeps forwarding output after the
// program exits; background descendants may hold the pty open for longer.
const DrainLimit = 250 * time.Millisecond

// Signals are the signals [Run] acts on, for the caller to subscribe to with
// signal.Notify before calling it.
var Signals = []os.Signal{unix.SIGWINCH, unix.SIGTERM, unix.SIGHUP, unix.SIGINT, unix.SIGQUIT}

// ErrNotTerminal is returned by [Run] when stdin is not a terminal: there is
// nothing to relay, and the caller runs the program directly ([Exec]).
var ErrNotTerminal = errors.New("relay: stdin is not a terminal")

// StartError is a failure before the program started; the terminal is left
// as it was found.
type StartError struct{ Err error }

func (e *StartError) Error() string { return "relay: starting: " + e.Err.Error() }

func (e *StartError) Unwrap() error { return e.Err }

// Options configure [Run].
type Options struct {
	// Argv is the program and its arguments; Argv[0] is looked up in PATH.
	Argv []string
	// Env is the program's environment; nil inherits the relay's.
	Env []string
	// Stdin and Stdout are the relay's terminal (the pane), usually the same
	// tty.
	Stdin, Stdout *os.File
	// Signals delivers the signals the relay received ([Signals]); nil
	// receives none.
	Signals <-chan os.Signal

	// forwarded observes each output chunk's forward time (tests only).
	forwarded func(time.Duration)
}

// Status is how the program ended.
type Status struct {
	Code   int            // the exit code, when Signal is zero
	Signal syscall.Signal // nonzero: the program died by this signal
}

// Run runs the program on a pty of its own and relays between it and the
// terminal until it exits (package doc). It returns the program's status,
// [ErrNotTerminal] when Stdin is not a terminal, or a [*StartError] when the
// program could not be started. The terminal is restored before Run
// returns.
func Run(opts Options) (Status, error) {
	if len(opts.Argv) == 0 {
		return Status{}, &StartError{Err: errors.New("no program to run")}
	}
	if !isTerminal(opts.Stdin) {
		return Status{}, ErrNotTerminal
	}
	master, slave, err := OpenPTY()
	if err != nil {
		return Status{}, &StartError{Err: err}
	}
	defer master.Close() //nolint:errcheck // the pty is done; a close error changes nothing
	cmd, restore, err := start(opts, master, slave)
	_ = slave.Close() // the program holds its own copies; a close error changes nothing
	if err != nil {
		return Status{}, &StartError{Err: err}
	}
	s := &session{opts: opts, master: master, cmd: cmd}
	st := s.relay()
	restore()
	return st, nil
}

// start gives the slave the terminal's modes and size, puts the terminal in
// raw mode and starts the program on the slave. restore puts the terminal
// back; start has already called it when it fails.
func start(opts Options, master, slave *os.File) (cmd *exec.Cmd, restore func(), err error) {
	var modes *unix.Termios
	var size *unix.Winsize
	var raw *term.State
	err = control(opts.Stdin, func(fd int) error {
		var err error
		if modes, err = unix.IoctlGetTermios(fd, ioctlGetTermios); err != nil {
			return fmt.Errorf("reading terminal modes: %w", err)
		}
		if size, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ); err != nil {
			return fmt.Errorf("reading window size: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	// The program's terminal starts as the pane's was: same modes, same size.
	err = control(slave, func(fd int) error {
		if err := unix.IoctlSetTermios(fd, ioctlSetTermios, modes); err != nil {
			return fmt.Errorf("setting pty modes: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if err := setSize(master, size); err != nil {
		return nil, nil, err
	}
	err = control(opts.Stdin, func(fd int) error {
		var err error
		if raw, err = term.MakeRaw(fd); err != nil {
			return fmt.Errorf("raw mode: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	restore = func() {
		// Restoring a pane that is gone fails, and stderr is that pane:
		// there is nowhere to report it.
		_ = control(opts.Stdin, func(fd int) error { return term.Restore(fd, raw) })
	}
	cmd = exec.Command(opts.Argv[0], opts.Argv[1:]...)
	cmd.Env = opts.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		restore()
		return nil, nil, fmt.Errorf("running %q: %w", opts.Argv[0], err)
	}
	return cmd, restore, nil
}

// session is one running program's relay.
type session struct {
	opts   Options
	master *os.File
	cmd    *exec.Cmd
	hungUp bool
}

// relay copies bytes both ways and acts on signals until the program exits,
// and returns its status.
func (s *session) relay() Status {
	b := NewBridge(s.master)
	b.forwarded = s.opts.forwarded
	defer b.Close()
	outDone := make(chan error, 1)
	go func() { outDone <- b.Output(s.master, s.opts.Stdout) }()
	inDone := make(chan error, 1)
	go func() { inDone <- b.Input(s.opts.Stdin) }()
	exited := make(chan Status, 1)
	go func() { exited <- wait(s.cmd) }()

	for {
		select {
		case sig := <-s.opts.Signals:
			s.signal(sig)
		case err := <-outDone:
			outDone = nil
			if errors.Is(err, ErrTerminalGone) {
				s.hangUp()
			}
			// Otherwise every holder of the pty closed it, but the program
			// has not exited yet: keep waiting for it.
		case err := <-inDone:
			inDone = nil
			if !errors.Is(err, ErrProgramGone) {
				s.hangUp() // the pane's input ended: the pane is gone
			}
		case st := <-exited:
			if outDone != nil && !s.hungUp {
				drain := time.NewTimer(DrainLimit)
				select {
				case <-outDone:
				case <-drain.C:
				}
				drain.Stop()
			}
			return st
		}
	}
}

// hangUp closes the pty master, which hangs the program up.
func (s *session) hangUp() {
	if s.hungUp {
		return
	}
	s.hungUp = true
	_ = s.master.Close() // the hangup is the point; a close error changes nothing
}

// signal acts on one signal the relay received.
func (s *session) signal(sig os.Signal) {
	ss, ok := sig.(syscall.Signal)
	if !ok {
		return
	}
	switch ss {
	case unix.SIGWINCH:
		if s.hungUp {
			return
		}
		var size *unix.Winsize
		err := control(s.opts.Stdin, func(fd int) error {
			var err error
			size, err = unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
			return err
		})
		if err == nil {
			// The kernel signals the program's foreground group. A failure
			// means the pty is going away; there is nowhere to report it.
			_ = setSize(s.master, size)
		}
	case unix.SIGTERM, unix.SIGHUP, unix.SIGINT, unix.SIGQUIT:
		pid := s.cmd.Process.Pid
		// The program leads its own session and process group; a program
		// that moved itself out of that group still gets the signal.
		if err := unix.Kill(-pid, ss); errors.Is(err, unix.ESRCH) {
			_ = unix.Kill(pid, ss) // ESRCH again: it already exited
		}
	}
}

// wait waits for the program and returns how it ended.
func wait(cmd *exec.Cmd) Status {
	_ = cmd.Wait() // a non-zero exit is an error too; the status carries it
	ps := cmd.ProcessState
	if ps == nil {
		return Status{Code: waitFailedCode}
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() {
		return Status{Signal: ws.Signal()}
	}
	return Status{Code: ps.ExitCode()}
}

// waitFailedCode is the status when the program's end could not be read.
const waitFailedCode = 255

// Exec replaces the process with the program: the relay's answer when it
// has no terminal to relay. It returns only on failure.
func Exec(argv []string, env []string) error {
	if len(argv) == 0 {
		return errors.New("relay: no program to run")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	if err := unix.Exec(path, argv, env); err != nil {
		return fmt.Errorf("relay: exec %s: %w", path, err)
	}
	return nil
}

// Exit ends the process with st: the program's exit code, or death by the
// program's signal, so whoever waits for the relay sees what the program
// did. A signal that does not end the process (its default action ignores
// or stops) exits 128+signal instead. Exit never returns.
func Exit(st Status) {
	if st.Signal != 0 {
		dieBySignal(st.Signal)
		os.Exit(128 + int(st.Signal))
	}
	os.Exit(st.Code)
}

// setSize sets the pty's window size.
func setSize(master *os.File, size *unix.Winsize) error {
	return control(master, func(fd int) error {
		if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, size); err != nil {
			return fmt.Errorf("setting pty size: %w", err)
		}
		return nil
	})
}

// isTerminal reports whether f is a terminal.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	is := false
	_ = control(f, func(fd int) error { is = term.IsTerminal(fd); return nil })
	return is
}

// control runs fn on f's descriptor without taking it out of the runtime
// poller (f.Fd would make it blocking, and Close could then no longer
// interrupt a read).
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return fmt.Errorf("relay: %s: %w", f.Name(), err)
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return fmt.Errorf("relay: %s: %w", f.Name(), err)
	}
	return ferr
}
