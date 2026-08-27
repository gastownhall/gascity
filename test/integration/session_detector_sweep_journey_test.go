//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// detectorSweepJourneyPatrolInterval is the city's patrol cadence for this
// journey. It is short so several sweeps land inside the observation window.
const detectorSweepJourneyPatrolInterval = "5s"

// detectorSweepJourneySweepBudget is the ABSOLUTE per-sweep wall-clock budget.
// It is deliberately not expressed relative to any tick/debounce interval: the
// sweep must complete in bounded time on its own terms.
const detectorSweepJourneySweepBudget = 5 * time.Second

// detectorSweepJourneyMinCycles is how many distinct patrol trace cycles must
// carry a detector sweep before the journey is satisfied.
const detectorSweepJourneyMinCycles = 2

type detectorSweepJourneyTraceShow struct {
	Records []detectorSweepJourneyTraceRecord `json:"records"`
}

type detectorSweepJourneyTraceRecord struct {
	Seq         uint64 `json:"seq"`
	TraceID     string `json:"trace_id"`
	TickID      string `json:"tick_id"`
	RecordType  string `json:"record_type"`
	SiteCode    string `json:"site_code"`
	ReasonCode  string `json:"reason_code"`
	OutcomeCode string `json:"outcome_code"`
	SessionName string `json:"session_name"`
	DurationMS  int64  `json:"duration_ms"`
	Fields      struct {
		EffectApplied  *bool  `json:"effect_applied"`
		EffectOwner    string `json:"effect_owner"`
		OperationName  string `json:"operation_name"`
		DetectorFamily string `json:"detector_family"`
		SweepTrigger   string `json:"sweep_trigger"`
		AnyFamilyActs  *bool  `json:"any_family_acts"`
		RowsEvaluated  int    `json:"rows_evaluated"`
		DurationMS     int64  `json:"duration_ms"`
		Action         string `json:"action"`
		Source         string `json:"source"`
	} `json:"fields"`
}

