//go:build linux

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
		cred, credErr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if credErr != nil {
			controlErr = credErr
			return
		}
		peerPID = int(cred.Pid)
	})
	if err != nil {
		return 0, fmt.Errorf("inspect socket: %w", err)
	}
	if controlErr != nil {
		return 0, fmt.Errorf("read SO_PEERCRED: %w", controlErr)
	}
	return peerPID, nil
}
