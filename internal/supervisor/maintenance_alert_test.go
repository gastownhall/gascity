package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
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
	emitUnderLock(loop, MaintenanceRun{
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
	emitUnderLock(loop, MaintenanceRun{
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
	emitUnderLock(loop, MaintenanceRun{
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
// does not panic or propagate an error out of the completion path. The event
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
	emitUnderLock(loop, MaintenanceRun{
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

// TestAlert_RepeatFailureSuppressed covers the dedup contract: the same
// failure fingerprint (stage + error) repeating across runs mails the
// operator once, not once per interval. A run whose fingerprint CHANGES
// mid-streak alerts again.
func TestAlert_RepeatFailureSuppressed(t *testing.T) {
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
	fail := func(offset time.Duration, stage, errMsg string) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      stage,
			Err:        errMsg,
		})
	}

	fail(0, "gc", "open dolt conn: connection refused")
	fail(time.Hour, "gc", "open dolt conn: connection refused")
	fail(2*time.Hour, "gc", "open dolt conn: connection refused")
	if got := len(fakeMail.Messages()); got != 1 {
		t.Fatalf("sent %d messages for 3 identical failures; want 1", got)
	}

	// The failure changing shape is new information — alert again. A different
	// error in the SAME stage counts: two ways for CALL DOLT_GC() to fail are
	// two conditions, and a fingerprint built from the stage alone would tell
	// the operator about only the first.
	fail(3*time.Hour, "gc", "CALL DOLT_GC() failed: out of disk")
	msgs := fakeMail.Messages()
	if got := len(msgs); got != 2 {
		t.Fatalf("sent %d messages after the error changed within one stage; want 2", got)
	}
	if want := "[ALERT] Dolt store maintenance failed: gc"; msgs[1].Subject != want {
		t.Fatalf("msg.Subject = %q; want %q", msgs[1].Subject, want)
	}
	if !strings.Contains(msgs[1].Body, "out of disk") {
		t.Errorf("msg.Body missing the new error; body=%s", msgs[1].Body)
	}

	// The stage changing is likewise new.
	fail(4*time.Hour, "smoke-test", "count query timed out")
	msgs = fakeMail.Messages()
	if got := len(msgs); got != 3 {
		t.Fatalf("sent %d messages after the stage changed; want 3", got)
	}
	if want := "[ALERT] Dolt store maintenance failed: smoke-test"; msgs[2].Subject != want {
		t.Fatalf("msg.Subject = %q; want %q", msgs[2].Subject, want)
	}
}

// TestAlert_RecoveryNoticeAfterFailureStreak covers the other half of the
// state machine: the first successful run after an alerted failure streak
// sends one recovery notice carrying the resolved failure, the streak
// start, and the suppressed-repeat count. Further successes stay silent.
func TestAlert_RecoveryNoticeAfterFailureStreak(t *testing.T) {
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
	for i := range 3 {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(time.Duration(i) * time.Hour),
			FinishedAt: started.Add(time.Duration(i)*time.Hour + time.Second),
			Stage:      "gc",
			Err:        "out of disk",
		})
	}
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started.Add(4 * time.Hour),
		FinishedAt: started.Add(4*time.Hour + time.Second),
		Stage:      "done",
	})
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started.Add(5 * time.Hour),
		FinishedAt: started.Add(5*time.Hour + time.Second),
		Stage:      "done",
	})

	msgs := fakeMail.Messages()
	if got := len(msgs); got != 2 {
		t.Fatalf("sent %d messages; want 1 alert + 1 recovery", got)
	}
	rec := msgs[1]
	if want := "[RECOVERED] Dolt store maintenance succeeded after failing: gc"; rec.Subject != want {
		t.Fatalf("recovery subject = %q; want %q", rec.Subject, want)
	}
	for _, want := range []string{
		"out of disk",
		started.UTC().Format(time.RFC3339), // failing since the streak start
		"2 suppressed",                     // runs 2 and 3 were deduped
	} {
		if !strings.Contains(rec.Body, want) {
			t.Errorf("recovery body missing %q; body=%s", want, rec.Body)
		}
	}
}

