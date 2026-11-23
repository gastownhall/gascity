package proctable

// ProcessNameKey is the form process names are compared in on this platform.
// On Windows it is case-folded with ".exe" dropped, so "CLAUDE.EXE" and
// "claude" share a key; elsewhere it is the name unchanged. Two names denote the
// same program exactly when their keys are equal.
func ProcessNameKey(name string) string {
	return processNameKey(name)
}
