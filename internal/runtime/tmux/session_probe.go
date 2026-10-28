package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
)

// SessionPresence is what ProbeSession observed of one tmux session.
type SessionPresence int

const (
	// PresenceUnobservable means nothing was observed: no server answered at
	// the socket path this process resolves, so the session may still be
	// running in a server this process cannot see. The zero value, so an
	// unset presence never reads as dead.
	PresenceUnobservable SessionPresence = iota
	// PresenceAlive means a server answered and hosts the session.
	PresenceAlive
	// PresenceDead means the session is attested gone: a server answered
	// without it, or the socket's server is gone.
	PresenceDead
)

// String names the presence for logs.
func (p SessionPresence) String() string {
	switch p {
	case PresenceAlive:
		return "alive"
	case PresenceDead:
		return "dead"
	default:
		return "unobservable"
	}
}

// ProbeSession reports whether session runs on the tmux server socketRef
// names (a -L name or an absolute -S path, resolved by SocketPath). It
// observes the socket before asking tmux, so a missing or unprovable socket
// is unobservable rather than dead. The error explains every unobservable
// result and wraps ctx.Err() when the context ended.
func ProbeSession(ctx context.Context, socketRef, session string) (SessionPresence, error) {
	return probeSessionWith(ctx, socketRef, session, sessionProbeDeps{
		lstat: os.Lstat,
		dial:  dialUnixSocket,
		run:   runTmuxProbe,
	})
}

// sessionProbeDeps are ProbeSession's side effects, injectable for tests.
type sessionProbeDeps struct {
	lstat func(string) (os.FileInfo, error)
	dial  func(context.Context, string) (net.Conn, error)
	run   func(ctx context.Context, args []string) (string, error)
}

func probeSessionWith(ctx context.Context, socketRef, session string, deps sessionProbeDeps) (SessionPresence, error) {
	path := SocketPath(socketRef)
	state, observation := classifyNamedSocketWith(ctx, path, deps.lstat, deps.dial)
	switch state {
	case namedSocketLive:
	case namedSocketStale:
		return PresenceDead, nil
	case namedSocketAbsent:
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q: no socket at %s", session, path)
	case namedSocketVanished:
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q: socket %s vanished during the probe", session, path)
	default:
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q: %w", session, observation)
	}

	_, err := deps.run(ctx, []string{"-L", socketRef, "has-session", "-t", session})
	if err == nil {
		return PresenceAlive, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q at %s: %w", session, path, ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return PresenceDead, nil
	}
	return PresenceUnobservable, fmt.Errorf("probing tmux session %q at %s: %w", session, path, err)
}

func dialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// runTmuxProbe runs one tmux client in this process's environment and returns
// its stdout. A failure keeps the *exec.ExitError and adds tmux's stderr.
func runTmuxProbe(ctx context.Context, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := newTmuxCommand(ctx, args, &stdout, &stderr).Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}
