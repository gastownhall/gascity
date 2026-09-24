//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/pidutil"

// rootArgv reads pid's argv via kern.procargs2 (the source ps itself uses).
func rootArgv(pid int) ([]string, error) {
	return pidutil.Cmdline(pid)
}
