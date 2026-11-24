// Package processfloor names the gc invocation an acceptance gc-side budget
// takes its process floor with: the cost of one gc process that starts and
// does nothing else, measured beside the command it is subtracted from
// (test/acceptance/beads_proxied_default_test.go, bd-list-gc-side-budget).
//
// It is a package of its own, with no dependencies, so a cmd/gc unit test can
// check the floor against gc's own startup code without cmd/gc's test binary
// depending on, and rebuilding with, the whole acceptance harness.
package processfloor

// Args is the gc argv of the process floor. It returns a fresh slice, so a
// caller that appends to it cannot change the next caller's floor.
func Args() []string {
	return []string{"--help"}
}
