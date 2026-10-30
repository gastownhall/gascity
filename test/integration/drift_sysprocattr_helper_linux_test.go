//go:build integration && linux

package integration

import "syscall"

// driftSupervisorSysProcAttr returns the launch attributes for a
// harness-spawned drift supervisor.
func driftSupervisorSysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
