//go:build windows

package acp

import "errors"

// errPromptOnCommandLine refuses prompt text on an ACP agent's command line.
var errPromptOnCommandLine = errors.New("refusing a prompt on the agent's command line: on Windows an ACP agent receives its prompts as session/prompt requests (runtime.Config.Nudge), because an agent command that is a batch file, such as an npm .cmd shim, hands its command line to cmd.exe, which runs & | < > as commands and expands %NAME% whatever the quoting")

// commandLinePromptSuffix returns the text Start appends to the session
// command for suffix, runtime.Config's PromptSuffix. On Windows no prompt text
// may reach that command line, so a non-empty suffix is refused.
func commandLinePromptSuffix(suffix string) (string, error) {
	if suffix != "" {
		return "", errPromptOnCommandLine
	}
	return "", nil
}
