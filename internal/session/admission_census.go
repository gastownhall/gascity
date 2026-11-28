package session

import (
	"encoding/json"
	"reflect"
	"time"
)

// poolCensusRow is the versioned census identity shared with the reader.
type poolCensusRow struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Route          string `json:"route"`
	State          string `json:"state"`
	SchedulerOwned *bool  `json:"scheduler_owned"`
	Suspended      *bool  `json:"suspended"`
}

// validatePoolCensusSnapshot rejects missing evidence before any grant spend.
// A recent publication cannot renew the age of the underlying observations.
func validatePoolCensusSnapshot(data []byte, now time.Time, managed func(string) bool) string {
	var snapshot struct {
		SchemaVersion int    `json:"schema_version"`
		GeneratedAt   string `json:"generated_at"`
		LiveCensus    struct {
			Contract   string          `json:"contract"`
			Complete   bool            `json:"complete"`
			ObservedAt string          `json:"observed_at"`
			EvidenceAt string          `json:"evidence_at"`
			Count      *int            `json:"count"`
			Active     []poolCensusRow `json:"active"`
		} `json:"live_census"`
		Capacity struct {
			ActiveCount *int            `json:"managed_active_count"`
			Active      []poolCensusRow `json:"active"`
		} `json:"capacity"`
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return "capacity_snapshot_invalid"
	}
	if snapshot.SchemaVersion != 5 {
		return "capacity_census_version_unavailable"
	}
	census := snapshot.LiveCensus
	if census.Contract != "managed-live-census/v1" || !census.Complete || census.Count == nil {
		return "capacity_census_incomplete"
	}
	times := []string{snapshot.GeneratedAt, census.ObservedAt, census.EvidenceAt}
	parsed := make([]time.Time, len(times))
	for i, text := range times {
		at, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || at.After(now) || now.Sub(at) > 60*time.Second {
			return "capacity_census_stale"
		}
		parsed[i] = at
	}
	if parsed[2].After(parsed[1]) || parsed[1].After(parsed[0]) {
		return "capacity_census_evidence_future"
	}
	// Compare full row objects too: a reader extension must not be silently
	// discarded when capacity and census disagree about the same SID.
	var wire struct {
		LiveCensus struct {
			Active []map[string]any `json:"active"`
		} `json:"live_census"`
		Capacity struct {
			Active []map[string]any `json:"active"`
		} `json:"capacity"`
	}
	if json.Unmarshal(data, &wire) != nil || !reflect.DeepEqual(wire.LiveCensus.Active, wire.Capacity.Active) {
		return "capacity_census_rows_conflict"
	}
	if *census.Count < 0 || census.Active == nil || snapshot.Capacity.Active == nil || snapshot.Capacity.ActiveCount == nil || *snapshot.Capacity.ActiveCount != *census.Count || len(census.Active) != *census.Count || !reflect.DeepEqual(census.Active, snapshot.Capacity.Active) {
		return "capacity_census_rows_conflict"
	}
	seen := map[string]bool{}
	for _, row := range census.Active {
		if row.ID == "" || row.Route == "" || row.State != "active" || seen[row.ID] || !managed(row.Route) {
			return "capacity_census_row_invalid"
		}
		seen[row.ID] = true
	}
	return ""
}
