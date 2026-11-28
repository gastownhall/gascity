package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestManagedStartAdmissionCoversNormalAndRuntimeResume(t *testing.T) {
	for _, runtimeOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "runtime-only"}[runtimeOnly], func(t *testing.T) {
			mgr, sp, store, id, name := seedResumableACPSession(t, func(string) error { return nil })
			mgr.cityPath = admissionTestCity(t)
			if err := store.SetMetadataBatch(id, map[string]string{"template": "worker", "continuation_reset_pending": "true", "continuation_epoch": "7"}); err != nil {
				t.Fatal(err)
			}
			var err error
			if runtimeOnly {
				by := runtimeOnlyActor(t, mgr, id)
				inc := Incarnation{ID: id, Generation: 1, ContinuationEpoch: 7, token: "tok", minted: true}
				err = mgr.StartRuntimeOnly(context.Background(), by, id, inc, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			} else {
				err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
			}
			if err == nil || !runtime.IsProviderCapacity(err) {
				t.Fatalf("managed resume without evidence: %v", err)
			}
			if got := startCallCount(sp); got != 0 {
				t.Fatalf("provider started %d times without complete census", got)
			}
			b, e := store.Get(id)
			if e != nil {
				t.Fatal(e)
			}
			if b.Metadata["session_key"] != capacityTestSessionKey || b.Metadata["started_config_hash"] != capacityTestConfigHash || b.Metadata[PrimedAtMetadataKey] != capacityTestPrimedAt || b.Metadata["continuation_reset_pending"] != "true" || b.Metadata["continuation_epoch"] != "7" {
				t.Fatalf("refusal changed resume identity: %v", b.Metadata)
			}
			found := false
			for _, n := range sp.unrouted {
				if n == name {
					found = true
				}
			}
			if !found {
				t.Fatal("ACP route was not released")
			}
		})
	}
}

func TestManagedStartAdmissionCoversDirectCreate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(admissionTestCity(t)))
	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
	if err == nil || !runtime.IsProviderCapacity(err) {
		t.Fatalf("managed create without evidence: %v", err)
	}
	for _, c := range sp.Calls {
		if c.Method == "Start" {
			t.Fatal("create bypassed admission")
		}
	}
}

// A complete reservation must admit one actual provider start and retain its
// concrete SID claim for every canonical manager entry point.
func TestManagedStartAdmissionSpendsGrantAtActualBoundary(t *testing.T) {
	for _, mode := range []string{"create", "normal", "runtime-only"} {
		t.Run(mode, func(t *testing.T) {
			city := admissionTestCity(t)
			now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
			ledgerPath := writeAdmissionGrant(t, city, now)
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(city), WithClock(&clock.Fake{Time: now}))
			var id string
			if mode == "create" {
				info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
				if err != nil {
					t.Fatal(err)
				}
				id = info.ID
			} else {
				b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateSuspended), "template": "worker", "provider": "fake", "work_dir": t.TempDir(), "command": "fake-command"}})
				if err != nil {
					t.Fatal(err)
				}
				id = b.ID
				if mode == "normal" {
					err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, "fake-command", runtime.Config{})
				} else {
					by := runtimeOnlyActor(t, mgr, id)
					inc := mintUnder(t, mgr, by.Lease, id)
					err = mgr.StartRuntimeOnly(context.Background(), by, id, inc, "fake-command", runtime.Config{})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			starts := 0
			for _, c := range sp.Calls {
				if c.Method == "Start" {
					starts++
				}
			}
			if starts != 1 {
				t.Fatalf("provider starts=%d", starts)
			}
			data, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			var claimed struct {
				Reservations map[string]struct {
					StartClaims map[string]string `json:"start_claims"`
				} `json:"reservations"`
			}
			if err = json.Unmarshal(data, &claimed); err != nil {
				t.Fatal(err)
			}
			if len(claimed.Reservations["worker"].StartClaims) != 1 || claimed.Reservations["worker"].StartClaims[id] != now.Format(time.RFC3339) {
				t.Fatalf("missing actual SID grant: %s", data)
			}
		})
	}
}

