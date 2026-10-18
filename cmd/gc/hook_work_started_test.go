package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
)

// The started write, the drain-ack fence and the unwind (PHASE1-2 §6.4, §6.5,
// W1 T1-T7, T10, T13). Each test names the mutant it kills.

// noStartedClaims is a ReadWorkStarted seam for a session whose work_started
// cell is empty: the drain-ack fence lets the ack through.
func noStartedClaims(string) (string, error) { return "", nil }

const (
	startedSessionID = "sess-1"
	startedToken     = "tok-1"
	startedStoreRef  = "rig:repo"
)

// startedEnv is a fenced seat's claim env: its row, incarnation and token.
func startedEnv(token, gen string) []string {
	return []string{"GC_SESSION_ID=" + startedSessionID, "GC_INSTANCE_TOKEN=" + token, "GC_RUNTIME_EPOCH=" + gen}
}

// casCountingStore is a stamped MemStore that counts the conditional writes
// that land on the session row, and can run one controller write of its own
// just before the agent's first one: a deterministic interleaving between the
// agent's read and its CAS.
type casCountingStore struct {
	*beads.MemStore
	rowID      string
	rowWrites  int
	interleave map[string]string // written by "the controller" before the first agent CAS
}

func (s *casCountingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if id == s.rowID && s.interleave != nil {
		ctl := s.interleave
		s.interleave = nil
		fresh, err := s.Get(id)
		if err != nil {
			return err
		}
		if err := s.MemStore.UpdateIfMatch(id, fresh.Revision, beads.UpdateOpts{Metadata: ctl}); err != nil {
			return err
		}
	}
	err := s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
	if err == nil && id == s.rowID {
		s.rowWrites++
	}
	return err
}

// startedRow is a session store holding one open row of incarnation 7 under
// startedToken, fenced by CAS only: blind writes are refused.
type startedRow struct {
	store *casCountingStore
	front *session.Store
	id    string
}

