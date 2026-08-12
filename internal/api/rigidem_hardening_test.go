package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestValidateRigNamePathContainment proves the tightened name guard: a path
// separator, a ".." path segment, or a whitespace-only name is rejected before
// the name can be joined onto the server-derived rigs/ clone destination or the
// per-name lock key (a whitespace name would otherwise 500 in withRigNameLock).
func TestValidateRigNamePathContainment(t *testing.T) {
	accept := []string{"web", "api-2", "api_v2", "a..b", "rig.v2"}
	for _, name := range accept {
		if err := validateRigName(name); err != nil {
			t.Errorf("validateRigName(%q) = %v, want nil", name, err)
		}
	}
	reject := []string{
		"",           // empty
		"   ",        // whitespace-only (fix #5: 400 not 500)
		"\t",         // whitespace-only tab
		"a/b",        // path separator
		"../etc",     // parent escape via separator
		"..",         // bare ".." segment
		`a\b`,        // windows-style separator
		"rigs/../hq", // traversal
		"123",        // JSON-inferable numeric (preserved)
		// Tightened allowlist (fix #9): a derived-from-git-URL basename could
		// smuggle these into a filename / lock key / URL path segment.
		"web%20",                // percent (URL-encoded space)
		"a?b",                   // query separator
		"a#b",                   // fragment separator
		"a b",                   // embedded space
		"café",                  // non-ASCII rune
		strings.Repeat("a", 65), // > 64 chars
	}
	for _, name := range reject {
		if err := validateRigName(name); !errors.Is(err, errInvalidRigName) {
			t.Errorf("validateRigName(%q) = %v, want errInvalidRigName", name, err)
		}
	}
}

// TestRigIdemDigestTrims proves the digest preimage is trimmed (fix #7): a body
// that differs only by surrounding whitespace on name/path/git_url digests
// identically to its trimmed form, so a retry does not surface a spurious 409
// body-mismatch. The golden hashes are unchanged because the golden inputs carry
// no surrounding whitespace.
func TestRigIdemDigestTrims(t *testing.T) {
	spaced := RigCreateBody{Name: "  web  ", Path: " /srv/web ", GitURL: "  https://example.com/web.git\n"}
	trimmed := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "https://example.com/web.git"}
	ds, err := rigCreateDigest(spaced)
	if err != nil {
		t.Fatalf("digest spaced: %v", err)
	}
	dt, err := rigCreateDigest(trimmed)
	if err != nil {
		t.Fatalf("digest trimmed: %v", err)
	}
	if ds != dt {
		t.Fatalf("whitespace leaked into digest: spaced=%s trimmed=%s", ds, dt)
	}
	// The argument is not mutated by trimming (copy-by-value).
	if spaced.Name != "  web  " {
		t.Fatalf("rigCreateDigest mutated its argument: %q", spaced.Name)
	}
}

