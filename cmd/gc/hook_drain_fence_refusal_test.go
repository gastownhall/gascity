package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The drain fence on a city the storage gate refuses (prod-evidence item 1).
//
// On 2026-10-18 a gc binary without the five-store gate ran `gc hook` on
// maintainer-city, whose retained work store held 790 infrastructure beads the
// binding could not read. Every class store refused ("this city converged on
// binding "infra", and the retained work store holds 790 infrastructure
// bead(s) ..."), the drain-pending probe failed, and the fence failed OPEN:
// draining seats went on claiming. That refusal is not a store hiccup that
// clears on the next poll — it holds until an operator recovers the city — so
// failing open there switches the fence off fleet-wide for as long as the city
// stays refused. It must fail CLOSED. A transient store fault still fails open.

// strandedSplitCity is a converged split city whose retained work store holds an
// infrastructure bead the binding does not: the exact shape the storage gate
// refuses on every one-shot command until `gc storage recover-stranded` runs.
func strandedSplitCity(t *testing.T) string {
	t.Helper()
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the work store: %v", err)
	}
	if _, err := work.Create(beads.Bead{Title: "auto-handoff", Type: "message", Labels: []string{"gc:message"}}); err != nil {
		t.Fatalf("planting the stranded infra bead: %v", err)
	}
	_ = closeBeadStoreHandle(work)
	return cityPath
}

func TestHookDrainPendingProbeCarriesTheStorageGateRefusal(t *testing.T) {
	strandedSplitCity(t)

	pending, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil {
		t.Fatalf("the probe answered pending=%v on a city the storage gate refuses", pending)
	}
	if !storeref.IsStandingRefusal(err) {
		t.Fatalf("the probe's error lost the standing-refusal mark, so the fence cannot tell it from a hiccup: %v", err)
	}
	if !strings.Contains(err.Error(), "infrastructure bead") {
		t.Errorf("the probe's error is not the storage gate's refusal: %v", err)
	}
}

func TestHookClaimDrainFenceFailsClosedOnAStorageGateRefusal(t *testing.T) {
	strandedSplitCity(t)
	var emitted []error
	old := hookEmitDrainFenceUnavailable
	hookEmitDrainFenceUnavailable = func(_ io.Writer, _, _ string, err error) { emitted = append(emitted, err) }
	t.Cleanup(func() { hookEmitDrainFenceUnavailable = old })

	e := newDrainPendingClaimEnv()
	ops := e.ops()
	ops.DrainPending = hookSessionDrainPending

	code := doHookClaim("query", "/rig", e.opts(true), ops, &e.stdout, &e.stderr)

	if code != hookExitWorkQueryFailed {
		t.Errorf("code = %d, want %d (fence failed closed: unknown, not empty and not drained); stderr=%s", code, hookExitWorkQueryFailed, e.stderr.String())
	}
	if e.queries != 0 || len(e.claimed) != 0 {
		t.Errorf("a refused city's seat ran %d work queries and claimed %v; the fence must fail closed", e.queries, e.claimed)
	}
	if e.drainAcked {
		t.Error("a storage refusal acknowledged a drain; the seat's row was never read, so it is not known to be draining")
	}
	if !strings.Contains(e.stderr.String(), hookStoreUnavailableToken) || !strings.Contains(e.stderr.String(), "infrastructure bead") {
		t.Errorf("stderr = %q, want the store-unavailable token and the gate's refusal", e.stderr.String())
	}
	if len(emitted) != 1 || !storeref.IsStandingRefusal(emitted[0]) {
		t.Errorf("emitted %v, want one fence event carrying the refusal", emitted)
	}
}

func TestHookDiscoveryDrainFenceFailsClosedOnAStorageGateRefusal(t *testing.T) {
	strandedSplitCity(t)
	var emitted int
	old := hookEmitDrainFenceUnavailable
	hookEmitDrainFenceUnavailable = func(io.Writer, string, string, error) { emitted++ }
	t.Cleanup(func() { hookEmitDrainFenceUnavailable = old })

	e := newDiscoveryDrainEnv()
	opts := hookClaimOptions{Env: []string{"GC_SESSION_ID=" + drainPendingTestSessionID, "GC_TEMPLATE=beads--gc__run-operator"}}
	ops := hookClaimOps{DrainPending: hookSessionDrainPending}

	code := doHookDiscovery("bd ready", "/rig", false, opts, ops, e.runner(), &e.stdout, &e.stderr, hookVisibility{})

	if code != hookExitWorkQueryFailed {
		t.Errorf("code = %d, want %d (fence failed closed); stderr=%s", code, hookExitWorkQueryFailed, e.stderr.String())
	}
	if e.queries != 0 || e.stdout.Len() != 0 {
		t.Errorf("discovery ran %d queries and printed %q on a refused city; the fence must fail closed", e.queries, e.stdout.String())
	}
	if !strings.Contains(e.stderr.String(), hookStoreUnavailableToken) {
		t.Errorf("stderr = %q, want the store-unavailable token", e.stderr.String())
	}
	if emitted != 1 {
		t.Errorf("emitted %d fence events, want 1", emitted)
	}
}

