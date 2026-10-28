package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type namedSocketWitness struct {
	canonicalPath string
	node          os.FileInfo
	serverPID     int
}

type namedSocketObservation struct {
	node     os.FileInfo
	isSocket bool
}

// errNamedSocketMissing marks the one witness failure that is a clean
// observation rather than an ambiguous one: the named socket does not exist,
// which the socket policy (observeNamedSocketWith) reads as no server able to
// listen. The failure still wraps ErrServerDegraded, so a caller that only
// needs to refuse keeps refusing; Provider.Attach reports it as an absent
// session instead.
var errNamedSocketMissing = errors.New("named tmux socket missing")

func (t *Tmux) captureAttachSocketWitness() (namedSocketWitness, error) {
	if t.cfg.SocketName == "" {
		return namedSocketWitness{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), newSessionProbeTimeout)
	defer cancel()
	witness, err := t.captureNamedSocketWitness(ctx)
	if err != nil {
		if errors.Is(err, ErrServerDegraded) {
			return namedSocketWitness{}, err
		}
		return namedSocketWitness{}, namedSocketWitnessFailure("capture-failed", nil, 0)
	}
	if witness.canonicalPath == "" || witness.node == nil || witness.serverPID <= 0 {
		return namedSocketWitness{}, namedSocketWitnessFailure("invalid-witness", witness.node, witness.serverPID)
	}
	return witness, nil
}

func (t *Tmux) verifyAttachSocketWitness(initial namedSocketWitness) error {
	if t.cfg.SocketName == "" {
		return nil
	}
	current, err := t.captureAttachSocketWitness()
	if err != nil {
		return err
	}
	if initial.canonicalPath != current.canonicalPath || !os.SameFile(initial.node, current.node) || initial.serverPID != current.serverPID {
		return fmt.Errorf("%w: attach_witness reason=replaced initial_inode=%s initial_server_pid=%d current_inode=%s current_server_pid=%d", ErrServerDegraded, witnessInode(initial.node), initial.serverPID, witnessInode(current.node), current.serverPID)
	}
	return nil
}

func (t *Tmux) captureNamedSocketWitness(ctx context.Context) (namedSocketWitness, error) {
	path, err := filepath.Abs(filepath.Clean(namedSocketPath(t.cfg.SocketName)))
	if err != nil {
		return namedSocketWitness{}, namedSocketWitnessFailure("canonical-path", nil, 0)
	}
	before, err := t.observeNamedSocketLstat(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return namedSocketWitness{}, fmt.Errorf("%w: %w", errNamedSocketMissing, namedSocketWitnessFailure("socket-missing", nil, 0))
		}
		return namedSocketWitness{}, namedSocketWitnessFailure("socket-lstat", nil, 0)
	}
	if !before.isSocket {
		return namedSocketWitness{}, namedSocketWitnessFailure("not-unix-socket", before.node, 0)
	}
	out, err := t.exec.executeCtx(ctx, []string{"-u", "-N", "-S", path, "display-message", "-p", "#{pid}"})
	if err != nil {
		return namedSocketWitness{}, namedSocketWitnessFailure("server-pid-query", before.node, 0)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid <= 0 {
		return namedSocketWitness{}, namedSocketWitnessFailure("malformed-server-pid", before.node, 0)
	}
	after, err := t.observeNamedSocketLstat(ctx, path)
	if err != nil {
		return namedSocketWitness{}, namedSocketWitnessFailure("socket-post-lstat", before.node, pid)
	}
	if !after.isSocket {
		return namedSocketWitness{}, namedSocketWitnessFailure("post-not-unix-socket", after.node, pid)
	}
	if !os.SameFile(before.node, after.node) {
		return namedSocketWitness{}, namedSocketWitnessFailure("socket-replaced-during-capture", after.node, pid)
	}
	return namedSocketWitness{canonicalPath: path, node: before.node, serverPID: pid}, nil
}

