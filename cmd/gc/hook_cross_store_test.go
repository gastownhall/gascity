package main

import (
	"errors"
	"testing"
)

var errTestStoreTimeout = errors.New("store timed out")

func TestFirstStoreWithWorkReturnsFirstStoreThatHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}, {dir: "rigb"}}
	var calls []string
	run := func(_, dir string, _ []string) (string, error) {
		calls = append(calls, dir)
		if dir == "riga" {
			return `[{"id":"va-1"}]`, nil
		}
		return `[]`, nil
	}
	out, err := firstStoreWithWork("q", stores, run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-1"}]` {
		t.Fatalf("out = %q, want riga work", out)
	}
	// Stops at the first store with work — does not query rigb.
	if len(calls) != 2 || calls[0] != "city" || calls[1] != "riga" {
		t.Fatalf("calls = %v, want [city riga]", calls)
	}
}

func TestFirstStoreWithWorkReturnsLastWhenNoneHasWork(t *testing.T) {
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, _ string, _ []string) (string, error) { return `[]`, nil }
	out, err := firstStoreWithWork("q", stores, run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want []", out)
	}
}

func TestFirstStoreWithWorkSurfacesOwnStoreErrorWhenNoWork(t *testing.T) {
	// The agent's own store (first) timing out must be surfaced even if a
	// federated rig store returns no work — otherwise emitCityWorkQueryFailure
	// never fires and a transient timeout is silently downgraded to "no work".
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return "", errTestStoreTimeout
		}
		return `[]`, nil
	}
	if _, err := firstStoreWithWork("q", stores, run); !errors.Is(err, errTestStoreTimeout) {
		t.Fatalf("own-store error must be surfaced when no store has work; got %v", err)
	}
}

func TestFirstStoreWithWorkIgnoresRigStoreErrorWhenOwnStoreHasNoWork(t *testing.T) {
	// A flaky federated rig store must not wedge the hook: when the agent's own
	// store is healthy (no work), a rig-store error is best-effort and dropped.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[]`, nil
		}
		return "", errTestStoreTimeout
	}
	out, err := firstStoreWithWork("q", stores, run)
	if err != nil {
		t.Fatalf("rig-store error must not surface when own store is healthy; got %v", err)
	}
	if out != `[]` {
		t.Fatalf("out = %q, want city store's no-work output", out)
	}
}

func TestFirstStoreWithWorkSkipsStoreWithOnlyUnreadyRows(t *testing.T) {
	// A store whose only row is dep-blocked is NOT a hit; federation moves on.
	stores := []hookStore{{dir: "city"}, {dir: "riga"}}
	run := func(_, dir string, _ []string) (string, error) {
		if dir == "city" {
			return `[{"id":"x","blocked_by":[{"status":"open"}]}]`, nil
		}
		return `[{"id":"va-2"}]`, nil
	}
	out, err := firstStoreWithWork("q", stores, run)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != `[{"id":"va-2"}]` {
		t.Fatalf("out = %q, want riga work (city row was unready)", out)
	}
}
