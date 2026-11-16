//go:build !darwin && !windows

package pidutil

func platformCmdline(pid int) ([]string, error) {
	return psCmdline(pid)
}