// TestDetectorSweepRunsBesideLegacyOnLiveV59City is WD.1's journey: on a real
// schema-v59 managed-Dolt city with an isolated tmux socket, the detector sweep
// runs on every patrol tick beside legacy, records only detector-shadow
// (effect_applied=false, effect_owner=detector-shadow), never auto-arms trace
// detail, and completes inside an absolute per-sweep wall-clock budget. Legacy
// outcomes are unchanged: the configured session still starts and stays live.
//
// Per DETECTOR.md §3, unarmed detail records are stashed and discarded, so the
// run detail-arms every template before any cycle is counted.
func TestDetectorSweepRunsBesideLegacyOnLiveV59City(t *testing.T) {
	if usingSubprocess() {
		t.Skip("detector sweep journey requires tmux")
	}

	cityDir := setupReconcilerCityWithManagedDolt(t, `session_reconciler = "auto"

[[agent]]
name = "worker"
start_command = "sleep 3600"
min_active_sessions = 1
max_active_sessions = 2
`, `patrol_interval = "`+detectorSweepJourneyPatrolInterval+`"
`, `conditional_writes = "auto"`)

	schemaStatus, err := bdDolt(cityDir, "migrate", "schema", "--json")
	if err != nil || !strings.Contains(schemaStatus, "v59") {
		t.Fatalf("bd schema status = %q, err=%v, want v59", schemaStatus, err)
	}

	// Detail-arm every template before counting cycles. Without an arm the
	// per-session detector records are stashed and never become evidence.
	if out, err := gcDolt(cityDir, "trace", "start", "--template", "worker", "--for", "5m", "--level", "detail"); err != nil {
		t.Fatalf("arm worker detail trace: %v\n%s", err, out)
	}

	waitForExpectedTmuxSessions(t, cityDir, []string{"worker"})

	var (
		sweeps    []detectorSweepJourneyTraceRecord
		shadow    []detectorSweepJourneyTraceRecord
		autoArms  []detectorSweepJourneyTraceRecord
		lastTrace detectorSweepJourneyTraceShow
	)
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		lastTrace, err = detectorSweepJourneyTrace(cityDir)
		if err != nil {
			t.Fatalf("read trace: %v", err)
		}
		sweeps, shadow, autoArms = detectorSweepJourneyPartition(lastTrace)
		if len(detectorSweepJourneyDistinctCycles(sweeps)) >= detectorSweepJourneyMinCycles {
			break
		}
		select {
		case <-poll.C:
			continue
		case <-deadline.C:
		case <-t.Context().Done():
		}
		break
	}

	cycles := detectorSweepJourneyDistinctCycles(sweeps)
	if len(cycles) < detectorSweepJourneyMinCycles {
		t.Fatalf("detector sweeps observed on %d distinct trace cycles, want >= %d (records seen: %d)",
			len(cycles), detectorSweepJourneyMinCycles, len(lastTrace.Records))
	}

	for _, sweep := range sweeps {
		if sweep.Fields.AnyFamilyActs == nil || *sweep.Fields.AnyFamilyActs {
			t.Fatalf("sweep %s reports any_family_acts=%v, want false during the WD wave", sweep.TickID, sweep.Fields.AnyFamilyActs)
		}
		budget := time.Duration(sweep.Fields.DurationMS) * time.Millisecond
		if budget > detectorSweepJourneySweepBudget {
			t.Fatalf("sweep on tick %s took %s, over the absolute per-sweep budget of %s",
				sweep.TickID, budget, detectorSweepJourneySweepBudget)
		}
		if sweep.Fields.SweepTrigger != "patrol" && sweep.Fields.SweepTrigger != "boot" {
			t.Fatalf("sweep on tick %s carries trigger %q, want patrol or boot", sweep.TickID, sweep.Fields.SweepTrigger)
		}
	}

	for _, rec := range shadow {
		if rec.Fields.EffectApplied == nil || *rec.Fields.EffectApplied {
			t.Fatalf("detector record at %s has effect_applied=%v, want false", rec.SiteCode, rec.Fields.EffectApplied)
		}
		if !strings.HasPrefix(rec.ReasonCode, "detector_") {
			t.Fatalf("detector record at %s carries non-detector reason %q", rec.SiteCode, rec.ReasonCode)
		}
		switch rec.OutcomeCode {
		case "failed", "provider_error", "deadline_exceeded":
			t.Fatalf("detector record at %s predicts auto-arming outcome %q", rec.SiteCode, rec.OutcomeCode)
		}
	}

	for _, arm := range autoArms {
		t.Fatalf("sweep triggered an auto-arm: %+v", arm)
	}

	// Legacy outcomes unchanged: the configured worker still starts and stays
	// live beside the sweep.
	out, err := gcDolt(cityDir, "session", "list", "--json")
	if err != nil {
		t.Fatalf("gc session list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "worker") {
		t.Fatalf("worker session missing after %d detector sweeps:\n%s", len(sweeps), out)
	}
}

func detectorSweepJourneyTrace(cityDir string) (detectorSweepJourneyTraceShow, error) {
	out, err := gcDolt(cityDir, "trace", "show", "--json")
	if err != nil {
		return detectorSweepJourneyTraceShow{}, fmt.Errorf("gc trace show: %w: %s", err, out)
	}
	var result detectorSweepJourneyTraceShow
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &result); err != nil {
		return detectorSweepJourneyTraceShow{}, fmt.Errorf("decode gc trace show: %w: %s", err, out)
	}
	return result, nil
}

// detectorSweepJourneyPartition splits the trace into sweep-completion
// records, detector-shadow records, and auto-arm trace-control records.
func detectorSweepJourneyPartition(trace detectorSweepJourneyTraceShow) (sweeps, shadow, autoArms []detectorSweepJourneyTraceRecord) {
	for _, rec := range trace.Records {
		if rec.RecordType == "trace_control" && rec.Fields.Action == "start" && rec.Fields.Source == "auto" {
			autoArms = append(autoArms, rec)
			continue
		}
		if rec.Fields.EffectOwner != "detector-shadow" {
			continue
		}
		shadow = append(shadow, rec)
		if rec.Fields.OperationName == "detector_sweep.complete" {
			sweeps = append(sweeps, rec)
		}
	}
	return sweeps, shadow, autoArms
}

func detectorSweepJourneyDistinctCycles(sweeps []detectorSweepJourneyTraceRecord) []string {
	seen := make(map[string]bool, len(sweeps))
	out := make([]string, 0, len(sweeps))
	for _, sweep := range sweeps {
		if seen[sweep.TickID] {
			continue
		}
		seen[sweep.TickID] = true
		out = append(out, sweep.TickID)
	}
	return out
}