func TestManagedAdmissionRefusalPrecedesOrphanCleanup(t *testing.T) {
	for _, mode := range []string{"create", "normal", "runtime-only"} {
		t.Run(mode, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := &orphanScanProvider{Fake: runtime.NewFake(), results: []runtime.LiveRuntime{{PID: 4321, City: admissionTestCity(t)}}}
			mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(admissionTestCity(t)))
			var err error
			if mode == "create" {
				_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
			} else {
				b, e := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateSuspended), "template": "worker", "provider": "fake", "work_dir": t.TempDir(), "command": "fake-command", "continuation_reset_pending": "true", "continuation_epoch": "7", "session_key": "prior-key", "started_config_hash": "prior-hash"}})
				if e != nil {
					t.Fatal(e)
				}
				if mode == "normal" {
					err = mgr.Start(context.Background(), testActor(mgr, ActorOperator), b.ID, "fake-command", runtime.Config{})
				} else {
					by := runtimeOnlyActor(t, mgr, b.ID)
					inc := Incarnation{ID: b.ID, Generation: 1, ContinuationEpoch: 7, token: "tok", minted: true}
					err = mgr.StartRuntimeOnly(context.Background(), by, b.ID, inc, "fake-command", runtime.Config{})
				}
				after, e := store.Get(b.ID)
				if e != nil {
					t.Fatal(e)
				}
				for _, key := range []string{"continuation_reset_pending", "continuation_epoch", "session_key", "started_config_hash"} {
					if after.Metadata[key] != b.Metadata[key] {
						t.Fatalf("denial mutated %s: %v", key, after.Metadata)
					}
				}
			}
			if !runtime.IsProviderCapacity(err) {
				t.Fatalf("denial=%v", err)
			}
			if len(sp.events) != 0 {
				t.Fatalf("denied start touched orphan/provider lifecycle: %v", sp.events)
			}
		})
	}
}

func writeAdmissionGrant(t *testing.T, city string, now time.Time) string {
	t.Helper()
	capacity := map[string]any{"managed_active_count": 0, "managed_worker_cap": 2, "over_cap_by": 0, "active": []any{}}
	snapshot := map[string]any{"schema_version": 5, "generated_at": now.Format(time.RFC3339), "zone": "green", "hysteresis": map[string]any{"green_streak": 3, "cooldown_until": nil}, "capacity": capacity, "live_census": map[string]any{"contract": "managed-live-census/v1", "complete": true, "count": 0, "observed_at": now.Format(time.RFC3339), "evidence_at": now.Format(time.RFC3339), "active": []any{}}}
	ledger := map[string]any{"schema": 1, "reservations": map[string]any{"worker": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)}}}
	ledgerPath := filepath.Join(city, ".gc/runtime/pool-capacity-admission/reservations.json")
	for path, value := range map[string]any{filepath.Join(city, ".gc/runtime/capacity-scheduler-v2/snapshot.json"): snapshot, ledgerPath: ledger} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return ledgerPath
}

func TestDedicatedClerkUsesOnlyExactAliasReservationAtNativeStart(t *testing.T) {
	alias := "mail-0123456789abcdef01234567-crm"
	key := "clerk@" + alias
	for _, mode := range []string{"create", "normal", "runtime-only"} {
		t.Run(mode, func(t *testing.T) {
			city := admissionTestCity(t)
			now := time.Date(2026, 10, 14, 14, 39, 2, 0, time.UTC)
			ledgerPath := writeAdmissionGrant(t, city, now)
			if err := os.WriteFile(filepath.Join(city, "config/capacity-scheduler-v2.toml"), []byte("[queue]\nmanaged_exact_routes=[\"clerk\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			grant := map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)}
			writeLedger := func(reservations map[string]any) {
				t.Helper()
				data, err := json.Marshal(map[string]any{"schema": 1, "reservations": reservations})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(ledgerPath, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(city), WithClock(&clock.Fake{Time: now}))
			countStarts := func() int {
				count := 0
				for _, call := range sp.Calls {
					if call.Method == "Start" {
						count++
					}
				}
				return count
			}
			var id string
			if mode == "create" {
				// Direct create persists a SID before the provider boundary, just as
				// controller-managed `session new` creates its bead before enqueue.
				writeLedger(map[string]any{"clerk": grant})
				_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "clerk", Alias: alias, Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
				if err == nil || !runtime.IsProviderCapacity(err) {
					t.Fatalf("generic grant admitted dedicated create: %v", err)
				}
				if got := countStarts(); got != 0 {
					t.Fatalf("generic grant started provider %d times", got)
				}
				writeLedger(map[string]any{key: grant})
				_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "clerk", Alias: "other/" + alias, Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
				if err == nil || !runtime.IsProviderCapacity(err) {
					t.Fatalf("qualified alias spent exact packet grant: %v", err)
				}
				if got := countStarts(); got != 0 {
					t.Fatalf("qualified alias started provider %d times", got)
				}
				info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "clerk", Alias: alias, Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
				if err != nil {
					t.Fatal(err)
				}
				id = info.ID
			} else {
				b, err := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateSuspended), "template": "clerk", "alias": alias, "provider": "fake", "work_dir": t.TempDir(), "command": "fake-command"}})
				if err != nil {
					t.Fatal(err)
				}
				id = b.ID
				start := func() error {
					if mode == "normal" {
						return mgr.Start(context.Background(), testActor(mgr, ActorOperator), id, "fake-command", runtime.Config{})
					}
					by := runtimeOnlyActor(t, mgr, id)
					defer by.Lease.Release()
					inc := mintUnder(t, mgr, by.Lease, id)
					return mgr.StartRuntimeOnly(context.Background(), by, id, inc, "fake-command", runtime.Config{})
				}
				writeLedger(map[string]any{"clerk": grant})
				if err := start(); err == nil || !runtime.IsProviderCapacity(err) {
					t.Fatalf("generic grant admitted dedicated resume: %v", err)
				}
				if got := countStarts(); got != 0 {
					t.Fatalf("generic grant started provider %d times", got)
				}
				writeLedger(map[string]any{key: grant})
				if err := start(); err != nil {
					t.Fatal(err)
				}
			}
			if got := countStarts(); got != 1 {
				t.Fatalf("exact grant provider starts=%d, want 1", got)
			}
			data, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			var ledger struct {
				Reservations map[string]struct {
					StartClaims map[string]string `json:"start_claims"`
				} `json:"reservations"`
			}
			if err := json.Unmarshal(data, &ledger); err != nil {
				t.Fatal(err)
			}
			if ledger.Reservations[key].StartClaims[id] != now.Format(time.RFC3339) {
				t.Fatalf("exact SID claim absent: %s", data)
			}
		})
	}
}

