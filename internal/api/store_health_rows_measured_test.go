package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// storehealth.Health carries RowsMeasured so a count that failed or timed out
// is distinguishable from a store that really is empty. Both the wire type and
// the view type used to drop it, so the CLI rebuilt its store-health struct
// from a payload that could not express "the count never happened" and
// zero-filled the qualifier into a claim that it had. These tests pin the
// qualifier at each hop it has to survive: domain to wire, and wire back to
// view.

func TestStatusStoreHealthFromDomainCarriesRowsMeasured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{name: "measured", measured: true},
		{name: "unmeasured", measured: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := storehealth.Compute("/c", 329_400_000, 0, tc.measured, time.Time{}, "")
			got := statusStoreHealthFromDomain(h)

			if got.RowsMeasured != tc.measured {
				t.Errorf("RowsMeasured = %v, want %v", got.RowsMeasured, tc.measured)
			}
		})
	}
}

// The flag is derived from the domain layer, never from the count. A store
// measured at zero rows is measured, and reporting it as unmeasured would be
// the original conflation running the other way.
func TestStatusStoreHealthFromDomainDoesNotInferFromLiveRows(t *testing.T) {
	measuredEmpty := statusStoreHealthFromDomain(storehealth.Compute("/c", 4096, 0, true, time.Time{}, ""))
	if !measuredEmpty.RowsMeasured {
		t.Error("a measured empty store is reported as unmeasured; the flag is being inferred from LiveRows")
	}

	unmeasuredNonZero := statusStoreHealthFromDomain(storehealth.Health{LiveRows: 4200, RowsMeasured: false})
	if unmeasuredNonZero.RowsMeasured {
		t.Error("an unmeasured count is reported as measured because the number was non-zero")
	}
}

// rows_measured has no omitempty: false must appear on the wire rather than
// vanish, because an absent key and a present false have to mean the same
// cautious thing to any client, and only one of them says so out loud.
func TestStatusStoreHealthSerializesRowsMeasuredWhenFalse(t *testing.T) {
	raw, err := json.Marshal(&StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 329_400_000})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["rows_measured"]; !ok {
		t.Errorf("rows_measured is omitted when false, so an unmeasured count is indistinguishable from an old payload:\n%s", raw)
	}
}

func TestStatusViewFromGenCarriesRowsMeasured(t *testing.T) {
	for _, tc := range []struct {
		name     string
		measured bool
	}{
		{name: "measured", measured: true},
		{name: "unmeasured", measured: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &genclient.StatusBody{
				Name: "bright-lights",
				Path: "/home/u/bright-lights",
				StoreHealth: &genclient.StatusStoreHealth{
					Path:              "/home/u/bright-lights/.beads/dolt",
					SizeBytes:         329_400_000,
					LiveRows:          0,
					RowsMeasured:      tc.measured,
					RatioMbPerRow:     0,
					ThresholdMbPerRow: 1.0,
				},
			}

			got := statusViewFromGen(body)

			if got.StoreHealth == nil {
				t.Fatal("statusViewFromGen dropped the store health block")
			}
			if got.StoreHealth.RowsMeasured != tc.measured {
				t.Errorf("RowsMeasured = %v, want %v", got.StoreHealth.RowsMeasured, tc.measured)
			}
		})
	}
}

// A supervisor that predates the field sends no rows_measured at all. The
// decoded flag stays false and the CLI renders the count as unknown, which is
// the honest answer: that server cannot tell us whether it measured. The
// alternative polarity would have the same silence assert a measurement.
func TestStatusViewFromGenTreatsAbsentRowsMeasuredAsUnmeasured(t *testing.T) {
	const legacyPayload = `{
		"name": "bright-lights",
		"path": "/home/u/bright-lights",
		"uptime_sec": 42,
		"agents": {"total": 0, "running": 0},
		"rigs": {"total": 0, "suspended": 0},
		"work": {},
		"mail": {},
		"store_health": {
			"path": "/home/u/bright-lights/.beads/dolt",
			"size_bytes": 329400000,
			"live_rows": 0,
			"ratio_mb_per_row": 0,
			"warning": false,
			"threshold_mb_per_row": 1
		}
	}`

	var body genclient.StatusBody
	if err := json.Unmarshal([]byte(legacyPayload), &body); err != nil {
		t.Fatalf("decoding a payload without rows_measured: %v", err)
	}

	got := statusViewFromGen(&body)

	if got.StoreHealth == nil {
		t.Fatal("statusViewFromGen dropped the store health block")
	}
	if got.StoreHealth.RowsMeasured {
		t.Error("a payload with no rows_measured decodes as measured, which is the omission asserting a measurement")
	}
}