func (t *Tmux) observeNamedSocketLstat(ctx context.Context, path string) (namedSocketObservation, error) {
	if t.namedSocketLstat != nil {
		return t.namedSocketLstat(ctx, path)
	}
	info, err := lstatWithContext(ctx, os.Lstat, path)
	if err != nil {
		return namedSocketObservation{}, err
	}
	return namedSocketObservation{node: info, isSocket: info.Mode()&os.ModeSocket != 0}, nil
}

func namedSocketWitnessFailure(reason string, node os.FileInfo, serverPID int) error {
	return fmt.Errorf("%w: attach_witness reason=%s inode=%s server_pid=%d", ErrServerDegraded, reason, witnessInode(node), serverPID)
}

func witnessInode(node os.FileInfo) string {
	if node == nil {
		return "unknown"
	}
	return socketInode(node)
}

func (t *Tmux) runForAttachWitness(witness namedSocketWitness, args ...string) (string, error) {
	if witness.canonicalPath == "" {
		return t.runForAttach(args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), tmuxSubprocessTimeout)
	defer cancel()
	allArgs := append([]string{"-u", "-N", "-S", witness.canonicalPath}, args...)
	return t.exec.executeCtx(ctx, allArgs)
}

func (t *Tmux) attachCommandArgs(target string, witness namedSocketWitness) []string {
	args := []string{"-u"}
	if witness.canonicalPath != "" {
		args = append(args, "-N", "-S", witness.canonicalPath)
	} else if t.cfg.SocketName != "" {
		args = append(args, "-N", "-L", t.cfg.SocketName)
	}
	return append(args, "attach-session", "-t", target)
}

// namedSocketPath resolves the exact path tmux uses for a named -L socket.
// tmux honors TMUX_TMPDIR here; TMPDIR is deliberately not a fallback.
func namedSocketPath(socketName string) string {
	tmpDir := os.Getenv("TMUX_TMPDIR")
	if tmpDir == "" {
		tmpDir = "/tmp"
	}
	return filepath.Join(tmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), socketName)
}

// SocketPath resolves the path tmux itself uses for a socket reference: an
// absolute path (the -S form) is returned clean, a named -L socket is joined
// under tmux's TMUX_TMPDIR root. Callers that must talk to the same server as
// a tmux client — or prove they cannot — resolve through here, never by
// re-deriving the join. TMPDIR is never consulted: tmux does not read it.
func SocketPath(ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return namedSocketPath(ref)
}

// serverSocketPath returns the socket path of the server t talks to: its
// named -L socket, or tmux's "default" socket when SocketName is empty.
func (t *Tmux) serverSocketPath() string {
	name := t.cfg.SocketName
	if name == "" {
		name = "default"
	}
	return namedSocketPath(name)
}

// observeServerSocket observes path with the injected observer, or with
// observeNamedSocket when none is set. Nil means the socket is safely absent
// or stale.
func (t *Tmux) observeServerSocket(ctx context.Context, path string) error {
	observer := t.serverSocketObserver
	if observer == nil {
		observer = observeNamedSocket
	}
	return observer(ctx, path)
}

// serverConfirmedDead reports whether the server's socket is missing, or
// refuses connections on a stable inode, so that no server can be listening.
// Any other observation, including a live or unreadable socket, is false.
func (t *Tmux) serverConfirmedDead() bool {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	return t.observeServerSocket(ctx, t.serverSocketPath()) == nil
}

// observeNamedSocket distinguishes a safely absent or stale named socket from
// a socket that might still belong to a live server. It fails closed whenever
// its filesystem and dial observations cannot prove it is safe to create.
func observeNamedSocket(ctx context.Context, path string) error {
	return observeNamedSocketWith(ctx, path, os.Lstat, func(ctx context.Context, path string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	})
}

