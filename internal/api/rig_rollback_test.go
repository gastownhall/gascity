package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rig"
)

// waitFor polls cond until it holds or the deadline elapses, failing otherwise.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition never held within %s: %s", timeout, what)
}

// TestRigProvisionFailureCodeCloneFailed pins the C4c §5 mapping: a git.Clone
// failure carried across the boundary as rig.ErrCloneFailed maps to the
// dedicated clone_failed code, not the provision_failed catch-all.
func TestRigProvisionFailureCodeCloneFailed(t *testing.T) {
	wrapped := errors.Join(rig.ErrCloneFailed, errors.New("fatal: repository not found"))
	if got := rigProvisionFailureCode(wrapped); got != "clone_failed" {
		t.Fatalf("rigProvisionFailureCode(clone) = %q, want clone_failed", got)
	}
	// A generic provisioning error still rides the catch-all.
	if got := rigProvisionFailureCode(errors.New("boom")); got != "provision_failed" {
		t.Fatalf("rigProvisionFailureCode(generic) = %q, want provision_failed", got)
	}
}

// TestRigCreateAsyncRollbackDropsThenMarks proves the runtime G14 rollback:
// a failed provision tears down the manifested dir (TeardownPartialRig called
// with the created_dir) and ONLY then marks the durable record rolled_back
// (drop-then-mark), and emits request.failed.
func TestRigCreateAsyncRollbackDropsThenMarks(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.provisionErr = errors.New("init store exploded")
	state.provisionFailN = 1
	h := newTestCityHandler(t, state)

	body := `{"name":"rbrig","git_url":"https://example.com/r.git","request_id":"req-rb-0001"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	waitForEventType(t, state.eventProv, events.RequestFailed, 3*time.Second)

	// Teardown was invoked with the created dir.
	teardowns := state.teardownManifests()
	if len(teardowns) != 1 || teardowns[0].CreatedDir == "" {
		t.Fatalf("teardown calls = %+v, want exactly one with a created_dir", teardowns)
	}

	// The durable record reached rolled_back (drop-then-mark: teardown ran first).
	city := filepath.Clean(state.CityPath())
	recBead, err := lookupIdemRecord(state.cityBeadStore, city, "req-rb-0001")
	if err != nil {
		t.Fatalf("lookup idem record: %v", err)
	}
	if recBead == nil || recBead.Metadata[metaIdemState] != idemStateRolledBack {
		t.Fatalf("record state = %v, want rolled_back", recBead)
	}
	// The manifest was persisted (record-then-create) so a boot sweep could
	// recover it too.
	if recBead.Metadata[metaIdemCreatedDir] == "" {
		t.Fatal("record missing persisted created_dir after rollback")
	}
}

// TestRigCreateAsyncTeardownFailureLeavesInFlight proves the invariant that a
// record never reaches rolled_back with debris on disk: when teardown fails,
// the record stays in_flight (un-retryable until the sweep completes it), the
// live entry is dropped, and request.failed still fires.
func TestRigCreateAsyncTeardownFailureLeavesInFlight(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.provisionErr = errors.New("provision failed")
	state.provisionFailN = 1
	state.teardownErr = errors.New("rm -rf refused")
	srv := New(state)
	h := newTestCityHandlerWith(t, state, srv)

	body := `{"name":"stuckrig","git_url":"https://example.com/r.git","request_id":"req-stuck-1"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}

	waitForEventType(t, state.eventProv, events.RequestFailed, 3*time.Second)

	city := filepath.Clean(state.CityPath())
	recBead, err := lookupIdemRecord(state.cityBeadStore, city, "req-stuck-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if recBead == nil || recBead.Metadata[metaIdemState] != idemStateInFlight {
		t.Fatalf("record state = %v, want in_flight (never rolled_back over debris)", recBead)
	}
	// Live entry dropped so a retry routes to re-clone, not a hung replay.
	if _, ok := srv.rigIdem.lookup(city, "req-stuck-1"); ok {
		t.Fatal("live entry still present after teardown failure; a retry would replay a dead goroutine")
	}
}

