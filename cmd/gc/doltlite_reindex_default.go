//go:build !gascity_native_beads

package main

import "errors"

// runDoltliteReindex reports that an in-process DoltLite reindex is unavailable
// in the default build. The SQLite driver used to REINDEX the physical
// .beads/doltlite/<db>.db file is linked only under the gascity_native_beads
// build tag (see internal/beads/doltlite_read_store.go), which the
// native-dependency-surface guard keeps out of the default binary. Deployments
// that manage DoltLite stores must build gc with -tags gascity_native_beads for
// the maintenance reindex (ga-7hei) to run.
func runDoltliteReindex(_ string) error {
	return errors.New("doltlite reindex requires gc built with -tags gascity_native_beads")
}

// doltliteReindexSupported reports that this build cannot rebuild DoltLite
// SQLite indexes in process. The maintenance path probes this before running
// the stale-index-producing flatten/gc so it never creates index corruption it
// cannot heal (ga-7hei).
func doltliteReindexSupported() bool { return false }
