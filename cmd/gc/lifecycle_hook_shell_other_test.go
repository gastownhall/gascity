//go:build !windows

package main

// shellLifecycleHookForTest runs command as shellWorkQueryWithEnv does.
func shellLifecycleHookForTest(command, dir string, env []string) (string, error) {
	return shellWorkQueryWithEnv(command, dir, env)
}
