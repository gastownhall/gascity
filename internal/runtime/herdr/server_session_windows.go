//go:build windows

package herdr

import "syscall"

// serverSysProcAttr returns nil on Windows, which has no Unix sessions; the
// herdr server launches with the default process attributes.
func serverSysProcAttr() *syscall.SysProcAttr {
	return nil
}
