//go:build !windows

package main

// Command text for the sling runner tests, written in the interpreter the
// platform runs: sh (runtime.ShellCommandContext). The tests are about the
// runner's environment, not the interpreter, so only the script is split.
const (
	// slingRunnerPrintStoreEnvCommand prints the four store-endpoint
	// variables, |-separated.
	slingRunnerPrintStoreEnvCommand = `printf '%s|%s|%s|%s' "$GC_DOLT_HOST" "$GC_DOLT_PORT" "$BEADS_DOLT_SERVER_HOST" "$BEADS_DOLT_SERVER_PORT"`
	// slingRunnerPrintSecretsCommand prints GITHUB_TOKEN and OPENAI_API_KEY,
	// |-separated, each as "unset" when empty or absent.
	slingRunnerPrintSecretsCommand = `printf '%s|%s' "${GITHUB_TOKEN:-unset}" "${OPENAI_API_KEY:-unset}"`
)
