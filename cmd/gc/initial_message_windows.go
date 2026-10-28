//go:build windows

package main

// initialMessageRidesStartupPrompt reports whether a session's initial message
// travels in its startup prompt (runtime.Config.Nudge) rather than on the
// agent's command line (PromptSuffix). On Windows it does for an ACP session:
// PowerShell runs the agent command, and when that command is a batch file,
// such as an npm .cmd shim, cmd.exe reads the message again, runs its & | < >
// as commands and expands its %NAME% references whatever the quoting. An ACP
// agent receives the startup prompt as its first session/prompt instead.
func initialMessageRidesStartupPrompt(isACP bool) bool {
	return isACP
}
