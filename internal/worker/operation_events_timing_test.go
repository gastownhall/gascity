package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestOperationEventDurationExcludesPostSealWork pins the timing contract
// the cockpit depends on: DurationMs times the wrapped operation only.
// Message/Nudge stamp best-effort usage telemetry (bead reads, transcript
// tail parse, a bead-store write) between completion and emission, and that
// I/O must never inflate the operation's duration — the timing is sealed
// first, telemetry stamps, then the event emits.
func TestOperationEventDurationExcludesPostSealWork(t *testing.T) {
	h, _, recorder := newUsageEventHandle(t)
	defer h.Stop(context.Background()) //nolint:errcheck

	event := h.beginOperationEvent(context.Background(), workerOperationMessage)
	event.sealTiming(nil)
	sealed := event.payload.DurationMs

	// Simulated telemetry cost after the seal: must not move the clock.
	time.Sleep(75 * time.Millisecond)
	event.emit()

	if len(recorder.events) == 0 {
		t.Fatal("no event recorded")
	}
	var payload operationEventPayload
	if err := json.Unmarshal(recorder.events[len(recorder.events)-1].Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.DurationMs != sealed {
		t.Errorf("DurationMs = %d, want sealed value %d (emit must not re-time)", payload.DurationMs, sealed)
	}
	if payload.DurationMs >= 75 {
		t.Errorf("DurationMs = %dms, want < 75ms — post-seal work leaked into the operation duration", payload.DurationMs)
	}
	if payload.Result != operationResultSucceeded {
		t.Errorf("Result = %q, want %q (seal must also fix the result)", payload.Result, operationResultSucceeded)
	}
}
