package dolt_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertCompactNoStatementsAfterFlattenButLiveProbes asserts that the dolt
// log shows nothing after the flatten transaction except the live gate's own
// served-HEAD probes: a failed gate after a successful flatten ends the pass
// before any integrity check, GC, push, or other statement.
func assertCompactNoStatementsAfterFlattenButLiveProbes(t *testing.T, log string) {
	t.Helper()
	lines := strings.Split(log, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "compaction: flatten history") {
			continue
		}
		for _, rest := range lines[i+1:] {
			rest = strings.TrimSpace(rest)
			if rest == "" {
				continue
			}
			if rest != "db=beads query="+compactLiveProbeQuery {
				t.Fatalf("compact issued %q after a flatten that failed its live gate:\n%s", rest, log)
			}
		}
		return
	}
	t.Fatalf("dolt log missing the flatten transaction:\n%s", log)
}

// assertCompactNoPendingRemediationMarkers asserts that compact wrote no
// pending-GC or pending-push marker for beads: neither a failed live gate nor
// a pass without a rewrite leaves remediation state behind.
func assertCompactNoPendingRemediationMarkers(t *testing.T, fixture compactScriptFixture, out string) {
	t.Helper()
	for _, markerDir := range []string{"compact-pending-gc", "compact-pending-push"} {
		marker := filepath.Join(fixture.cityPath, ".gc", "runtime", "packs", "dolt", markerDir, "beads")
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("compact left a %s marker for beads (stat err=%v):\n%s", markerDir, err, out)
		}
	}
}

func TestCompactScriptLiveGateStopsAfterAFlattenThatLosesService(t *testing.T) {
	fixture := newCompactScriptFixture(t)
	setCompactBeadsLiveFault(t, fixture, "flatten", "unserved")

	out, err := fixture.run(t, "success", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
	if err == nil {
		t.Fatalf("compact succeeded after the flattened database stopped being served:\n%s", out)
	}
	assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
		"live_gate_failed_step": "flatten",
		"live_gate_failure":     "served-HEAD probe failed",
	})
	assertCompactNoStatementsAfterFlattenButLiveProbes(t, readCompactDoltLog(t, fixture))
	assertCompactNoPendingRemediationMarkers(t, fixture, out)
}

func TestCompactScriptLiveGateQuarantineAllowsFlattenRestoration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		restoration []string
	}{
		{
			name:        "flatten reset fails",
			mode:        "flatten_failure",
			restoration: []string{"reset exploded", "already at pre-flatten HEAD=headcommit"},
		},
		{
			name:        "flatten commit fails and HEAD is restored",
			mode:        "commit_failure_after_reset",
			restoration: []string{"commit rejected after reset", "restored pre-flatten HEAD=headcommit"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newCompactScriptFixture(t)
			setCompactBeadsLiveFault(t, fixture, "flatten", "unserved")

			out, err := fixture.run(t, tc.mode, "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil {
				t.Fatalf("compact succeeded although the failed flatten left beads unserved:\n%s", out)
			}
			assertCompactBeadsLiveGateMarker(t, fixture, map[string]string{
				"live_gate_failed_step": "flatten",
				"live_gate_failure":     "served-HEAD probe failed",
			})
			gate := strings.Index(out, "live database gate failed after flatten")
			if gate < 0 {
				t.Fatalf("compact output missing the live gate diagnostic:\n%s", out)
			}
			for _, want := range tc.restoration {
				if at := strings.Index(out, want); at < 0 || at > gate {
					t.Fatalf("compact output missing %q before the live gate diagnostic:\n%s", want, out)
				}
			}
			assertCompactBeadsQuarantineAlert(t, fixture, "mayor", compactBeadsQuarantineMarkerPath(fixture), "compact-quarantine", compactLiveGateReason)
			assertCompactNoPendingRemediationMarkers(t, fixture, out)
		})
	}
}
