//go:build !unix

package acceptancehelpers

// dirWritable reports HOME as writable: non-unix hosts have no read-only-HOME
// sandbox to route around, so they keep the behaviour from before the check.
func dirWritable(string) bool { return true }
