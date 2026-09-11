//go:build gascity_native_beads

package main

import "github.com/gastownhall/gascity/internal/beads"

// runDoltliteReindex rebuilds the DoltLite store's SQLite secondary indexes in
// process using the native beads SQLite driver. Built only under the
// gascity_native_beads tag, where modernc.org/sqlite is linked.
func runDoltliteReindex(dir string) error {
	return beads.ReindexDoltliteStore(dir)
}

// doltliteReindexSupported reports that this build can rebuild DoltLite SQLite
// indexes in process (the native beads SQLite driver is linked). The
// maintenance path probes this before running flatten/gc so it heals the
// resulting stale indexes rather than latching them in (ga-7hei).
func doltliteReindexSupported() bool { return true }
