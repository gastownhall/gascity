package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionKillTakesTheRuntimeLease (O3): a kill waits for a held lease,
// then fails retryably without fencing or stopping; --force overrides a hung
// flock holder only once its record has expired.
func TestCmdSessionKillTakesTheRuntimeLease(t *testing.T) {
	const identity = killPokeSessionIdentity
	store, bead, city := newKillPokeSession(t, "s-gc-kill-lease")
	stubKillPoke(t)
	defer sessionpkg.SetOperatorLeaseWaitForTest(300 * time.Millisecond)()
	inner := wrapKillPokeProvider(t, &killHookProvider{})
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{
		City: city, Name: "s-gc-kill-lease", ID: bead.ID, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{identity}, &stdout, &stderr, false, force); code != 1 || !strings.Contains(stderr.String(), "session is starting, retry") {
			t.Fatalf("kill (force=%v) under an unexpired lease = %d, %q; want a retryable refusal", force, code, stderr.String())
		}
	}
	if !inner.IsRunning("s-gc-kill-lease") || mustGetBead(t, store, bead.ID).Metadata["state_reason"] != "" {
		t.Fatal("a refused kill fenced the row or stopped the runtime")
	}
	held.Release()
	hung, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "s-gc-kill-lease"})
	if err != nil {
		t.Fatal(err)
	}
	defer hung.Release()
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{
		sessionpkg.RuntimeLeaseHolderKey: "hung/1/n", sessionpkg.RuntimeLeaseEpochKey: "5", sessionpkg.RuntimeLeaseTTLKey: "60",
		sessionpkg.RuntimeLeaseExpiresKey: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKill([]string{identity}, &stdout, &stderr); code != 1 {
		t.Fatalf("kill past a hung flock holder without --force = %d, want refused", code)
	}
	stderr.Reset()
	released := false
	sessionKillPokeController = func(string, reconcilekey.Key) error {
		released = mustGetBead(t, store, bead.ID).Metadata[sessionpkg.RuntimeLeaseHolderKey] == ""
		return nil
	}
	if code := cmdSessionKillWithForce([]string{identity}, &stdout, &stderr, false, true); code != 0 || inner.IsRunning("s-gc-kill-lease") {
		t.Fatalf("kill --force past a hung holder with an expired record = %d, %q", code, stderr.String())
	}
	if epoch := mustGetBead(t, store, bead.ID).Metadata[sessionpkg.RuntimeLeaseEpochKey]; epoch != "6" {
		t.Fatalf("record epoch after the override = %q, want 6", epoch)
	}
	if !released {
		t.Fatal("the kill still held its runtime lease when it poked the controller")
	}
	if log, err := os.ReadFile(filepath.Join(city, ".gc", "events.jsonl")); err != nil || !strings.Contains(string(log), "killed past a hung runtime lease holder") {
		t.Fatalf("events = %q (%v), want the override recorded", log, err)
	}
}

// TestKillRuntimeLeaseFallsBackToTheFlock: a kill whose row record cannot be
// taken for a reason other than busy (here the runtime is not the row's) is
// not held hostage: it runs under the name's flock alone, with a warning.
func TestKillRuntimeLeaseFallsBackToTheFlock(t *testing.T) {
	store, bead, city := newKillPokeSession(t, "s-gc-kill-flock")
	var stderr bytes.Buffer
	req := sessionpkg.RuntimeLeaseRequest{City: city, Name: "not-the-rows-runtime", ID: bead.ID, TTL: time.Minute}
	lease, overridden, err := killRuntimeLease(context.Background(), sessionFrontDoor(store), req, false, &stderr)
	if err != nil || overridden || lease.Epoch() != 0 || !strings.Contains(stderr.String(), "under the name's flock alone") {
		t.Fatalf("kill lease = %v, %v, %v (stderr %q), want the flock alone", lease, overridden, err, stderr.String())
	}
	defer lease.Release()
	if _, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "not-the-rows-runtime"}); !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) {
		t.Fatalf("the fallback holds no flock: %v", err)
	}
}
