package session

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

var resumeNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// resumeVerbs are the two operator verbs that resume a dormant row (D8).
var resumeVerbs = map[string]func(m *Manager, id string) error{
	"attach": func(m *Manager, id string) error {
		return m.Attach(context.Background(), id, "claude --resume k", runtime.Config{})
	},
	"send": func(m *Manager, id string) error {
		return m.Send(context.Background(), id, "hello", "claude --resume k", runtime.Config{})
	},
}

// resumeRow is a session row carrying every residue a resume consumes: a
// pending wake, both stop-request halves and a start baseline, at generation
// 3. extra sets its dormant state.
func resumeRow(extra map[string]string) map[string]string {
	meta := map[string]string{
		"session_name":            "s-resume",
		"template":                "worker",
		"work_dir":                "/tmp",
		"provider":                "claude",
		"generation":              "3",
		"instance_token":          "tok-3",
		"started_config_hash":     "base-hash",
		"awake_started_at":        "2026-10-01T00:00:00Z",
		"wake_request":            string(WakeCauseExplicit),
		"wake_requested_at":       "2026-10-08T11:00:00Z",
		DrainIntentReasonKey:      "idle",
		DrainIntentAtKey:          "2026-10-08T11:00:00Z",
		DrainIntentIncarnationKey: "3",
		DrainAckIncarnationKey:    "3",
		DrainAckAtKey:             "2026-10-08T11:01:00Z",
	}
	maps.Copy(meta, extra)
	return meta
}

func createResumeRow(t *testing.T, store beads.Store, meta map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return b
}

