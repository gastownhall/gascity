package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Run with GC_LIVE_CITY_PATH before installing the candidate binary. This
// constructs the same city provider as the supervisor and checks its scanner
// interface. The gc pool-admission-probe command performs the live /proc scan
// outside go test's safety guard.
func TestLiveCityPoolAdmissionProviderScanner(t *testing.T) {
	cityPath := os.Getenv("GC_LIVE_CITY_PATH")
	if cityPath == "" {
		t.Skip("set GC_LIVE_CITY_PATH for the pre-install live provider probe")
	}
	if override := os.Getenv("GC_SESSION"); override != "" {
		t.Fatalf("GC_SESSION=%q changes the supervisor's provider route", override)
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	sp, err := newSessionProviderForCity(cfg, cityPath)
	if err != nil {
		t.Fatal(err)
	}
	scanner, ok := sp.(runtime.ProcessTableScanner)
	if !ok {
		t.Fatalf("live city session provider %T does not implement ProcessTableScanner", sp)
	}
	_ = scanner
	t.Logf("live city provider=%T supports ProcessTableScanner; run candidate gc pool-admission-probe for the real /proc scan", sp)
}

// One shared admission must bound the actual provider starts even when two
// already-assigned pool sessions request resume independently of scale_check.
func TestExecutePlannedStartsTraced_PoolResumeUsesSharedAdmission(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cityPath := t.TempDir()
	writePoolScope(t, cityPath)
	writeJSON := func(name string, value any) {
		t.Helper()
		path := filepath.Join(cityPath, ".gc", "runtime", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/snapshot.json") {
			value = versionedPoolSnapshotFixture(value)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON("capacity-scheduler-v2/snapshot.json", map[string]any{
		"generated_at": now.Format(time.RFC3339),
		"zone":         "green",
		"hysteresis":   map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
		"capacity": map[string]any{
			"managed_active_count": 1, "managed_worker_cap": 2, "over_cap_by": 0,
			"active": []any{map[string]any{"id": "qualifier-session", "route": "deal-qualifier", "state": "active"}},
		},
	})
	writeJSON("pool-capacity-admission/reservations.json", map[string]any{
		"schema": 1,
		"reservations": map[string]any{
			"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
		},
	})
	cfg := &config.City{Agents: []config.Agent{{Name: "deal-executor", MaxActiveSessions: intPtr(2)}}}
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for _, name := range []string{"deal-executor-1", "deal-executor-2"} {
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: "deal-executor"}
		desired[name] = tp
		created, err := store.Create(beads.Bead{
			ID: name + "-id", Title: name, Type: sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: creatingMeta(map[string]string{
				"session_name": name, "template": "deal-executor",
				"generation": "1", "continuation_epoch": "1",
				"instance_token": "tok-" + name, "pool_managed": "true",
				"pending_create_claim": "true",
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, created), tp: tp})
	}
	woken := executePlannedStartsTraced(context.Background(), candidates, cfg, desired,
		provider, store, "test-city", cityPath, &clock.Fake{Time: now},
		events.Discard, 5*time.Second, io.Discard, io.Discard, nil)
	if woken != 1 {
		t.Fatalf("started %d pool providers with one shared admission; want 1", woken)
	}
	// Isolate ledger reuse from the separate live census discrepancy gate.
	running, err := provider.ListRunning("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range running {
		if err := provider.Stop(name); err != nil {
			t.Fatal(err)
		}
	}
	ledgerPath := filepath.Join(cityPath, ".gc", "runtime", "pool-capacity-admission", "reservations.json")
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var rewritten map[string]any
	if err := json.Unmarshal(data, &rewritten); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(rewritten) // reader preserves unknown entry fields on a later tick
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "third-session-id", now.Add(10*time.Second), provider, store)
	if allowed || reason != "capacity_grant_consumed" {
		t.Fatalf("later tick reused one-slot grant: allowed=%t reason=%q", allowed, reason)
	}
}

func TestClaimPoolStartAdmission_FailsClosedOnUnsafeCapacityState(t *testing.T) {
	now := time.Date(2026, 9, 27, 1, 40, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		zone       string
		active     []any
		ledger     bool
		age        time.Duration
		wantReason string
	}{
		{"yellow", "yellow", []any{}, true, 0, "capacity_not_admissible"},
		{"red", "red", []any{}, true, 0, "capacity_not_admissible"},
		{"missing_ledger", "green", []any{}, false, 0, "capacity_state_unavailable"},
		{"stale_snapshot", "green", []any{}, true, 16 * time.Minute, "capacity_census_stale"},
		{"invalid_active", "green", []any{map[string]any{"id": "q", "route": "deal-qualifier", "state": "asleep"}}, true, 0, "capacity_census_row_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			writePoolScope(t, cityPath)
			write := func(name string, value any) {
				t.Helper()
				path := filepath.Join(cityPath, ".gc", "runtime", name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(name, "/snapshot.json") {
					value = versionedPoolSnapshotFixture(value)
				}
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("capacity-scheduler-v2/snapshot.json", map[string]any{
				"generated_at": now.Add(-tc.age).Format(time.RFC3339), "zone": tc.zone,
				"hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
				"capacity":   map[string]any{"managed_active_count": len(tc.active), "managed_worker_cap": 2, "over_cap_by": 0, "active": tc.active},
			})
			if tc.ledger {
				write("pool-capacity-admission/reservations.json", map[string]any{
					"schema": 1, "reservations": map[string]any{
						"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
					},
				})
			}
			allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "session-1", now, runtime.NewFake(), beads.NewMemStore())
			if allowed || reason != tc.wantReason {
				t.Fatalf("allowed=%t reason=%q, want false %q", allowed, reason, tc.wantReason)
			}
		})
	}
}

func TestClaimPoolStartAdmission_ConfiguredCityMissingAllRuntimeState(t *testing.T) {
	cityPath := t.TempDir()
	configPath := filepath.Join(cityPath, "config", "capacity-scheduler-v2.toml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[queue]\nmanaged_exact_routes = [\"deal-executor\", \"deal-qualifier\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "session-1", time.Now().UTC(), runtime.NewFake(), beads.NewMemStore())
	if allowed || reason != "capacity_state_unavailable" {
		t.Fatalf("configured managed route bypassed missing state: allowed=%t reason=%q", allowed, reason)
	}
}

func TestClaimPoolStartAdmission_MissingScopeWithResidualState(t *testing.T) {
	now := time.Date(2026, 9, 27, 1, 40, 0, 0, time.UTC)
	cityPath := t.TempDir()
	writePoolAdmissionState(t, cityPath, now, map[string]any{
		"managed_active_count": 0, "managed_worker_cap": 2, "over_cap_by": 0, "active": []any{},
	})
	allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "candidate", now, runtime.NewFake(), beads.NewMemStore())
	if allowed || reason != "capacity_scope_unavailable" {
		t.Fatalf("residual state without scope admitted: allowed=%t reason=%q", allowed, reason)
	}
}

func TestClaimPoolStartAdmission_RejectsMissingCapacityFields(t *testing.T) {
	now := time.Date(2026, 9, 27, 1, 40, 0, 0, time.UTC)
	for _, field := range []string{"managed_active_count", "managed_worker_cap", "over_cap_by", "active"} {
		for _, replacement := range []any{nil, "missing"} {
			name := field + "_null"
			if replacement == "missing" {
				name = field + "_missing"
			}
			t.Run(name, func(t *testing.T) {
				cityPath := t.TempDir()
				writePoolScope(t, cityPath)
				capacity := map[string]any{"managed_active_count": 0, "managed_worker_cap": 2, "over_cap_by": 0, "active": []any{}}
				if replacement == "missing" {
					delete(capacity, field)
				} else {
					capacity[field] = nil
				}
				writePoolAdmissionState(t, cityPath, now, capacity)
				allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "candidate", now, runtime.NewFake(), beads.NewMemStore())
				if allowed || reason != "capacity_snapshot_invalid" {
					t.Fatalf("incomplete %s admitted: allowed=%t reason=%q", field, allowed, reason)
				}
			})
		}
	}
}

func writePoolScope(t *testing.T, cityPath string) {
	t.Helper()
	path := filepath.Join(cityPath, "config", "capacity-scheduler-v2.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[queue]\nmanaged_exact_routes = [\"deal-executor\", \"deal-qualifier\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writePoolAdmissionState(t *testing.T, cityPath string, now time.Time, capacity map[string]any) {
	t.Helper()
	for name, value := range map[string]any{
		"capacity-scheduler-v2/snapshot.json": map[string]any{
			"generated_at": now.Format(time.RFC3339), "zone": "green",
			"hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
			"capacity":   capacity,
		},
		"pool-capacity-admission/reservations.json": map[string]any{
			"schema": 1, "reservations": map[string]any{
				"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
			},
		},
	} {
		path := filepath.Join(cityPath, ".gc", "runtime", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/snapshot.json") {
			value = versionedPoolSnapshotFixture(value)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaimPoolStartAdmission_RejectsLiveProcessMissingFromSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 27, 1, 40, 0, 0, time.UTC)
	cityPath := t.TempDir()
	write := func(name string, value any) {
		t.Helper()
		path := filepath.Join(cityPath, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/snapshot.json") {
			value = versionedPoolSnapshotFixture(value)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "config", "capacity-scheduler-v2.toml"),
		[]byte("[queue]\nmanaged_exact_routes = [\"deal-executor\", \"deal-qualifier\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	write(".gc/runtime/capacity-scheduler-v2/snapshot.json", map[string]any{
		"generated_at": now.Format(time.RFC3339), "zone": "green",
		"hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
		"capacity":   map[string]any{"managed_active_count": 0, "managed_worker_cap": 2, "over_cap_by": 0, "active": []any{}},
	})
	write(".gc/runtime/pool-capacity-admission/reservations.json", map[string]any{
		"schema": 1, "reservations": map[string]any{
			"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
		},
	})
	store := beads.NewMemStore()
	created, err := store.Create(beads.Bead{
		ID: "live-session", Title: "live DE", Type: sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: creatingMeta(map[string]string{
			"session_name": "deal-executor-1", "template": "deal-executor",
			"generation": "1", "continuation_epoch": "1",
			"instance_token": "live-token", "pool_managed": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.NewFake()
	if err := provider.Start(context.Background(), "deal-executor-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	allowed, reason := claimPoolStartAdmission(cityPath, "deal-executor", "candidate-2", now, provider, store)
	if allowed || reason != "capacity_live_census_discrepancy" {
		t.Fatalf("allowed=%t reason=%q, want false capacity_live_census_discrepancy", allowed, reason)
	}
	if err := provider.Stop("deal-executor-1"); err != nil {
		t.Fatal(err)
	}
	provider.OrphanedRuntimes[created.ID] = runtime.LiveRuntime{SessionID: created.ID, City: cityPath}
	allowed, reason = claimPoolStartAdmission(cityPath, "deal-executor", "candidate-2", now, provider, store)
	if allowed || reason != "capacity_process_census_discrepancy" {
		t.Fatalf("untracked live process admitted: allowed=%t reason=%q", allowed, reason)
	}
}

func TestExecutePlannedStartsTraced_AssignedPoolResumeSpendsOneGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cityPath := t.TempDir()
	writePoolScope(t, cityPath)
	write := func(name string, value any) {
		t.Helper()
		path := filepath.Join(cityPath, ".gc", "runtime", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/snapshot.json") {
			value = versionedPoolSnapshotFixture(value)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("capacity-scheduler-v2/snapshot.json", map[string]any{
		"generated_at": now.Format(time.RFC3339), "zone": "green",
		"hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
		"capacity":   map[string]any{"managed_active_count": 0, "managed_worker_cap": 1, "over_cap_by": 0, "active": []any{}},
	})
	write("pool-capacity-admission/reservations.json", map[string]any{
		"schema": 1, "reservations": map[string]any{
			"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
		},
	})
	cfg := &config.City{Agents: []config.Agent{{Name: "deal-executor", MaxActiveSessions: intPtr(2)}}}
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for _, name := range []string{"deal-executor-1", "deal-executor-2"} {
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: "deal-executor"}
		desired[name] = tp
		created, err := store.Create(beads.Bead{
			ID: name + "-id", Title: name, Type: sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"state": "asleep", "session_name": name, "template": "deal-executor",
				"generation": "1", "continuation_epoch": "1",
				"instance_token": "tok-" + name, "pool_managed": "true",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(beads.Bead{
			ID: "work-" + name, Title: "assigned work", Type: "task", Status: "in_progress",
			Assignee: created.ID, Metadata: map[string]string{"gc.routed_to": "deal-executor"},
		}); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, created), tp: tp})
	}
	woken := executePlannedStartsTraced(context.Background(), candidates, cfg, desired,
		provider, store, "test-city", cityPath, &clock.Fake{Time: now},
		events.Discard, 5*time.Second, io.Discard, io.Discard, nil)
	if woken != 1 {
		t.Fatalf("started %d assigned pool resumes with one grant; want 1", woken)
	}
}

func TestExecutePlannedStartsTraced_DedicatedResumeSpendsOneGrant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	cityPath := t.TempDir()
	writePoolScope(t, cityPath)
	write := func(name string, value any) {
		t.Helper()
		path := filepath.Join(cityPath, ".gc", "runtime", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/snapshot.json") {
			value = versionedPoolSnapshotFixture(value)
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("capacity-scheduler-v2/snapshot.json", map[string]any{
		"generated_at": now.Format(time.RFC3339), "zone": "green",
		"hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)},
		"capacity":   map[string]any{"managed_active_count": 0, "managed_worker_cap": 1, "over_cap_by": 0, "active": []any{}},
	})
	write("pool-capacity-admission/reservations.json", map[string]any{
		"schema": 1, "reservations": map[string]any{
			"deal-executor": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)},
		},
	})
	cfg := &config.City{Agents: []config.Agent{{Name: "deal-executor", MaxActiveSessions: intPtr(2)}}}
	store := beads.NewMemStore()
	provider := runtime.NewFake()
	desired := map[string]TemplateParams{}
	var candidates []startCandidate
	for _, name := range []string{"deal-executor-1", "deal-executor-2"} {
		tp := TemplateParams{Command: name, SessionName: name, TemplateName: "deal-executor"}
		desired[name] = tp
		created, err := store.Create(beads.Bead{
			ID: name + "-id", Title: name, Type: sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"state": "asleep", "session_name": name, "template": "deal-executor",
				"generation": "1", "continuation_epoch": "1",
				"instance_token": "tok-" + name, "session_key": "prior-key", "started_config_hash": "prior-hash",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create(beads.Bead{
			ID: "work-" + name, Title: "assigned work", Type: "task", Status: "in_progress",
			Assignee: created.ID, Metadata: map[string]string{"gc.routed_to": "deal-executor"},
		}); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, startCandidate{info: sessiontest.SeedBead(t, created), tp: tp})
	}
	woken := executePlannedStartsTraced(context.Background(), candidates, cfg, desired,
		provider, store, "test-city", cityPath, &clock.Fake{Time: now},
		events.Discard, 5*time.Second, io.Discard, io.Discard, nil)
	if woken != 1 {
		t.Fatalf("started %d dedicated resumes with one grant; want 1", woken)
	}
	for _, candidate := range candidates {
		if provider.IsRunning(candidate.name()) {
			continue
		}
		b, err := store.Get(candidate.info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Status == "closed" || b.Metadata["session_key"] != "prior-key" || b.Metadata["started_config_hash"] != "prior-hash" || b.Metadata["wake_attempts"] != "" || b.Metadata["continuation_reset_pending"] != "" {
			t.Fatalf("deferred dedicated identity changed: %v", b.Metadata)
		}
	}
}

// Keep the existing grant/caller fixtures on the reader's current contract.
// Version rejection cases deliberately bypass this helper in the v5 test.
func versionedPoolSnapshotFixture(value any) any {
	snapshot, ok := value.(map[string]any)
	if !ok {
		return value
	}
	capacity, ok := snapshot["capacity"].(map[string]any)
	if !ok {
		return value
	}
	snapshot["schema_version"] = 5
	snapshot["live_census"] = map[string]any{
		"contract": "managed-live-census/v1", "complete": true,
		"observed_at": snapshot["generated_at"], "evidence_at": snapshot["generated_at"],
		"count": capacity["managed_active_count"], "active": capacity["active"],
	}
	return snapshot
}
