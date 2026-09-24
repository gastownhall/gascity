//go:build linux

package proxyendpoint

import (
	"fmt"
	"os"
	"strconv"
)

// bootIDPath and procStatPath are the two reads the token needs. bootIDPath is
// a variable so the recompute can be exercised against a fixture without a
// process whose start time is known in advance.
var (
	bootIDPath   = "/proc/sys/kernel/random/boot_id"
	procStatPath = func(pid int) string { return "/proc/" + strconv.Itoa(pid) + "/stat" }
)

// capturePlatformBirth recomputes bd's birth token for pid from /proc.
//
// A missing or unreadable boot id is ErrBirthUnavailable, not a mismatch: it is
// a fact about this host's /proc, and treating it as evidence that the proxy
// died would make gc refuse a perfectly live endpoint on a hardened kernel.
func capturePlatformBirth(pid int) (string, error) {
	bootID, err := os.ReadFile(bootIDPath) // #nosec G304 -- a fixed kernel path, or a test fixture
	if err != nil {
		return "", fmt.Errorf("%w: read boot id %s: %w", ErrBirthUnavailable, bootIDPath, err)
	}
	stat, err := os.ReadFile(procStatPath(pid)) // #nosec G304 -- a /proc path derived from an int
	if err != nil {
		return "", fmt.Errorf("read proc stat for pid %d: %w", pid, err)
	}
	startTime, err := ParseProcStatStartTime(string(stat))
	if err != nil {
		return "", err
	}
	return BirthToken(string(bootID), startTime), nil
}