func newStartedRow(t *testing.T, meta map[string]string) *startedRow {
	t.Helper()
	m := beads.NewMemStore()
	m.HonorExplicitIDs = true
	if err := beads.StampOpenedStore(m, "MemStore", gate.Auto, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, err := m.Create(sessionRow(startedSessionID, "session_name", "worker-1", "state", "active", "generation", "7", "instance_token", startedToken))
	if err != nil {
		t.Fatal(err)
	}
	cs := &casCountingStore{MemStore: m, rowID: b.ID}
	if len(meta) > 0 {
		if err := m.UpdateIfMatch(b.ID, b.Revision, beads.UpdateOpts{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	return &startedRow{store: cs, front: session.NewStore(beads.SessionStore{Store: blindWriteRefusingStore{inner: cs}}), id: b.ID}
}

func (r *startedRow) meta(t *testing.T, key string) string {
	t.Helper()
	b, err := r.store.Get(r.id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata[key]
}

func (r *startedRow) cell(t *testing.T) session.WorkStarted {
	t.Helper()
	cell, err := session.DecodeWorkStarted(r.meta(t, session.WorkStartedKey))
	if err != nil {
		t.Fatal(err)
	}
	return cell
}

// startedLog is one hook invocation's ordered effects.
type startedLog struct {
	log         []string
	releases    []string
	acked       bool
	unrecorded  []string
	stepStarted []string
}

func (l *startedLog) add(s string) { l.log = append(l.log, s) }

// index is the position of the first entry with prefix, or -1.
func (l *startedLog) index(prefix string) int {
	for i, s := range l.log {
		if strings.HasPrefix(s, prefix) {
			return i
		}
	}
	return -1
}

// orderedWriter logs "result" for every write, failing them with err.
type orderedWriter struct {
	l   *startedLog
	buf bytes.Buffer
	err error
}

func (w *orderedWriter) Write(p []byte) (int, error) {
	w.l.add("result")
	if w.err != nil {
		return 0, w.err
	}
	return w.buf.Write(p)
}

// startedOps is a claim of work-1 (or the given work query output) whose
// started write goes to row, or to record when row is nil.
func (l *startedLog) ops(output string, row *startedRow, record hookRecordWorkStartedFunc) hookClaimOps {
	if record == nil {
		record = func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
			return recordWorkStarted(row.front, sessionID, fence, key, via, time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC))
		}
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return output, nil },
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			l.add("claim:" + beadID)
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, true, nil
		},
		Release: func(_ context.Context, _ string, _ []string, beadID, _ string) (bool, error) {
			l.add("release:" + beadID)
			l.releases = append(l.releases, beadID)
			return true, nil
		},
		DrainAck: func(io.Writer) error {
			l.add("ack")
			l.acked = true
			return nil
		},
		DrainPending: func(string) (bool, error) { return false, nil },
		RecordWorkStarted: func(sessionID string, fence hookStartedFence, key hookStartedKey, via string) (hookStartedResult, error) {
			l.add("record:" + key.Bead + ":" + via)
			return record(sessionID, fence, key, via)
		},
		ForgetWorkStarted: func(sessionID string, key hookStartedKey) error {
			l.add("forget:" + key.Bead)
			if row == nil {
				return nil
			}
			return forgetWorkStarted(row.front, sessionID, key)
		},
		ReadWorkStarted: noStartedClaims,
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
			return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee}, nil
		},
		StoreRef: func(string) string { return startedStoreRef },
		EmitWorkStartedUnrecorded: func(_, _, beadID string, _ error) {
			l.add("unrecorded:" + beadID)
			l.unrecorded = append(l.unrecorded, beadID)
		},
		EmitExecutionStepStarted: func(step beads.Bead, _ string, _ []string, _ string) {
			l.stepStarted = append(l.stepStarted, step.ID)
		},
		StampSessionClaim: func(_, beadID string) error { l.add("current:" + beadID); return nil },
		EmitClaimReleased: func(hookClaimReleaseRecord) {},
		PublishRunMap:     func(string, string, ...string) error { return nil },
		StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { return nil },
		ResolveWorkBranch: func(hookClaimWorkTree) string { return "" },
	}
}

func startedOpts(env []string) hookClaimOptions {
	return hookClaimOptions{
		Assignee:     "worker-1",
		SessionID:    startedSessionID,
		RouteTargets: []string{"worker"},
		Env:          env,
		DrainAck:     true,
		JSON:         true,
	}
}

const (
	startedRoutedWork   = `[{"id":"work-1","status":"open","metadata":{"gc.routed_to":"worker"}}]`
	startedAssignedWork = `[{"id":"work-1","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}}]`
)

// TestHookClaimRecordsStartedBeforeTheResult (T1): a minted claim is recorded
// in work_started by one conditional write, before the result line leaves the
// process. Kills the write after the result line, and no write at all.
func TestHookClaimRecordsStartedBeforeTheResult(t *testing.T) {
	row := newStartedRow(t, nil)
	l := &startedLog{}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(startedRoutedWork, row, nil), stdout, &stderr)

	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, stderr.String())
	}
	rec, res := l.index("record:work-1:claim"), l.index("result")
	if rec < 0 || res < 0 || rec > res {
		t.Fatalf("effects %v: want the started record before the result line", l.log)
	}
	if row.store.rowWrites != 1 {
		t.Fatalf("conditional row writes = %d, want 1", row.store.rowWrites)
	}
	cell := row.cell(t)
	if len(cell.Claims) != 1 || cell.Claims[0] != (session.WorkStartedEntry{Store: startedStoreRef, Bead: "work-1", Gen: 7, At: time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), Via: session.WorkStartedViaClaim}) {
		t.Fatalf("work_started = %+v, want the one minted entry", cell.Claims)
	}
	if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Action != "work" || got.BeadID != "work-1" {
		t.Fatalf("result %+v, want work-1 handed out", got)
	}
}