// TestRigCreateAsyncRetryPoisonReclones is the C4c §3 retry-poison closure: a
// first attempt fails leaving a manifested store, and a same-request_id retry
// re-clones — the goroutine pre-drops the prior debris (teardown with the prior
// created_dir) and drives to success instead of wedging.
func TestRigCreateAsyncRetryPoisonReclones(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.provisionErr = errors.New("first attempt store init killed")
	state.provisionFailN = 1 // only the FIRST provision fails
	h := newTestCityHandler(t, state)

	body := `{"name":"poisonrig","git_url":"https://example.com/p.git","request_id":"req-poison-1"}`

	// Attempt 1 → fails → rolled_back with a persisted created_dir.
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("attempt 1 status = %d, want 202; body=%s", rec1.Code, rec1.Body.String())
	}
	waitForEventType(t, state.eventProv, events.RequestFailed, 3*time.Second)

	city := filepath.Clean(state.CityPath())
	first, _ := lookupIdemRecord(state.cityBeadStore, city, "req-poison-1")
	if first == nil || first.Metadata[metaIdemState] != idemStateRolledBack || first.Metadata[metaIdemCreatedDir] == "" {
		t.Fatalf("after attempt 1 record = %v, want rolled_back with created_dir", first)
	}

	// Attempt 2 (same request_id) → re-clone 202 → pre-drop + success.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
	if rec2.Code != http.StatusAccepted {
		t.Fatalf("attempt 2 status = %d, want 202 (re-clone); body=%s", rec2.Code, rec2.Body.String())
	}
	waitForEventType(t, state.eventProv, events.RequestResultRigCreate, 3*time.Second)

	// The record is now succeeded, and the pre-drop tore down the prior debris.
	waitFor(t, time.Second, "record succeeded after re-clone", func() bool {
		r, _ := lookupIdemRecord(state.cityBeadStore, city, "req-poison-1")
		return r != nil && r.Metadata[metaIdemState] == idemStateSucceeded
	})
	teardowns := state.teardownManifests()
	sawPreDrop := false
	for _, m := range teardowns {
		if strings.HasSuffix(m.CreatedDir, "poisonrig") {
			sawPreDrop = true
		}
	}
	if !sawPreDrop {
		t.Fatalf("re-clone did not pre-drop the prior debris; teardowns = %+v", teardowns)
	}
}

// fakeSweepDeps is a minimal RigSweepDeps recorder for the boot-sweep tests.
type fakeSweepDeps struct {
	complete    map[string][2]string // rigName -> {prefix, branch}; presence ⇒ complete
	teardowns   []RigProvisionManifest
	teardownErr error
}

func (f *fakeSweepDeps) RigComplete(name string) (bool, string, string) {
	if pb, ok := f.complete[name]; ok {
		return true, pb[0], pb[1]
	}
	return false, "", ""
}

func (f *fakeSweepDeps) TeardownPartialRig(_ context.Context, m RigProvisionManifest) error {
	f.teardowns = append(f.teardowns, m)
	return f.teardownErr
}

// seedInFlightRecord creates a durable in_flight idem record with a created_dir
// manifest key, modeling a crashed provision the boot sweep must reconcile.
func seedInFlightRecord(t *testing.T, store beads.Store, city, requestID, rigName, createdDir string) string {
	t.Helper()
	id, err := createIdemRecord(store, city, requestID, "digest-"+requestID, "0", rigName, idemStateInFlight)
	if err != nil {
		t.Fatalf("create idem record: %v", err)
	}
	if createdDir != "" {
		if err := store.SetMetadataBatch(id, map[string]string{metaIdemCreatedDir: createdDir}); err != nil {
			t.Fatalf("seed created_dir: %v", err)
		}
	}
	return id
}