// TestAlert_RecoveryNoticeReportsTheStreakStartNotTheLatestShape pins what
// "Failing since" means when the failure changes shape mid-streak. The operator
// reads that line as the length of the outage, so it has to name the run that
// opened it, not the run where the symptom last changed: a store broken from
// 12:00 that starts failing a different way at 13:00 was still broken from 12:00.
func TestAlert_RecoveryNoticeReportsTheStreakStartNotTheLatestShape(t *testing.T) {
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

	streakStart := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	shapeChange := streakStart.Add(time.Hour)
	run := func(at time.Time, stage, errMsg string) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  at,
			FinishedAt: at.Add(time.Second),
			Stage:      stage,
			Err:        errMsg,
		})
	}

	run(streakStart, "backup", "sync target: disk full")
	run(shapeChange, "gc", "open dolt conn: connection refused")
	run(streakStart.Add(2*time.Hour), "done", "")

	msgs := fakeMail.Messages()
	if got := len(msgs); got != 3 {
		t.Fatalf("sent %d messages; want 2 alerts + 1 recovery", got)
	}
	rec := msgs[2]
	if want := "Failing since: " + streakStart.UTC().Format(time.RFC3339); !strings.Contains(rec.Body, want) {
		t.Errorf("recovery body missing %q; body=%s", want, rec.Body)
	}
	if notWant := "Failing since: " + shapeChange.UTC().Format(time.RFC3339); strings.Contains(rec.Body, notWant) {
		t.Errorf("recovery body reports %q, the shape change, not the streak start; body=%s", notWant, rec.Body)
	}
	// The notice still describes the failure it is closing, which is the last
	// one alerted.
	if !strings.Contains(rec.Body, "connection refused") {
		t.Errorf("recovery body missing the last alerted error; body=%s", rec.Body)
	}
}

// TestAlert_DiskCriticalSkipIsNotRecovery pins the one cycle outcome that looks
// like a success and is not. A disk-critical pre-flight skips the snapshot and
// CALL DOLT_GC and finishes with no error, so reading the absence of an error as
// "the store is healthy again" mails the operator a recovery notice for a store
// nothing has touched. The skipped cycle must leave the alert state exactly where
// it was, which the suppressed repeat afterwards proves.
func TestAlert_DiskCriticalSkipIsNotRecovery(t *testing.T) {
	t.Parallel()
	fakeMail := mail.NewFake()
	fakeEvents := events.NewFake()
	free := int64(1 << 40) // plenty, until the test lowers it
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath:         t.TempDir(),
		Recorder:         fakeEvents,
		Mail:             fakeMail,
		DiskFreeBytes:    func(string) (int64, error) { return free, nil },
		DiskMinFreeBytes: 1 << 30,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	fail := func(offset time.Duration) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      "gc",
			Err:        "open dolt conn: connection refused",
		})
	}

	fail(0)
	if got := len(fakeMail.Messages()); got != 1 {
		t.Fatalf("delivered %d messages for the opening failure; want 1 alert", got)
	}

	// Disk drops below the floor, so the next cycle attempts nothing.
	free = 100
	loop.mu.Lock()
	skipped := loop.executeCycleLocked(context.Background())
	loop.mu.Unlock()
	if skipped.Err != "" || skipped.SnapshotPath != "" {
		t.Fatalf("run = %+v; want the pre-flight to have skipped both stages without failing", skipped)
	}
	if !recorded(fakeEvents, events.StoreDiskCritical) {
		t.Fatalf("recorded %v; want a StoreDiskCritical event, which is what proves the pre-flight skipped", eventTypes(fakeEvents))
	}
	if got := len(fakeMail.Messages()); got != 1 {
		t.Fatalf("delivered %d messages after a skipped cycle; want no recovery notice for a store nothing ran against", got)
	}

	// The failure state survived, so the same failure is still a repeat.
	free = 1 << 40
	fail(2 * time.Hour)
	if got := len(fakeMail.Messages()); got != 1 {
		t.Fatalf("delivered %d messages for a repeat of the alerted failure; want 1, the skip must not have cleared the state", got)
	}
}

// TestAlert_NoRecoveryNoticeWithoutAlert ensures success-after-failure
// stays silent when no failure alert was ever mailed (AlertTo empty for
// the whole streak): there is no open loop to close.
func TestAlert_NoRecoveryNoticeWithoutAlert(t *testing.T) {
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
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "boom",
	})
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started.Add(time.Hour),
		FinishedAt: started.Add(time.Hour + time.Second),
		Stage:      "done",
	})

	if got := len(fakeMail.Messages()); got != 0 {
		t.Fatalf("sent %d messages with empty AlertTo; want 0", got)
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
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "boom",
	})
}

