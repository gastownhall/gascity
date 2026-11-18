//go:build windows

package tmux

// ownedRegularFile reports no file owned on Windows, where file ownership is
// not a uid: a session's held files are never named there.
func ownedRegularFile(string) bool { return false }
