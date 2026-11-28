package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// readNudgeStateFile is state.json's bytes, nil when the queue was never
// written.
func readNudgeStateFile(t *testing.T, cityPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(nudgequeue.StatePath(cityPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading state.json: %v", err)
	}
	return data
}

// closeContentOpSession closes c's session row through the session manager.
func closeContentOpSession(t *testing.T, c *contentOpCity) {
	t.Helper()
	mgr := newSessionManagerWithConfig(c.dir, c.store, c.fake, nil)
	if err := mgr.Close(context.Background(), testActorIn(t, session.ActorOperator, c.dir), c.target.sessionID); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// decodeSessionNudgeLine decodes stdout, which must be exactly one JSON line.
func decodeSessionNudgeLine(t *testing.T, stdout string) sessionNudgeJSON {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout = %q, want exactly one JSON line", stdout)
	}
	var got sessionNudgeJSON
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decoding %q: %v", lines[0], err)
	}
	return got
}

// wantUndeliverableLine fails unless stdout is one undeliverable JSON line
// with reason and state.
func wantUndeliverableLine(t *testing.T, stdout, reason, state string) sessionNudgeJSON {
	t.Helper()
	got := decodeSessionNudgeLine(t, stdout)
	if got.OK || got.Queued || got.Outcome != nudgeOutcomeUndeliverable || got.Reason != reason || got.SessionState != state {
		t.Fatalf("JSON = %+v, want ok:false queued:false outcome:%s reason:%s session_state:%s", got, nudgeOutcomeUndeliverable, reason, state)
	}
	contentOpValidate(t, "schemas/session/nudge/result.schema.json", stdout)
	return got
}

// failingSessionGetStore fails every Get, as a degraded session store does.
type failingSessionGetStore struct {
	beads.Store
}

func (s failingSessionGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("session store unavailable")
}