// TestAlert_UnchangedConditionSuppressedAcrossCyclesWithAdvancingClock drives
// real maintenance cycles rather than hand-built MaintenanceRun values, because
// the fingerprint is the stage plus the rendered error and therefore only
// suppresses what the failing stage renders identically. The snapshot stage
// builds its destination path from the clock, so the error text is exactly where
// a per-run value can leak in and turn one stuck condition into one alert per
// cycle.
func TestAlert_UnchangedConditionSuppressedAcrossCyclesWithAdvancingClock(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	const cycles = 3

	cycleStart := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }

	cases := []struct {
		name  string
		setup func(t *testing.T, cityPath string) *fakeDoltBackupRunner
	}{
		{
			// The rotation destination is occupied, so rename(2) fails
			// ENOTEMPTY. Blocking it this way rather than with a read-only
			// parent keeps the failure independent of file modes, which a root
			// test runner would ignore.
			name: "the rotation destination is blocked every cycle",
			setup: func(t *testing.T, cityPath string) *fakeDoltBackupRunner {
				t.Helper()
				successDir := filepath.Join(cityPath, ".beads", "dolt-backups", "success")
				for i := range cycles {
					occupied := filepath.Join(successDir, cycleStart(i).UTC().Format(snapshotTimestampFormat))
					if err := os.MkdirAll(occupied, 0o755); err != nil {
						t.Fatalf("MkdirAll(%s): %v", occupied, err)
					}
					if err := os.WriteFile(filepath.Join(occupied, "occupied"), []byte("x"), 0o644); err != nil {
						t.Fatalf("WriteFile in %s: %v", occupied, err)
					}
				}
				return &fakeDoltBackupRunner{writeOnSync: true}
			},
		},
		{
			name: "the backup target cannot be added",
			setup: func(*testing.T, string) *fakeDoltBackupRunner {
				return &fakeDoltBackupRunner{addErr: errors.New("target URL duplicate with remote X")}
			},
		},
		{
			name: "the backup target cannot be synced",
			setup: func(*testing.T, string) *fakeDoltBackupRunner {
				return &fakeDoltBackupRunner{syncErr: errors.New("network unreachable")}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cityPath := t.TempDir()
			runner := tc.setup(t, cityPath)
			cycle := 0
			fakeMail := mail.NewFake()
			var stderr bytes.Buffer
			loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
				Cfg: config.DoltMaintenance{
					Enabled:  true,
					Interval: "168h",
					AlertTo:  "gascity/mayor",
				},
				CityPath: cityPath,
				Recorder: events.NewFake(),
				Mail:     fakeMail,
				Stderr:   &stderr,
				Clock:    func() time.Time { return cycleStart(cycle) },
				OpenDoltBackup: func(context.Context) (DoltBackupRunner, error) {
					return runner, nil
				},
			})

			rendered := make([]string, 0, cycles)
			for cycle = 0; cycle < cycles; cycle++ {
				loop.mu.Lock()
				run := loop.executeCycleLocked(context.Background())
				loop.mu.Unlock()
				if run.Err == "" {
					t.Fatalf("cycle %d succeeded; want the configured failure", cycle)
				}
				rendered = append(rendered, run.Err)
			}
			for i, got := range rendered[1:] {
				if got != rendered[0] {
					t.Errorf("cycle %d error = %q, cycle 0 = %q; one unchanged condition must render one text or the fingerprint cannot recognize it", i+1, got, rendered[0])
				}
			}
			if got := len(fakeMail.Messages()); got != 1 {
				t.Fatalf("delivered %d alerts for one unchanged condition over %d cycles; want 1. Errors: %q", got, cycles, rendered)
			}
		})
	}
}

// recorded reports whether the fake recorder saw an event of this type.
func recorded(fake *events.Fake, eventType string) bool {
	for _, e := range fake.Events {
		if e.Type == eventType {
			return true
		}
	}
	return false
}

// eventTypes lists what the fake recorder saw, for failure messages.
func eventTypes(fake *events.Fake) []string {
	types := make([]string, 0, len(fake.Events))
	for _, e := range fake.Events {
		types = append(types, e.Type)
	}
	return types
}

// failNthMail delivers every message except the failAt'th, which fails the way a
// mail backend that is briefly down does. Everything else is the in-memory fake,
// so Messages() still reports exactly what was delivered.
type failNthMail struct {
	*mail.Fake
	failAt   int
	attempts int
}

func (f *failNthMail) Send(from, to, subject, body string) (mail.Message, error) {
	f.attempts++
	if f.attempts == f.failAt {
		return mail.Message{}, errors.New("mail provider unavailable")
	}
	return f.Fake.Send(from, to, subject, body)
}

