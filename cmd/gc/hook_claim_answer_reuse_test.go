package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// One `gc hook --claim` invocation reads each store at most once to select,
// plus one fresh re-read of each store a claim is about to act on
// (hookStoreAnswers). These rows pin the per-store read counts on the paths
// that used to re-read every store: a lost claim race, a selection that
// emptied before the claim, and a retried read failure.

// answerReuseStores is a four-leg fan-out: the agent's own store first, then
// three rig stores.
func answerReuseStores() []hookStore {
	return []hookStore{
		{dir: "/city", env: []string{"GC_STORE=city"}, storeRef: "city:c"},
		{dir: "/riga", env: []string{"GC_STORE=riga"}, rig: "riga", storeRef: "rig:riga"},
		{dir: "/rigb", env: []string{"GC_STORE=rigb"}, rig: "rigb", storeRef: "rig:rigb"},
		{dir: "/rigc", env: []string{"GC_STORE=rigc"}, rig: "rigc", storeRef: "rig:rigc"},
	}
}

// routedRowWithPriorityJSON is one ready, route-matched row at priority p.
func routedRowWithPriorityJSON(id string, p int) string {
	rows, _ := json.Marshal([]beads.Bead{{
		ID:       id,
		Status:   "open",
		Type:     "task",
		Priority: &p,
		Metadata: map[string]string{"gc.routed_to": "rig/worker"},
	}})
	return string(rows)
}

// losingClaimOps is the harness's ops with a Claim that loses every row in lost
// to another claimant and records the dir of each claim it wins.
func losingClaimOps(h *failureHookHarness, lost map[string]bool, wonDirs *[]string) hookClaimOps {
	ops := h.ops()
	ops.Claim = func(_ context.Context, dir string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		if lost[beadID] {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "gc__worker-2", Metadata: map[string]string{"gc.routed_to": "rig/worker"}}, false, nil
		}
		*wonDirs = append(*wonDirs, dir)
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "rig/worker"}}, true, nil
	}
	return ops
}

func runAnswerReuseClaim(t *testing.T, h *failureHookHarness, stores []hookStore, ops hookClaimOps) (hookClaimJSONResult, int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("work-query", stores[0].dir, stores[0].env, stores, failureClaimOptions(), ops,
		h.run, h.emitFailure, &stdout, &stderr)
	var result hookClaimJSONResult
	if trimmed := strings.TrimSpace(stdout.String()); trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &result); err != nil {
			t.Fatalf("decoding claim result %q: %v", trimmed, err)
		}
	}
	return result, code, stderr.String()
}

func assertStoreReads(t *testing.T, h *failureHookHarness, want map[string]int) {
	t.Helper()
	for dir, n := range want {
		if got := h.calls[dir]; got != n {
			t.Errorf("reads of %s = %d, want %d (all reads: %v)", dir, got, n, h.calls)
		}
	}
}

// AC-1: a lost race on the best-ranked store reselects from the answers this
// invocation already holds; only the newly picked store is read again, and
// the claim acts on that fresh read.
func TestClaimHookWorkLostRaceReselectsFromSavedAnswers(t *testing.T) {
	withFastClaimRetries(t)
	stores := answerReuseStores()
	h := newFailureHookHarness()
	h.script("/riga", hookRunnerAnswer{out: routedRowWithPriorityJSON("ra-1", 1)})
	h.script("/rigb",
		hookRunnerAnswer{out: routedRowWithPriorityJSON("rb-1", 2)},
		hookRunnerAnswer{out: routedRowWithPriorityJSON("rb-2", 2)},
	)
	var won []string
	ops := losingClaimOps(h, map[string]bool{"ra-1": true}, &won)

	result, code, stderr := runAnswerReuseClaim(t, h, stores, ops)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if result.Action != "work" || result.BeadID != "rb-2" {
		t.Fatalf("result = %+v, want rb-2 claimed from rigb's fresh re-read", result)
	}
	if len(won) != 1 || won[0] != "/rigb" {
		t.Fatalf("won claims ran against %v, want [/rigb]", won)
	}
	assertStoreReads(t, h, map[string]int{"/city": 1, "/riga": 2, "/rigb": 2, "/rigc": 1})
}

