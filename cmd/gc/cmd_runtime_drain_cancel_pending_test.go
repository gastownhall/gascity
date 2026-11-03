package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ci-soai behavior 5 (mayor, 2026-11-02T07:55Z): the witness's two drain
// attempts on stuck start-pending pool mints were refused ("not running"),
// leaving zero-bead seats no operator path could retire. A v2 drain of a
// not-running session whose row still reads start-pending/creating now
// cancels the pending start instead: the claimed shape through the
// reconciler's own fenced rollback (closed failed-create), the claimless
// shape to asleep by fenced CAS. A start in flight is reported and left
// (the stop sweep's flock rule); every other not-running shape keeps the
// generic refusal.

// stampStartPendingRow turns drainAckCity's awake worker row into a
// start-pending one, optionally holding the pending-create claim.
func stampStartPendingRow(t *testing.T, store interface {
	SetMetadataBatch(id string, batch map[string]string) error
}, id string, claimed bool) {
	t.Helper()
	batch := map[string]string{
		"state": string(sessionpkg.StateStartPending),
	}
	if claimed {
		batch["pending_create_claim"] = "true"
		batch["pending_create_started_at"] = "2026-11-02T07:43:00Z"
	}
	if err := store.SetMetadataBatch(id, batch); err != nil {
		t.Fatalf("stamp start-pending row: %v", err)
	}
}

func TestDrainCancelsClaimedStartPending(t *testing.T) {
	cityPath, store, id := drainAckCity(t, "v2")
	stampStartPendingRow(t, store, id, true)
	dops := newFakeDrainOps()
	sp := runtime.NewFake() // never started: the probe answers not running
	rec := events.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(dops, sp, rec, target, false, &stdout, &stderr); code != 0 {
		t.Fatalf("drain = %d, want 0 with the pending start canceled; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Canceled pending start of session 'worker'") {
		t.Errorf("stdout %q, want the cancel line", stdout.String())
	}
	if dops.draining["worker"] {
		t.Error("env drain flag set on a session that never ran")
	}
	b := freshDrainAckRow(t, cityPath, id)
	if b.Status != "closed" {
		t.Errorf("row status = %q, want closed: the reconciler's own rollback closes a claimed pending create", b.Status)
	}
	if got := b.Metadata["state"]; got != string(sessionpkg.StateFailedCreate) {
		t.Errorf("row state = %q, want failed-create", got)
	}
	if b.Metadata["pending_create_claim"] != "" {
		t.Errorf("row keeps pending_create_claim %q, want it cleared", b.Metadata["pending_create_claim"])
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionStopped {
		t.Errorf("events = %v, want one session.stopped", rec.Events)
	}
}

func TestDrainCancelsClaimedStartPendingJSON(t *testing.T) {
	cityPath, store, id := drainAckCity(t, "v2")
	stampStartPendingRow(t, store, id, true)
	sp := runtime.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(newFakeDrainOps(), sp, events.NewFake(), target, true, &stdout, &stderr); code != 0 {
		t.Fatalf("drain = %d; stderr=%q", code, stderr.String())
	}
	var line struct {
		OK     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &line); err != nil {
		t.Fatalf("stdout %q is not one JSON line: %v", stdout.String(), err)
	}
	if !line.OK || line.Status != "start-pending-canceled" {
		t.Errorf("json = (ok %v, status %q), want ok with start-pending-canceled", line.OK, line.Status)
	}
}

func TestDrainCancelsClaimlessStartPending(t *testing.T) {
	cityPath, store, id := drainAckCity(t, "v2")
	stampStartPendingRow(t, store, id, false)
	sp := runtime.NewFake()
	rec := events.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(newFakeDrainOps(), sp, rec, target, false, &stdout, &stderr); code != 0 {
		t.Fatalf("drain = %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	b := freshDrainAckRow(t, cityPath, id)
	if b.Status != "open" {
		t.Errorf("row status = %q, want open: a claimless pending create is parked, not closed", b.Status)
	}
	if got := b.Metadata["state"]; got != string(sessionpkg.StateAsleep) {
		t.Errorf("row state = %q, want asleep (the reconciler's dead-row projection)", got)
	}
	if got := b.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonRuntimeMissing) {
		t.Errorf("row sleep_reason = %q, want runtime-missing", got)
	}
}

func TestDrainStartPendingInFlightRefuses(t *testing.T) {
	cityPath, store, id := drainAckCity(t, "v2")
	stampStartPendingRow(t, store, id, false)
	_, release, err := tryRuntimeLease(nil, cityPath, "worker", "", 0)
	if err != nil {
		t.Fatalf("hold the runtime lease: %v", err)
	}
	defer release()
	sp := runtime.NewFake()
	rec := events.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(newFakeDrainOps(), sp, rec, target, false, &stdout, &stderr); code != 1 {
		t.Fatalf("drain = %d, want 1 with the start in flight; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "start still in flight") {
		t.Errorf("stderr %q, want the in-flight notice", stderr.String())
	}
	b := freshDrainAckRow(t, cityPath, id)
	if b.Status != "open" || b.Metadata["state"] != string(sessionpkg.StateStartPending) {
		t.Errorf("row moved under a live start (status %q state %q), want it untouched", b.Status, b.Metadata["state"])
	}
}

func TestDrainNotRunningAwakeStillRefused(t *testing.T) {
	cityPath, _, id := drainAckCity(t, "v2") // the fixture row is awake
	sp := runtime.NewFake()
	rec := events.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(newFakeDrainOps(), sp, rec, target, false, &stdout, &stderr); code != 1 {
		t.Fatalf("drain = %d, want the generic refusal; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "is not running") {
		t.Errorf("stderr %q, want the not-running refusal", stderr.String())
	}
	b := freshDrainAckRow(t, cityPath, id)
	if b.Metadata["state"] != string(sessionpkg.StateAwake) || b.Metadata[sessionpkg.OperatorDrainKey] != "" {
		t.Errorf("row moved on a plain refusal (state %q operator drain %q), want it untouched", b.Metadata["state"], b.Metadata[sessionpkg.OperatorDrainKey])
	}
	if len(rec.Events) != 0 {
		t.Errorf("events = %v, want none on a refusal", rec.Events)
	}
}

func TestDrainLegacyCityNeverCancelsPendingStart(t *testing.T) {
	cityPath, store, id := drainAckCity(t, "") // legacy: no [daemon] reconciler
	stampStartPendingRow(t, store, id, false)
	sp := runtime.NewFake()
	target := sessionRuntimeTarget{cityPath: cityPath, display: "worker", sessionName: "worker"}
	var stdout, stderr bytes.Buffer
	if code := runtimeDrain(newFakeDrainOps(), sp, events.NewFake(), target, false, &stdout, &stderr); code != 1 {
		t.Fatalf("drain = %d, want the legacy refusal; stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "is not running") {
		t.Errorf("stderr %q, want the not-running refusal", stderr.String())
	}
	if b := freshDrainAckRow(t, cityPath, id); b.Metadata["state"] != string(sessionpkg.StateStartPending) {
		t.Errorf("legacy row state = %q, want the start-pending row untouched", b.Metadata["state"])
	}
}