// TestSweepOrphanRigProvisionsDropsThenMarks proves a partial orphan (crash mid
// provision) is torn down then marked rolled_back — the boot-sweep drop-then-mark.
func TestSweepOrphanRigProvisionsDropsThenMarks(t *testing.T) {
	store := beads.NewMemStore()
	const city = "/city/a"
	id := seedInFlightRecord(t, store, city, "req-orphan-1", "orphrig", "/city/a/rigs/orphrig")

	deps := &fakeSweepDeps{} // RigComplete false ⇒ partial
	if err := SweepOrphanRigProvisions(context.Background(), store, city, deps); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if len(deps.teardowns) != 1 || deps.teardowns[0].CreatedDir != "/city/a/rigs/orphrig" {
		t.Fatalf("teardowns = %+v, want one with the seeded created_dir", deps.teardowns)
	}
	rec, _ := store.Get(id)
	if rec.Metadata[metaIdemState] != idemStateRolledBack {
		t.Fatalf("record state = %q, want rolled_back", rec.Metadata[metaIdemState])
	}
}

// TestSweepOrphanRigProvisionsReconcilesCompleteForward proves the completeness
// probe: an orphan whose rig is fully provisioned (crash in the success window)
// is reconciled FORWARD to succeeded, never torn down.
func TestSweepOrphanRigProvisionsReconcilesCompleteForward(t *testing.T) {
	store := beads.NewMemStore()
	const city = "/city/b"
	id := seedInFlightRecord(t, store, city, "req-complete-1", "goodrig", "/city/b/rigs/goodrig")

	deps := &fakeSweepDeps{complete: map[string][2]string{"goodrig": {"gr", "main"}}}
	if err := SweepOrphanRigProvisions(context.Background(), store, city, deps); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if len(deps.teardowns) != 0 {
		t.Fatalf("a complete rig was torn down: %+v", deps.teardowns)
	}
	rec, _ := store.Get(id)
	if rec.Metadata[metaIdemState] != idemStateSucceeded {
		t.Fatalf("record state = %q, want succeeded", rec.Metadata[metaIdemState])
	}
	if rec.Metadata[metaIdemResultRig] != "goodrig" || rec.Metadata[metaIdemResultPrefix] != "gr" {
		t.Fatalf("forward reconcile did not record result fields: %v", rec.Metadata)
	}
}

// TestSweepOrphanRigProvisionsTeardownFailureLeavesInFlight proves a failed
// teardown leaves the record in_flight (never marked clean over debris).
func TestSweepOrphanRigProvisionsTeardownFailureLeavesInFlight(t *testing.T) {
	store := beads.NewMemStore()
	const city = "/city/c"
	id := seedInFlightRecord(t, store, city, "req-fail-1", "failrig", "/city/c/rigs/failrig")

	deps := &fakeSweepDeps{teardownErr: errors.New("disk error")}
	err := SweepOrphanRigProvisions(context.Background(), store, city, deps)
	if err == nil {
		t.Fatal("sweep returned nil, want a joined teardown error")
	}
	rec, _ := store.Get(id)
	if rec.Metadata[metaIdemState] != idemStateInFlight {
		t.Fatalf("record state = %q, want in_flight after teardown failure", rec.Metadata[metaIdemState])
	}
}

// TestSweepOrphanRigProvisionsIgnoresTerminalRecords proves the sweep only
// touches in_flight orphans: succeeded and rolled_back records are left alone.
func TestSweepOrphanRigProvisionsIgnoresTerminalRecords(t *testing.T) {
	store := beads.NewMemStore()
	const city = "/city/d"
	sid, _ := createIdemRecord(store, city, "req-s", "d", "0", "srig", idemStateSucceeded)
	rid, _ := createIdemRecord(store, city, "req-r", "d", "0", "rrig", idemStateRolledBack)

	deps := &fakeSweepDeps{}
	if err := SweepOrphanRigProvisions(context.Background(), store, city, deps); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(deps.teardowns) != 0 {
		t.Fatalf("sweep touched a terminal record: %+v", deps.teardowns)
	}
	s, _ := store.Get(sid)
	r, _ := store.Get(rid)
	if s.Metadata[metaIdemState] != idemStateSucceeded || r.Metadata[metaIdemState] != idemStateRolledBack {
		t.Fatalf("terminal records mutated: succeeded=%q rolled_back=%q", s.Metadata[metaIdemState], r.Metadata[metaIdemState])
	}
}
