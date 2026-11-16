package main

import (
	"encoding/json"
	"testing"
)

func TestCityStatusSchemaAcceptsPopulatedStoreHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		health StoreHealth
	}{
		{
			name: "measured with last maintenance",
			health: StoreHealth{
				Path: "/city/.beads/dolt", SizeBytes: 1048576, LiveRows: 2,
				RatioMB: 0.5, Warning: true, ThresholdMB: 0.25,
				LastGCAt: "2026-11-15T03:00:00Z", LastGCStatus: "success",
			},
		},
		{
			name: "row count unknown",
			health: StoreHealth{
				Path: "/city/.beads/dolt", SizeBytes: 1048576,
				LiveRowsUnknown: true, ThresholdMB: 1,
			},
		},
		{
			name:   "zero rows without maintenance history",
			health: StoreHealth{Path: "/city/.beads/dolt", ThresholdMB: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := statusSchemaInstance(t, cityStatusSnapshot{
				Summary: StatusSummaryJSON{StoreHealth: &tc.health},
			})
			health, ok := payload["summary"].(map[string]any)["store_health"].(map[string]any)
			if !ok {
				t.Fatal("populated store_health omitted from serialized status")
			}
			if tc.health.LiveRowsUnknown && health["live_rows_unknown"] != true {
				t.Fatal("serialized status lost live_rows_unknown=true")
			}
			if tc.health.LastGCAt != "" && (health["last_gc_at"] != tc.health.LastGCAt || health["last_gc_status"] != tc.health.LastGCStatus) {
				t.Fatal("serialized status lost maintenance history")
			}
			if err := validateStatusSchemaInstance(t, payload); err != nil {
				t.Fatalf("populated store_health must validate: %v", err)
			}
		})
	}
}

func TestCityStatusSchemaAcceptsFallbackWithoutStoreHealth(t *testing.T) {
	payload := statusSchemaInstance(t, cityStatusSnapshot{})
	if _, present := payload["summary"].(map[string]any)["store_health"]; present {
		t.Fatal("fallback status must omit unmeasured store_health")
	}
	if err := validateStatusSchemaInstance(t, payload); err != nil {
		t.Fatalf("fallback without store_health must validate: %v", err)
	}
}

func TestCityStatusSchemaRejectsInvalidStoreHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown field", mutate: func(h map[string]any) { h["unexpected"] = true }},
		{name: "missing live rows", mutate: func(h map[string]any) { delete(h, "live_rows") }},
		{name: "string size", mutate: func(h map[string]any) { h["size_bytes"] = "1048576" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := statusSchemaInstance(t, cityStatusSnapshot{
				Summary: StatusSummaryJSON{StoreHealth: &StoreHealth{Path: "/city/.beads/dolt", ThresholdMB: 1}},
			})
			health := payload["summary"].(map[string]any)["store_health"].(map[string]any)
			tc.mutate(health)
			if err := validateStatusSchemaInstance(t, payload); err == nil {
				t.Fatal("invalid store_health accepted by published schema")
			}
		})
	}
}

// statusSchemaInstance exercises the same typed projection and serialization
// used by gc status, without a controller, store, or process fixture.
func statusSchemaInstance(t *testing.T, snapshot cityStatusSnapshot) map[string]any {
	t.Helper()
	data, err := json.Marshal(cityStatusJSONFromSnapshot(snapshot, snapshot.Summary))
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode serialized status: %v", err)
	}
	return payload
}

func validateStatusSchemaInstance(t *testing.T, payload map[string]any) error {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal schema instance: %v", err)
	}
	return validateJSONAgainstResultSchemaE([]string{"status"}, data)
}
