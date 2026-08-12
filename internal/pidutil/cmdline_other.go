//go:build !darwin

package pidutil

func platformCmdline(pid int) ([]string, error) {
	return psCmdline(pid)
}
