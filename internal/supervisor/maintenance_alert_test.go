package supervisor

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
)

// TestAlert_SentOnFailureWithAlertTo covers the primary happy path for
// alert mail: a failing MaintenanceRun with AlertTo configured must
// produce exactly one message whose subject names the failing stage
// and whose body carries the stage, error, duration, and snapshot path.
func TestAlert_SentOnFailureWithAlertTo(t *testing.T) {
	t.Parallel()
	fakeMail := mail.NewFake()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     fakeMail,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	finished := started.Add(2500 * time.Millisecond)
	loop.emitRunEvent(MaintenanceRun{
		StartedAt:    started,
		FinishedAt:   finished,
		Stage:        "gc",
		Err:          "CALL DOLT_GC() failed: out of disk",
		SnapshotPath: "/tmp/city/.beads/dolt-backups/success/2026-04-22T12-00-00Z",
	})

	msgs := fakeMail.Messages()
	if got := len(msgs); got != 1 {
		t.Fatalf("sent %d messages; want exactly 1", got)
	}
	m := msgs[0]
	if m.To != "gascity/mayor" {
		t.Fatalf("msg.To = %q; want %q", m.To, "gascity/mayor")
	}
	if m.From == "" {
		t.Fatalf("msg.From = empty; want supervisor identity")
	}
	wantSubject := "[ALERT] Dolt store maintenance failed: gc"
	if m.Subject != wantSubject {
		t.Fatalf("msg.Subject = %q; want %q", m.Subject, wantSubject)
	}
	checks := []string{
		"gc",
		"CALL DOLT_GC() failed: out of disk",
		"2.500",
		"/tmp/city/.beads/dolt-backups/success/2026-04-22T12-00-00Z",
	}
	for _, want := range checks {
		if !strings.Contains(m.Body, want) {
			t.Errorf("msg.Body missing %q; body=%s", want, m.Body)
		}
	}
}

// TestAlert_NotSentOnSuccess ensures a successful run does not trigger
// an alert mail even when AlertTo is configured. The alert channel
// carries failures only.
func TestAlert_NotSentOnSuccess(t *testing.T) {
	t.Parallel()
	fakeMail := mail.NewFake()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled: true,
			AlertTo: "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     fakeMail,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop.emitRunEvent(MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(100 * time.Millisecond),
		Stage:      "done",
	})

	if got := len(fakeMail.Messages()); got != 0 {
		t.Fatalf("sent %d messages on success; want 0", got)
	}
}

// TestAlert_NotSentWithEmptyAlertTo ensures a failing run with an empty
// AlertTo silently skips the mail step. The failed event still fires
// (covered in maintenance_events_test.go); this test verifies only the
// mail side.
func TestAlert_NotSentWithEmptyAlertTo(t *testing.T) {
	t.Parallel()
	fakeMail := mail.NewFake()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled: true,
			AlertTo: "",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     fakeMail,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	loop.emitRunEvent(MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "boom",
	})

	if got := len(fakeMail.Messages()); got != 0 {
		t.Fatalf("sent %d messages with empty AlertTo; want 0", got)
	}
}

// TestAlert_SendFailureDoesNotPropagate ensures a broken mail provider
// does not panic or propagate an error out of emitRunEvent. The event
// is still recorded; the send failure is logged to stderr.
func TestAlert_SendFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()
	fakeMail := mail.NewFailFake()
	fakeEvents := events.NewFake()
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled: true,
			AlertTo: "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: fakeEvents,
		Mail:     fakeMail,
		Stderr:   &stderr,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	// Should not panic.
	loop.emitRunEvent(MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "boom",
	})

	if got := len(fakeEvents.Events); got != 1 {
		t.Fatalf("recorded %d events; want 1 failed event despite mail failure", got)
	}
	if stderr.Len() == 0 {
		t.Fatalf("stderr is empty; want a send-failure log line")
	}
	if !strings.Contains(stderr.String(), "alert mail") {
		t.Fatalf("stderr missing 'alert mail' marker; got %q", stderr.String())
	}
}

// TestAlert_NilMailProviderSkips ensures the loop is safe to construct
// with Mail unset. Tests that exercise only the event path (e.g.,
// maintenance_events_test.go) continue to work unchanged.
func TestAlert_NilMailProviderSkips(t *testing.T) {
	t.Parallel()
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled: true,
			AlertTo: "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		// Mail unset on purpose.
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	// Must not panic.
	loop.emitRunEvent(MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "boom",
	})
}
