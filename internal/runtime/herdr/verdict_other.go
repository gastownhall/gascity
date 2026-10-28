//go:build !windows

package herdr

// herdrVerdictCode returns the error code herdr answered an invocation with,
// read from the typed envelope.
func herdrVerdictCode(err error) string { return herdrErrorCode(err) }