func TestCapacityAdmissionProofIsBoundedAndOneShot(t *testing.T) {
	for _, mode := range []string{"concurrent-duplicate", "different-SID", "expired-evidence"} {
		t.Run(mode, func(t *testing.T) {
			city := admissionTestCity(t)
			now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
			ledgerPath := writeAdmissionGrant(t, city, now)
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			clk := &clock.Fake{Time: now}
			mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(city), WithClock(clk))
			ctx, allowed, reason := AdmitCapacityStart(context.Background(), city, "worker", "SID-a", now, sp, NewStore(beads.SessionStore{Store: store}))
			if !allowed {
				t.Fatalf("initial admission denied: %s", reason)
			}
			before, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg := runtime.Config{Command: "fake-command", Env: map[string]string{"GC_SESSION_ID": "SID-a", "GC_TEMPLATE": "worker"}}
			startsWant := 0
			if mode == "concurrent-duplicate" {
				var wg sync.WaitGroup
				results := make(chan error, 2)
				for i := 0; i < 2; i++ {
					wg.Add(1)
					go func() { defer wg.Done(); results <- mgr.startRuntime(ctx, "runtime-a", cfg) }()
				}
				wg.Wait()
				close(results)
				admitted := 0
				denied := 0
				for e := range results {
					switch {
					case e == nil:
						admitted++
					case runtime.IsProviderCapacity(e):
						denied++
					default:
						t.Fatal(e)
					}
				}
				if admitted != 1 || denied != 1 {
					t.Fatalf("same proof admitted=%d denied=%d", admitted, denied)
				}
				startsWant = 1
			} else {
				if mode == "different-SID" {
					cfg.Env["GC_SESSION_ID"] = "SID-b"
				} else {
					clk.Time = now.Add(61 * time.Second)
				}
				if e := mgr.startRuntime(ctx, "runtime-a", cfg); !runtime.IsProviderCapacity(e) {
					t.Fatalf("invalid proof allowed: %v", e)
				}
			}
			starts := 0
			for _, c := range sp.Calls {
				if c.Method == "Start" {
					starts++
				}
			}
			if starts != startsWant {
				t.Fatalf("provider starts=%d want%d", starts, startsWant)
			}
			after, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("proof reuse rewrote or spent a second ledger claim")
			}
		})
	}
}

// The scheduler emits explicit null when no cooldown is active. Missing or
// malformed cooldown evidence must still refuse without spending the grant.
func TestCapacityAdmissionCooldownContract(t *testing.T) {
	now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   any
		missing bool
		allowed bool
	}{
		{name: "explicit-null", value: nil, allowed: true},
		{name: "expired", value: now.Add(-time.Hour).Format(time.RFC3339), allowed: true},
		{name: "deadline-now", value: now.Format(time.RFC3339), allowed: true},
		{name: "future", value: now.Add(time.Minute).Format(time.RFC3339)},
		{name: "missing", missing: true},
		{name: "empty", value: ""},
		{name: "malformed", value: "no-cooldown"},
		{name: "nonstring", value: 123},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := admissionTestCity(t)
			ledger := writeAdmissionGrant(t, city, now)
			path := filepath.Join(city, ".gc/runtime/capacity-scheduler-v2/snapshot.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]any
			if err := json.Unmarshal(data, &snapshot); err != nil {
				t.Fatal(err)
			}
			hysteresis := snapshot["hysteresis"].(map[string]any)
			if tc.missing {
				delete(hysteresis, "cooldown_until")
			} else {
				hysteresis["cooldown_until"] = tc.value
			}
			data, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			allowed, reason := ClaimCapacityStart(city, "worker", "SID-a", now, runtime.NewFake(), NewStore(beads.SessionStore{Store: beads.NewMemStore()}))
			if allowed != tc.allowed {
				t.Fatalf("admitted=%v want=%v reason=%s", allowed, tc.allowed, reason)
			}
			after, err := os.ReadFile(ledger)
			if err != nil {
				t.Fatal(err)
			}
			if !allowed && string(before) != string(after) {
				t.Fatal("refusal spent or rewrote reservation")
			}
			if allowed {
				duplicate, why := ClaimCapacityStart(city, "worker", "SID-b", now, runtime.NewFake(), NewStore(beads.SessionStore{Store: beads.NewMemStore()}))
				if duplicate {
					t.Fatalf("second SID reused capacity grant: %s", why)
				}
			}
		})
	}
}

