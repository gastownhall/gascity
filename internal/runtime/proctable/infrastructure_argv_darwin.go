//go:build darwin

package proctable

import "github.com/gastownhall/gascity/internal/pidutil"

// rootArgv reads pid's argv via kern.procargs2 (the source ps itself uses).
func rootArgv(pid int) ([]string, error) {
	return pidutil.Cmdline(pid)
}

// rootAlive reports liveness through the platform process utilities, which
// treat a zombie as gone.
func rootAlive(pid int) bool {
	return pidutil.Alive(pid)
}

func rootStartIdentity(pid int) string {
	identity, err := ProcessIdentity(pid)
	if err != nil {
		return ""
	}
	return identity
}
