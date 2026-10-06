//go:build !linux && !darwin

package tmux

import (
	"fmt"
	"net"
	"os"
)

func socketInode(os.FileInfo) string { return "unknown" }

func socketPeerPID(*net.UnixConn) (int, error) {
	return 0, fmt.Errorf("peer PID lookup is unsupported on this platform")
}
