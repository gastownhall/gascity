package main

// T1 (boot-check-design.md §4.7): the hook request budget on a BORN-SPLIT
// city, with and without a served containment lease. hook_budget_test.go
// measures a single-store city, which never reaches confirmInfraContainment
// at all; this file measures the city F1 is about, whose five infra classes
// live on their own binding, so every hook invocation's containment
// re-check walks that binding's whole work store (classifyInfraContainmentGapLeaving's
// readInfraSnapshotRows) unless a lease stands in for it.
//
// TestHookBudgetBornSplitUnleased pins today's cost — unchanged by this
// slice, since it is what a one-shot process pays when the kill switch is
// on, the controller is down, or the lease is rejected for any reason. It
// matches the measurements boot-check-design.md recorded (23->203,
// 33->213) and the LAT9237 scratch harness's own run against this same
// fixture shape, confirmed against an unmodified main before this slice
// landed (see the PR description for that run's output).
//
// TestHookBudgetBornSplitLeased is the fix: the same two commands, the same
// fixture, with a lease served from a real net.Listen("unix", ...) at
// controllerSocketPath — captured by an actual storageBootGate +
// publishBootContainmentLease call (a real controller-side check), never
// built by hand and never wired through registerResidencyRoutes (which
// would register a long-lived runtime's routes globally; this test wants
// only the socket protocol, not the registry). The request counts must stay
// flat from 4k rows to 40k.
import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
)

// newBornSplitHookBudgetFixture builds a city whose five infra classes are
// configured onto a sqlite-beads binding BEFORE the fixture's session is
// created, so the session — and the containment marker it provokes — is
// born on the split (genesis), the shape F1 measured.
func newBornSplitHookBudgetFixture(t *testing.T) hookBudgetFixture {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_MAIL", "")
	t.Setenv("GC_INJECT_CONTEXT", "")

	cityDir := t.TempDir()
	writeHookBudgetCityTOML(t, cityDir)
	toml := "[workspace]\n\n[beads]\nprovider = \"file\"\n\n" +
		"[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n" +
		fmt.Sprintf("[storage.bindings.infra]\nprovider = %q\npath = %q\n", config.StorageProviderSQLiteBeads, filepath.Join(cityDir, ".gc", "store"))
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	return finishBornSplitHookBudgetFixture(t, cityDir)
}

func finishBornSplitHookBudgetFixture(t *testing.T, cityDir string) hookBudgetFixture {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	if code := cmdSessionNew([]string{hookBudgetIdentity}, "", "", "", true, false, 0, &stdoutBuf, &stderrBuf); code != 0 {
		t.Fatalf("cmdSessionNew = %d; stderr=%s", code, stderrBuf.String())
	}
	sessionBead := onlySessionBead(t, cityDir)
	resetHookBudgetProcessState(t)

	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_SESSION_NAME", hookBudgetIdentity)
	t.Setenv("GC_ALIAS", hookBudgetIdentity)
	t.Setenv("GC_AGENT", hookBudgetIdentity)
	t.Setenv("GC_TEMPLATE", hookBudgetIdentity)
	t.Setenv(managedSessionHookEnv, "1")

	prevAPI := mailInjectProbeClient
	mailInjectProbeClient = func(string) *api.Client { return nil }
	t.Cleanup(func() { mailInjectProbeClient = prevAPI })
	prevStepAPI := activeWorkStepAPIClient
	activeWorkStepAPIClient = func(string) (*api.Client, string) { return nil, "controller-down" }
	t.Cleanup(func() { activeWorkStepAPIClient = prevStepAPI })
	prevPoller := startNudgePoller
	startNudgePoller = func(string, string, string) error { return nil }
	t.Cleanup(func() { startNudgePoller = prevPoller })

	return hookBudgetFixture{cityDir: cityDir, sessionID: sessionBead.ID}
}

// bornSplitHookBudgetScenarios is the subset of hook_budget_test.go's
// scenario runners this file reuses.
var bornSplitHookBudgetScenarios = []struct {
	name string
	run  func(t *testing.T, f hookBudgetFixture, stdout, stderr io.Writer) int
}{
	{"mail check --inject", mailCheckInject},
	{"nudge drain --inject", nudgeDrainInject},
}

