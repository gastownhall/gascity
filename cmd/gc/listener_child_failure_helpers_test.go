package main

import (
	"errors"
	"os/exec"
	"time"
)

// RED placeholders (ga-os7kifr): the shared child-failure formatter and the
// error-returning launchers behind the python listener fixtures are not
// implemented yet. The GREEN step replaces these stubs.

var errListenerLaunchNotImplemented = errors.New("listener launcher not implemented")

// listenerChildFailure is the RED placeholder for the shared formatter.
func listenerChildFailure(what string, readyErr, exitErr error, stderr []byte) error {
	_, _, _, _ = what, readyErr, exitErr, stderr
	return nil
}

func launchTCPListenerProcess(scratchDir string, port int) (*exec.Cmd, error) {
	_, _ = scratchDir, port
	return nil, errListenerLaunchNotImplemented
}

func launchTCPListenerProcessInDir(scratchDir string, port int, dir string) (*exec.Cmd, error) {
	_, _, _ = scratchDir, port, dir
	return nil, errListenerLaunchNotImplemented
}

func launchLockedDelayedTCPListenerProcessInDir(scratchDir, lockFile string, port int, dir string, delay time.Duration) (*exec.Cmd, error) {
	_, _, _, _, _ = scratchDir, lockFile, port, dir, delay
	return nil, errListenerLaunchNotImplemented
}

func launchUnixSocketProcess(scratchDir, socketPath string) (*exec.Cmd, error) {
	_, _ = scratchDir, socketPath
	return nil, errListenerLaunchNotImplemented
}

func launchOpenFileProcess(scratchDir, path string) (*exec.Cmd, error) {
	_, _ = scratchDir, path
	return nil, errListenerLaunchNotImplemented
}
