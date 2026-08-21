//go:build !darwin && !linux

package hostboot

import (
	"errors"
	"time"
)

// bootTime is unavailable on this platform; callers fail safe.
func bootTime() (time.Time, error) {
	return time.Time{}, errors.New("hostboot: boot time unavailable on this platform")
}
