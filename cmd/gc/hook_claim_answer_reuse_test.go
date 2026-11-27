package main

import (
	"bytes"
	"context"
	"encoding/json"
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