func TestSessionNudgeToRetiredSessionIsUndeliverable(t *testing.T) {
	modes := []nudgeDeliveryMode{nudgeDeliveryImmediate, nudgeDeliveryWaitIdle, nudgeDeliveryQueue}
	for _, mode := range modes {
		for _, asJSON := range []bool{false, true} {
			name := string(mode) + "/text"
			if asJSON {
				name = string(mode) + "/json"
			}
			t.Run("closed pool row/"+name, func(t *testing.T) {
				c := newContentOpCity(t, "auto")
				closeContentOpSession(t, c)
				before := readNudgeStateFile(t, c.dir)

				code, stdout, stderr := c.nudge(mode, "", asJSON)
				if code != 1 {
					t.Fatalf("nudge = %d, want 1; stdout=%s stderr=%s", code, stdout, stderr)
				}
				if asJSON {
					got := wantUndeliverableLine(t, stdout, cliOpRefusedSessionClosed, "closed")
					if got.SessionID != c.target.sessionID || got.Delivery != string(mode) {
						t.Fatalf("JSON = %+v, want session %s delivery %s", got, c.target.sessionID, mode)
					}
				} else if stdout != "" {
					t.Fatalf("stdout = %q, want empty in text mode", stdout)
				}
				if !strings.Contains(stderr, "undeliverable: session "+c.target.sessionID+" is closed") || !strings.Contains(stderr, "nothing was queued") {
					t.Fatalf("stderr = %q, want the undeliverable line", stderr)
				}
				if after := readNudgeStateFile(t, c.dir); !bytes.Equal(before, after) {
					t.Fatalf("state.json changed:\nbefore=%s\nafter=%s", before, after)
				}
				if n := len(c.delivered()); n != 0 {
					t.Fatalf("deliveries = %d, want none", n)
				}
				op := cliOpOnly(t, c.h.opStore)
				cliOpWantOutcome(t, op, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedSessionClosed, "")
			})
		}
	}

	t.Run("closed named row whose identity left config", func(t *testing.T) {
		c := newContentOpCity(t, "auto")
		if err := c.store.SetMetadata(c.target.sessionID, session.NamedSessionIdentityMetadata, "lead"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		closeContentOpSession(t, c)
		before := readNudgeStateFile(t, c.dir)
		code, stdout, stderr := c.nudge(nudgeDeliveryQueue, "", true)
		if code != 1 {
			t.Fatalf("nudge = %d, want 1; stdout=%s stderr=%s", code, stdout, stderr)
		}
		wantUndeliverableLine(t, stdout, cliOpRefusedSessionClosed, "closed")
		if after := readNudgeStateFile(t, c.dir); !bytes.Equal(before, after) {
			t.Fatalf("state.json changed:\nbefore=%s\nafter=%s", before, after)
		}
	})

	t.Run("session store read error", func(t *testing.T) {
		c := newContentOpCity(t, "")
		before := readNudgeStateFile(t, c.dir)
		var stdout, stderr bytes.Buffer
		code := deliverSessionNudgeKeyed(c.target, failingSessionGetStore{Store: c.store}, c.fake, testNudgeMessage, nudgeDeliveryQueue, "", false, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("nudge = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "checking session \""+c.target.sessionID+"\" before nudge") || !strings.Contains(stderr.String(), "session store unavailable") {
			t.Fatalf("stderr = %q, want the wrapped read error", stderr.String())
		}
		if strings.Contains(stderr.String(), nudgeOutcomeUndeliverable) {
			t.Fatalf("stderr = %q: a read error must not read as undeliverable", stderr.String())
		}
		if after := readNudgeStateFile(t, c.dir); !bytes.Equal(before, after) {
			t.Fatalf("state.json changed:\nbefore=%s\nafter=%s", before, after)
		}
	})
}

// managedNudgeRowFixture is a suspended managed session row with state set by
// md, whose city routes nudges through the managed reconciler and whose
// runtime reads not running. pokes counts controller pokes.
type managedNudgeRowFixture struct {
	dir    string
	store  beads.Store
	fake   *runtime.Fake
	target nudgeTarget
	pokes  *int
}

func newManagedNudgeRowFixture(t *testing.T, md map[string]string) managedNudgeRowFixture {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "claude", WorkDir: dir, Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActorIn(t, session.ActorOperator, dir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(md) > 0 {
		if err := store.SetMetadataBatch(info.ID, md); err != nil {
			t.Fatalf("SetMetadataBatch: %v", err)
		}
	}

	prevManaged, prevPoke, prevObserve := nudgeCityUsesManagedReconciler, nudgePokeController, nudgeObserveTarget
	pokes := 0
	nudgeCityUsesManagedReconciler = func(cityPath string) bool { return cityPath == dir }
	nudgePokeController = func(string, reconcilekey.Key) error {
		pokes++
		return nil
	}
	nudgeObserveTarget = func(nudgeTarget, beads.Store, runtime.Provider) (worker.LiveObservation, error) {
		return worker.LiveObservation{Running: false}, nil
	}
	t.Cleanup(func() {
		nudgeCityUsesManagedReconciler, nudgePokeController, nudgeObserveTarget = prevManaged, prevPoke, prevObserve
	})
	return managedNudgeRowFixture{
		dir:   dir,
		store: store,
		fake:  fake,
		target: nudgeTarget{
			cityPath:    dir,
			cfg:         &config.City{Agents: []config.Agent{{Name: "worker", Provider: "claude"}}},
			sessionID:   info.ID,
			sessionName: info.SessionName,
			identity:    "worker",
			agent:       config.Agent{Name: "worker", Provider: "claude"},
		},
		pokes: &pokes,
	}
}

// nudge sends testNudgeMessage to f's session in mode.
func (f managedNudgeRowFixture) nudge(mode nudgeDeliveryMode, asJSON bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeKeyed(f.target, f.store, f.fake, testNudgeMessage, mode, "", asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSessionNudgeWakeConflictIsStructuredUndeliverable(t *testing.T) {
	cases := []struct {
		name  string
		md    map[string]string
		state string
	}{
		{name: "closing", md: map[string]string{"state": "closing"}, state: "closing"},
		{name: "archived without continuity", md: map[string]string{"state": string(session.StateArchived), "continuity_eligible": "false"}, state: string(session.StateArchived)},
	}
	for _, tc := range cases {
		for _, asJSON := range []bool{false, true} {
			name := tc.name + "/text"
			if asJSON {
				name = tc.name + "/json"
			}
			t.Run(name, func(t *testing.T) {
				f := newManagedNudgeRowFixture(t, tc.md)
				code, stdout, stderr := f.nudge(nudgeDeliveryImmediate, asJSON)
				if code != 1 {
					t.Fatalf("nudge = %d, want 1; stdout=%s stderr=%s", code, stdout, stderr)
				}
				if asJSON {
					wantUndeliverableLine(t, stdout, cliOpWakeConflictReason(tc.state), tc.state)
				} else if stdout != "" {
					t.Fatalf("stdout = %q, want empty in text mode", stdout)
				}
				if !strings.Contains(stderr, "undeliverable: session "+f.target.sessionID+" is "+tc.state) || !strings.Contains(stderr, "dead-lettered as "+nudgeDeadReasonSessionTerminated) {
					t.Fatalf("stderr = %q, want the dead-lettered undeliverable line", stderr)
				}
				if *f.pokes != 0 {
					t.Fatalf("pokes = %d, want 0", *f.pokes)
				}
				pending, inFlight, dead, err := listQueuedNudgesForTarget(f.dir, f.target, time.Now())
				if err != nil {
					t.Fatalf("listQueuedNudgesForTarget: %v", err)
				}
				if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 1 {
					t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 0/0/1", len(pending), len(inFlight), len(dead))
				}
				if dead[0].LastError != nudgeDeadReasonSessionTerminated {
					t.Fatalf("dead LastError = %q, want %q", dead[0].LastError, nudgeDeadReasonSessionTerminated)
				}
				shadow, ok, err := nudgeFrontDoor(openNudgeBeadStore(f.dir)).FindIncludingTerminal(dead[0].ID)
				if err != nil || !ok {
					t.Fatalf("FindIncludingTerminal(%s) = ok %v, err %v; want the shadow", dead[0].ID, ok, err)
				}
				if shadow.State != "failed" || shadow.TerminalReason != nudgeDeadReasonSessionTerminated {
					t.Fatalf("shadow state/reason = %q/%q, want failed/%s", shadow.State, shadow.TerminalReason, nudgeDeadReasonSessionTerminated)
				}
			})
		}
	}

	t.Run("a non-conflict wake error stays raw", func(t *testing.T) {
		f := newManagedNudgeRowFixture(t, nil)
		item := newQueuedNudgeWithOptions(f.target.agentKey(), testNudgeMessage, "session", time.Now(), queuedNudgeOptionsFromTarget(f.target))
		f.target.sessionID = "gc-missing"
		_, err := enqueueManagedNudgeThenWake(f.target, f.store, item)
		if err == nil {
			t.Fatal("enqueueManagedNudgeThenWake to a missing row = nil, want an error")
		}
		if got := undeliverableWakeConflict(err); !errors.Is(got, err) || got.Error() != err.Error() {
			t.Fatalf("undeliverableWakeConflict(%v) = %v, want the raw error", err, got)
		}
		var undeliverable *nudgeUndeliverableError
		if errors.As(err, &undeliverable) {
			t.Fatalf("err = %v, a non-conflict wake error must not read as undeliverable", err)
		}
		state, loadErr := nudgequeue.LoadState(f.dir)
		if loadErr != nil {
			t.Fatalf("LoadState: %v", loadErr)
		}
		if len(state.Pending) != 0 || len(state.Dead) != 1 || state.Dead[0].ID != item.ID || !strings.HasPrefix(state.Dead[0].LastError, "managed wake failed: ") {
			t.Fatalf("pending %+v dead %+v, want only item %s dead with the managed-wake-failed reason", state.Pending, state.Dead, item.ID)
		}
	})
}
