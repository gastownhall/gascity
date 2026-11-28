package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/session"
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