// TestHookAdoptionRecordsStartedOnce (T2): the hook re-adopts its own claim on
// every turn; the second hand-over finds it recorded and writes nothing. Kills
// a write without the compare-and-skip.
func TestHookAdoptionRecordsStartedOnce(t *testing.T) {
	row := newStartedRow(t, nil)
	for i := 0; i < 2; i++ {
		l := &startedLog{}
		stdout := &orderedWriter{l: l}
		var stderr bytes.Buffer
		if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(startedAssignedWork, row, nil), stdout, &stderr); code != 0 {
			t.Fatalf("turn %d: code %d; stderr=%s", i, code, stderr.String())
		}
		if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Reason != "existing_assignment" || l.index("record:work-1:adopt") < 0 {
			t.Fatalf("turn %d: result %+v, effects %v; want the adoption recorded as adopt", i, got, l.log)
		}
	}
	if row.store.rowWrites != 1 || len(row.cell(t).Claims) != 1 {
		t.Fatalf("conditional row writes = %d, cell %+v; want one write of one entry", row.store.rowWrites, row.cell(t))
	}
}

// TestHookClaimRefusesAClaimTheLadderIsReleasing (T3): the controller's
// work_ladder.release names the claim. The hook does not hand it out: a
// minted claim is given back through the unwind, an adopted one is left to the
// controller; the answer is the no-work drain, exit 0, with --drain-ack NOT
// consumed, and nothing is recorded. Kills skipping the release check.
func TestHookClaimRefusesAClaimTheLadderIsReleasing(t *testing.T) {
	for _, c := range []struct {
		name, output string
		minted       bool
	}{
		{"minted", startedRoutedWork, true},
		{"adopted", startedAssignedWork, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := newStartedRow(t, map[string]string{session.WorkLadderKey: `{"v":1,"gen":7,"release":{"store":"rig:repo","bead":"work-1","gen":7}}`})
			l := &startedLog{}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(c.output, row, nil), stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			switch {
			case code != 0 || got.Action != "drain" || got.Reason != hookClaimReasonNoWork || got.DrainAcknowledged:
				t.Fatalf("code %d, result %+v; want exit 0 and the no-work drain, unacknowledged", code, got)
			case l.acked:
				t.Fatal("--drain-ack consumed on a refused hand-over")
			case c.minted != (len(l.releases) == 1):
				t.Fatalf("releases %v; want the minted claim given back and an adopted one left", l.releases)
			case row.store.rowWrites != 0 || row.meta(t, session.WorkStartedKey) != "":
				t.Fatalf("row writes %d, work_started %q; want nothing recorded", row.store.rowWrites, row.meta(t, session.WorkStartedKey))
			case len(l.stepStarted) != 0:
				t.Fatalf("step_started emitted for %v, a claim not handed out", l.stepStarted)
			}
		})
	}
}

