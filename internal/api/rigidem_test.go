package api

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fixedCursor returns a cursor func that always yields v. A distinct value per
// call site lets a test prove which cursor an admission echoed.
func fixedCursor(v string) func() (string, error) {
	return func() (string, error) { return v, nil }
}

func TestRigIdemDigestGolden(t *testing.T) {
	// The digest is computed over json.Marshal(body with RequestID zeroed).
	// These literals pin field order + tags: any reorder/omitempty flip breaks
	// the build, which is deliberate (a silent digest change turns every
	// in-flight retry across a deploy into a spurious 409 body-mismatch).
	cases := []struct {
		name string
		body RigCreateBody
		json string
		hex  string
	}{
		{
			name: "full body, request_id ignored",
			body: RigCreateBody{
				Name: "web", Path: "/srv/web", Prefix: "w",
				DefaultBranch: "main", GitURL: "https://example.com/web.git",
				RequestID: "req-ignored-in-digest",
			},
			json: `{"name":"web","path":"/srv/web","prefix":"w","default_branch":"main","git_url":"https://example.com/web.git"}`,
			hex:  "579422dca414bfa0cd79e0c1e97f270bd1a68159d6d40dafed35a1c5e8d0af1b",
		},
		{
			name: "minimal body",
			body: RigCreateBody{Name: "api", Path: "/srv/api"},
			json: `{"name":"api","path":"/srv/api"}`,
			hex:  "8908428e9370d6fd02f9c9e9cd78ad01c881ccce238b7695b1495eae843af6ed",
		},
		{
			name: "unicode + percent-encoded name",
			body: RigCreateBody{Name: "café-%20", Path: "/srv/x"},
			json: `{"name":"café-%20","path":"/srv/x"}`,
			hex:  "008b323369ff2e3441c6c45580967989e141b989a93a515b7766d59af3b610b9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zeroed := tc.body
			zeroed.RequestID = ""
			raw, err := json.Marshal(zeroed)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.json {
				t.Fatalf("json encoding drifted:\n got %s\nwant %s", raw, tc.json)
			}
			got, err := rigCreateDigest(tc.body)
			if err != nil {
				t.Fatalf("rigCreateDigest: %v", err)
			}
			if got != tc.hex {
				t.Fatalf("digest drifted:\n got %s\nwant %s", got, tc.hex)
			}
		})
	}
}

func TestRigIdemDigestDeterministic(t *testing.T) {
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x"}
	d1, err := rigCreateDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := rigCreateDigest(body)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("digest not deterministic: %s vs %s", d1, d2)
	}

	// request_id must not affect the digest.
	a := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-aaaa1111"}
	b := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-bbbb2222"}
	da, _ := rigCreateDigest(a)
	db, _ := rigCreateDigest(b)
	if da != db {
		t.Fatalf("request_id leaked into digest: %s vs %s", da, db)
	}

	// The zeroed input is never observed by the caller (copy-by-value).
	orig := "req-keepme01"
	c := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: orig}
	if _, err := rigCreateDigest(c); err != nil {
		t.Fatal(err)
	}
	if c.RequestID != orig {
		t.Fatalf("rigCreateDigest mutated its argument: %q", c.RequestID)
	}
}

// TestRigIdemDigestIgnoresGitURLCredential proves the digest binds a request_id
// to the logical repository, not the embedded credential: the original
// credential-bearing URL, the CLI's redacted retry recipe, and a rotated-token
// retry all digest identically, so a same-request_id retry replays cleanly
// instead of surfacing a spurious request_id conflict. A different repository
// still digests differently.
func TestRigIdemDigestIgnoresGitURLCredential(t *testing.T) {
	base := RigCreateBody{Name: "web", Path: "rigs/web", RequestID: "req-cred0001"}
	variant := func(gitURL string) string {
		b := base
		b.GitURL = gitURL
		d, err := rigCreateDigest(b)
		if err != nil {
			t.Fatalf("rigCreateDigest(%q): %v", gitURL, err)
		}
		return d
	}
	original := variant("https://alice:s3cr3t-tok@github.com/o/r.git")
	redacted := variant("https://***@github.com/o/r.git") // gitcred.RedactUserinfo form
	rotated := variant("https://alice:new-tok-99@github.com/o/r.git")
	anon := variant("https://github.com/o/r.git")
	for name, d := range map[string]string{"redacted": redacted, "rotated": rotated, "anon": anon} {
		if d != original {
			t.Errorf("%s retry digest %s != original %s (credential leaked into digest)", name, d, original)
		}
	}
	// A genuinely different repository must not collide.
	if other := variant("https://alice:s3cr3t-tok@github.com/o/OTHER.git"); other == original {
		t.Errorf("different repository digested identically: %s", other)
	}
}

