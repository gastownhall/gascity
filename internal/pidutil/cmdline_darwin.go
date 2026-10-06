//go:build darwin

package pidutil

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func platformCmdline(pid int) ([]string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, fmt.Errorf("pidutil: reading argv for PID %d via kern.procargs2: %w", pid, err)
	}
	argv, err := parseProcArgs2(data)
	if err != nil {
		return nil, fmt.Errorf("pidutil: parsing argv for PID %d: %w", pid, err)
	}
	return NormalizeArgv(argv), nil
}
