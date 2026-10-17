package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the multi-store half of "an empty queue and a failed query are
// different answers". The hook runs the work query against every store in its
// fan-out (hookWorkQueryStores). A secondary store whose read failed used to be
// skipped silently, so a primary that answered "[]" made the whole fan-out read
// as no work: plain `gc hook` exited 1 and `gc hook --claim` drained the seat
// as no_work, though a store that could hold this agent's work had never been
// read. A failed store is now a failed query unless another store has work.

// twoStoreFanOut is a primary store and one secondary (rig) store.
func twoStoreFanOut() []hookStore {
	return []hookStore{
		{dir: "city", env: []string{"GC_STORE=city"}},
		{dir: "riga", env: []string{"GC_STORE=riga"}},
	}
}

// primaryAnswersSecondaryFails runs a fan-out whose primary answers primaryOut
// and whose secondary store "riga" cannot be read.
func primaryAnswersSecondaryFails(t *testing.T, primaryOut string) hookStoreRunner {
	t.Helper()
	return func(_, dir string, _ []string) (string, error) {
		switch dir {
		case "city":
			return primaryOut, nil
		case "riga":
			return "", errTestStoreTimeout
		default:
			t.Fatalf("unexpected store dir %q", dir)
			return "", nil
		}
	}
}

const routedCityRow = `[{"id":"hw-city","status":"open","metadata":{"gc.routed_to":"worker"}}]`

func TestBestStoreWithWorkReportsAFailedSecondaryWhenNoStoreHasWork(t *testing.T) {
	stores := twoStoreFanOut()
	out, selected, err := bestStoreWithWork("q", stores, stores[0], primaryAnswersSecondaryFails(t, "[]"))
	if err == nil {
		t.Fatalf("bestStoreWithWork = (%q, %+v, nil); a fan-out with an unread store and no work anywhere must be an error, not \"no work\"", out, selected)
	}
	if !strings.Contains(err.Error(), "riga") {
		t.Errorf("error = %q, want the failed store named", err)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("output = %q, want none: \"[]\" next to an error still reads as an empty queue to a careless caller", out)
	}
}

// TestHookDiscoveryReportsAFailedSecondaryStore is plain `gc hook`: the
// work-query-failed exit, not the no-work exit 1, with the store named.
func TestHookDiscoveryReportsAFailedSecondaryStore(t *testing.T) {
	stores := twoStoreFanOut()
	var stdout, stderr bytes.Buffer
	runner := hookDiscoveryRunner(stores, primaryAnswersSecondaryFails(t, "[]"), func(string, error) {}, &stderr)
	code := doHook("q", "", false, runner, &stdout, &stderr, hookVisibility{})
	if code != hookExitWorkQueryFailed {
		t.Fatalf("gc hook over a failed secondary store = exit %d, want %d; stdout=%q stderr=%s", code, hookExitWorkQueryFailed, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "[]") {
		t.Errorf("stdout = %q, want no empty-queue answer", stdout.String())
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the failed store named", stderr.String())
	}
}

// TestHookDiscoveryServesWorkAndLogsAFailedSecondaryStore: when another store
// does have work, serving it is right, but the unread store is still reported.
func TestHookDiscoveryServesWorkAndLogsAFailedSecondaryStore(t *testing.T) {
	stores := twoStoreFanOut()
	var stdout, stderr bytes.Buffer
	runner := hookDiscoveryRunner(stores, primaryAnswersSecondaryFails(t, routedCityRow), func(string, error) {}, &stderr)
	code := doHook("q", "", false, runner, &stdout, &stderr, hookVisibility{})
	if code != 0 {
		t.Fatalf("gc hook with work in the primary = exit %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "hw-city") {
		t.Errorf("stdout = %q, want the primary's work served", stdout.String())
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the unread store reported even though work was served", stderr.String())
	}
}

func secondaryFailureClaimOpts() hookClaimOptions {
	return hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		DrainAck:           true,
		JSON:               true,
	}
}

func zeroClaimRetryPacing(t *testing.T) {
	t.Helper()
	prev := hookClaimQueryRetryInterval
	hookClaimQueryRetryInterval = 0
	t.Cleanup(func() { hookClaimQueryRetryInterval = prev })
}

// TestClaimHookWorkDoesNotDrainWhenASecondaryStoreFails is `gc hook --claim`:
// exit 1 with nothing on stdout, never a no_work drain or a drain-ack.
func TestClaimHookWorkDoesNotDrainWhenASecondaryStoreFails(t *testing.T) {
	zeroClaimRetryPacing(t)
	stores := twoStoreFanOut()
	acked := false
	ops := hookClaimOps{
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			t.Fatal("nothing to claim, yet a claim ran")
			return beads.Bead{}, false, nil
		},
		DrainAck: func(io.Writer) error { acked = true; return nil },
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), ops, primaryAnswersSecondaryFails(t, "[]"), func(string, error) {}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("claim over a failed secondary store = %d, want 1; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no protocol result: a drain here retires a seat on a read that never happened", stdout.String())
	}
	if acked {
		t.Fatal("the seat acknowledged a drain over an unread store")
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the failed store named", stderr.String())
	}
}

// TestClaimHookWorkServesWorkAndLogsAFailedSecondaryStore: the claim takes the
// work it can see and still reports the store it could not read.
func TestClaimHookWorkServesWorkAndLogsAFailedSecondaryStore(t *testing.T) {
	zeroClaimRetryPacing(t)
	stores := twoStoreFanOut()
	ops := hookClaimOps{
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		EmitClaimRejected: func(string, string, string) {},
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			return nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("q", "city", stores[0].env, stores, secondaryFailureClaimOpts(), ops, primaryAnswersSecondaryFails(t, routedCityRow), func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claim with work in the primary = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "hw-city" {
		t.Fatalf("claim result = %+v, want work hw-city", result)
	}
	if !strings.Contains(stderr.String(), "riga") {
		t.Errorf("stderr = %q, want the unread store reported even though work was claimed", stderr.String())
	}
}