// TestHookBudgetBornSplitUnleased is this slice's "before": the full
// containment check's cost on a born-split city, with no controller to ask
// (the fallback every rejection in acceptContainmentLease takes). Matches
// boot-check-design.md's field measurement and must keep matching it —
// this is the F1 number the lease exists to stop paying.
func TestHookBudgetBornSplitUnleased(t *testing.T) {
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	bigger := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: 10 * hookBudgetLedgerRows}
	// mail check matches boot-check-design.md's field measurement exactly
	// (23 -> 203). nudge drain measures one List fewer than the design's
	// 33 -> 213: this branch's base already carries a wisp-type
	// activeMoleculeIn lookup the design's snapshot of main still made
	// (stepResolution.activeMoleculeIn, wisp_step_inject.go), landed on
	// main since the design was drafted and unrelated to this slice. Confirmed
	// by running the design's own unmodified scratch harness
	// (/tmp/lat9237/gascity/cmd/gc/zz_lat9237_splitbudget_test.go) against
	// this worktree: it reproduces 23/203 for mail check and measures
	// 31/211, not 33/213, for nudge drain, with the wisp-type List simply
	// absent from the recorded ops. The unleased cost still scales with
	// the ledger exactly the same way; only the fixed offset moved.
	want := map[string]struct{ requests, bigRequests, unkeyed int }{
		"mail check --inject":  {requests: 23, bigRequests: 203, unkeyed: 1},
		"nudge drain --inject": {requests: 31, bigRequests: 211, unkeyed: 1},
	}
	for _, sc := range bornSplitHookBudgetScenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := newBornSplitHookBudgetFixture(t)
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			cost := model.Price(run.opens, run.ops)
			big := bigger.Price(run.opens, run.ops)
			w := want[sc.name]
			if cost.Requests != w.requests || big.Requests != w.bigRequests {
				t.Fatalf("%s: requests=%d (10x=%d), want %d (10x=%d): %s", sc.name, cost.Requests, big.Requests, w.requests, w.bigRequests, cost)
			}
			if cost.Unkeyed != w.unkeyed || big.Unkeyed != w.unkeyed {
				t.Fatalf("%s: unkeyed=%d (10x=%d), want %d at both sizes", sc.name, cost.Unkeyed, big.Unkeyed, w.unkeyed)
			}
		})
	}
}

// bornSplitLeaseListener serves a real containment lease — captured by an
// actual controller-side check, never built by hand — over
// listenControllerSocketForTest, answering only the storage-verdict
// command. It deliberately bypasses registerResidencyRoutes/
// handleControllerConn's full dispatch: this test is only exercising the
// socket protocol slice 2's fetchContainmentLease speaks, not the
// controller's residency registry.
func bornSplitLeaseListener(t *testing.T, cityDir string, lease *containmentLease) {
	t.Helper()
	lis := listenControllerSocketForTest(t, cityDir)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				if scanner.Text() == storageVerdictCommand {
					writeJSONLine(conn, lease.wire(time.Now()))
				}
			}()
		}
	}()
}

// bornSplitRealLease runs a real, full containment check against f's city
// (storageBootGate with no lease option — the controller's own boot path)
// and publishes it exactly as CityRuntime.run does, then hands back the
// captured lease. The routes this opens are closed before returning: only
// the captured *containmentLease value is kept, never a live store a
// concurrent hook invocation could collide with.
func bornSplitRealLease(t *testing.T, cityDir string) *containmentLease {
	t.Helper()
	cfg := infraSplitConfig(filepath.Join(cityDir, ".gc", "store"))
	var diag bytes.Buffer
	routes, err := storageBootGate(cityDir, cfg, "gc start", nil, &diag)
	if err != nil {
		t.Fatalf("the born-split city's own boot check refused: %v\n%s", err, diag.String())
	}
	publishBootContainmentLease(routes)
	lease := routes.lease.Load()
	if lease == nil {
		t.Fatal("the real controller-side check published no lease")
	}
	if err := routes.close(); err != nil {
		t.Fatalf("routes.close: %v", err)
	}
	return lease
}

