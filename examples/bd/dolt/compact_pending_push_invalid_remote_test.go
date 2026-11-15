//go:build unix

package dolt_test

import (
	"os"
	"strings"
	"testing"
)

func pendingPushMarkerWithRemote(t *testing.T, remote string) (compactScriptFixture, string, string) {
	t.Helper()
	fixture := newCompactScriptFixture(t)
	data := "reason=flatten and full GC succeeded but remote push failed\n" +
		"created_at=1970-01-01T00:00:00Z\nremote=" + remote + "\n" +
		"local_branch=main\nremote_branch=main\n" +
		"expected_remote_head=headcommit\nexpected_remote_head_verified=1\n"
	marker := writeCompactStateMarker(t, fixture, "compact-pending-push", data)
	return fixture, marker, data
}

func assertCompactRemoteWasNotProbed(t *testing.T, fixture compactScriptFixture, remote string) {
	t.Helper()
	for _, line := range strings.Split(readCompactDoltLog(t, fixture), "\n") {
		if strings.Contains(line, "FROM dolt_remotes") && strings.Contains(line, remote) {
			t.Errorf("invalid marker remote reached a Dolt remote probe: %s", line)
		}
	}
}

func TestCompactScriptSharedHistoryGuardHoldsInvalidPendingPushRemote(t *testing.T) {
	for _, tc := range []struct{ name, remote string }{
		{name: "single_quote", remote: "bad'remote"},
		{name: "space", remote: "bad remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, marker, markerBefore := pendingPushMarkerWithRemote(t, tc.remote)
			for cycle := 1; cycle <= 2; cycle++ {
				out, err := fixture.run(t, "multiple_remotes_no_origin", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
				if err != nil {
					t.Fatalf("cycle %d: holding an invalid remote must still allow bare GC: %v\n%s", cycle, err, out)
				}
			}

			log := readCompactGCLog(t, fixture)
			if strings.Contains(log, "compact-pending-push-unsatisfiable") {
				t.Errorf("an invalid remote must not emit an unsatisfiable event or mail:\n%s", log)
			}
			events := compactGCLogLinesWithPrefix(log, "gc event emit dolt.compact.quarantine --actor controller --message db=beads type=compact-pending-push-held ")
			if len(events) != 2 {
				t.Errorf("held marker must emit one held event per cycle, got %d:\n%s", len(events), log)
			}
			mails := compactGCLogLinesWithPrefix(log, "gc mail send ")
			if len(mails) != 1 {
				t.Errorf("unchanged held marker must mail once across two cycles, got %d:\n%s", len(mails), log)
			} else if !strings.Contains(mails[0], "-s dolt compact quarantine: beads compact-pending-push-held ") {
				t.Errorf("invalid remote must send the held alert: %s", mails[0])
			}
			assertCompactRemoteWasNotProbed(t, fixture, tc.remote)
			if data, err := os.ReadFile(marker); err != nil || string(data) != markerBefore {
				t.Errorf("held marker must remain byte-for-byte intact: %v\n%s", err, data)
			}
		})
	}
}

func TestCompactScriptFlattenRefusesInvalidPendingPushRemoteBeforeProbing(t *testing.T) {
	for _, tc := range []struct{ name, remote string }{
		{name: "single_quote", remote: "bad'remote"},
		{name: "space", remote: "bad remote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, marker, markerBefore := pendingPushMarkerWithRemote(t, tc.remote)
			out, err := fixture.run(t, "multiple_remotes_no_origin", "GC_DOLT_COMPACT_ALLOW_FEDERATED=1", "GC_DOLT_COMPACT_THRESHOLD_COMMITS=500")
			if err == nil {
				t.Errorf("invalid pending-push remote must fail the database's compaction:\n%s", out)
			}
			want := "db=beads pending_push marker has invalid remote=" + tc.remote + " — manual intervention required"
			if !strings.Contains(out, want) {
				t.Errorf("missing invalid-remote refusal %q:\n%s", want, out)
			}
			assertCompactRemoteWasNotProbed(t, fixture, tc.remote)
			if log := readCompactGCLog(t, fixture); strings.Contains(log, "compact-pending-push-unsatisfiable") {
				t.Errorf("invalid remote must be refused before an unsatisfiable alert:\n%s", log)
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != markerBefore {
				t.Errorf("refused marker must remain byte-for-byte intact: %v\n%s", err, data)
			}
		})
	}
}
