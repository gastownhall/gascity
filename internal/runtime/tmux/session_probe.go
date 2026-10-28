package tmux

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// SessionPresence is what a session probe observed about one tmux session on
// one socket.
type SessionPresence int

const (
	// PresenceAlive means the server answered list-sessions and hosts the
	// session.
	PresenceAlive SessionPresence = iota
	// PresenceDead means either the socket refused a dial on a stable inode
	// (the server that owned it is gone), or the server answered
	// list-sessions and does not host the session.
	PresenceDead
	// PresenceUnobservable means no server could be attested at the
	// canonical socket path, or a live server could not be asked.
	// Unobservable is never evidence that the session is gone.
	PresenceUnobservable
)

// ProbeSession attests whether the tmux server named by socketRef hosts
// session. It resolves socketRef the way tmux does (SocketPath), classifies
// the socket file, and only when a live server answered the dial asks it for
// its session list. A socket that is absent, unreadable, or unstable makes
// the session unobservable: the session may be alive under an environment
// this process cannot see, so callers must treat the result as "do not
// know", never as death.
func ProbeSession(ctx context.Context, socketRef, session string) (SessionPresence, error) {
	return probeSessionWith(ctx, socketRef, session, os.Lstat, probeSessionDial, probeSessionExec)
}

// probeSessionDial is ProbeSession's production socket dial.
func probeSessionDial(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// probeSessionExec runs one tmux client command with the process environment
// and newTmuxCommand's pipe hygiene. Its failures bypass wrapError on
// purpose: a connect failure here is an unobservable server, not ErrNoServer
// and not proof the session is gone. list-sessions needs no client -N: it
// starts no server against a missing socket (verified against tmux 3.4), and
// -N is a 3.2 flag while the contract floor is 3.0; -N stays on the attach
// path, where attach-session would start a server.
func probeSessionExec(ctx context.Context, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := newTmuxCommand(ctx, args, &stdout, &stderr)
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("tmux %s: %w: %s", args[0], err, detail)
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return stdout.String(), nil
}

// probeSessionWith is ProbeSession's testable core, injectable the way
// observeNamedSocketWith is: lstat, dial, and exec default to the real
// filesystem, socket dial, and tmux client.
func probeSessionWith(
	ctx context.Context,
	socketRef, session string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
	exec func(context.Context, []string) (string, error),
) (SessionPresence, error) {
	path := SocketPath(socketRef)
	kind, observeErr := classifyNamedSocketWith(ctx, path, lstat, dial)
	switch kind {
	case socketStaleRefused:
		// The server that owned this socket inode is gone.
		return PresenceDead, nil
	case socketLive:
		// Ask the attested server below.
	case socketAbsent:
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q on socket %q: socket not at its canonical path", session, socketRef)
	default:
		// socketUncertain and socketStaleVanished: nothing was proven, and
		// an expired probe bound surfaces as the context error.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return PresenceUnobservable, fmt.Errorf("probing tmux session %q on socket %q: %w", session, socketRef, ctxErr)
		}
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q on socket %q: %w", session, socketRef, observeErr)
	}

	out, err := exec(ctx, []string{"-u", "-S", path, "list-sessions", "-F", "#{session_name}"})
	if err != nil {
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q on socket %q: %w", session, socketRef, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == session {
			return PresenceAlive, nil
		}
	}
	// The server answered; it does not host the session.
	return PresenceDead, nil
}
