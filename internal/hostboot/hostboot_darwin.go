//go:build darwin

package hostboot

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// bootTime reads the kern.boottime sysctl.
func bootTime() (time.Time, error) {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return time.Time{}, fmt.Errorf("hostboot: reading kern.boottime: %w", err)
	}
	if tv.Sec <= 0 {
		return time.Time{}, fmt.Errorf("hostboot: implausible kern.boottime seconds %d", tv.Sec)
	}
	return time.Unix(int64(tv.Sec), int64(tv.Usec)*1000), nil
}
