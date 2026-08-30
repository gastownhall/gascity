//go:build linux

package beads

import "github.com/gastownhall/gascity/internal/testutil"

// beadsSequenceFloorHangBudget bounds the pure hang-detector wait in
// sqlite_store_sequence_floor_process_linux_test.go's (*sqliteSequenceFloorChild).line:
// the parent re-execs the test binary as a child that opens a SQLiteStore and
// arms a barrier before printing its "ready" protocol line over stdout: no
// assertion depends on how long that takes, only on the line's content once it
// arrives, so this is a hang detector, not a latency assertion. Structurally
// the same shape as storebinding/sqlite's sqliteFenceHangBudget (ga-sptey3): a
// re-exec'd child clearing Go runtime + package init, flag parse and test
// enumeration, plus a SQLite open, before its first protocol line.
// testutil.ExecRaceTimeout (10s) is a floor, not a target, for the same reason
// beadsHangBudget treats testutil.GoroutineRaceTimeout as a floor above.
// Mirrors the same 6x multiplier precedent (60s, well under the gate package
// timeout).
//
// Declared in this linux-tagged file rather than in hangbudget_test.go because
// its only consumer is sqlite_store_sequence_floor_process_linux_test.go; an
// untagged declaration is dead code under any other GOOS and trips `unused`.
// This mirrors internal/storebinding/sqlite/sqlite_fence_process_linux_test.go,
// which keeps sqliteFenceHangBudget inside its own tagged file for this reason.
const beadsSequenceFloorHangBudget = 6 * testutil.ExecRaceTimeout
