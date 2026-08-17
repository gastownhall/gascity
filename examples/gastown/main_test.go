package gastown_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/gastownhall/gascity/test/dolttest"
)

// gastownTestRootPrefix scopes this binary's temp root so dolttest.Guard
// only ever reaps dolt sql-servers this run itself spawned, never an
// unrelated process elsewhere under os.TempDir().
const gastownTestRootPrefix = "gc-examples-gastown-"

// TestMain establishes a scoped temp root for the whole examples/gastown
// test binary — including TestReaperWorkflowRootCleanupRealDoltSemantics
// (maintenance_scripts_dolt_integration_test.go, built only with
// -tags=integration or dolt_integration), which starts a real dolt
// sql-server under t.TempDir() — and layers three independent orphan
// defenses over it so a crash, SIGKILL, or `go test -timeout` cannot leak
// that process or its data directory (ga-ntbpyb.2):
//
//  1. dolttest.SweepStale reaps sql-servers left by a PRIOR run of this
//     binary that was SIGKILLed before its own Guard could react (SIGKILL
//     is uncatchable in-process, so only a next-run sweep catches that).
//  2. dolttest.SweepOrphanStoreDirs is the symptom-based fallback
//     (acceptance criterion 2): age > 60m, a .dolt marker present, not
//     held open by any live process — catches the directory left behind
//     regardless of what created it, including cases 1 above still misses.
//  3. dolttest.Guard reaps any sql-server still alive under this run's own
//     root on SIGINT/SIGTERM/SIGQUIT (go test -timeout) or normal exit.
//
// Setting TMPDIR before m.Run() makes every subsequent t.TempDir() call in
// this binary nest under runRoot, the same technique cmd/gc's own TestMain
// uses (cmd/gc/main_test.go) to keep Guard's kill-scope limited to this
// run's own processes rather than every dolt sql-server on the host.
func TestMain(m *testing.M) {
	parent := os.TempDir()
	runRoot, err := os.MkdirTemp(parent, fmt.Sprintf("%s%d-", gastownTestRootPrefix, os.Getpid()))
	if err != nil {
		panic("examples/gastown TestMain: creating scoped temp root: " + err.Error())
	}
	if err := os.Setenv("TMPDIR", runRoot); err != nil {
		panic("examples/gastown TestMain: setting TMPDIR: " + err.Error())
	}

	dolttest.SweepStale(parent, gastownTestRootPrefix)
	dolttest.SweepOrphanStoreDirs(parent)
	stopGuard := dolttest.Guard(runRoot)

	code := m.Run()
	stopGuard()
	_ = os.RemoveAll(runRoot)
	os.Exit(code)
}
