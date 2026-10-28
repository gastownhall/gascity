//go:build !windows

package main

import "os/exec"

// posixEnvPrefix reports whether prefixShellEnv renders its POSIX
// `KEY='value' command` prefix. Every command string runs under `sh -c` here.
const posixEnvPrefix = true

// generatedHookUnavailable reports why this platform cannot run command, an
// agent's generated on_boot or on_death recovery hook as it stands after
// template expansion, or "" when it can. Every hook runs under `sh -c` here.
func generatedHookUnavailable(string) string { return "" }

// runPreparedCommand applies prepare, when it is non-nil, and runs cmd to
// completion, returning its standard output as cmd.Output does.
func runPreparedCommand(cmd *exec.Cmd, prepare func(*exec.Cmd)) ([]byte, error) {
	if prepare != nil {
		prepare(cmd)
	}
	return cmd.Output()
}