// The event says which way the fence went: an operator reading it must not be
// told "failed open" about a fence that refused every claim.
func TestDrainFenceUnavailableReasonNamesTheDirection(t *testing.T) {
	transient := drainFenceUnavailableReason(errors.New("database is locked"))
	if !strings.Contains(transient, "failed open") {
		t.Errorf("transient reason = %q, want failed open", transient)
	}
	refused := drainFenceUnavailableReason(standingStorageRefusal{err: errors.New("retained work store holds 1 infrastructure bead(s)")})
	if !strings.Contains(refused, "failed closed") || strings.Contains(refused, "failed open") {
		t.Errorf("refusal reason = %q, want failed closed", refused)
	}
}

// Review probe I: a TRANSIENT read failure inside the gate (a hosted work
// store that times out during the containment check) is not a standing
// verdict about the city. The class stores still refuse for this attempt, but
// the drain fence keeps its fail-open contract and the refusal is not
// memoized: the next resolution, after the store answers, serves.
func TestHookDrainFenceFailsOpenOnATransientGateFault(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) {
		return nil, errors.New(`Get "https://hosted-bd/v1/issues": dial tcp: i/o timeout`)
	}
	t.Cleanup(func() { openInfraMigrationSource = prev })

	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil {
		t.Fatal("the probe answered while the gate could not check the city")
	}
	if !isTransientStorageGateFault(err) {
		t.Fatalf("the gate's read failure is not marked transient: %v", err)
	}
	if hookDrainProbeFailsClosed(err) {
		t.Errorf("a transient gate fault fails the drain fence closed: %v", err)
	}

	e := newDrainPendingClaimEnv()
	ops := e.ops()
	ops.DrainPending = hookSessionDrainPending
	if code := doHookClaim("query", "/rig", e.opts(true), ops, &e.stdout, &e.stderr); code != 0 || len(e.claimed) != 1 {
		t.Errorf("claim on a transient gate fault = (%d, claimed %v), want the fail-open claim; stderr=%s", code, e.claimed, e.stderr.String())
	}

	// The store answers again: the same process now resolves served routes.
	openInfraMigrationSource = prev
	if _, err := cliSessionStore(beads.NewMemStore(), nil, cityPath).List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Errorf("after the store recovered the session class still refuses (a transient fault was memoized): %v", err)
	}
}

// The control: a standing refusal (the stranded city) stays closed.
func TestStandingGateRefusalIsNotTransient(t *testing.T) {
	strandedSplitCity(t)
	_, err := hookSessionDrainPending(drainPendingTestSessionID)
	if err == nil || isTransientStorageGateFault(err) || !hookDrainProbeFailsClosed(err) {
		t.Errorf("stranded-city refusal: err=%v transient=%v failsClosed=%v; want a standing, fail-closed refusal", err, isTransientStorageGateFault(err), hookDrainProbeFailsClosed(err))
	}
}

// Upgrading a city an earlier build migrated. That build's classifier called a
// startup-health episode and an own-session wait work, so its copy left them in
// the work store, unlisted. They are not strands: a fence that failed closed
// over them would stop every seat's claims on the first hook after the
// upgrade. A real strand under the same earlier manifest still fails closed
// (the control row).
func TestAnEarlierMigrationsReclassifiedRowsKeepTheFenceOpen(t *testing.T) {
	cases := []struct {
		name        string
		plant       beads.Bead
		failsClosed bool
	}{
		{"startup-health episode", reclassifiedShapes()[0], false},
		{"own-session wait without gc:wait", reclassifiedShapes()[1], false},
		{"mail message stranded after the copy", beads.Bead{Title: "auto-handoff", Type: "message"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, cfg := migratedOneShotCLICity(t)
			captureCLIStorageStderr(t)
			writeLegacyManifest(t, mustResolveInfraTarget(t, cityPath, cfg))
			if err := closeCLIStorageRoutes(); err != nil {
				t.Fatal(err)
			}
			if _, err := hookSessionDrainPending(drainPendingTestSessionID); err != nil && hookDrainProbeFailsClosed(err) {
				t.Fatalf("control: the earlier build's city is refused before anything is planted: %v", err)
			}
			if err := closeCLIStorageRoutes(); err != nil {
				t.Fatal(err)
			}
			work, err := openCityStoreAt(cityPath)
			if err != nil {
				t.Fatalf("opening the work store: %v", err)
			}
			if _, err := work.Create(tc.plant); err != nil {
				t.Fatalf("planting %+v: %v", tc.plant, err)
			}
			_ = closeBeadStoreHandle(work)

			_, err = hookSessionDrainPending(drainPendingTestSessionID)
			if got := err != nil && hookDrainProbeFailsClosed(err); got != tc.failsClosed {
				t.Errorf("drain fence fails closed = %v, want %v (probe error: %v)", got, tc.failsClosed, err)
			}
		})
	}
}
