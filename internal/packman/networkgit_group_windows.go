//go:build windows

package packman

import "os/exec"

// configureNetworkGitDeadline does nothing on Windows, so the deadline reaches
// git alone and the bound rests on WaitDelay closing the pipes rather than on
// the helpers writing into them being stopped.
//
// The Unix side reaches those helpers by signalling git's process group.
// Windows has no group with that meaning; the equivalent is a Job Object, and
// internal/processgroup does own trees that way — but only through
// StartCommand, which starts the process itself so it can assign the job before
// the first instruction runs. This hook only configures a command someone else
// starts, so it cannot use that path, and assigning a job after the fact loses
// whatever the process spawned in between: precisely the helpers this would be
// for.
//
// Left as a no-op rather than a hand-rolled cmd.Process.Kill, which would be
// byte-for-byte what exec already does and so dead code claiming to be a
// fallback.
func configureNetworkGitDeadline(*exec.Cmd) {}
