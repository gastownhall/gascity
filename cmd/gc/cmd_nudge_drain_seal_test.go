package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

const drainSealTestSeal = "gc-hook-run-seal-TEST"

// drainSealTestMark is drainSealTestSeal as drain writes it.
const drainSealTestMark = "\x00" + drainSealTestSeal

// sealObservingWriter records drain output write by write and snapshots the
// nudge queue each time a gc hook run seal arrives.
type sealObservingWriter struct {
	cityDir string
	writes  []string
	atSeal  []nudgequeue.State
	loadErr error
}

func (w *sealObservingWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(drainSealTestMark)) {
		state, err := nudgequeue.LoadState(w.cityDir)
		w.atSeal = append(w.atSeal, state)
		w.loadErr = errors.Join(w.loadErr, err)
	}
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

var errDrainTestWrite = errors.New("no space left on device")

// failingDrainWriter fails its failOn-th write (1-based) part-way, as a write
// to a full disk can.
type failingDrainWriter struct {
	failOn int
	writes int
}

func (w *failingDrainWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failOn {
		return len(p) / 2, errDrainTestWrite
	}
	return len(p), nil
}

func queuedNudgesContain(items []queuedNudge, id string) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}

// setUpDrainSealTest creates a city with one session and one nudge queued
// for it, as seen from inside gc hook run, and returns the city directory,
// the session ID and the nudge.
func setUpDrainSealTest(t *testing.T) (string, string, queuedNudge) {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_INJECT_CLOCK", "0")
	t.Setenv(hookRunOutputSealEnv, drainSealTestSeal)

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	item := newQueuedNudgeWithOptions("worker", "check hook output", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
		SessionID: created.ID,
	})
	if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
	return cityDir, created.ID, item
}

// TestCmdNudgeDrainSealsInjectedOutputBeforeAck pins the drain half of the gc
// hook run seal: drain must frame its output in seals, one before and one
// after, and write the closing seal before it acks the nudges. If the seal
// followed the ack, a wrapper deadline landing between the two would drop
// the output of nudges that are already acked.
func TestCmdNudgeDrainSealsInjectedOutputBeforeAck(t *testing.T) {
	cityDir, sessionID, item := setUpDrainSealTest(t)

	stdout := &sealObservingWriter{cityDir: cityDir}
	var stderr bytes.Buffer
	if code := cmdNudgeDrainWithFormat([]string{sessionID}, true, "", stdout, &stderr); code != 0 {
		t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%s", code, stderr.String())
	}

	writes := stdout.writes
	if len(writes) != 3 || writes[0] != drainSealTestMark || writes[2] != drainSealTestMark {
		t.Fatalf("drain writes = %q, want a seal, the payload and a seal", writes)
	}
	if !strings.Contains(writes[1], "check hook output") {
		t.Fatalf("framed payload = %q, want the nudge", writes[1])
	}
	if stdout.loadErr != nil {
		t.Fatalf("loading the nudge queue at a seal: %v", stdout.loadErr)
	}
	atClose := stdout.atSeal[len(stdout.atSeal)-1]
	if !queuedNudgesContain(atClose.InFlight, item.ID) {
		t.Fatalf("nudge %s was not in flight when the closing seal was written (pending=%v in_flight=%v); the ack must follow the seal",
			item.ID, atClose.Pending, atClose.InFlight)
	}
	after, err := nudgequeue.LoadState(cityDir)
	if err != nil {
		t.Fatalf("loading the nudge queue after drain: %v", err)
	}
	if queuedNudgesContain(after.Pending, item.ID) || queuedNudgesContain(after.InFlight, item.ID) {
		t.Fatalf("nudge %s still queued after drain (pending=%v in_flight=%v), want acked", item.ID, after.Pending, after.InFlight)
	}
}

// TestCmdNudgeDrainReleasesNudgesItCouldNotWrite pins that a failed write of
// drain's output returns the nudges to pending without counting a failed
// delivery attempt against them. gc hook run captures drain's stdout in a
// file, which a full disk can make fail; counting each failure would
// dead-letter every queued nudge after a few prompts.
func TestCmdNudgeDrainReleasesNudgesItCouldNotWrite(t *testing.T) {
	for _, tt := range []struct {
		name   string
		failOn int
	}{
		{name: "opening seal", failOn: 1},
		{name: "payload", failOn: 2},
		{name: "closing seal", failOn: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityDir, sessionID, item := setUpDrainSealTest(t)

			stdout := &failingDrainWriter{failOn: tt.failOn}
			var stderr bytes.Buffer
			if code := cmdNudgeDrainWithFormat([]string{sessionID}, true, "", stdout, &stderr); code != 0 {
				t.Fatalf("cmdNudgeDrainWithFormat = %d, want fail-open 0; stderr=%s", code, stderr.String())
			}
			if stdout.writes < tt.failOn {
				t.Fatalf("drain made %d writes, want at least %d", stdout.writes, tt.failOn)
			}
			after, err := nudgequeue.LoadState(cityDir)
			if err != nil {
				t.Fatalf("loading the nudge queue after drain: %v", err)
			}
			var pending *queuedNudge
			for i := range after.Pending {
				if after.Pending[i].ID == item.ID {
					pending = &after.Pending[i]
				}
			}
			if pending == nil {
				t.Fatalf("nudge %s not pending after the failed write (pending=%v in_flight=%v dead=%v)",
					item.ID, after.Pending, after.InFlight, after.Dead)
			}
			if pending.Attempts != 0 || pending.LastError != "" {
				t.Fatalf("nudge %s attempts=%d last_error=%q, want no failed attempt recorded", item.ID, pending.Attempts, pending.LastError)
			}
		})
	}
}
