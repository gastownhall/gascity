//go:build linux

package hostboot

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// procStatPath is overridable in tests.
var procStatPath = "/proc/stat"

// bootTime reads the btime field from /proc/stat, which records the boot
// instant as seconds since the epoch.
func bootTime() (time.Time, error) {
	f, err := os.Open(procStatPath)
	if err != nil {
		return time.Time{}, fmt.Errorf("hostboot: opening %s: %w", procStatPath, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "btime" {
			continue
		}
		secs, convErr := strconv.ParseInt(fields[1], 10, 64)
		if convErr != nil {
			return time.Time{}, fmt.Errorf("hostboot: parsing btime %q: %w", fields[1], convErr)
		}
		if secs <= 0 {
			return time.Time{}, fmt.Errorf("hostboot: implausible btime %d", secs)
		}
		return time.Unix(secs, 0), nil
	}
	if scanErr := scanner.Err(); scanErr != nil {
		return time.Time{}, fmt.Errorf("hostboot: scanning %s: %w", procStatPath, scanErr)
	}
	return time.Time{}, fmt.Errorf("hostboot: no btime field in %s", procStatPath)
}
