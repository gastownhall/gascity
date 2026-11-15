package tmux

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
)

// SessionPresence is what a session probe observed about one tmux session.
type SessionPresence int

const (
	// PresenceUnobservable means no server answered for the session's socket
	// in this process's environment. It is not evidence the session is gone.
	PresenceUnobservable SessionPresence = iota
	// PresenceAlive means the server at the socket lists the session.
	PresenceAlive
	// PresenceDead means the server at the socket answered without the
	// session, or the socket refused connections so its server is gone.
	PresenceDead
)

// ProbeSession reports whether the tmux server for socketRef hosts the
// session named exactly session. socketRef is a -L socket name or an
// absolute -S socket path, resolved with SocketPath in this process's
// environment. Only an answering server or a refusing socket attests
// PresenceDead; a missing socket file, an unreadable or replaced socket, a
// failed tmux client, or the end of ctx is PresenceUnobservable with an error
// saying why (wrapping ctx.Err() when ctx ended).
func ProbeSession(ctx context.Context, socketRef, session string) (SessionPresence, error) {
	return probeSessionWith(ctx, SocketPath(socketRef), session, os.Lstat,
		func(ctx context.Context, path string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
		runSessionProbeTmux)
}

// probeSessionWith is ProbeSession over injected filesystem, dial and tmux
// client seams; path is the resolved socket path.
func probeSessionWith(
	ctx context.Context,
	path, session string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
	run func(context.Context, []string) (string, error),
) (SessionPresence, error) {
	state, err := classifyNamedSocketWith(ctx, path, lstat, dial)
	switch state {
	case namedSocketLive:
	case namedSocketRefused:
		return PresenceDead, nil
	case namedSocketMissing, namedSocketVanished:
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q: no tmux socket at %s", session, path)
	default:
		if ctxErr := ctx.Err(); ctxErr != nil {
			return PresenceUnobservable, fmt.Errorf("probing tmux session %q: %w", session, ctxErr)
		}
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q: socket %w", session, err)
	}

	out, err := run(ctx, []string{"-u", "-S", path, "list-sessions", "-F", "#{session_name}"})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return PresenceUnobservable, fmt.Errorf("probing tmux session %q at %s: %w", session, path, ctxErr)
		}
		return PresenceUnobservable, fmt.Errorf("probing tmux session %q at %s: %w", session, path, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimRight(line, "\r") == session {
			return PresenceAlive, nil
		}
	}
	return PresenceDead, nil
}

// runSessionProbeTmux runs one tmux client and returns its stdout. Its error
// keeps tmux's stderr verbatim: a connect failure must stay distinguishable
// from an answer, so it is not folded into ErrNoServer as wrapError does.
func runSessionProbeTmux(ctx context.Context, args []string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := newTmuxCommand(ctx, args, &stdout, &stderr)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