// TestRigIdemForwardReconcileCompleteOrphan proves fix #2: an orphan in_flight
// record whose rig is actually COMPLETE (a markIdemSucceeded that failed after a
// successful provision) is forward-reconciled to succeeded and served as
// rigAdmitExisting (200) — NOT re-cloned, which would tear the live rig down.
func TestRigIdemForwardReconcileCompleteOrphan(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-fwd-00001"}
	digest, _ := rigCreateDigest(body)

	id, err := createIdemRecord(store, "c1", "req-fwd-00001", digest, "3", "web", idemStateInFlight)
	if err != nil {
		t.Fatal(err)
	}

	rigComplete := func(name string) (bool, string, string) {
		if name == "web" {
			return true, "w", "main"
		}
		return false, "", ""
	}

	res, err := admitRigCreate(idx, store, fixedCursor("9"), nil, rigComplete, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitExisting {
		t.Fatalf("outcome = %d, want rigAdmitExisting (forward-reconcile, not re-clone)", res.outcome)
	}
	if res.entry != nil {
		t.Fatal("forward-reconcile must not register a live entry / spawn a re-clone")
	}
	if res.record == nil || res.record.Metadata[metaIdemResultRig] != "web" || res.record.Metadata[metaIdemResultPrefix] != "w" {
		t.Fatalf("result record = %+v, want succeeded result fields", res.record)
	}
	// The durable record was actually transitioned to succeeded.
	rec, _ := store.Get(id)
	if rec.Metadata[metaIdemState] != idemStateSucceeded {
		t.Fatalf("durable record state = %q, want succeeded", rec.Metadata[metaIdemState])
	}
}

// TestRigIdemIncompleteOrphanStillReclones proves the completeness probe does
// NOT over-reach: an orphan in_flight record whose rig is genuinely partial
// (rigComplete=false) still re-clones (row 7), preserving the retry-poison path.
func TestRigIdemIncompleteOrphanStillReclones(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-part-00001"}
	digest, _ := rigCreateDigest(body)
	id, err := createIdemRecord(store, "c1", "req-part-00001", digest, "3", "web", idemStateInFlight)
	if err != nil {
		t.Fatal(err)
	}
	rigComplete := func(string) (bool, string, string) { return false, "", "" }

	res, err := admitRigCreate(idx, store, fixedCursor("9"), nil, rigComplete, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitReclone || res.entry == nil || res.entry.beadID != id {
		t.Fatalf("incomplete orphan outcome = %d entry=%+v, want rigAdmitReclone reusing %s", res.outcome, res.entry, id)
	}
}

// TestRigIdemDeletedRigSucceededReclones proves fix #4: a succeeded record whose
// rig is ABSENT from config (deleted via gc rig remove / DeleteRig) is
// re-executable — admission re-clones rather than replaying a stale 200-exists.
func TestRigIdemDeletedRigSucceededReclones(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-del-00001"}
	digest, _ := rigCreateDigest(body)
	id, err := createIdemRecord(store, "c1", "req-del-00001", digest, "3", "web", idemStateSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := markIdemSucceeded(store, id, "web", "w", "main"); err != nil {
		t.Fatal(err)
	}

	// rig deleted ⇒ not in config.
	deleted := func(string) bool { return false }
	res, err := admitRigCreate(idx, store, fixedCursor("8"), deleted, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitReclone || res.entry == nil || res.entry.beadID != id {
		t.Fatalf("deleted-rig succeeded outcome = %d entry=%+v, want rigAdmitReclone reusing %s", res.outcome, res.entry, id)
	}

	// Contrast: while the rig still exists, the same record replays 200-exists.
	// Use a fresh store/record — the reclone above already reset the first record
	// to in_flight.
	store2 := beads.NewMemStore()
	id2, err := createIdemRecord(store2, "c1", "req-del-00001", digest, "3", "web", idemStateSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := markIdemSucceeded(store2, id2, "web", "w", "main"); err != nil {
		t.Fatal(err)
	}
	idx2 := newRigIdemIndex()
	present := func(string) bool { return true }
	res2, err := admitRigCreate(idx2, store2, fixedCursor("8"), present, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit (present): %v", err)
	}
	if res2.outcome != rigAdmitExisting {
		t.Fatalf("present-rig outcome = %d, want rigAdmitExisting", res2.outcome)
	}
}

// TestDurableRigNameScanIgnoresDeletedSucceeded proves the name-axis backstop
// stops blocking a name whose only durable record is succeeded-but-deleted.
func TestDurableRigNameScanIgnoresDeletedSucceeded(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := createIdemRecord(store, "c1", "req-scan-del", "d", "0", "web", idemStateSucceeded); err != nil {
		t.Fatal(err)
	}
	// Present in config ⇒ blocks.
	hit, err := durableRigNameScan(store, "c1", "web", func(string) bool { return true })
	if err != nil || !hit {
		t.Fatalf("scan (present) = (%v,%v), want (true,nil)", hit, err)
	}
	// Absent from config ⇒ name is free.
	hit, err = durableRigNameScan(store, "c1", "web", func(string) bool { return false })
	if err != nil || hit {
		t.Fatalf("scan (deleted) = (%v,%v), want (false,nil)", hit, err)
	}
	// nil predicate preserves the pre-fix blocking behavior.
	hit, err = durableRigNameScan(store, "c1", "web", nil)
	if err != nil || !hit {
		t.Fatalf("scan (nil predicate) = (%v,%v), want (true,nil)", hit, err)
	}
}

// TestLookupIdemRecordSelfHealsDuplicates proves fix #3's read-path self-heal: a
// (city, request_id) that resolved to two durable records is healed to one — the
// survivor is returned and the duplicate is neutralized out of future lookups —
// rather than erroring forever with a 500.
func TestLookupIdemRecordSelfHealsDuplicates(t *testing.T) {
	store := beads.NewMemStore()
	// Two durable records for the SAME (city, request_id) — the double-record
	// poison a pre-serialization race would leave.
	if _, err := createIdemRecord(store, "c1", "req-dup-00001", "d", "1", "web", idemStateInFlight); err != nil {
		t.Fatal(err)
	}
	if _, err := createIdemRecord(store, "c1", "req-dup-00001", "d", "1", "web", idemStateInFlight); err != nil {
		t.Fatal(err)
	}

	rec, err := lookupIdemRecord(store, "c1", "req-dup-00001")
	if err != nil {
		t.Fatalf("lookup healed = %v, want nil (self-heal, not 500)", err)
	}
	if rec == nil {
		t.Fatal("self-heal returned nil, want the surviving record")
	}

	// A follow-up lookup now resolves to exactly one record (the duplicate was
	// neutralized out of the rig-create kind).
	rec2, err := lookupIdemRecord(store, "c1", "req-dup-00001")
	if err != nil || rec2 == nil {
		t.Fatalf("second lookup = (%v,%v), want the single survivor", rec2, err)
	}
	// Exactly one record still carries the rig-create kind; the other was
	// re-kinded to the duplicate marker.
	all, _ := store.List(beads.ListQuery{
		Metadata:      map[string]string{metaIdemCity: "c1", metaIdemRequestID: "req-dup-00001"},
		IncludeClosed: true,
	})
	live := 0
	for _, b := range all {
		if b.Metadata[metaIdemKind] == idemKindRigCreate {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("rig-create-kind records for (city,req) = %d, want exactly 1 after heal", live)
	}
}

// TestWithRigRequestIDLockSerializes proves the request_id axis is mutually
// exclusive per (city, request_id), and that an empty request_id runs fn
// directly (no serialization needed for a unique synthetic id).
func TestWithRigRequestIDLockSerializes(t *testing.T) {
	const city = "/city"
	var counter int
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = withRigRequestIDLock(context.Background(), city, "req-shared-01", func() error {
				n := counter
				counter = n + 1
				return nil
			})
		}()
	}
	wg.Wait()
	if counter != 20 {
		t.Fatalf("serialized counter = %d, want 20 (lost updates ⇒ not mutually exclusive)", counter)
	}

	// Empty request_id is a direct pass-through (not a serialized bypass bug: an
	// absent id mints a unique synthetic id and reserves no durable record).
	ran := false
	if err := withRigRequestIDLock(context.Background(), city, "  ", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("empty request_id lock = %v, want nil", err)
	}
	if !ran {
		t.Fatal("empty request_id did not run fn")
	}

	// No leak after all waiters release.
	rigRequestIDLockSet.mu.Lock()
	_, present := rigRequestIDLockSet.locks[city+"\x00req-shared-01"]
	rigRequestIDLockSet.mu.Unlock()
	if present {
		t.Fatal("request_id lock entry leaked after all waiters released")
	}
}

// TestRigCreateAsyncSameRequestIDDifferentNameSerialized proves fix #3
// end-to-end: concurrent POSTs sharing a request_id but naming DIFFERENT rigs
// (which take different name locks) are serialized on the request_id axis, so
// exactly ONE durable record is reserved for (city, request_id) — no
// double-record 500-poison — and the losers get a request_id_conflict 409.
func TestRigCreateAsyncSameRequestIDDifferentNameSerialized(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cityBeadStore = beads.NewMemStore()
	// Hold every spawned provision in flight so live entries persist through the
	// race window.
	release := make(chan struct{})
	state.provisionGate = release
	h := newTestCityHandler(t, state)

	const req = "req-samerace-01"
	const n = 5
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"name":"race%d","git_url":"https://example.com/r.git","request_id":%q}`, i, req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, newPostRequest(cityURL(state, "/rigs"), strings.NewReader(body)))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	close(release)

	// Exactly one durable record for (city, request_id) — the poison is a second
	// record, which would make every later lookupIdemRecord a 500.
	city := filepath.Clean(state.CityPath())
	recs, err := state.cityBeadStore.List(beads.ListQuery{
		Metadata:      map[string]string{metaIdemKind: idemKindRigCreate, metaIdemCity: city, metaIdemRequestID: req},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatalf("list durable records: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("durable records for (city,req) = %d, want exactly 1 (double-record poison)", len(recs))
	}

	// Exactly one admission won (202); the rest are request_id_conflict 409s
	// (names differ ⇒ digests differ), none a 500.
	n202, n409, nOther := 0, 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusAccepted:
			n202++
		case http.StatusConflict:
			n409++
		default:
			nOther++
		}
	}
	if n202 != 1 || n409 != n-1 || nOther != 0 {
		t.Fatalf("codes = %v; want exactly one 202 and %d 409s, no others", codes, n-1)
	}

	// And a fresh lookup does not 500 (no poison).
	if _, err := lookupIdemRecord(state.cityBeadStore, city, req); err != nil {
		t.Fatalf("post-race lookupIdemRecord = %v, want nil (unpoisoned)", err)
	}
}

// TestRigIdemRecloneRefusesForeignSameName proves the reviewed blocker fix: a
// re-clone re-checks the name axis before re-registering byName and pre-dropping
// the prior manifest. After this request_id's attempt rolled back, a DIFFERENT
// actor may have taken the same rig name; the re-clone must return a rig_name
// conflict instead of overwriting byName and tearing down that actor's live
// working tree. The final sub-case is the negative control: a genuinely-free
// name still re-clones (the request's own record must not self-block).
func TestRigIdemRecloneRefusesForeignSameName(t *testing.T) {
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-A-000001"}
	digest, _ := rigCreateDigest(body)

	t.Run("foreign live byName blocks reclone", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		if _, err := createIdemRecord(store, "c1", "req-A-000001", digest, "3", "web", idemStateRolledBack); err != nil {
			t.Fatal(err)
		}
		// A second actor is mid-provision under the same name.
		foreign := &liveProvision{requestID: "req-B-000002", rigName: "web", eventCursor: "7", done: make(chan struct{})}
		idx.register("c1", foreign)

		res, err := admitRigCreate(idx, store, fixedCursor("9"), nil, nil, "c1", body)
		var conflict *rigNameConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v (outcome %d), want *rigNameConflictError", err, res.outcome)
		}
		if conflict.InFlightRequestID != "req-B-000002" {
			t.Fatalf("conflict.InFlightRequestID = %q, want req-B-000002", conflict.InFlightRequestID)
		}
		if res.entry != nil {
			t.Fatalf("res.entry = %+v, want nil (no fresh registration on conflict)", res.entry)
		}
		if !res.recloneManifest.IsEmpty() {
			t.Fatalf("res.recloneManifest = %+v, want empty (no teardown of the foreign rig)", res.recloneManifest)
		}
		// The foreign live entry must be untouched by the refused admission.
		if live, ok := idx.lookupByName("c1", "web"); !ok || live != foreign {
			t.Fatalf("foreign byName entry disturbed: ok=%v live=%+v", ok, live)
		}
	})

	t.Run("foreign live in config blocks reclone", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		if _, err := createIdemRecord(store, "c1", "req-A-000001", digest, "3", "web", idemStateRolledBack); err != nil {
			t.Fatal(err)
		}
		inConfig := func(string) bool { return true }
		res, err := admitRigCreate(idx, store, fixedCursor("9"), inConfig, nil, "c1", body)
		var conflict *rigNameConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %v (outcome %d), want *rigNameConflictError", err, res.outcome)
		}
		if res.entry != nil {
			t.Fatalf("res.entry = %+v, want nil", res.entry)
		}
	})

	t.Run("own free name still reclones", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		id, err := createIdemRecord(store, "c1", "req-A-000001", digest, "3", "web", idemStateRolledBack)
		if err != nil {
			t.Fatal(err)
		}
		free := func(string) bool { return false }
		res, err := admitRigCreate(idx, store, fixedCursor("9"), free, nil, "c1", body)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if res.outcome != rigAdmitReclone || res.entry == nil || res.entry.beadID != id {
			t.Fatalf("free-name outcome = %d entry=%+v, want rigAdmitReclone reusing %s", res.outcome, res.entry, id)
		}
	})
}
