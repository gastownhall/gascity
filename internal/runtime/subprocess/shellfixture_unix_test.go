//go:build !windows

package subprocess

import "fmt"

// Command text for the portable provider tests, written in the interpreter the
// platform actually runs. What these tests assert — session tracking, the
// control socket, environment overlay, working directory, stop and interrupt —
// is provider behavior, not shell behavior, so only the script splits.

// cmdSleepLong outlives any test that starts it.
const cmdSleepLong = "sleep 3600"

// cmdSleepLongExec replaces the shell with the sleeping process so the session
// has no intermediate shell parent. See TestSeamsInterrupt for why that matters
// to a single-shot signal.
const cmdSleepLongExec = "exec sleep 3600"

// cmdExitsImmediately succeeds and exits at once.
const cmdExitsImmediately = "true"

// cmdWriteEnvVarToFile writes one environment variable, newline-terminated, to
// path.
func cmdWriteEnvVarToFile(key, path string) string {
	return fmt.Sprintf("echo $%s > %s", key, path)
}

// cmdWriteEnvDumpToFile writes the whole environment to path as sorted
// KEY=VALUE lines.
func cmdWriteEnvDumpToFile(path string) string {
	return fmt.Sprintf("env | sort > %s", path)
}

// cmdWriteCwdToFile writes the command's working directory, newline-terminated,
// to path.
func cmdWriteCwdToFile(path string) string {
	return fmt.Sprintf("pwd > %s", path)
}
