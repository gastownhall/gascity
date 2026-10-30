//go:build integration && !linux

package integration

import "syscall"

// driftSupervisorSysProcAttr is the non-Linux form of the launch attributes
// (no Pdeathsig outside Linux): its own process group, plus cred when the
// supervisor runs under another uid.
func driftSupervisorSysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred}
}
