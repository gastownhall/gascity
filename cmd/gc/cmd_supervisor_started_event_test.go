package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestEmitSupervisorStartedRecordsTypedEvent(t *testing.T) {
	for _, cause := range []string{
		supervisor.PreviousExitClean,
		supervisor.PreviousExitCrash,
		supervisor.PreviousExitUnknown,
	} {
		t.Run(cause, func(t *testing.T) {
			var stderr bytes.Buffer
			rec := events.NewFake()

			emitSupervisorStarted(&stderr, rec, cause, nil)

			wantLine := "gc supervisor: started: previous_exit=" + cause + "\n"
			if got := stderr.String(); got != wantLine {
				t.Fatalf("stderr = %q, want %q", got, wantLine)
			}
			if len(rec.Events) != 1 {
				t.Fatalf("recorded events = %d, want 1", len(rec.Events))
			}
			event := rec.Events[0]
			if event.Type != events.SupervisorStarted {
				t.Fatalf("event.Type = %q, want %q", event.Type, events.SupervisorStarted)
			}
			if event.Actor != "supervisor" {
				t.Fatalf("event.Actor = %q, want %q", event.Actor, "supervisor")
			}
			if event.Subject != "supervisor" {
				t.Fatalf("event.Subject = %q, want %q", event.Subject, "supervisor")
			}
			var payload api.SupervisorStartedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatalf("Unmarshal payload: %v", err)
			}
			if payload.PreviousExit != cause {
				t.Fatalf("payload.PreviousExit = %q, want %q", payload.PreviousExit, cause)
			}
		})
	}
}

func TestEmitSupervisorStartedWithoutRecorderStillLogs(t *testing.T) {
	var stderr bytes.Buffer

	emitSupervisorStarted(&stderr, nil, supervisor.PreviousExitUnknown, nil)

	wantLine := "gc supervisor: started: previous_exit=unknown\n"
	if got := stderr.String(); got != wantLine {
		t.Fatalf("stderr = %q, want %q", got, wantLine)
	}
}

func TestEmitSupervisorStartedIncludesDetailReason(t *testing.T) {
	var stderr bytes.Buffer
	rec := events.NewFake()

	emitSupervisorStarted(&stderr, rec, supervisor.PreviousExitUnknown,
		errors.New("removing shutdown handoff token: permission denied"))

	wantLine := "gc supervisor: started: previous_exit=unknown reason=removing shutdown handoff token: permission denied\n"
	if got := stderr.String(); got != wantLine {
		t.Fatalf("stderr = %q, want %q", got, wantLine)
	}
	// The detail is breadcrumb-only: the wire payload carries the
	// classification alone.
	if len(rec.Events) != 1 {
		t.Fatalf("recorded events = %d, want 1", len(rec.Events))
	}
	var payload api.SupervisorStartedPayload
	if err := json.Unmarshal(rec.Events[0].Payload, &payload); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	if payload.PreviousExit != supervisor.PreviousExitUnknown {
		t.Fatalf("payload.PreviousExit = %q, want %q", payload.PreviousExit, supervisor.PreviousExitUnknown)
	}
}

// TestRunSupervisorEmitsStartedEventWithRestartCause runs the supervisor
// three times through full start → SIGTERM → stop cycles and verifies
// the restart-cause handoff: the first start (no prior instance) reports
// previous_exit=unknown, the clean shutdown leaves the handoff token,
// the second start consumes the token (checked while it is running,
// before its own clean stop re-arms the token) and reports
// previous_exit=clean, and the third start — after the re-armed token is
// deleted to simulate a crash, with the prior runs' lock file still
// present — reports previous_exit=crash.
func TestRunSupervisorEmitsStartedEventWithRestartCause(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", filepath.Dir(gcHome))
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("GC_BEADS", "file")

	if err := os.WriteFile(supervisor.ConfigPath(), []byte("[supervisor]\nport = "+freeLoopbackPort(t)+"\npatrol_interval = \"10m\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sigChReady := make(chan chan<- os.Signal, 2)
	oldSignalNotify := supervisorSignalNotify
	supervisorSignalNotify = func(c chan<- os.Signal, _ ...os.Signal) {
		sigChReady <- c
	}
	t.Cleanup(func() {
		supervisorSignalNotify = oldSignalNotify
	})

	runOnce := func(whileRunning func()) {
		t.Helper()
		var stdout, stderr lockedBuffer
		done := make(chan int, 1)
		go func() {
			done <- runSupervisor(&stdout, &stderr)
		}()
		var sigCh chan<- os.Signal
		select {
		case sigCh = <-sigChReady:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for supervisor signal hook; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) && !strings.Contains(stdout.String(), "Supervisor started.") {
			time.Sleep(10 * time.Millisecond)
		}
		if !strings.Contains(stdout.String(), "Supervisor started.") {
			t.Fatalf("timed out waiting for supervisor readiness; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
		if whileRunning != nil {
			whileRunning()
		}
		sigCh <- syscall.SIGTERM
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("runSupervisor code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("runSupervisor did not exit after SIGTERM; stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	}

	markerPath := supervisor.ShutdownMarkerPath(supervisor.DefaultHome())

	runOnce(nil)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("shutdown handoff token after clean stop: %v", err)
	}

	runOnce(func() {
		if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
			t.Errorf("handoff token not consumed by second start (stat err = %v)", err)
		}
	})
	// The second clean stop re-arms the token for the next instance.
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("shutdown handoff token after second clean stop: %v", err)
	}

	// Simulate a crash before the third start: delete the re-armed
	// handoff token. The supervisor lock file from the prior runs
	// remains, so the third start sees prior-instance evidence without
	// a token and classifies the previous exit as a crash.
	if err := os.Remove(markerPath); err != nil {
		t.Fatalf("removing handoff token to simulate crash: %v", err)
	}
	runOnce(nil)

	data, err := os.ReadFile(filepath.Join(supervisor.RuntimeDir(), "events.jsonl"))
	if err != nil {
		t.Fatalf("reading supervisor events log: %v", err)
	}
	var causes []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var event events.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parsing event line %q: %v", line, err)
		}
		if event.Type != events.SupervisorStarted {
			continue
		}
		var payload api.SupervisorStartedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("parsing supervisor.started payload %q: %v", string(event.Payload), err)
		}
		causes = append(causes, payload.PreviousExit)
	}
	want := []string{supervisor.PreviousExitUnknown, supervisor.PreviousExitClean, supervisor.PreviousExitCrash}
	if len(causes) != len(want) {
		t.Fatalf("supervisor.started causes = %v, want %v", causes, want)
	}
	for i := range want {
		if causes[i] != want[i] {
			t.Fatalf("supervisor.started causes = %v, want %v", causes, want)
		}
	}
}