// TestHookBudgetBornSplitLeased is this slice's "after": the same two
// commands, the same fixture, with a lease served from a real test
// listener. The request counts must stay flat across the ledger
// multiplier — the whole point of not re-walking the work store.
func TestHookBudgetBornSplitLeased(t *testing.T) {
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	bigger := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: 10 * hookBudgetLedgerRows}
	// 3 and 11, not the design's 3 and 13 — see
	// TestHookBudgetBornSplitUnleased's comment: this branch's nudge drain
	// already makes one fewer List than the design's snapshot of main, so
	// removing the containment check's unkeyed List leaves one fewer
	// keyed request behind it too (31-1 List minus its 20-request walk =
	// 11, not 33-1-20=13).
	want := map[string]struct{ requests, unkeyed int }{
		"mail check --inject":  {requests: 3, unkeyed: 0},
		"nudge drain --inject": {requests: 11, unkeyed: 0},
	}
	for _, sc := range bornSplitHookBudgetScenarios {
		t.Run(sc.name, func(t *testing.T) {
			f := newBornSplitHookBudgetFixture(t)
			lease := bornSplitRealLease(t, f.cityDir)
			bornSplitLeaseListener(t, f.cityDir, lease)

			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return sc.run(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			cost := model.Price(run.opens, run.ops)
			big := bigger.Price(run.opens, run.ops)
			w := want[sc.name]
			// Flat across the ledger multiplier is the headline property:
			// nothing left in the trace scales with ledger size once the
			// lease stands in for the work-store read.
			if cost.Requests != big.Requests {
				t.Fatalf("%s: requests=%d at 1x, %d at 10x — not flat", sc.name, cost.Requests, big.Requests)
			}
			if cost.Requests != w.requests {
				t.Fatalf("%s: requests=%d, want %d with a lease: %s", sc.name, cost.Requests, w.requests, cost)
			}
			if cost.Unkeyed != w.unkeyed {
				t.Fatalf("%s: unkeyed=%d, want %d with a lease", sc.name, cost.Unkeyed, w.unkeyed)
			}
		})
	}
}

// TestHookBudgetBornSplitLeasedControllerUpEmptyDrain is the one case where
// the baseline was already cheap: a nudge drain with nothing queued and a
// live controller never reaches the containment check's List at all, lease
// or not. This guards against the lease path adding a cost where none
// existed.
func TestHookBudgetBornSplitLeasedControllerUpEmptyDrain(t *testing.T) {
	f := newBornSplitHookBudgetFixture(t)
	lease := bornSplitRealLease(t, f.cityDir)
	bornSplitLeaseListener(t, f.cityDir, lease)
	withActiveWorkStepClient(t, readyWorkTestServer(t, hookCityController(t, f)), "")

	run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return nudgeDrainInject(t, f, stdout, stderr) })
	if run.code != 0 {
		t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
	}
	model := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: hookBudgetLedgerRows}
	cost := model.Price(run.opens, run.ops)
	if cost.Requests != 0 || cost.Opens != 0 {
		t.Fatalf("nudge drain --inject controller up, empty: requests=%d opens=%d, want 0/0: %s", cost.Requests, cost.Opens, cost)
	}
}

// TestHookBudgetBornSplitRowCountInvariance is T1's counter variant: the
// containment check's own List count — not merely the wire model's priced
// total — must not depend on the ledger's row count, with or without a
// lease. Unkeyed is derived straight from the recorded op's shape
// (beads.NativeListPlan), never from how many rows the fixture or the
// model's LedgerRows claims the ledger holds, so this is a second,
// independent measurement from TestHookBudgetBornSplit{Unleased,Leased}'s
// Requests comparison, over the same recorded trace priced at two
// unrelated ledger sizes.
func TestHookBudgetBornSplitRowCountInvariance(t *testing.T) {
	for _, leased := range []bool{false, true} {
		name := "unleased"
		wantUnkeyed := 1
		if leased {
			name = "leased"
			wantUnkeyed = 0
		}
		t.Run(name, func(t *testing.T) {
			f := newBornSplitHookBudgetFixture(t)
			if leased {
				lease := bornSplitRealLease(t, f.cityDir)
				bornSplitLeaseListener(t, f.cityDir, lease)
			}
			run := recordHookCommand(t, func(stdout, stderr io.Writer) int { return nudgeDrainInject(t, f, stdout, stderr) })
			if run.code != 0 {
				t.Fatalf("exit %d; stderr=%s", run.code, run.stderr)
			}
			for _, rows := range []int{50, 500} {
				cost := beadstest.WireModel{Handshake: hookBudgetHandshake, LedgerRows: rows}.Price(run.opens, run.ops)
				if cost.Unkeyed != wantUnkeyed {
					t.Fatalf("%s at %d rows: unkeyed=%d, want %d (op count must not depend on ledger size)", name, rows, cost.Unkeyed, wantUnkeyed)
				}
			}
		})
	}
}
