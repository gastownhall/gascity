//go:build windows

package main

// Command text for the sling runner tests, written in the interpreter the
// platform runs: PowerShell (runtime.ShellCommandContext). The tests are about
// the runner's environment, not the interpreter, so only the script is split.
const (
	// slingRunnerPrintStoreEnvCommand prints the four store-endpoint
	// variables, |-separated.
	slingRunnerPrintStoreEnvCommand = `[Console]::Out.Write($env:GC_DOLT_HOST + '|' + $env:GC_DOLT_PORT + '|' + $env:BEADS_DOLT_SERVER_HOST + '|' + $env:BEADS_DOLT_SERVER_PORT)`
	// slingRunnerPrintSecretsCommand prints GITHUB_TOKEN and OPENAI_API_KEY,
	// |-separated, each as "unset" when empty or absent.
	slingRunnerPrintSecretsCommand = `[Console]::Out.Write($(if ($env:GITHUB_TOKEN) { $env:GITHUB_TOKEN } else { 'unset' }) + '|' + $(if ($env:OPENAI_API_KEY) { $env:OPENAI_API_KEY } else { 'unset' }))`
)
