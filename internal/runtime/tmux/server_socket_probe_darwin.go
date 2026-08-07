//go:build darwin

package tmux

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func socketInode(info os.FileInfo) string {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "unknown"
	}
	return strconv.FormatUint(stat.Ino, 10)
}

func socketPeerPID(conn *net.UnixConn) (int, error) {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("get raw connection: %w", err)
	}
	var peerPID int
	var controlErr error
	err = rawConn.Control(func(fd uintptr) {
		peerPID, controlErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil {
		return 0, fmt.Errorf("inspect socket: %w", err)
	}
	if controlErr != nil {
		return 0, fmt.Errorf("read LOCAL_PEERPID: %w", controlErr)
	}
	return peerPID, nil
}
