// Package processfloor names the gc invocation an acceptance gc-side budget
// takes its process floor with: the cost of one gc process that starts and
// does nothing else, measured beside the command it is subtracted from
// (test/acceptance/beads_proxied_default_test.go, bd-list-gc-side-budget).
//
// It is a package of its own, with no dependencies, so a cmd/gc unit test can
// check the floor against gc's own startup code without cmd/gc's test binary
// depending on, and rebuilding with, the whole acceptance harness.
package processfloor

// Args is the gc argv of the process floor: `gc ready --help`, one gc process
// that starts the way `gc bd` starts and then does nothing else. It returns a
// fresh slice, so a caller that appends to it cannot change the next caller's
// floor.
//
// `gc --help` is not such a process. With no command word on its line, gc runs
// eager pack-command discovery before it prints anything, and discovery loads
// the city config and the pack tree (cmd/gc/root_argv.go). `gc bd` is one of
// the built-ins that skip it, so a `gc --help` floor carries city-config
// loading the gc side never does, and on a loaded box that work alone puts the
// floor more than the whole budget above the gc side (gate tracker
// ga-97b2v9wd). `gc ready` skips discovery the way `gc bd` does and, unlike
// `gc metrics`, opens the product-metrics lifecycle every other command opens,
// so its --help starts exactly as `gc bd` starts and stops there. cmd/gc's
// TestAcceptanceProcessFloorTakesGCBDsDiscoveryBranch fails if the two stop
// taking the same discovery branch.
func Args() []string {
	return []string{"ready", "--help"}
}