// AC-2: the selected store emptied between discovery and its claim-time
// re-read. The fallback reselects from saved answers, and the store it picks is
// re-read before the claim acts on it, so the claim takes the fresh row against
// that store's dir and env.
func TestClaimHookWorkEmptiedSelectionFallsBackWithoutAFullPass(t *testing.T) {
	withFastClaimRetries(t)
	stores := answerReuseStores()[:3]
	h := newFailureHookHarness()
	h.script("/riga",
		hookRunnerAnswer{out: routedRowWithPriorityJSON("ra-1", 1)},
		hookRunnerAnswer{out: "[]"},
	)
	h.script("/rigb",
		hookRunnerAnswer{out: routedRowWithPriorityJSON("rb-1", 2)},
		hookRunnerAnswer{out: routedRowWithPriorityJSON("rb-2", 2)},
	)
	var claimEnv []string
	ops := h.ops()
	var won []string
	ops.Claim = func(_ context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
		won = append(won, dir+"#"+beadID)
		claimEnv = env
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "rig/worker"}}, true, nil
	}

	result, code, stderr := runAnswerReuseClaim(t, h, stores, ops)

	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if result.Action != "work" || result.BeadID != "rb-2" {
		t.Fatalf("result = %+v, want rb-2 from rigb's second read", result)
	}
	if len(won) != 1 || won[0] != "/rigb#rb-2" {
		t.Fatalf("claims = %v, want [/rigb#rb-2]", won)
	}
	if !strings.Contains(strings.Join(claimEnv, " "), "GC_STORE=rigb") {
		t.Fatalf("claim env = %v, want rigb's env", claimEnv)
	}
	assertStoreReads(t, h, map[string]int{"/city": 1, "/riga": 2, "/rigb": 2})
}

// AC-3: a retried read failure re-runs only the store whose read failed; the
// stores that already answered are not read again. A read that recovers is
// claimed (after its claim-time re-read), and one that never recovers keeps
// the failed-read contract: non-zero exit, no result, no drain-ack, and one
// failure event naming the store.
func TestClaimHookWorkReadRetryRereadsOnlyFailedStores(t *testing.T) {
	readErr := errors.New("bd ready: exit status 1")
	t.Run("transient", func(t *testing.T) {
		withFastClaimRetries(t)
		stores := answerReuseStores()[:3]
		h := newFailureHookHarness()
		h.script("/riga",
			hookRunnerAnswer{err: readErr},
			hookRunnerAnswer{out: routedRowJSON("ra-1")},
		)

		result, code, stderr := runAnswerReuseClaim(t, h, stores, h.ops())

		if code != 0 {
			t.Fatalf("exit = %d, want 0 after the read recovered; stderr=%s", code, stderr)
		}
		if result.Action != "work" || result.BeadID != "ra-1" {
			t.Fatalf("result = %+v, want the recovered riga row claimed", result)
		}
		assertStoreReads(t, h, map[string]int{"/city": 1, "/riga": 3, "/rigb": 1})
	})
	t.Run("persistent", func(t *testing.T) {
		withFastClaimRetries(t)
		stores := answerReuseStores()[:3]
		h := newFailureHookHarness()
		h.script("/riga", hookRunnerAnswer{err: readErr})

		result, code, stderr := runAnswerReuseClaim(t, h, stores, h.ops())

		if code != 1 {
			t.Fatalf("exit = %d, want 1 for a persistent secondary read failure; stderr=%s", code, stderr)
		}
		if result.Action != "" || h.drained {
			t.Fatalf("result = %+v drained = %v, want no result and no drain-ack", result, h.drained)
		}
		if !strings.HasPrefix(stderr, "gc hook --claim: ") || !strings.Contains(stderr, "exit status 1") || strings.Contains(stderr, hookStoreUnavailableToken) {
			t.Fatalf("stderr = %q, want the ordinary failed-read report without the store-unavailable token", stderr)
		}
		if len(h.events) != 1 || !strings.Contains(h.events[0].Message, `rig "riga"`) {
			t.Fatalf("events = %+v, want exactly one failure event naming riga", h.events)
		}
		assertStoreReads(t, h, map[string]int{"/city": 1, "/riga": 1 + hookClaimQueryRetryAttempts, "/rigb": 1})
	})
}
