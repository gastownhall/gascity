//go:build integration && linux

package integration

import "syscall"

// driftSupervisorSysProcAttr returns the launch attributes for a
// harness-spawned drift supervisor: its own process group, cred when the
// supervisor runs under another uid, and SIGKILL from the kernel when the
// spawning test process dies, so a crashed or interrupted run cannot leak
// the supervisor (ga-rlnmsp). Pdeathsig tracks the spawning thread; the Go
// runtime keeps those threads alive for the life of the test binary.
func driftSupervisorSysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred, Pdeathsig: syscall.SIGKILL}
}
