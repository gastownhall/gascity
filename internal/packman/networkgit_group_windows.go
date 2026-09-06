//go:build windows

package packman

import "os/exec"

// configureNetworkGitDeadline does nothing on Windows, which has no POSIX
// process group — internal/processgroup has no Windows files at all.
//
// Leaving cmd.Cancel unset keeps exec's default, which kills git alone; the
// bound then rests on WaitDelay closing the pipes rather than on the writers
// being stopped. This is deliberately a no-op rather than a hand-rolled
// cmd.Process.Kill, which would be byte-for-byte what exec already does and so
// dead code claiming to be a fallback. Reaching descendants here needs a job
// object in internal/processgroup, which is where that work belongs.
func configureNetworkGitDeadline(*exec.Cmd) {}
