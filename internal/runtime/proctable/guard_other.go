//go:build !windows

package proctable

import (
	"fmt"
	"testing"
)

// hostScanGuard is the refusal every entry point that reads the live host
// process table consults under `go test`. Here it is liveScanGuard: an injected
// procfs root confines the scanner, so setting one is what lifts the refusal.
func hostScanGuard() error {
	return liveScanGuard()
}

// liveScanGuard returns an error when a `go test` run would scan the live
// "/proc" without injecting a fake root first.
//
// Why this guard exists (gastownhall/gascity#2839): the process-table scanner
// reads the real /proc, and the orphan sweep SIGTERMs any runtime that is not
// present in its bead store. Under `go test` that store is empty/sandboxed, so
// on a host actually running gascity EVERY live agent (the mayor and every rig
// worker) looks like an orphan and gets killed. Dev laptops and CI runners have
// no live agents, so the orphan-cleanup tests pass there and the footgun only
// fires on a machine running a real fleet. Refusing the live scan under test
// closes that hole; a test that genuinely needs the scanner must inject a fake
// procfs via SetScanRootForTesting.
func liveScanGuard() error {
	if scanRoot == "/proc" && testing.Testing() {
		return fmt.Errorf("proctable: refusing to scan the live /proc under go test; " +
			"inject a fake procfs root with SetScanRootForTesting (guards against reaping real agent runtimes — see gastownhall/gascity#2839)")
	}
	return nil
}
