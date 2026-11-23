//go:build windows

package main

// primeHookPlatformNote closes the context a hook injects into an agent's
// session. Prompts name commands as gc ...; in PowerShell a bare gc is the
// Get-Content alias, so the agent is told to type gc.exe.
const primeHookPlatformNote = "\n\nOn this Windows host, run Gas City commands as `gc.exe`: in PowerShell, `gc` is the Get-Content alias."
