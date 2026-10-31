//go:build !windows

package runtime

// ShellCommandUnavailable reports why this platform refuses to run command
// through [ShellCommandContext], or "" when it does not. Every command string
// runs under `sh -c` here, so it never refuses.
func ShellCommandUnavailable(string) string { return "" }
