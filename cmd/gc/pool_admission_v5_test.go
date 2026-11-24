package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestClaimPoolAdmissionRequiresVersionedFreshEvidenceBeforeSpend(t *testing.T) {
	now := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	for _, name := range []string{"complete", "legacy", "wrong-version", "missing-census", "wrong-contract", "incomplete", "null-count", "stale-published", "stale-evidence", "future-evidence", "missing-evidence", "count-conflict", "rows-conflict", "duplicate-SID", "cap3", "cap4", "null-cap", "missing-overcap", "extended-row-conflict"} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			configDir := filepath.Join(city, "config")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(configDir, "capacity-scheduler-v2.toml"), []byte("[queue]\nmanaged_exact_routes=[\"worker\"]\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			capacity := map[string]any{"managed_active_count": 0, "managed_worker_cap": 2, "over_cap_by": 0, "active": []any{}}
			census := map[string]any{"contract": "managed-live-census/v1", "complete": true, "count": 0, "observed_at": now.Format(time.RFC3339), "evidence_at": now.Format(time.RFC3339), "active": []any{}}
			snapshot := map[string]any{"schema_version": 5, "live_census": census, "generated_at": now.Format(time.RFC3339), "zone": "green", "hysteresis": map[string]any{"green_streak": 3, "cooldown_until": now.Add(-time.Hour).Format(time.RFC3339)}, "capacity": capacity}
			switch name {
			case "cap3":
				capacity["managed_worker_cap"] = 3
			case "cap4":
				capacity["managed_worker_cap"] = 4
			case "null-cap":
				capacity["managed_worker_cap"] = nil
			case "missing-overcap":
				delete(capacity, "over_cap_by")
			case "legacy":
				delete(snapshot, "schema_version")
				delete(snapshot, "live_census")
			case "wrong-version":
				snapshot["schema_version"] = 4
			case "missing-census":
				delete(snapshot, "live_census")
			case "wrong-contract":
				census["contract"] = "aggregate/v0"
			case "incomplete":
				census["complete"] = false
			case "null-count":
				census["count"] = nil
			case "stale-published":
				snapshot["generated_at"] = now.Add(-61 * time.Second).Format(time.RFC3339)
			case "stale-evidence":
				census["evidence_at"] = now.Add(-61 * time.Second).Format(time.RFC3339)
			case "future-evidence":
				census["evidence_at"] = now.Add(time.Second).Format(time.RFC3339)
			case "missing-evidence":
				delete(census, "evidence_at")
			case "count-conflict":
				census["count"] = 1
			case "extended-row-conflict":
				census["count"] = 1
				capacity["managed_active_count"] = 1
				census["active"] = []any{map[string]any{"id": "a", "route": "worker", "state": "active", "new_evidence": "first"}}
				capacity["active"] = []any{map[string]any{"id": "a", "route": "worker", "state": "active", "new_evidence": "other"}}
			case "rows-conflict":
				census["count"] = 1
				census["active"] = []any{map[string]any{"id": "a", "route": "worker", "state": "active"}}
			case "duplicate-SID":
				rows := []any{map[string]any{"id": "a", "route": "worker", "state": "active"}, map[string]any{"id": "a", "route": "worker", "state": "active"}}
				census["active"] = rows
				census["count"] = 2
				capacity["active"] = rows
				capacity["managed_active_count"] = 2
			}
			snapPath := filepath.Join(city, ".gc/runtime/capacity-scheduler-v2/snapshot.json")
			ledgerPath := filepath.Join(city, ".gc/runtime/pool-capacity-admission/reservations.json")
			for path, value := range map[string]any{snapPath: snapshot, ledgerPath: map[string]any{"schema": 1, "reservations": map[string]any{"worker": map[string]any{"slots": 1, "active_at_reservation": 0, "created_at": now.Format(time.RFC3339)}}}} {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, encoded, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			allowed, reason := claimPoolStartAdmission(city, "worker", "candidate-SID", now, runtime.NewFake(), beads.NewMemStore())
			positive := name == "complete" || name == "cap3"
			if allowed != positive {
				t.Fatalf("allowed=%v reason=%s; expected fresh complete v5 with cap2 or cap3", allowed, reason)
			}
			after, err := os.ReadFile(ledgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if !positive && string(after) != string(before) {
				t.Fatal("denied census spent or rewrote reservation")
			}
			if positive {
				var spent struct {
					Reservations map[string]struct {
						Slots       int               `json:"slots"`
						StartClaims map[string]string `json:"start_claims"`
					} `json:"reservations"`
				}
				if err := json.Unmarshal(after, &spent); err != nil {
					t.Fatal(err)
				}
				grant := spent.Reservations["worker"]
				if string(after) == string(before) || grant.Slots != 1 || len(grant.StartClaims) != 1 || grant.StartClaims["candidate-SID"] != now.Format(time.RFC3339Nano) {
					t.Fatal("allowed start did not spend exactly one reader-issued slot for candidate-SID")
				}
				for _, retry := range []struct{ sid, reason string }{
					{"candidate-SID", "capacity_session_already_claimed"},
					{"different-candidate-SID", "capacity_grant_consumed"},
				} {
					allowed, reason := claimPoolStartAdmission(city, "worker", retry.sid, now, runtime.NewFake(), beads.NewMemStore())
					if allowed || reason != retry.reason {
						t.Fatalf("retry %s: allowed=%v reason=%s; expected %s", retry.sid, allowed, reason, retry.reason)
					}
					retried, err := os.ReadFile(ledgerPath)
					if err != nil {
						t.Fatal(err)
					}
					if string(retried) != string(after) {
						t.Fatalf("denied retry %s rewrote spent reservation", retry.sid)
					}
				}
			}
		})
	}
}
