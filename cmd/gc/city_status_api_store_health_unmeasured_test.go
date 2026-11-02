package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// The store-health block reaches the renderer by two construction paths. The
// local one builds *StoreHealth through storeHealthFromInputs, which sets
// LiveRowsUnknown from the domain layer's RowsMeasured; store_health_test.go
// covers it. The API path rebuilds the same struct in snapshotFromStatusView
// from the server's view type, and until the view carried a measured-or-unknown
// field there was nothing there to copy: LiveRowsUnknown zero-filled to false
// and the renderer was told every API-path count was real, including the ones
// that were never taken. The tests below drive that path by injecting a view
// directly, which reproduces the degraded case without waiting for a store read
// to fail.

// apiStoreHealthView is the wire-shaped input the supervisor would have sent.
// Tests vary rowsMeasured and read the rendered block, so the assertion is
// about operator-visible output rather than about field plumbing.
func apiStoreHealthView(liveRows int, rowsMeasured bool, ratioMB float64) api.StatusView {
	return api.StatusView{
		CityName: "bright-lights",
		CityPath: "/home/user/bright-lights",
		StoreHealth: &api.StatusStoreHealthView{
			Path:         "/home/user/bright-lights/.beads/dolt",
			SizeBytes:    329_400_000,
			LiveRows:     liveRows,
			RowsMeasured: rowsMeasured,
			RatioMB:      ratioMB,
			ThresholdMB:  1.0,
		},
	}
}

func renderAPIPathStoreHealth(t *testing.T, view api.StatusView) string {
	t.Helper()
	snapshot := snapshotFromStatusView(view.CityPath, view)
	if snapshot.Summary.StoreHealth == nil {
		t.Fatal("snapshotFromStatusView dropped the store health block entirely")
	}
	var buf bytes.Buffer
	renderStoreHealthBlock(&buf, snapshot.Summary.StoreHealth)
	return buf.String()
}

// The defect, stated as a test: a count the supervisor never took must not
// arrive at the operator as a definite zero, and must not carry a ratio
// derived from it. A 329 MB store reporting "Live rows: 0 / Ratio: 0.0 MB/row"
// reads as comfortably under the threshold, when in truth nothing was measured
// and the comparison means nothing.
func TestSnapshotFromStatusViewUnmeasuredRowsRendersUnknownAndOmitsRatio(t *testing.T) {
	out := renderAPIPathStoreHealth(t, apiStoreHealthView(0, false, 0))

	if strings.Contains(out, "Live rows:   0") {
		t.Errorf("API path prints a definite zero for a count that was never taken:\n%s", out)
	}
	if !strings.Contains(out, "Live rows:   unknown") {
		t.Errorf("API path does not report the row count as unknown:\n%s", out)
	}
	if strings.Contains(out, "Ratio:") {
		t.Errorf("API path prints a ratio derived from an unmeasured count:\n%s", out)
	}
}

// The positive control, and the reason the test above is not satisfiable by
// never printing a count at all. A store that really is empty, and whose
// emptiness was really measured, keeps its definite zero and its ratio line.
// Withholding the number here would be the same defect with its polarity
// flipped: an honest measurement reported as unavailable.
func TestSnapshotFromStatusViewMeasuredEmptyStoreKeepsZeroAndRatio(t *testing.T) {
	out := renderAPIPathStoreHealth(t, apiStoreHealthView(0, true, 0))

	if !strings.Contains(out, "Live rows:   0") {
		t.Errorf("API path hides a genuinely measured zero:\n%s", out)
	}
	if !strings.Contains(out, "Ratio:") {
		t.Errorf("API path omits the ratio line for a measured store:\n%s", out)
	}
	if strings.Contains(out, "unknown") {
		t.Errorf("API path reports a measured count as unknown:\n%s", out)
	}
}

// A populated store is the ordinary case and must be untouched by the change:
// the count and the real ratio both render.
func TestSnapshotFromStatusViewMeasuredPopulatedStoreRendersRealRatio(t *testing.T) {
	out := renderAPIPathStoreHealth(t, apiStoreHealthView(1121, true, 0.29))

	if !strings.Contains(out, "Live rows:   1121") {
		t.Errorf("API path drops a measured row count:\n%s", out)
	}
	if !strings.Contains(out, "Ratio:       0.3 MB/row") {
		t.Errorf("API path does not render the measured ratio:\n%s", out)
	}
}

// The two states must not render identically. This is the property the domain
// layer pins in TestComputeUnmeasuredIsDistinguishableFromRealZero, restated
// at the end of the API path, where it was previously false.
func TestSnapshotFromStatusViewUnmeasuredDiffersFromMeasuredEmpty(t *testing.T) {
	unmeasured := renderAPIPathStoreHealth(t, apiStoreHealthView(0, false, 0))
	measuredEmpty := renderAPIPathStoreHealth(t, apiStoreHealthView(0, true, 0))

	if unmeasured == measuredEmpty {
		t.Errorf("an unmeasured count renders byte-identically to a measured empty store:\n%s", unmeasured)
	}
}

// The mapper must carry the flag across rather than derive it from the count.
// Deriving it — LiveRowsUnknown: v.StoreHealth.LiveRows == 0 — is the tempting
// one-line fix and reintroduces the original conflation from the other side,
// declaring every empty store unmeasured. The measured-empty case above already
// fails on that mistake; this asserts the direction the mapper actually reads.
func TestSnapshotFromStatusViewCarriesRowsMeasuredRatherThanInferringIt(t *testing.T) {
	measured := snapshotFromStatusView("/c", apiStoreHealthView(0, true, 0))
	if measured.Summary.StoreHealth.LiveRowsUnknown {
		t.Error("mapper marks a measured empty store unknown, inferring the flag from the count")
	}

	unmeasured := snapshotFromStatusView("/c", apiStoreHealthView(4200, false, 0))
	if !unmeasured.Summary.StoreHealth.LiveRowsUnknown {
		t.Error("mapper trusts an unmeasured count because the number happened to be non-zero")
	}
}

// gc status --json is read by scripts and dashboards that never see the text
// block, so the qualifier has to survive into the JSON too. live_rows_unknown
// is omitempty: present and true when the count is unavailable, absent when the
// count is real.
func TestCityStatusJSONFromAPIPathMarksUnmeasuredRows(t *testing.T) {
	for _, tc := range []struct {
		name         string
		rowsMeasured bool
		wantUnknown  bool
	}{
		{name: "unmeasured", rowsMeasured: false, wantUnknown: true},
		{name: "measured", rowsMeasured: true, wantUnknown: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := snapshotFromStatusView("/c", apiStoreHealthView(0, tc.rowsMeasured, 0))
			var buf bytes.Buffer
			writeCityStatusJSONWithCache(snapshot, snapshot.Summary, 0, &buf)

			var got struct {
				Summary struct {
					StoreHealth struct {
						LiveRows        int   `json:"live_rows"`
						LiveRowsUnknown *bool `json:"live_rows_unknown"`
					} `json:"store_health"`
				} `json:"summary"`
			}
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatalf("gc status --json is not valid JSON: %v\n%s", err, buf.String())
			}

			sh := got.Summary.StoreHealth
			switch {
			case tc.wantUnknown && (sh.LiveRowsUnknown == nil || !*sh.LiveRowsUnknown):
				t.Errorf("JSON reports live_rows %d without marking it unknown:\n%s", sh.LiveRows, buf.String())
			case !tc.wantUnknown && sh.LiveRowsUnknown != nil && *sh.LiveRowsUnknown:
				t.Errorf("JSON marks a measured count unknown:\n%s", buf.String())
			}
		})
	}
}