// assertResumed checks D8 rule 2's one CAS landed on the row.
func assertResumed(t *testing.T, got map[string]string) {
	t.Helper()
	want := map[string]string{
		"state":                   string(StateActive),
		"sleep_reason":            "",
		"slept_at":                "",
		"suspended_at":            "",
		"sleep_intent":            "",
		"held_until":              "",
		"last_woke_at":            resumeNow.Format(time.RFC3339),
		"awake_started_at":        resumeNow.Format(time.RFC3339Nano),
		"wake_request":            "",
		"wake_requested_at":       "",
		DrainIntentReasonKey:      "",
		DrainIntentAtKey:          "",
		DrainIntentIncarnationKey: "",
		DrainAckIncarnationKey:    "",
		DrainAckAtKey:             "",
		"generation":              "3",
		"instance_token":          "tok-3",
		"started_config_hash":     "base-hash",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

// TestResumeConsumesDormantState is J27 A (CONTRACT v5.7 D8, §12.2 row 30):
// attaching or sending to a suspended, held, drained or sleep-intent row
// leaves an active row with last_woke_at set, no sleep_reason, no hold, no
// pending wake or stop request, and the same generation and baseline. Kills
// legacy's state-only blind write, a resume that skips ClearWakeBlockersPatch
// (held_until stays), and one that misses the live-runtime branch.
func TestResumeConsumesDormantState(t *testing.T) {
	rows := []struct {
		name    string
		meta    map[string]string
		running bool
	}{
		{name: "suspended, drained reason (J27 A)", meta: map[string]string{
			"state": string(StateSuspended), "suspended_at": "2026-10-08T10:00:00Z", "sleep_reason": "drained",
			"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold",
		}},
		{name: "held asleep", meta: map[string]string{
			"state": string(StateAsleep), "sleep_reason": string(SleepReasonUserHold), "slept_at": "2026-10-08T10:00:00Z",
			"held_until": "2099-01-01T00:00:00Z",
		}},
		{name: "drained", meta: map[string]string{
			"state": string(StateDrained), "sleep_reason": "drained", "slept_at": "2026-10-08T10:00:00Z",
		}},
		{name: "asleep, runtime live", running: true, meta: map[string]string{
			"state": string(StateAsleep), "sleep_reason": "idle", "slept_at": "2026-10-08T10:00:00Z",
		}},
		{name: "active with sleep intent, runtime live", running: true, meta: map[string]string{
			"state": string(StateActive), "sleep_intent": "idle-stop", "held_until": "2099-01-01T00:00:00Z",
		}},
	}
	for verb, run := range resumeVerbs {
		for _, row := range rows {
			t.Run(verb+"/"+row.name, func(t *testing.T) {
				store := beads.NewMemStore()
				sp := runtime.NewFake()
				mgr := NewManagerWithOptions(store, sp, WithClock(&clock.Fake{Time: resumeNow}))
				b := createResumeRow(t, store, resumeRow(row.meta))
				if row.running {
					if err := sp.Start(context.Background(), "s-resume", runtime.Config{}); err != nil {
						t.Fatal(err)
					}
				}
				if err := run(mgr, b.ID); err != nil {
					t.Fatalf("%s: %v", verb, err)
				}
				got, err := store.Get(b.ID)
				if err != nil {
					t.Fatal(err)
				}
				assertResumed(t, got.Metadata)
			})
		}
	}
}

// TestResumeLeavesAwakeRowAlone: a row that is not dormant takes no resume
// writes. Its pending wake is A6's to clear, not the attach's. Kills a resume
// that fires on every ensureRunning.
func TestResumeLeavesAwakeRowAlone(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithClock(&clock.Fake{Time: resumeNow}))
	b := createResumeRow(t, store, resumeRow(map[string]string{"state": string(StateActive), "last_woke_at": "2026-10-08T09:00:00Z"}))
	if err := sp.Start(context.Background(), "s-resume", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := resumeVerbs["attach"](mgr, b.ID); err != nil {
		t.Fatalf("attach: %v", err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["last_woke_at"] != "2026-10-08T09:00:00Z" || got.Metadata["wake_request"] != string(WakeCauseExplicit) ||
		got.Metadata[DrainIntentReasonKey] != "idle" {
		t.Fatalf("awake row rewritten: %v", got.Metadata)
	}
}

// resumeCASHook runs before each UpdateIfMatch, so a test can land a
// concurrent write between the resume's read and its CAS.
type resumeCASHook struct {
	*rowWriteRecorder
	before func(opts beads.UpdateOpts)
}

func (h *resumeCASHook) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if h.before != nil {
		h.before(opts)
	}
	return h.rowWriteRecorder.UpdateIfMatch(id, rev, opts)
}

// TestResumeCASLosesToConcurrentWrite: the confirmation is a fenced CAS. A
// write landing between its read and its CAS makes it re-read and re-decide
// (an unrelated key) or refuse (a new generation or token, or a close) —
// never write blind. Kills SetMetadataBatch in place of the CAS, a retry that
// replays the stale patch, and a premise missing any of its three facts.
func TestResumeCASLosesToConcurrentWrite(t *testing.T) {
	setMeta := func(kv map[string]string) func(beads.Store, string) error {
		return func(s beads.Store, id string) error { return s.SetMetadataBatch(id, kv) }
	}
	for _, tc := range []struct {
		name    string
		write   func(beads.Store, string) error
		refused bool
	}{
		{name: "unrelated key re-decides", write: setMeta(map[string]string{"nudge_at": "x"})},
		{name: "new generation refuses", write: setMeta(map[string]string{"generation": "4"}), refused: true},
		{name: "new token refuses", write: setMeta(map[string]string{"instance_token": "tok-4"}), refused: true},
		{name: "close refuses", write: func(s beads.Store, id string) error { return s.Close(id) }, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
			hook := &resumeCASHook{rowWriteRecorder: &rowWriteRecorder{Store: stamped}}
			confirms, rowID := 0, ""
			hook.before = func(opts beads.UpdateOpts) {
				if _, ok := opts.Metadata["last_woke_at"]; !ok {
					return
				}
				if confirms++; confirms == 1 {
					if err := tc.write(stamped, rowID); err != nil {
						t.Fatalf("concurrent write: %v", err)
					}
				}
			}
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(hook, sp, WithClock(&clock.Fake{Time: resumeNow}))
			b := createResumeRow(t, stamped, resumeRow(map[string]string{
				"state": string(StateSuspended), "held_until": "2099-01-01T00:00:00Z", "sleep_intent": "user-hold",
			}))
			rowID = b.ID

			err := resumeVerbs["attach"](mgr, b.ID)
			got, getErr := stamped.Get(b.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if tc.refused {
				if !errors.Is(err, errResumeSuperseded) || !errors.Is(err, ErrStateSync) {
					t.Fatalf("attach = %v, want the superseded state-sync error", err)
				}
				if confirms != 1 || got.Metadata["state"] == string(StateActive) || got.Metadata["last_woke_at"] != "" ||
					got.Metadata["wake_request"] == "" {
					t.Fatalf("refused resume wrote (confirm CASes %d): %v", confirms, got.Metadata)
				}
				return
			}
			if err != nil {
				t.Fatalf("attach: %v", err)
			}
			if confirms != 2 || got.Metadata["nudge_at"] != "x" {
				t.Fatalf("confirm CASes = %d, nudge_at = %q; want a second CAS over the fresh row", confirms, got.Metadata["nudge_at"])
			}
			assertResumed(t, got.Metadata)
		})
	}
}

// TestOperatorSuspendClearsPendingWake is D7 rule 3: `gc session suspend`
// (its fallback and the API both reach Manager.Suspend) supersedes a pending
// wake in its own write; the city stop sweep leaves it for the next start.
func TestOperatorSuspendClearsPendingWake(t *testing.T) {
	for name, tc := range map[string]struct {
		suspend func(m *Manager, id string) error
		cleared bool
	}{
		"operator": {suspend: (*Manager).Suspend, cleared: true},
		"shutdown": {suspend: (*Manager).SuspendForShutdown},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := NewManagerWithOptions(store, sp)
			b := createResumeRow(t, store, resumeRow(map[string]string{"state": string(StateActive)}))
			if err := sp.Start(context.Background(), "s-resume", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := tc.suspend(mgr, b.ID); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata["state"] != string(StateSuspended) {
				t.Fatalf("state = %q, want suspended", got.Metadata["state"])
			}
			if cleared := got.Metadata["wake_request"] == "" && got.Metadata["wake_requested_at"] == ""; cleared != tc.cleared {
				t.Fatalf("wake request %q/%q, want cleared=%v", got.Metadata["wake_request"], got.Metadata["wake_requested_at"], tc.cleared)
			}
		})
	}
}
