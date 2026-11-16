//go:build !windows

package acp

// commandLinePromptSuffix returns the text Start appends to the session
// command for suffix, runtime.Config's PromptSuffix: suffix itself, which is
// already quoted for sh, the interpreter the session command runs under.
func commandLinePromptSuffix(suffix string) (string, error) {
	return suffix, nil
}
