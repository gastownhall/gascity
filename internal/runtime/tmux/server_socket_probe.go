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
			return namedSocketWitness{}, namedSocketWitnessFailure("socket-missing", nil, 0)
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
	before, err := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, contextErr)
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("path=%s inode=unknown peer_pid=unknown lstat=%w", path, err)
	}
	inode := socketInode(before)
	if before.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=not-unix-socket", path, inode)
	}

	conn, err := dial(ctx, path)
	if err == nil {
		defer func() { _ = conn.Close() }()
		unixConn, ok := conn.(*net.UnixConn)
		if !ok {
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown reason=unexpected-connection-type-%T", path, inode, conn)
		}
		peerPID, peerErr := socketPeerPID(unixConn)
		if peerErr != nil {
			return fmt.Errorf("path=%s inode=%s peer_pid=unknown peer_pid_reason=%w", path, inode, peerErr)
		}
		return fmt.Errorf("path=%s inode=%s peer_pid=%d reason=live-unix-socket", path, inode, peerPID)
	}

	after, afterErr := lstatWithContext(ctx, lstat, path)
	if contextErr := ctx.Err(); contextErr != nil {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown post_lstat=%w", path, inode, contextErr)
	}
	pathAbsent := errors.Is(afterErr, os.ErrNotExist)
	stable := afterErr == nil && os.SameFile(before, after)
	if errors.Is(err, syscall.ECONNREFUSED) && (pathAbsent || stable) {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) && pathAbsent {
		return nil
	}
	if afterErr != nil {
		return fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_lstat=%w", path, inode, err, afterErr)
	}
	return fmt.Errorf("path=%s inode=%s peer_pid=unknown dial=%w post_inode=%s reason=socket-identity-changed-or-dial-failed", path, inode, err, socketInode(after))
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
