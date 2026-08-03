package tmux

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

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