// TestAlert_FailureAlertRetriedAfterSendFailure pins that a transient mail
// outage costs the operator one attempt, not the whole streak. The alert state
// records a fingerprint to suppress repeats of a failure the operator has
// already been told about; advancing it on a send that failed would suppress
// every identical run after it, so the operator learns nothing once mail
// recovers.
func TestAlert_FailureAlertRetriedAfterSendFailure(t *testing.T) {
	t.Parallel()
	failOnce := &failNthMail{Fake: mail.NewFake(), failAt: 1}
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     failOnce,
		Stderr:   &stderr,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	fail := func(offset time.Duration) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      "gc",
			Err:        "open dolt conn: connection refused",
		})
	}

	fail(0)
	if got := len(failOnce.Messages()); got != 0 {
		t.Fatalf("delivered %d messages while mail was down; want 0", got)
	}

	fail(time.Hour)
	msgs := failOnce.Messages()
	if got := len(msgs); got != 1 {
		t.Fatalf("delivered %d messages after mail recovered; want the retried alert", got)
	}
	if want := "[ALERT] Dolt store maintenance failed: gc"; msgs[0].Subject != want {
		t.Fatalf("msg.Subject = %q; want %q", msgs[0].Subject, want)
	}

	// The delivered alert is what arms suppression, so the next identical run
	// is a repeat and stays quiet.
	fail(2 * time.Hour)
	if got := len(failOnce.Messages()); got != 1 {
		t.Fatalf("delivered %d messages for a repeat of a delivered alert; want 1", got)
	}
	if failOnce.attempts != 2 {
		t.Fatalf("attempted %d sends; want 2 (one failed, one delivered, then suppressed)", failOnce.attempts)
	}
}

// TestAlert_NewOutageAfterAnUndeliveredRecoveryStillAlerts pins the other half of
// a lost recovery notice. The store failed, recovered while mail was down, then
// broke the same way again. Holding the old fingerprint as still-delivered makes
// the second outage look like a repeat of the first and the operator hears
// nothing about a store that is down right now.
func TestAlert_NewOutageAfterAnUndeliveredRecoveryStillAlerts(t *testing.T) {
	t.Parallel()
	// Attempt 1 is the opening alert (delivered), attempt 2 is the recovery
	// notice (lost), attempt 3 is the new outage's alert.
	failOnce := &failNthMail{Fake: mail.NewFake(), failAt: 2}
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     failOnce,
		Stderr:   &stderr,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	const errMsg = "open dolt conn: connection refused"
	run := func(offset time.Duration, stage, runErr string) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      stage,
			Err:        runErr,
		})
	}

	run(0, "gc", errMsg)
	run(time.Hour, "done", "")
	run(2*time.Hour, "gc", errMsg)

	msgs := failOnce.Messages()
	if got := len(msgs); got != 2 {
		t.Fatalf("delivered %d messages; want the opening alert and the new outage's alert", got)
	}
	if failOnce.attempts != 3 {
		t.Fatalf("attempted %d sends; want 3 (alert, lost recovery, alert)", failOnce.attempts)
	}
	want := "[ALERT] Dolt store maintenance failed: gc"
	for i, m := range msgs {
		if m.Subject != want {
			t.Errorf("msgs[%d].Subject = %q; want %q", i, m.Subject, want)
		}
	}

	// The new outage started at 14:00, not when the first one did, so a later
	// recovery reports the outage the operator is actually in.
	run(3*time.Hour, "done", "")
	msgs = failOnce.Messages()
	if got := len(msgs); got != 3 {
		t.Fatalf("delivered %d messages; want a recovery notice for the second outage", got)
	}
	if want := "Failing since: " + started.Add(2*time.Hour).UTC().Format(time.RFC3339); !strings.Contains(msgs[2].Body, want) {
		t.Errorf("recovery body missing %q; body=%s", want, msgs[2].Body)
	}
}