// TestHookClaimStaleIncarnationRecordsNothing (T4): a runtime whose token or
// generation is not the row's gets the stale-session drain and writes
// nothing. Kills skipping the identity fence.
func TestHookClaimStaleIncarnationRecordsNothing(t *testing.T) {
	for name, env := range map[string][]string{
		"token":      startedEnv("tok-0", "7"),
		"generation": startedEnv(startedToken, "6"),
	} {
		t.Run(name, func(t *testing.T) {
			row := newStartedRow(t, nil)
			l := &startedLog{}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			doHookClaim("q", "/rig", startedOpts(env), l.ops(startedRoutedWork, row, nil), stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if got.Action != "drain" || got.Reason != hookClaimReasonStaleSession {
				t.Fatalf("result %+v, want the stale-session drain", got)
			}
			if row.store.rowWrites != 0 || row.meta(t, session.WorkStartedKey) != "" {
				t.Fatalf("row writes %d, work_started %q; want none", row.store.rowWrites, row.meta(t, session.WorkStartedKey))
			}
		})
	}
	// The pure decision too: a closed row is stale.
	if res, patch, _ := decideWorkStarted(session.Info{Closed: true, InstanceToken: startedToken, Generation: "7"}, hookStartedFence{Token: startedToken, Gen: 7, HasGen: true}, hookStartedKey{Bead: "b"}, "claim", time.Now()); res != hookStartedStale || patch != nil {
		t.Fatalf("closed row: %v %v, want stale and no patch", res, patch)
	}
}

// TestStartedWriteSurvivesAControllerWriteBetweenReadAndCAS (T5, L3's
// interleaving, deterministic): the controller writes the row between the
// agent's read and its CAS. The agent's CAS loses, re-reads and lands, and
// both cells are intact. With the controller's write a release of the same
// claim, the agent's re-read sees it and records nothing: whichever CAS lands
// first, the other refuses. Kills a blind write in place of UpdateRowFenced
// (the session store here refuses blind writes).
func TestStartedWriteSurvivesAControllerWriteBetweenReadAndCAS(t *testing.T) {
	key := hookStartedKey{Store: startedStoreRef, Bead: "work-1"}
	fence := hookStartedFence{Token: startedToken, Gen: 7, HasGen: true}
	now := time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC)

	row := newStartedRow(t, nil)
	ladder := `{"v":1,"gen":7,"entries":[]}`
	row.store.interleave = map[string]string{session.WorkLadderKey: ladder}
	res, err := recordWorkStarted(row.front, startedSessionID, fence, key, session.WorkStartedViaClaim, now)
	if err != nil || res != hookStartedRecorded {
		t.Fatalf("record = %v, %v; want recorded after a retry", res, err)
	}
	if row.meta(t, session.WorkLadderKey) != ladder || !row.cell(t).Names(key.Store, key.Bead) {
		t.Fatalf("row %q / %q: want the controller's cell and the agent's entry both intact", row.meta(t, session.WorkLadderKey), row.meta(t, session.WorkStartedKey))
	}

	row = newStartedRow(t, nil)
	row.store.interleave = map[string]string{session.WorkLadderKey: `{"v":1,"gen":7,"release":{"store":"rig:repo","bead":"work-1"}}`}
	res, err = recordWorkStarted(row.front, startedSessionID, fence, key, session.WorkStartedViaClaim, now)
	if err != nil || res != hookStartedReleasing || row.meta(t, session.WorkStartedKey) != "" {
		t.Fatalf("record = %v, %v, work_started %q; want releasing and nothing recorded", res, err, row.meta(t, session.WorkStartedKey))
	}
}

// TestHookDrainAckFenceHoldsAStartedClaim (T6, OD9; mc-ilr3b's e2e): no ready
// work (the started step is gate-blocked, so the work query does not serve
// it), and the seat's work_started names a claim still in_progress under its
// identity. The hook answers action=work, reason=started_claim_held, and does
// NOT ack the drain, on the no-work door and the drain-pending one; a claim
// closed or held by another seat lets the ack through. Kills consuming the ack.
func TestHookDrainAckFenceHoldsAStartedClaim(t *testing.T) {
	cell := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Store: startedStoreRef, Bead: "g-1", Gen: 7, Via: session.WorkStartedViaClaim}))
	for _, c := range []struct {
		name            string
		status, holder  string
		draining, wantH bool
	}{
		{"held, no work", "in_progress", "worker-1", false, true},
		{"held, drain pending", "in_progress", "worker-1", true, true},
		{"closed", "closed", "worker-1", false, false},
		{"another seat's", "in_progress", "worker-2", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &startedLog{}
			ops := l.ops(`[]`, nil, func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
				return hookStartedRecorded, nil
			})
			ops.DrainPending = func(string) (bool, error) { return c.draining, nil }
			ops.ReadWorkStarted = func(string) (string, error) { return cell, nil }
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
				return beads.Bead{ID: beadID, Status: c.status, Assignee: c.holder}, nil
			}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if c.wantH {
				if code != 0 || got.Action != "work" || got.Reason != hookClaimWorkReasonStartedClaimHeld || got.BeadID != "g-1" || got.Assignee != "worker-1" || l.acked {
					t.Fatalf("code %d, result %+v, acked %v; want work started_claim_held g-1 and no ack", code, got, l.acked)
				}
				return
			}
			if got.Action != "drain" || !l.acked || !got.DrainAcknowledged {
				t.Fatalf("result %+v, acked %v; want the drain acknowledged", got, l.acked)
			}
		})
	}
}

