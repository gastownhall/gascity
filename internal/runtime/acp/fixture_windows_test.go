//go:build windows

package acp

// fixtureIdleAgentCommand runs until its stdin closes and answers nothing.
const fixtureIdleAgentCommand = `$null = [Console]::In.ReadToEnd()`