// The reviewed hard ceiling is three; this changes no freshness, one-shot,
// complete-census, zone or reservation prerequisite at the native boundary.
func TestCapacityCapThreeBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cap    int
		active int
		mutate func(map[string]any)
		allow  bool
	}{
		{name: "legacy-two", cap: 2, allow: true},
		{name: "three", cap: 3, allow: true},
		{name: "third-slot", cap: 3, active: 2, allow: true},
		{name: "four-refused", cap: 4},
		{name: "three-yellow", cap: 3, mutate: func(s map[string]any) { s["zone"] = "yellow" }},
		{name: "three-stale", cap: 3, mutate: func(s map[string]any) { s["generated_at"] = "2026-10-04T07:58:59Z" }},
		{name: "three-partial", cap: 3, mutate: func(s map[string]any) { s["live_census"].(map[string]any)["complete"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := admissionTestCity(t)
			now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
			ledgerPath := writeAdmissionGrant(t, city, now)
			snapshotPath := filepath.Join(city, ".gc/runtime/capacity-scheduler-v2/snapshot.json")
			raw, err := os.ReadFile(snapshotPath)
			if err != nil {
				t.Fatal(err)
			}
			var snap map[string]any
			if err = json.Unmarshal(raw, &snap); err != nil {
				t.Fatal(err)
			}
			sp := runtime.NewFake()
			store := beads.NewMemStore()
			starts := func() int {
				n := 0
				for _, call := range sp.Calls {
					if call.Method == "Start" {
						n++
					}
				}
				return n
			}
			rows := []any{}
			for i := 0; i < tc.active; i++ {
				name := []string{"active-a", "active-b"}[i]
				b, e := store.Create(beads.Bead{Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"template": "worker", "state": "active", "session_name": name}})
				if e != nil {
					t.Fatal(e)
				}
				if e = sp.Start(context.Background(), name, runtime.Config{Env: map[string]string{"GC_CITY": city, "GC_SESSION_ID": b.ID, "GC_TEMPLATE": "worker"}}); e != nil {
					t.Fatal(e)
				}
				rows = append(rows, map[string]any{"id": b.ID, "name": name, "route": "worker", "state": "active", "scheduler_owned": false, "suspended": false})
			}
			capacity := snap["capacity"].(map[string]any)
			capacity["managed_worker_cap"], capacity["managed_active_count"], capacity["active"] = tc.cap, tc.active, rows
			census := snap["live_census"].(map[string]any)
			census["count"], census["active"] = tc.active, rows
			if tc.mutate != nil {
				tc.mutate(snap)
			}
			raw, err = json.Marshal(snap)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(snapshotPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			mgr := NewManagerWithOptions(beadstest.Sessions(store), sp, WithCityPath(city), WithClock(&clock.Fake{Time: now}))
			_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
			if tc.allow {
				if err != nil {
					t.Fatalf("allowed cap %d refused: %v", tc.cap, err)
				}
				if got := starts(); got != tc.active+1 {
					t.Fatalf("starts=%d want1", got)
				}
				// A second SID cannot spend the same reservation, even at ceiling three.
				_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "fake-command", WorkDir: t.TempDir(), Provider: "fake"})
				if !runtime.IsProviderCapacity(err) || starts() != tc.active+1 {
					t.Fatalf("second spend: err=%v starts=%d", err, starts())
				}
			} else {
				if !runtime.IsProviderCapacity(err) || starts() != 0 {
					t.Fatalf("denial err=%v starts=%d", err, starts())
				}
				after, e := os.ReadFile(ledgerPath)
				if e != nil {
					t.Fatal(e)
				}
				if string(before) != string(after) {
					t.Fatal("denied admission changed ledger")
				}
			}
		})
	}
}