// TestHookDrainAckFenceFailsSafe (T7): a read the fence needs that fails, of a
// bead, the row, or a cell it cannot decode, leaves the drain unacknowledged.
// Kills failing open.
func TestHookDrainAckFenceFailsSafe(t *testing.T) {
	cell := session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Bead: "g-1", Via: session.WorkStartedViaClaim}))
	for _, c := range []struct {
		name string
		row  func(string) (string, error)
		bead error
	}{
		{"bead read fails", func(string) (string, error) { return cell, nil }, errors.New("bd: connection refused")},
		{"row read fails", func(string) (string, error) { return "", errors.New("store down") }, nil},
		{"cell unreadable", func(string) (string, error) { return "{", nil }, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := &startedLog{}
			ops := l.ops(`[]`, nil, nil)
			ops.ReadWorkStarted = c.row
			ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
				if c.bead != nil {
					return beads.Bead{}, c.bead
				}
				return beads.Bead{ID: beadID, Status: "closed"}, nil
			}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if code == 0 || l.acked || got.Action != "drain" || got.DrainAcknowledged || !strings.Contains(stderr.String(), "drain not acknowledged") {
				t.Fatalf("code %d, result %+v, acked %v, stderr %q; want the drain unacknowledged", code, got, l.acked, stderr.String())
			}
		})
	}
}

// TestHookDiscoveryDrainAckFenceHoldsAStartedClaim: the drain-pending refusal
// on the plain `gc hook` door is fenced as on --claim.
func TestHookDiscoveryDrainAckFenceHoldsAStartedClaim(t *testing.T) {
	l := &startedLog{}
	ops := l.ops(`[]`, nil, nil)
	ops.DrainPending = func(string) (bool, error) { return true, nil }
	ops.ReadWorkStarted = func(string) (string, error) {
		return session.EncodeWorkStarted(session.WorkStarted{}.With(session.WorkStartedEntry{Bead: "g-1"})), nil
	}
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: "worker-1"}, nil
	}
	opts := startedOpts(startedEnv(startedToken, "7"))
	opts.IdentityCandidates = []string{"worker-1"}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	runner := func(string, string) (string, error) { return "", nil }
	code := doHookDiscovery("q", "/rig", false, opts, ops, runner, stdout, &stderr, hookVisibility{})

	got := decodeTurnBoundResult(t, stdout.buf.String())
	if code != 0 || got.Reason != hookClaimWorkReasonStartedClaimHeld || l.acked {
		t.Fatalf("code %d, result %+v, acked %v; want started_claim_held and no ack", code, got, l.acked)
	}
}

// TestUndeliveredClaimForgetsStartedBeforeRelease (T10): the F-C unwind of a
// minted claim whose result could not be written (EPIPE) drops its
// work_started entry before it gives the claim back, so a released claim never
// reads as started. Kills no removal and the removal after the release.
func TestUndeliveredClaimForgetsStartedBeforeRelease(t *testing.T) {
	row := newStartedRow(t, nil)
	l := &startedLog{}
	stdout := &orderedWriter{l: l, err: syscall.EPIPE}
	var stderr bytes.Buffer
	code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(startedRoutedWork, row, nil), stdout, &stderr)

	rec, forget, release := l.index("record:work-1"), l.index("forget:work-1"), l.index("release:work-1")
	if code != 1 || rec < 0 || forget < 0 || release < 0 || rec >= forget || forget >= release {
		t.Fatalf("code %d, effects %v; want record, then forget, then release", code, l.log)
	}
	if row.cell(t).Names(startedStoreRef, "work-1") {
		t.Fatalf("work_started %+v still names the released claim", row.cell(t))
	}
}