func TestRigIdemValidateRequestID(t *testing.T) {
	accept := []string{
		"550e8400-e29b-41d4-a716-446655440000", // UUIDv4
		"req-0a1b2c3d4e5f6a7b",                 // synthetic-shaped
		"0a1b2c3d",                             // hex, invalid JSON (leading 0 then letter)
		"deadbeef",                             // hex letters
		"caf~babe.01:07",                       // full charset sample
	}
	for _, id := range accept {
		if err := validateRequestID(id); err != nil {
			t.Errorf("validateRequestID(%q) = %v, want nil", id, err)
		}
	}

	reject := []string{
		"",                       // empty
		"short7",                 // 6 chars < 8
		"12345678",               // pure numeric — the bd type-inference foot-gun (json.Valid)
		"1234567890",             // longer numeric
		"-1234567",               // negative number, valid JSON
		"1.234567",               // float, valid JSON
		"true",                   // boolean (also < 8, but must reject)
		"false",                  // boolean
		"null",                   // null literal
		"1e5",                    // exponent (< 8; must reject)
		"bad space1",             // whitespace outside charset
		"bad\ttab1",              // control char outside charset
		strings.Repeat("a", 201), // > 200 chars
	}
	for _, id := range reject {
		if err := validateRequestID(id); !errors.Is(err, errInvalidRequestID) {
			t.Errorf("validateRequestID(%q) = %v, want errInvalidRequestID", id, err)
		}
	}
}

