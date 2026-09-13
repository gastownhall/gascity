package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type recordingEventRecorder struct {
	events []events.Event
}

func (r *recordingEventRecorder) Record(e events.Event) {
	r.events = append(r.events, e)
}

func TestSessionHandleStartRecordsWorkerOperationEvent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, mgr := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	event := lastRecordedWorkerOperation(t, recorder)
	if event.Type != events.WorkerOperation {
		t.Fatalf("event.Type = %q, want %q", event.Type, events.WorkerOperation)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationStart); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.Result, operationResultSucceeded; got != want {
		t.Fatalf("payload.Result = %q, want %q", got, want)
	}
	if payload.SessionID == "" {
		t.Fatal("payload.SessionID is empty")
	}
	info, err := mgr.Get(payload.SessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", payload.SessionID, err)
	}
	if got, want := payload.SessionName, info.SessionName; got != want {
		t.Fatalf("payload.SessionName = %q, want %q", got, want)
	}
	if payload.OpID == "" {
		t.Fatal("payload.OpID is empty")
	}
}

func TestSessionHandleMessageRecordsQueuedState(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	recorder.events = nil

	result, err := handle.Message(context.Background(), MessageRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Message: %v", err)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationMessage); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if payload.Queued == nil {
		t.Fatal("payload.Queued is nil")
	}
	if got, want := *payload.Queued, result.Queued; got != want {
		t.Fatalf("payload.Queued = %v, want %v", got, want)
	}
	if got, want := payload.Result, operationResultSucceeded; got != want {
		t.Fatalf("payload.Result = %q, want %q", got, want)
	}
	if !bytes.Contains(lastRecordedWorkerOperation(t, recorder).Payload, []byte(`"queued":false`)) {
		t.Fatalf("payload JSON = %s, want explicit queued=false", string(lastRecordedWorkerOperation(t, recorder).Payload))
	}
}

func TestSessionHandleHistoryRecordsFailureEvent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	if _, err := handle.History(context.Background(), HistoryRequest{}); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("History err = %v, want %v", err, ErrHistoryUnavailable)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationHistory); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.Result, operationResultFailed; got != want {
		t.Fatalf("payload.Result = %q, want %q", got, want)
	}
	if payload.Error == "" {
		t.Fatal("payload.Error is empty")
	}
}

func TestSessionHandleHistoryOmitsEventWhenOperationEventsSuppressed(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)

	if _, err := handle.History(WithoutOperationEvents(context.Background()), HistoryRequest{}); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("History err = %v, want %v", err, ErrHistoryUnavailable)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("recorded %d events, want none for suppressed history", len(recorder.events))
	}
}

func TestSessionHandleNudgeRecordsDeliveredFalse(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, _ := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)
	if _, err := handle.Create(context.Background(), CreateModeDeferred); err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}
	recorder.events = nil

	result, err := handle.Nudge(context.Background(), NudgeRequest{
		Text:     "queued reminder",
		Delivery: NudgeDeliveryWaitIdle,
		Source:   "mail",
		Wake:     NudgeWakeLiveOnly,
	})
	if err != nil {
		t.Fatalf("Nudge(wait_idle live_only): %v", err)
	}
	if result.Delivered {
		t.Fatal("Nudge(wait_idle live_only) Delivered = true, want false")
	}

	event := lastRecordedWorkerOperation(t, recorder)
	var payload operationEventPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if payload.Delivered == nil {
		t.Fatal("payload.Delivered is nil")
	}
	if got, want := *payload.Delivered, result.Delivered; got != want {
		t.Fatalf("payload.Delivered = %v, want %v", got, want)
	}
	if !bytes.Contains(event.Payload, []byte(`"delivered":false`)) {
		t.Fatalf("payload JSON = %s, want explicit delivered=false", string(event.Payload))
	}
}

func TestSessionHandleCloseKeepsRuntimeSessionNameInWorkerOperationEvent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	handle, _, _, mgr := newTestSessionHandleWithRecorder(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	}, recorder)
	if err := handle.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := mgr.Get(handle.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", handle.sessionID, err)
	}
	recorder.events = nil

	if err := handle.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.SessionName, info.SessionName; got != want {
		t.Fatalf("payload.SessionName = %q, want %q", got, want)
	}
}

func TestRuntimeHandleInterruptRecordsWorkerOperationEvent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "claude",
		Transport:    "tmux-cli",
		Recorder:     recorder,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	if err := handle.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationInterrupt); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.Result, operationResultSucceeded; got != want {
		t.Fatalf("payload.Result = %q, want %q", got, want)
	}
	if got, want := payload.SessionName, "legacy-worker"; got != want {
		t.Fatalf("payload.SessionName = %q, want %q", got, want)
	}
	if got, want := payload.Provider, "claude"; got != want {
		t.Fatalf("payload.Provider = %q, want %q", got, want)
	}
}

func TestRuntimeHandleHistoryRecordsFailureEvent(t *testing.T) {
	recorder := &recordingEventRecorder{}
	sp := runtime.NewFake()
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:     sp,
		SessionName:  "legacy-worker",
		ProviderName: "claude",
		Transport:    "tmux-cli",
		Recorder:     recorder,
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}

	if _, err := handle.History(context.Background(), HistoryRequest{}); !errors.Is(err, ErrHistoryUnavailable) {
		t.Fatalf("History err = %v, want %v", err, ErrHistoryUnavailable)
	}

	var payload operationEventPayload
	if err := json.Unmarshal(lastRecordedWorkerOperation(t, recorder).Payload, &payload); err != nil {
		t.Fatalf("Unmarshal(payload): %v", err)
	}
	if got, want := payload.Operation, string(workerOperationHistory); got != want {
		t.Fatalf("payload.Operation = %q, want %q", got, want)
	}
	if got, want := payload.Result, operationResultFailed; got != want {
		t.Fatalf("payload.Result = %q, want %q", got, want)
	}
	if payload.Error == "" {
		t.Fatal("payload.Error is empty")
	}
}

func lastRecordedWorkerOperation(t *testing.T, recorder *recordingEventRecorder) events.Event {
	t.Helper()
	if len(recorder.events) == 0 {
		t.Fatal("no events recorded")
	}
	return recorder.events[len(recorder.events)-1]
}