// TestHookClaimFailsOpenWhenTheStartedWriteFails (T13): a store error, and a
// seat with no instance token to fence by, still deliver the claim, with one
// stderr line and work.started_unrecorded. Kills failing closed and failing
// silently.
func TestHookClaimFailsOpenWhenTheStartedWriteFails(t *testing.T) {
	for name, c := range map[string]struct {
		env    []string
		record error
	}{
		"store error":       {startedEnv(startedToken, "7"), errors.New("sessions store unavailable")},
		"no instance token": {[]string{"GC_SESSION_ID=" + startedSessionID}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			l := &startedLog{}
			ops := l.ops(startedRoutedWork, nil, func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
				return hookStartedUnrecorded, c.record
			})
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			code := doHookClaim("q", "/rig", startedOpts(c.env), ops, stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if code != 0 || got.Action != "work" || got.BeadID != "work-1" {
				t.Fatalf("code %d, result %+v; want work-1 delivered", code, got)
			}
			if len(l.unrecorded) != 1 || l.unrecorded[0] != "work-1" || strings.Count(stderr.String(), "delivering it anyway") != 1 {
				t.Fatalf("unrecorded %v, stderr %q; want one event and one line", l.unrecorded, stderr.String())
			}
		})
	}
	// Contention past every attempt is a failure too, not a quiet success.
	row := newStartedRow(t, nil)
	front := session.NewStore(beads.SessionStore{Store: blindWriteRefusingStore{inner: alwaysMovedStore{row.store}}})
	if _, err := recordWorkStarted(front, startedSessionID, hookStartedFence{Token: startedToken}, hookStartedKey{Bead: "b"}, "claim", time.Now()); !errors.Is(err, errWorkStartedContended) {
		t.Fatalf("contended record err = %v, want errWorkStartedContended", err)
	}
}

// alwaysMovedStore loses every CAS, as a row the controller rewrites between
// every read and write would.
type alwaysMovedStore struct{ *casCountingStore }

func (s alwaysMovedStore) UpdateIfMatch(id string, _ int64, opts beads.UpdateOpts) error {
	return s.casCountingStore.UpdateIfMatch(id, -1, opts)
}

// TestHookStartedFenceOf reads the fence from the env: the token, and the
// generation only when it parses.
func TestHookStartedFenceOf(t *testing.T) {
	if f := hookStartedFenceOf(startedEnv(" tok ", "12")); f != (hookStartedFence{Token: "tok", Gen: 12, HasGen: true}) {
		t.Errorf("fence = %+v", f)
	}
	if f := hookStartedFenceOf([]string{"GC_INSTANCE_TOKEN=tok", "GC_RUNTIME_EPOCH=x"}); f != (hookStartedFence{Token: "tok"}) {
		t.Errorf("unparsable generation: fence = %+v, want the token alone", f)
	}
}

// TestStartedClaimHeldIsInThePublishedSchema: the fence's work reason is one
// the result schema admits, and not as a drain reason.
func TestStartedClaimHeldIsInThePublishedSchema(t *testing.T) {
	chdirToRealPackageDir(t)
	all, drain := hookClaimSchemaReasonEnums(t)
	if !slices.Contains(all, hookClaimWorkReasonStartedClaimHeld) || slices.Contains(drain, hookClaimWorkReasonStartedClaimHeld) {
		t.Fatalf("schema reasons %v (drain %v): want %q admitted, as a work reason only", all, drain, hookClaimWorkReasonStartedClaimHeld)
	}
}
