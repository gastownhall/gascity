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

// TmpdirEnv is the environment variable tmux places its -L sockets under
// (SocketDir).
const TmpdirEnv = "TMUX_TMPDIR"

// SocketPath returns the socket path a tmux client in this process's
// environment talks to for ref. An absolute ref is a -S socket path and is
// returned cleaned. Any other ref is a -L socket name in SocketDir of this
// process's TMUX_TMPDIR.
func SocketPath(ref string) string {
	if filepath.IsAbs(ref) {
		return filepath.Clean(ref)
	}
	return filepath.Join(SocketDir(os.Getenv(TmpdirEnv)), ref)
}

// LiveSockets returns the names of the sockets in dir, a SocketDir, that a
// tmux server may still answer on: each unix socket there unless it refuses
// connections on a stable inode or is gone by the time it is dialed, so one
// whose observation ctx cuts short counts. A dir that does not exist holds
// none.
func LiveSockets(ctx context.Context, dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var live []string
	for _, entry := range entries {
		if entry.Type()&os.ModeSocket == 0 {
			continue
		}
		if observeNamedSocket(ctx, filepath.Join(dir, entry.Name())) != nil {
			live = append(live, entry.Name())
		}
	}
	return live, nil
}

// SocketDir returns the tmux-<uid> directory a tmux client whose
// TMUX_TMPDIR is tmuxTmpdir keeps its -L sockets in. Its parent is
// tmuxTmpdir resolved through symlinks, or /tmp resolved through symlinks
// when tmuxTmpdir is empty or does not resolve; tmux never reads TMPDIR. A
// tmuxTmpdir that resolves to something other than a directory is kept, as
// tmux keeps it: tmux then fails to create the directory instead of falling
// back to /tmp, so a socket path below it observes nothing.
func SocketDir(tmuxTmpdir string) string {
	return filepath.Join(socketBaseDir(tmuxTmpdir), fmt.Sprintf("tmux-%d", os.Getuid()))
}

// socketBaseDir is the directory tmux puts its tmux-<uid> directory in for
// a TMUX_TMPDIR value (see SocketDir).
func socketBaseDir(tmuxTmpdir string) string {
	if tmuxTmpdir != "" {
		if resolved, err := filepath.EvalSymlinks(tmuxTmpdir); err == nil {
			return resolved
		}
	}
	if resolved, err := filepath.EvalSymlinks("/tmp"); err == nil {
		return resolved
	}
	return "/tmp"
}

// namedSocketPath resolves the exact path tmux uses for a named -L socket.
func namedSocketPath(socketName string) string {
	return SocketPath(socketName)
}

// serverSocketPath returns the socket path of the server t talks to.
func (t *Tmux) serverSocketPath() string {
	return namedSocketPath(t.socketLabel())
}

// socketLabel returns the -L socket name of the server t talks to: its
// SocketName, or tmux's "default" when SocketName is empty.
func (t *Tmux) socketLabel() string {
	if t.cfg.SocketName == "" {
		return "default"
	}
	return t.cfg.SocketName
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

// observeNamedSocketWith keeps the socket policy testable without opening a
// listener. The lstat calls are context-bounded from the caller's perspective:
// an OS syscall already in progress cannot be canceled, but its buffered result
// cannot hold the caller after the context ends.
func observeNamedSocketWith(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
) error {
	state, err := classifyNamedSocketWith(ctx, path, lstat, dial)
	switch state {
	case namedSocketMissing, namedSocketVanished, namedSocketRefused:
		return nil
	default:
		return err
	}
}

// namedSocketState is what one observation of a named socket path proved.
type namedSocketState int

const (
	// namedSocketUnknown proves nothing: an lstat, dial or context failure,
	// a non-socket file, or a socket whose identity changed mid-observation.
	namedSocketUnknown namedSocketState = iota
	// namedSocketMissing: no file exists at the path.
	namedSocketMissing
	// namedSocketVanished: a socket file was there, then the dial found no
	// file and the path is now absent.
	namedSocketVanished
	// namedSocketRefused: the socket refused the connection on a stable
	// inode, or refused it and was then removed. The server that owned the
	// inode is gone.
	namedSocketRefused
	// namedSocketLive: a server accepted the connection and its peer pid
	// was read.
	namedSocketLive
)

// classifyNamedSocketWith observes path once. Every state except
// namedSocketMissing, namedSocketVanished and namedSocketRefused comes with
// an error describing the observation.
func classifyNamedSocketWith(
	ctx context.Context,
	path string,
	lstat func(string) (os.FileInfo, error),
	dial func(context.Context, string) (net.Conn, error),
) (namedSocketState, error) {
	before, err := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return namedSocketUnknown, fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, contextErr)
	}
	if errors.Is(err, os.ErrNotExist) {
		return namedSocketMissing, nil
	}
	if err != nil {
		return namedSocketUnknown, fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, err)
	}
	inode := socketInode(before)
	if before.Mode()&os.ModeSocket == 0 {
		return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=not-unix-socket", path, inode)
	}

	conn, err := dial(ctx, path)
	if err == nil {
		defer func() { _ = conn.Close() }()
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=unexpected-connection-type-%T", path, inode, conn)
		}
		peerPID, peerErr := socketPeerPID(unixConn)
		if peerErr != nil {
			return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown peer_pid_reason=%w", path, inode, peerErr)
		}
		return namedSocketLive, fmt.Errorf("path=%s inode=%s peer_pid=%d reason=live-unix-socket", path, inode, peerPID)
	}

	after, afterErr := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown post_lstat=%w", path, inode, contextErr)
	}
	pathAbsent := errors.Is(afterErr, os.ErrNotExist)
	stable := afterErr == nil && os.SameFile(before, after)
	if errors.Is(err, syscall.ECONNREFUSED) && (pathAbsent || stable) {
		return namedSocketRefused, nil
	}
	if errors.Is(err, os.ErrNotExist) && pathAbsent {
		return namedSocketVanished, nil
	}
	if afterErr != nil {
		return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_lstat=%w", path, inode, err, afterErr)
	}
	return namedSocketUnknown, fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_inode=%s reason=socket-identity-changed-or-dial-failed", path, inode, err, socketInode(after))
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
