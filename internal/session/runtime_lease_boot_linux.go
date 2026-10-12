package session

import "os"

// readBootID reads Linux's per-boot kernel ID.
func readBootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return string(b)
}
