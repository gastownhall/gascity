//go:build !windows

package main

// initialMessageRidesStartupPrompt reports whether a session's initial message
// travels in its startup prompt (runtime.Config.Nudge) rather than on the
// agent's command line (PromptSuffix). Outside Windows the command line keeps
// it for every transport: sh reads the quoted message as one literal argument.
func initialMessageRidesStartupPrompt(bool) bool {
	return false
}