// TestAlert_UndeliveredChangedFailureIsStillTheOneReported pins the case that
// needs the observed-state and known-state split. Failure A is alerted, the store
// then starts failing a different way and THAT alert cannot be delivered, and then
// the store recovers. Tracking only what the operator knows leaves the loop
// pointed at A, so it closes the streak by naming A and the operator never hears
// of B at all.
func TestAlert_UndeliveredChangedFailureIsStillTheOneReported(t *testing.T) {
	t.Parallel()
	// Attempt 1 is A's alert (delivered), attempt 2 is B's alert (lost),
	// attempt 3 is the recovery notice.
	failOnce := &failNthMail{Fake: mail.NewFake(), failAt: 2}
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     failOnce,
		Stderr:   &stderr,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	run := func(offset time.Duration, stage, errMsg string) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      stage,
			Err:        errMsg,
		})
	}

	run(0, "gc", "open dolt conn: connection refused")
	run(time.Hour, "backup", "sync target: disk full")
	run(2*time.Hour, "done", "")

	msgs := failOnce.Messages()
	if got := len(msgs); got != 2 {
		t.Fatalf("delivered %d messages; want A's alert and one recovery notice", got)
	}
	if failOnce.attempts != 3 {
		t.Fatalf("attempted %d sends; want 3 (A alerted, B lost, recovery)", failOnce.attempts)
	}
	rec := msgs[1]
	if want := "[RECOVERED] Dolt store maintenance succeeded after failing: backup"; rec.Subject != want {
		t.Fatalf("recovery subject = %q; want %q, the failure the store was actually in", rec.Subject, want)
	}
	if !strings.Contains(rec.Body, "sync target: disk full") {
		t.Errorf("recovery body does not name the undelivered failure; body=%s", rec.Body)
	}
	if strings.Contains(rec.Body, "connection refused") {
		t.Errorf("recovery body names the earlier, resolved failure; body=%s", rec.Body)
	}
	if !strings.Contains(rec.Body, "could not be delivered") {
		t.Errorf("recovery body does not say the alert never reached the operator; body=%s", rec.Body)
	}
	// The streak started with A, not with the shape change.
	if want := "Failing since: " + started.UTC().Format(time.RFC3339); !strings.Contains(rec.Body, want) {
		t.Errorf("recovery body missing %q; body=%s", want, rec.Body)
	}
}

// TestAlert_RecoveryNoticeRetriedAfterSendFailure covers the other edge of the
// same state machine. Clearing the fingerprint on a recovery notice that never
// went out loses the notice outright: the loop is back in the healthy state, so
// no later run has anything to report.
func TestAlert_RecoveryNoticeRetriedAfterSendFailure(t *testing.T) {
	t.Parallel()
	// Attempt 1 is the failure alert, so let it through and break attempt 2,
	// which is the recovery notice.
	failOnce := &failNthMail{Fake: mail.NewFake(), failAt: 2}
	var stderr bytes.Buffer
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg: config.DoltMaintenance{
			Enabled:  true,
			Interval: "168h",
			AlertTo:  "gascity/mayor",
		},
		CityPath: "/tmp/city",
		Recorder: events.NewFake(),
		Mail:     failOnce,
		Stderr:   &stderr,
	})

	started := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	emitUnderLock(loop, MaintenanceRun{
		StartedAt:  started,
		FinishedAt: started.Add(time.Second),
		Stage:      "gc",
		Err:        "open dolt conn: connection refused",
	})
	if got := len(failOnce.Messages()); got != 1 {
		t.Fatalf("delivered %d messages for the opening failure; want 1 alert", got)
	}

	ok := func(offset time.Duration) {
		emitUnderLock(loop, MaintenanceRun{
			StartedAt:  started.Add(offset),
			FinishedAt: started.Add(offset + time.Second),
			Stage:      "",
		})
	}

	ok(time.Hour)
	if got := len(failOnce.Messages()); got != 1 {
		t.Fatalf("delivered %d messages while mail was down; want only the earlier alert", got)
	}

	ok(2 * time.Hour)
	msgs := failOnce.Messages()
	if got := len(msgs); got != 2 {
		t.Fatalf("delivered %d messages after mail recovered; want the retried recovery notice", got)
	}
	if want := "[RECOVERED] Dolt store maintenance succeeded after failing: gc"; msgs[1].Subject != want {
		t.Fatalf("recovery msg.Subject = %q; want %q", msgs[1].Subject, want)
	}

	// The notice landed, so the loop is healthy again and a further success
	// owes nothing.
	ok(3 * time.Hour)
	if got := len(failOnce.Messages()); got != 2 {
		t.Fatalf("delivered %d messages for a healthy run after recovery; want 2", got)
	}
}

// emitUnderLock drives one run through the completion path the way production
// does, holding loop.mu: emitRunEventLocked mutates the mu-guarded alert state,
// and the lock is the cycle lease in production.
func emitUnderLock(loop *StoreMaintenanceLoop, run MaintenanceRun) {
	loop.mu.Lock()
	defer loop.mu.Unlock()
	loop.emitRunEventLocked(run)
}