func TestRigIdemValidateRigName(t *testing.T) {
	for _, name := range []string{"web", "api-2", "api_v2"} {
		if err := validateRigName(name); err != nil {
			t.Errorf("validateRigName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "123", "true", "null", "42", "café-%20"} {
		if err := validateRigName(name); !errors.Is(err, errInvalidRigName) {
			t.Errorf("validateRigName(%q) = %v, want errInvalidRigName", name, err)
		}
	}
}

func TestRigIdemRecordClosedAtCreateNeverReady(t *testing.T) {
	store := beads.NewMemStore()
	id, err := createIdemRecord(store, "c1", "req-abc12345", "digestval", "5", "web", idemStateInFlight)
	if err != nil {
		t.Fatalf("createIdemRecord: %v", err)
	}

	// An open "task" bead would be Ready-eligible actionable work; the record
	// must be closed at birth so the dispatcher never claims it.
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range ready {
		if b.ID == id {
			t.Fatalf("idem record %s is Ready-eligible", id)
		}
	}

	// It is absent from an open (non-IncludeClosed) list...
	open, err := store.List(beads.ListQuery{Metadata: map[string]string{metaIdemKind: idemKindRigCreate}})
	if err != nil {
		t.Fatalf("open List: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("closed record leaked into open list: %d beads", len(open))
	}

	// ...but the machine lookup finds it via IncludeClosed.
	rec, err := lookupIdemRecord(store, "c1", "req-abc12345")
	if err != nil {
		t.Fatalf("lookupIdemRecord: %v", err)
	}
	if rec == nil {
		t.Fatal("lookupIdemRecord returned nil for a closed record")
	}
	if rec.Status != "closed" {
		t.Fatalf("record status = %q, want closed", rec.Status)
	}
	if rec.Metadata[metaIdemState] != idemStateInFlight {
		t.Fatalf("record state = %q, want in_flight", rec.Metadata[metaIdemState])
	}
}

func TestRigIdemPointerIdentityRemove(t *testing.T) {
	idx := newRigIdemIndex()
	a := &liveProvision{requestID: "req-xxxxxxxx", rigName: "foo", done: make(chan struct{})}
	idx.register("c1", a)
	idx.remove("c1", a) // A's terminal step

	// A re-clone reuses the same keys with a distinct successor pointer.
	b := &liveProvision{requestID: "req-xxxxxxxx", rigName: "foo", done: make(chan struct{})}
	idx.register("c1", b)

	// A late/duplicate terminal for A must not evict B, and must not panic on
	// a second close of A's done channel.
	idx.remove("c1", a)

	if got, ok := idx.lookup("c1", "req-xxxxxxxx"); !ok || got != b {
		t.Fatalf("inflight successor evicted by stale remove: got=%v ok=%v", got, ok)
	}
	if got, ok := idx.lookupByName("c1", "foo"); !ok || got != b {
		t.Fatalf("byName successor evicted by stale remove: got=%v ok=%v", got, ok)
	}
	select {
	case <-a.done:
	default:
		t.Fatal("a.done was not closed by its terminal remove")
	}
	select {
	case <-b.done:
		t.Fatal("b.done was closed by a stale remove of a")
	default:
	}
}

func TestRigIdemAdmitNew(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-new-00001"}

	res, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitNew {
		t.Fatalf("outcome = %d, want rigAdmitNew", res.outcome)
	}
	if res.requestID != "req-new-00001" || res.eventCursor != "7" {
		t.Fatalf("res = %+v, want requestID=req-new-00001 cursor=7", res)
	}
	if res.entry == nil || res.entry.synthetic || res.entry.beadID == "" {
		t.Fatalf("entry = %+v, want non-synthetic with a durable beadID", res.entry)
	}

	rec, err := lookupIdemRecord(store, "c1", "req-new-00001")
	if err != nil || rec == nil {
		t.Fatalf("lookupIdemRecord: rec=%v err=%v", rec, err)
	}
	if rec.ID != res.entry.beadID {
		t.Fatalf("entry.beadID %q != record ID %q", res.entry.beadID, rec.ID)
	}
	if rec.Metadata[metaIdemState] != idemStateInFlight {
		t.Fatalf("record state = %q, want in_flight", rec.Metadata[metaIdemState])
	}
	if rec.Metadata[metaIdemDigest] != res.entry.digest {
		t.Fatalf("record digest %q != entry digest %q", rec.Metadata[metaIdemDigest], res.entry.digest)
	}
	if rec.Metadata[metaIdemEventCursor] != "7" || rec.Metadata[metaIdemRigName] != "web" {
		t.Fatalf("record metadata mismatch: %+v", rec.Metadata)
	}

	live, ok := idx.lookup("c1", "req-new-00001")
	if !ok || live != res.entry {
		t.Fatalf("live entry not registered: ok=%v live=%v", ok, live)
	}
	if byName, ok := idx.lookupByName("c1", "web"); !ok || byName != res.entry {
		t.Fatalf("byName entry not registered: ok=%v entry=%v", ok, byName)
	}
}

func TestRigIdemAdmitInflightReplay(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-replay-001"}

	if _, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", body); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	// A second identical POST while the live entry exists replays the ORIGINAL
	// cursor (7), not the cursor this call would capture (9), and spawns nothing.
	res, err := admitRigCreate(idx, store, fixedCursor("9"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("replay admit: %v", err)
	}
	if res.outcome != rigAdmitInflightReplay {
		t.Fatalf("outcome = %d, want rigAdmitInflightReplay", res.outcome)
	}
	if res.eventCursor != "7" {
		t.Fatalf("replay cursor = %q, want the original 7", res.eventCursor)
	}
	if res.entry != nil {
		t.Fatal("replay must not carry a spawn entry")
	}
	if len(idx.inflight) != 1 {
		t.Fatalf("live index has %d entries, want 1 (no double register)", len(idx.inflight))
	}
	recs, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{metaIdemKind: idemKindRigCreate, metaIdemCity: "c1"},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("durable records = %d, want 1", len(recs))
	}
}

func TestRigIdemAdmitExisting(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "api", Path: "/srv/api", RequestID: "req-exist-0001"}
	digest, _ := rigCreateDigest(body)

	// Mirror the G13 §8 sync-201 shape: reserve the record already succeeded,
	// then merge the result fields.
	id, err := createIdemRecord(store, "c1", "req-exist-0001", digest, "3", "api", idemStateSucceeded)
	if err != nil {
		t.Fatal(err)
	}
	if err := markIdemSucceeded(store, id, "api", "ap", "main"); err != nil {
		t.Fatal(err)
	}

	res, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitExisting {
		t.Fatalf("outcome = %d, want rigAdmitExisting", res.outcome)
	}
	if res.record == nil {
		t.Fatal("existing outcome must carry the durable record")
	}
	if got := res.record.Metadata[metaIdemResultRig]; got != "api" {
		t.Fatalf("result.rig = %q, want api", got)
	}
	if got := res.record.Metadata[metaIdemResultPrefix]; got != "ap" {
		t.Fatalf("result.prefix = %q, want ap", got)
	}
	if got := res.record.Metadata[metaIdemResultBranch]; got != "main" {
		t.Fatalf("result.branch = %q, want main", got)
	}
	if res.entry != nil {
		t.Fatal("existing outcome must not register a live entry")
	}
	if _, ok := idx.lookup("c1", "req-exist-0001"); ok {
		t.Fatal("existing outcome must not touch the live index")
	}
}

func TestRigIdemAdmitReclone(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-reclone-01"}
	digest, _ := rigCreateDigest(body)

	id, err := createIdemRecord(store, "c1", "req-reclone-01", digest, "3", "web", idemStateInFlight)
	if err != nil {
		t.Fatal(err)
	}
	if err := markIdemRolledBack(store, id); err != nil {
		t.Fatal(err)
	}

	res, err := admitRigCreate(idx, store, fixedCursor("8"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitReclone {
		t.Fatalf("outcome = %d, want rigAdmitReclone", res.outcome)
	}
	if res.entry == nil || res.entry.beadID != id {
		t.Fatalf("re-clone must reuse the durable record %q, entry=%+v", id, res.entry)
	}
	if res.eventCursor != "8" {
		t.Fatalf("re-clone cursor = %q, want the fresh 8", res.eventCursor)
	}
	rec, _ := lookupIdemRecord(store, "c1", "req-reclone-01")
	if rec.Metadata[metaIdemState] != idemStateInFlight {
		t.Fatalf("record not reset to in_flight: %q", rec.Metadata[metaIdemState])
	}
	if rec.Metadata[metaIdemEventCursor] != "8" {
		t.Fatalf("record cursor not refreshed: %q", rec.Metadata[metaIdemEventCursor])
	}
	if live, ok := idx.lookup("c1", "req-reclone-01"); !ok || live != res.entry {
		t.Fatal("re-clone did not register a fresh live entry")
	}
}

func TestRigIdemAdmitOrphanNoReplay(t *testing.T) {
	// A durable in_flight record with NO live entry is an orphan (a crash
	// survivor or a lost goroutine). A same-id retry must re-clone, never hang
	// on a passive in-flight replay.
	store := beads.NewMemStore()
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-orphan-01"}
	digest, _ := rigCreateDigest(body)

	id, err := createIdemRecord(store, "c1", "req-orphan-01", digest, "3", "web", idemStateInFlight)
	if err != nil {
		t.Fatal(err)
	}

	res, err := admitRigCreate(idx, store, fixedCursor("9"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitReclone {
		t.Fatalf("orphan outcome = %d, want rigAdmitReclone (not a hung replay)", res.outcome)
	}
	if res.entry == nil || res.entry.beadID != id {
		t.Fatalf("orphan re-clone must reuse record %q, entry=%+v", id, res.entry)
	}
}

func TestRigIdemAdmitRequestIDConflict(t *testing.T) {
	t.Run("live entry, different digest", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		first := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-conf-0001"}
		if _, err := admitRigCreate(idx, store, fixedCursor("1"), nil, nil, "c1", first); err != nil {
			t.Fatal(err)
		}
		// Same id, different body (name differs ⇒ digest differs).
		second := RigCreateBody{Name: "web2", Path: "/srv/web2", RequestID: "req-conf-0001"}
		_, err := admitRigCreate(idx, store, fixedCursor("1"), nil, nil, "c1", second)
		var rc *requestIDConflictError
		if !errors.As(err, &rc) {
			t.Fatalf("err = %v, want *requestIDConflictError", err)
		}
		if rc.RequestID != "req-conf-0001" {
			t.Fatalf("conflict id = %q, want req-conf-0001", rc.RequestID)
		}
	})

	t.Run("durable rolled_back, different digest", func(t *testing.T) {
		// A different-digest reuse is a conflict in EVERY state, including the
		// re-executable rolled_back terminal (G13 §4.3).
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		orig := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-conf-0002"}
		digest, _ := rigCreateDigest(orig)
		id, err := createIdemRecord(store, "c1", "req-conf-0002", digest, "3", "web", idemStateInFlight)
		if err != nil {
			t.Fatal(err)
		}
		if err := markIdemRolledBack(store, id); err != nil {
			t.Fatal(err)
		}
		changed := RigCreateBody{Name: "web", Path: "/srv/DIFFERENT", RequestID: "req-conf-0002"}
		_, err = admitRigCreate(idx, store, fixedCursor("1"), nil, nil, "c1", changed)
		var rc *requestIDConflictError
		if !errors.As(err, &rc) {
			t.Fatalf("err = %v, want *requestIDConflictError", err)
		}
	})
}

func TestRigIdemAdmitRigNameConflict(t *testing.T) {
	t.Run("live byName under a different id", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		a := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-name-aaa1"}
		if _, err := admitRigCreate(idx, store, fixedCursor("5"), nil, nil, "c1", a); err != nil {
			t.Fatal(err)
		}
		b := RigCreateBody{Name: "web", Path: "/srv/other", RequestID: "req-name-bbb2"}
		_, err := admitRigCreate(idx, store, fixedCursor("5"), nil, nil, "c1", b)
		var nc *rigNameConflictError
		if !errors.As(err, &nc) {
			t.Fatalf("err = %v, want *rigNameConflictError", err)
		}
		if nc.Rig != "web" || nc.InFlightRequestID != "req-name-aaa1" || nc.InFlightCursor != "5" {
			t.Fatalf("conflict = %+v, want rig=web inflight=req-name-aaa1 cursor=5", nc)
		}
	})

	t.Run("rig already in config", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		inConfig := func(name string) bool { return name == "web" }
		b := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-cfg-00001"}
		_, err := admitRigCreate(idx, store, fixedCursor("5"), inConfig, nil, "c1", b)
		var nc *rigNameConflictError
		if !errors.As(err, &nc) {
			t.Fatalf("err = %v, want *rigNameConflictError", err)
		}
		if nc.Rig != "web" || nc.InFlightRequestID != "" {
			t.Fatalf("conflict = %+v, want rig=web no in-flight id", nc)
		}
	})

	t.Run("durable rig_name scan hits in_flight under another id", func(t *testing.T) {
		store := beads.NewMemStore()
		idx := newRigIdemIndex()
		// A different id already holds an in_flight durable record for "web"
		// with no live entry (the committed-but-invisible / orphan window).
		otherBody := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-other-0001"}
		otherDigest, _ := rigCreateDigest(otherBody)
		if _, err := createIdemRecord(store, "c1", "req-other-0001", otherDigest, "3", "web", idemStateInFlight); err != nil {
			t.Fatal(err)
		}
		b := RigCreateBody{Name: "web", Path: "/srv/web", RequestID: "req-scan-0001"}
		_, err := admitRigCreate(idx, store, fixedCursor("5"), nil, nil, "c1", b)
		var nc *rigNameConflictError
		if !errors.As(err, &nc) {
			t.Fatalf("err = %v, want *rigNameConflictError", err)
		}
		if nc.Rig != "web" {
			t.Fatalf("conflict rig = %q, want web", nc.Rig)
		}
	})

	t.Run("re-clone must not clobber a live same-name provision under a different id", func(t *testing.T) {
		// Regression for the re-clone-vs-live-byName admission gap: a rolled_back
		// request's retry short-circuits on its request_id axis straight to
		// re-clone, which (before the fix) registered byName without ever
		// consulting the name axis — overwriting a DIFFERENT live same-name
		// provision and tearing down its in-flight working tree via the re-clone
		// pre-drop. Admission must instead 409 rig_name_conflict and leave the
		// live provision untouched.
		store := beads.NewMemStore()
		idx := newRigIdemIndex()

		// req-X previously failed: a rolled_back durable record for "web".
		reqX := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-x-00001"}
		xDigest, _ := rigCreateDigest(reqX)
		xid, err := createIdemRecord(store, "c1", "req-x-00001", xDigest, "3", "web", idemStateInFlight)
		if err != nil {
			t.Fatal(err)
		}
		if err := markIdemRolledBack(store, xid); err != nil {
			t.Fatal(err)
		}

		// A DIFFERENT request req-Y then claims the same name "web" and is live.
		reqY := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-y-00002"}
		yres, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", reqY)
		if err != nil {
			t.Fatal(err)
		}
		if yres.outcome != rigAdmitNew {
			t.Fatalf("req-Y outcome = %d, want rigAdmitNew", yres.outcome)
		}

		// req-X retries: its request_id axis routes to re-clone, but "web" is now
		// held by req-Y. It must 409 at the name axis, pointing at req-Y's stream.
		_, err = admitRigCreate(idx, store, fixedCursor("9"), nil, nil, "c1", reqX)
		var nc *rigNameConflictError
		if !errors.As(err, &nc) {
			t.Fatalf("re-clone-vs-live-byName err = %v, want *rigNameConflictError", err)
		}
		if nc.Rig != "web" || nc.InFlightRequestID != "req-y-00002" || nc.InFlightCursor != "7" {
			t.Fatalf("conflict = %+v, want it to point at live req-Y (cursor 7)", nc)
		}

		// req-Y's live entry must be intact (NOT overwritten by req-X's re-clone).
		if live, ok := idx.lookupByName("c1", "web"); !ok || live != yres.entry {
			t.Fatal("re-clone clobbered req-Y's live byName entry")
		}
		if live, ok := idx.lookup("c1", "req-y-00002"); !ok || live != yres.entry {
			t.Fatal("re-clone clobbered req-Y's live request_id entry")
		}
		// req-X's durable record must remain rolled_back (never reset to in_flight).
		rec, _ := lookupIdemRecord(store, "c1", "req-x-00001")
		if rec == nil || rec.Metadata[metaIdemState] != idemStateRolledBack {
			t.Fatalf("req-X record must stay rolled_back, got %+v", rec)
		}
	})

	t.Run("re-clone must not clobber a committed same-name rig owned by a different request", func(t *testing.T) {
		// Regression for the re-clone-vs-COMMITTED admission gap (F1). The
		// re-clone-vs-live guard only inspects the live byName index; once the rival
		// request has SUCCEEDED it removes its live byName entry, so the live guard
		// no longer sees it, but the rig persists in config. Without the config gate
		// a rolled_back request's retry would drive the re-clone pre-drop's
		// os.RemoveAll over the committed rig's working tree + .beads store.
		// Admission must instead 409 rig_name_conflict and never reach re-clone.
		store := beads.NewMemStore()
		idx := newRigIdemIndex()

		// req-X previously failed: a rolled_back durable record for "web" with NO
		// live entry. A rolled_back provision never committed its own name to config.
		reqX := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-x-commit01"}
		xDigest, _ := rigCreateDigest(reqX)
		xid, err := createIdemRecord(store, "c1", "req-x-commit01", xDigest, "3", "web", idemStateInFlight)
		if err != nil {
			t.Fatal(err)
		}
		if err := markIdemRolledBack(store, xid); err != nil {
			t.Fatal(err)
		}

		// A DIFFERENT request req-Y has since COMMITTED "web" to config: it succeeded
		// and removed its live byName entry, so the live index is empty for "web" but
		// rigInConfig("web") is true.
		inConfig := func(name string) bool { return name == "web" }

		// req-X retries: its request_id axis routes to re-clone, but "web" is now a
		// committed rig owned by req-Y. It must 409 rig_name_conflict.
		_, err = admitRigCreate(idx, store, fixedCursor("9"), inConfig, nil, "c1", reqX)
		var nc *rigNameConflictError
		if !errors.As(err, &nc) {
			t.Fatalf("re-clone-vs-committed err = %v, want *rigNameConflictError", err)
		}
		if nc.Rig != "web" {
			t.Fatalf("conflict rig = %q, want web", nc.Rig)
		}

		// The rejected re-clone must have zero side effects: req-X's record stays
		// rolled_back (never reset to in_flight) and no live entry was registered.
		rec, _ := lookupIdemRecord(store, "c1", "req-x-commit01")
		if rec == nil || rec.Metadata[metaIdemState] != idemStateRolledBack {
			t.Fatalf("req-X record must stay rolled_back after a rejected re-clone, got %+v", rec)
		}
		if live, ok := idx.lookup("c1", "req-x-commit01"); ok {
			t.Fatalf("rejected re-clone must not register a live request_id entry, got %+v", live)
		}
		if _, ok := idx.lookupByName("c1", "web"); ok {
			t.Fatal("rejected re-clone must not register a byName entry")
		}
	})
}

func TestRigIdemAdmitAbsentRequestID(t *testing.T) {
	store := beads.NewMemStore()
	idx := newRigIdemIndex()

	// Absent id ⇒ synthetic correlation id, no durable record, but a byName
	// marker so the name axis still protects the async provision.
	res, err := admitRigCreate(idx, store, fixedCursor("2"), nil, nil, "c2", RigCreateBody{Name: "web", Path: "/srv/web"})
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if res.outcome != rigAdmitNew {
		t.Fatalf("outcome = %d, want rigAdmitNew", res.outcome)
	}
	if !strings.HasPrefix(res.requestID, "req-") {
		t.Fatalf("synthetic id = %q, want a req- prefix", res.requestID)
	}
	if res.entry == nil || !res.entry.synthetic || res.entry.beadID != "" {
		t.Fatalf("entry = %+v, want synthetic with no durable beadID", res.entry)
	}
	recs, err := store.List(beads.ListQuery{Metadata: map[string]string{metaIdemKind: idemKindRigCreate}, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 0 {
		t.Fatalf("absent-id admit created %d durable records, want 0", len(recs))
	}
	if _, ok := idx.lookupByName("c2", "web"); !ok {
		t.Fatal("absent-id admit did not register a byName marker")
	}

	// A second absent-id add for the SAME name hits the name axis.
	_, err = admitRigCreate(idx, store, fixedCursor("2"), nil, nil, "c2", RigCreateBody{Name: "web", Path: "/srv/web2"})
	var nc *rigNameConflictError
	if !errors.As(err, &nc) {
		t.Fatalf("second same-name absent-id add err = %v, want *rigNameConflictError", err)
	}

	// A different name proceeds.
	res3, err := admitRigCreate(idx, store, fixedCursor("2"), nil, nil, "c2", RigCreateBody{Name: "other", Path: "/srv/other"})
	if err != nil {
		t.Fatalf("different-name admit: %v", err)
	}
	if res3.outcome != rigAdmitNew {
		t.Fatalf("different-name outcome = %d, want rigAdmitNew", res3.outcome)
	}
}

// laggingStore models a remote store's cross-connection read-after-write
// lag: while lagging, List returns nothing, so a just-Created row is invisible
// to a lookup on another connection. Create/Tx/SetMetadataBatch delegate to the
// embedded store, so the row IS written — just not yet visible via List.
type laggingStore struct {
	beads.Store
	mu      sync.Mutex
	lagging bool
}

func (l *laggingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	l.mu.Lock()
	lag := l.lagging
	l.mu.Unlock()
	if lag {
		return nil, nil
	}
	return l.Store.List(q)
}

func TestRigIdemAdmitLedgerLagDoubleClone(t *testing.T) {
	// The critical regression guard: two identical retries within the lag
	// window must yield exactly ONE provision. If admission consulted the
	// durable store first, both would List→miss (lag) and both Create → double
	// clone. Consulting the live index first makes the second a replay. The two
	// admissions are issued sequentially here, exactly as the per-rig-name lock
	// serializes them in production.
	mem := beads.NewMemStore()
	store := &laggingStore{Store: mem, lagging: true}
	idx := newRigIdemIndex()
	body := RigCreateBody{Name: "web", Path: "/srv/web", GitURL: "g://x", RequestID: "req-web-0001"}

	r1, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if r1.outcome != rigAdmitNew {
		t.Fatalf("first outcome = %d, want rigAdmitNew", r1.outcome)
	}

	r2, err := admitRigCreate(idx, store, fixedCursor("7"), nil, nil, "c1", body)
	if err != nil {
		t.Fatalf("second admit: %v", err)
	}
	if r2.outcome != rigAdmitInflightReplay {
		t.Fatalf("second outcome = %d, want rigAdmitInflightReplay (index defeats lag)", r2.outcome)
	}
	if r2.entry != nil {
		t.Fatal("replay must not spawn a second provision")
	}

	if len(idx.inflight) != 1 {
		t.Fatalf("live index has %d provisions, want exactly 1", len(idx.inflight))
	}
	// Exactly one durable record was reserved (read the underlying store
	// directly, bypassing the lag).
	recs, err := mem.List(beads.ListQuery{
		Metadata:      map[string]string{metaIdemKind: idemKindRigCreate, metaIdemCity: "c1"},
		IncludeClosed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("durable records = %d, want exactly 1 (no double clone)", len(recs))
	}
}