// socketObservationKind classifies one observation of a named-socket path.
// observeNamedSocketWith folds the safe kinds into "creation is safe"; the
// session probe (session_probe.go) reads the kinds directly to separate an
// attested death from a namespace it cannot observe.
type socketObservationKind int

const (
	// socketAbsent: the first lstat reported os.ErrNotExist. No socket file
	// is at this process's canonical path — also the state a TMUX_TMPDIR
	// split produces.
	socketAbsent socketObservationKind = iota
	// socketStaleRefused: the dial was ECONNREFUSED and the inode stayed
	// stable or the path went absent. The server that owned this socket is
	// gone.
	socketStaleRefused
	// socketStaleVanished: the dial reported os.ErrNotExist with the path
	// now absent. The socket raced away mid-observation; its owner is
	// unproven, so this is not safe to treat as a gone server.
	socketStaleVanished
	// socketLive: a unix socket answered the dial with an attested peer pid.
	socketLive
	// socketUncertain: every outcome the observation could not prove either
	// way, including a canceled or expired context.
	socketUncertain
)

// classifyNamedSocketWith is observeNamedSocketWith's core: it observes path
// with the injected lstat and dial and classifies the result. The returned
// error carries the diagnostics observeNamedSocketWith reports for the
// non-safe kinds (nil for the safe ones), so the folded policy keeps its
// messages. The lstat calls are context-bounded from the caller's
// perspective: an OS syscall already in progress cannot be canceled, but its
// buffered result cannot hold the caller after the context ends.
func classifyNamedSocketWith(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
) (socketObservationKind, error) {
	before, err := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return socketUncertain, fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, contextErr)
	}
	if errors.Is(err, os.ErrNotExist) {
		return socketAbsent, nil
	}
	if err != nil {
		return socketUncertain, fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, err)
	}
	inode := socketInode(before)
	if before.Mode()&os.ModeSocket == 0 {
		return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=not-unix-socket", path, inode)
	}

	conn, err := dial(ctx, path)
	if err == nil {
		defer func() { _ = conn.Close() }()
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=unexpected-connection-type-%T", path, inode, conn)
		}
		peerPID, peerErr := socketPeerPID(unixConn)
		if peerErr != nil {
			return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown peer_pid_reason=%w", path, inode, peerErr)
		}
		return socketLive, fmt.Errorf("path=%s inode=%s peer_pid=%d reason=live-unix-socket", path, inode, peerPID)
	}

	after, afterErr := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown post_lstat=%w", path, inode, contextErr)
	}
	pathAbsent := errors.Is(afterErr, os.ErrNotExist)
	stable := afterErr == nil && os.SameFile(before, after)
	if errors.Is(err, syscall.ECONNREFUSED) && (pathAbsent || stable) {
		return socketStaleRefused, nil
	}
	if errors.Is(err, os.ErrNotExist) && pathAbsent {
		return socketStaleVanished, nil
	}
	if afterErr != nil {
		return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_lstat=%w", path, inode, err, afterErr)
	}
	return socketUncertain, fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_inode=%s reason=socket-identity-changed-or-dial-failed", path, inode, err, socketInode(after))
}

// observeNamedSocketWith keeps the socket policy testable without opening a
// listener. It folds classifyNamedSocketWith's safe kinds — absent, stale by
// stable refusal, or vanished mid-dial — into nil; every other outcome,
// including a live or unreadable socket, is a non-nil error.
func observeNamedSocketWith(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
) error {
	kind, err := classifyNamedSocketWith(ctx, path, lstat, dial)
	switch kind {
	case socketAbsent, socketStaleRefused, socketStaleVanished:
		return nil
	default:
		return err
	}
}

type lstatResult struct {
	info os.FileInfo
	err  error
}

func lstatWithContext(ctx context.Context, lstat func(string) (os.FileInfo, error), path string) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make(chan lstatResult, 1)
	go func() {
		info, err := lstat(path)
		result <- lstatResult{info: info, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return result.info, result.err
	}
}
