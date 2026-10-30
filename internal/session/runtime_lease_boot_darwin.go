package session

import "syscall"

// readBootID reads macOS's per-boot session UUID.
func readBootID() string {
	id, err := syscall.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return id
}
