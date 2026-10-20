package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
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
	causes      []string // the work.started_unrecorded causes, in order
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
		EmitWorkStartedUnrecorded: func(_, _, beadID string, cause error) {
			l.add("unrecorded:" + beadID)
			l.unrecorded = append(l.unrecorded, beadID)
			l.causes = append(l.causes, cause.Error())
		},
		EmitExecutionStepStarted: func(step beads.Bead, _ string, _ []string, _ string) {
			l.add("step_started:" + step.ID)
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
		StartedFence: hookStartedFenceOf(env),
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
	// Resident: the minted claim's compare-and-swap committed in the work
	// leg, the residence every reader keys it by (claimStoreRef).
	if len(cell.Claims) != 1 || cell.Claims[0] != (session.WorkStartedEntry{Store: startedStoreRef, Bead: "work-1", Gen: 7, At: time.Date(2026, 10, 17, 20, 0, 0, 0, time.UTC), Via: session.WorkStartedViaClaim, Resident: true}) {
		t.Fatalf("work_started = %+v, want the one minted entry, marked resident", cell.Claims)
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
// nothing. A claim minted before the started write found the row stale (a
// generation-only mismatch passes the pre-claim fence) is given back, not
// parked on the seat. Kills skipping the identity fence, and a stale arm that
// does not unwind.
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
			if minted := l.index("claim:work-1") >= 0; minted != (len(l.releases) == 1) || len(l.releases) > 1 {
				t.Fatalf("effects %v, releases %v; want every claim minted for a stale seat given back", l.log, l.releases)
			}
			if name == "generation" && len(l.releases) != 1 {
				t.Fatalf("effects %v; want the generation-only mismatch to mint the claim and give it back", l.log)
			}
			if len(l.stepStarted) != 0 {
				t.Fatalf("step_started emitted for %v, a claim not handed out", l.stepStarted)
			}
		})
	}
	// The pure decision too: a closed row is refused, as closed.
	if res, patch, _ := decideWorkStarted(session.Info{Closed: true, InstanceToken: startedToken, Generation: "7"}, hookStartedFence{Token: startedToken, Gen: 7, HasGen: true}, hookStartedKey{Bead: "b"}, "claim", time.Now()); res != hookStartedClosed || patch != nil {
		t.Fatalf("closed row: %v %v, want closed and no patch", res, patch)
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
	if row.meta(t, session.WorkLadderKey) != ladder || !row.cell(t).Names(key.Store, key.Bead, 7) {
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
				return beads.Bead{ID: beadID, Status: c.status, Assignee: c.holder, Metadata: map[string]string{
					beadmeta.RootBeadIDMetadataKey:        "root-1",
					beadmeta.ContinuationGroupMetadataKey: "main",
				}}, nil
			}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if c.wantH {
				if code != 0 || got.Action != "work" || got.Reason != hookClaimWorkReasonStartedClaimHeld || got.BeadID != "g-1" || got.Assignee != "worker-1" || l.acked {
					t.Fatalf("code %d, result %+v, acked %v; want work started_claim_held g-1 and no ack", code, got, l.acked)
				}
				// The held answer carries the workflow position every other
				// action=work answer does.
				if got.RootBeadID != "root-1" || got.ContinuationGroup != "main" {
					t.Fatalf("result %+v; want root_bead_id root-1 and continuation_group main", got)
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
	if row.cell(t).Names(startedStoreRef, "work-1", 7) {
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
			if len(l.unrecorded) != 1 || l.unrecorded[0] != "work-1" || !strings.HasPrefix(l.causes[0], "delivered without a work_started record") ||
				strings.Count(stderr.String(), "delivering it anyway") != 1 {
				t.Fatalf("unrecorded %v, causes %q, stderr %q; want one event and one line", l.unrecorded, l.causes, stderr.String())
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

// TestUnrecordedClaimOutsideTheDrainAckFence pins what keeps a claim the
// started write failed open on (recordHookWorkStarted): re-served, the next
// hand-over re-attempts the record through the adopt door; not re-served (its
// step gate-blocked) and unrecorded, the fence does not hold the ack, so only
// D5's execution_claim_started keep (with the ladder rolled back) stands
// between it and the release. Kills a re-served claim not re-recorded, and a
// fence that silently holds an ack for a claim it has no record of.
func TestUnrecordedClaimOutsideTheDrainAckFence(t *testing.T) {
	row := newStartedRow(t, nil)
	l := &startedLog{}
	failing := l.ops(startedRoutedWork, nil, func(string, hookStartedFence, hookStartedKey, string) (hookStartedResult, error) {
		return hookStartedUnrecorded, errors.New("sessions store unavailable")
	})
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), failing, stdout, &stderr); code != 0 {
		t.Fatalf("code %d; stderr=%s", code, stderr.String())
	}
	if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Action != "work" || got.BeadID != "work-1" || row.meta(t, session.WorkStartedKey) != "" {
		t.Fatalf("result %+v, work_started %q; want work-1 delivered and unrecorded", got, row.meta(t, session.WorkStartedKey))
	}

	t.Run("re-served", func(t *testing.T) {
		l := &startedLog{}
		stdout := &orderedWriter{l: l}
		var stderr bytes.Buffer
		if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(startedAssignedWork, row, nil), stdout, &stderr); code != 0 {
			t.Fatalf("code %d; stderr=%s", code, stderr.String())
		}
		if l.index("record:work-1:adopt") < 0 || !row.cell(t).Names(startedStoreRef, "work-1", 7) || l.acked {
			t.Fatalf("effects %v, cell %+v; want the adoption to record the claim, without an ack", l.log, row.cell(t))
		}
	})

	t.Run("gate-blocked", func(t *testing.T) {
		l := &startedLog{}
		ops := l.ops(`[]`, nil, nil)
		ops.ReadWorkStarted = func(string) (string, error) { return "", nil } // the failed write left the cell empty
		stdout := &orderedWriter{l: l}
		var stderr bytes.Buffer
		doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)
		if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Action != "drain" || !l.acked {
			t.Fatalf("result %+v, acked %v; an unrecorded claim does not hold the ack: D5's stamp keep is its only cover", got, l.acked)
		}
	})
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

// drainAckFenceRun is one no-work `gc hook --claim --drain-ack` against a
// work_started cell and a live bead read, through the fence.
func drainAckFenceRun(t *testing.T, cell string, read func(beadID string) (beads.Bead, error)) (hookClaimJSONResult, *startedLog, string) {
	t.Helper()
	l := &startedLog{}
	ops := l.ops(`[]`, nil, nil)
	ops.ReadWorkStarted = func(string) (string, error) { return cell, nil }
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return read(beadID)
	}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)
	return decodeTurnBoundResult(t, stdout.buf.String()), l, stderr.String()
}

func startedCell(entries ...session.WorkStartedEntry) string {
	w := session.WorkStarted{}
	for _, e := range entries {
		w = w.With(e)
	}
	return session.EncodeWorkStarted(w)
}

// TestHookDrainAckFenceSkipsDispatchHolds (must-fix 2, gas-kg6): a started
// claim parked on a dispatch hold is never work the hook serves, so it does
// not hold the ack: the drain is acknowledged. Kills a fence that ignores
// either hold label.
func TestHookDrainAckFenceSkipsDispatchHolds(t *testing.T) {
	for _, hold := range []string{beadmeta.HoldMayorLabel, beadmeta.HoldExternalLabel} {
		t.Run(hold, func(t *testing.T) {
			got, l, stderr := drainAckFenceRun(t, startedCell(session.WorkStartedEntry{Store: startedStoreRef, Bead: "p-1"}), func(id string) (beads.Bead, error) {
				return beads.Bead{ID: id, Status: "in_progress", Assignee: "worker-1", Labels: []string{hold}}, nil
			})
			if got.Action != "drain" || !l.acked || !got.DrainAcknowledged {
				t.Fatalf("result %+v, acked %v, stderr %q; want the drain acknowledged past a %s claim", got, l.acked, stderr, hold)
			}
		})
	}
}

// TestHookDrainAckFenceReadsAnswerNotFoundAndForeignStores (must-fix 3): a
// bead the store no longer has holds nothing, and an entry naming another
// store whose read fails here is skipped; either way the ack goes through. A
// failed read of an entry of this store still fails safe (T7). Kills an entry
// for a purged or foreign bead blocking every drain-ack.
func TestHookDrainAckFenceReadsAnswerNotFoundAndForeignStores(t *testing.T) {
	for _, c := range []struct {
		name  string
		entry session.WorkStartedEntry
		err   error
		acked bool
	}{
		{"not found here", session.WorkStartedEntry{Store: startedStoreRef, Bead: "gone-1"}, fmt.Errorf("bd show gone-1: %w", beads.ErrNotFound), true},
		{"another store, unreadable here", session.WorkStartedEntry{Store: "rig:other", Bead: "o-1"}, errors.New("bd: no issue o-1 in this database"), true},
		{"this store, unreadable", session.WorkStartedEntry{Store: startedStoreRef, Bead: "h-1"}, errors.New("bd: connection refused"), false},
		{"unknown store, unreadable", session.WorkStartedEntry{Bead: "h-1"}, errors.New("bd: connection refused"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, l, stderr := drainAckFenceRun(t, startedCell(c.entry), func(string) (beads.Bead, error) { return beads.Bead{}, c.err })
			if l.acked != c.acked || got.DrainAcknowledged != c.acked {
				t.Fatalf("result %+v, acked %v, stderr %q; want acked=%v", got, l.acked, stderr, c.acked)
			}
		})
	}
}

// TestHookDrainAckFenceFindsAHeldClaimBehindOthers (E20): the fence reads
// every entry, not only the newest: a held claim behind a closed one, a held
// one and a purged one still holds the ack.
func TestHookDrainAckFenceFindsAHeldClaimBehindOthers(t *testing.T) {
	cell := startedCell(
		session.WorkStartedEntry{Store: startedStoreRef, Bead: "held-1"},
		session.WorkStartedEntry{Store: startedStoreRef, Bead: "gone-1"},
		session.WorkStartedEntry{Store: startedStoreRef, Bead: "parked-1"},
		session.WorkStartedEntry{Store: startedStoreRef, Bead: "done-1"},
	)
	got, l, _ := drainAckFenceRun(t, cell, func(id string) (beads.Bead, error) {
		switch id {
		case "held-1":
			return beads.Bead{ID: id, Status: "in_progress", Assignee: "worker-1"}, nil
		case "gone-1":
			return beads.Bead{}, beads.ErrNotFound
		case "parked-1":
			return beads.Bead{ID: id, Status: "in_progress", Assignee: "worker-1", Labels: []string{beadmeta.HoldMayorLabel}}, nil
		default:
			return beads.Bead{ID: id, Status: "closed", Assignee: "worker-1"}, nil
		}
	})
	if got.Reason != hookClaimWorkReasonStartedClaimHeld || got.BeadID != "held-1" || l.acked {
		t.Fatalf("result %+v, acked %v; want started_claim_held for held-1, the oldest entry", got, l.acked)
	}
}

// TestHookClaimRecordsStartedBeforeTheStepStartedEvent (E3): a graph step
// handed to a turn is recorded in work_started before its
// execution.step_started is emitted, so a refused hand-over emits none.
func TestHookClaimRecordsStartedBeforeTheStepStartedEvent(t *testing.T) {
	row := newStartedRow(t, nil)
	l := &startedLog{}
	ops := l.ops(startedRoutedWork, row, nil)
	ops.ReadWorkMeta = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, error) {
		return beads.Bead{ID: beadID, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: "root-1", beadmeta.StepIDMetadataKey: "step-1", beadmeta.SessionIDMetadataKey: startedSessionID,
		}}, nil
	}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr); code != 0 {
		t.Fatalf("code %d; stderr=%s", code, stderr.String())
	}
	rec, step := l.index("record:work-1"), l.index("step_started:work-1")
	if rec < 0 || step < 0 || rec > step {
		t.Fatalf("effects %v: want the started record before execution.step_started", l.log)
	}
}

// TestHookClaimResetsAnUnreadableCell (E2b, nit 9): a work_started cell the
// hook cannot read is replaced by a readable one holding the claim it hands
// out, and the reset is reported off the pane (work.started_unrecorded: the
// claims the old cell held are no longer recorded). The claim is delivered.
func TestHookClaimResetsAnUnreadableCell(t *testing.T) {
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: "{"})
	l := &startedLog{}
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(startedRoutedWork, row, nil), stdout, &stderr)

	if got := decodeTurnBoundResult(t, stdout.buf.String()); code != 0 || got.BeadID != "work-1" {
		t.Fatalf("code %d, result %+v; want work-1 delivered", code, got)
	}
	if cell := row.cell(t); len(cell.Claims) != 1 || !cell.Names(startedStoreRef, "work-1", 7) {
		t.Fatalf("work_started = %+v, want a readable cell holding work-1", cell)
	}
	// Reported as a reset, never as a claim delivered without a record: it
	// is recorded.
	if len(l.causes) != 1 || !strings.Contains(l.causes[0], "was reset to this claim") || strings.Contains(l.causes[0], "delivered without") ||
		!strings.Contains(stderr.String(), "recorded work-1 as started") || strings.Contains(stderr.String(), "delivering it anyway") {
		t.Fatalf("causes %q, stderr %q; want the reset reported as a reset", l.causes, stderr.String())
	}
}

// TestHookClaimOnAClosedRowGivesTheMintedClaimBack (nit 7): no turn of a
// closed row runs a claim, so a minted one is given back before the stale
// drain, and nothing is recorded; an adopted one is left to the close
// cascade.
func TestHookClaimOnAClosedRowGivesTheMintedClaimBack(t *testing.T) {
	for _, c := range []struct {
		name, output string
		minted       bool
	}{
		{"minted", startedRoutedWork, true},
		{"adopted", startedAssignedWork, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := newStartedRow(t, nil)
			if err := row.store.Close(row.id); err != nil {
				t.Fatal(err)
			}
			l := &startedLog{}
			stdout := &orderedWriter{l: l}
			var stderr bytes.Buffer
			doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), l.ops(c.output, row, nil), stdout, &stderr)

			got := decodeTurnBoundResult(t, stdout.buf.String())
			if got.Action != "drain" || got.Reason != hookClaimReasonStaleSession || c.minted != (len(l.releases) == 1) || row.store.rowWrites != 0 {
				t.Fatalf("result %+v, releases %v, row writes %d; want the stale drain, the minted claim given back, nothing recorded", got, l.releases, row.store.rowWrites)
			}
		})
	}
}

// TestForgetWorkStartedLeavesAClosedRow (E21): the unwind writes nothing to a
// closed row.
func TestForgetWorkStartedLeavesAClosedRow(t *testing.T) {
	cell := startedCell(session.WorkStartedEntry{Store: startedStoreRef, Bead: "work-1"})
	row := newStartedRow(t, map[string]string{session.WorkStartedKey: cell})
	if err := row.store.Close(row.id); err != nil {
		t.Fatal(err)
	}
	if err := forgetWorkStarted(row.front, startedSessionID, hookStartedKey{Store: startedStoreRef, Bead: "work-1"}); err != nil {
		t.Fatal(err)
	}
	if row.store.rowWrites != 0 || row.meta(t, session.WorkStartedKey) != cell {
		t.Fatalf("row writes %d, cell %q; want the closed row untouched", row.store.rowWrites, row.meta(t, session.WorkStartedKey))
	}
}

// TestHookDiscoveryOptionsCarryIdentities (E1): plain `gc hook` hands the
// drain-ack fence the seat's identities, as --claim does.
func TestHookDiscoveryOptionsCarryIdentities(t *testing.T) {
	got := hookDiscoveryOptions("worker-1", []string{"worker-1", "sess-1"}, []string{"GC_SESSION_ID=sess-1"}, hookCommandOptions{DrainAck: true, JSON: true}, true)
	if got.Assignee != "worker-1" || !slices.Equal(got.IdentityCandidates, []string{"worker-1", "sess-1"}) || !got.DrainAck || !got.JSON || !got.StrictDrainAck || len(got.Env) != 1 {
		t.Fatalf("discovery options %+v", got)
	}
}

// TestRuntimeDrainAckRefusesASeatHoldingStartedWork (should-fix 4): a seat's
// own `gc runtime drain-ack` is refused, with nothing acknowledged, when the
// release pass refuses (it holds started work the release would keep). An
// --operator ack reaches the pass as an operator's, but one whose caller's
// GC_SESSION_ID names the target is refused before it: an agent retrying as
// --operator on itself would bring the stop/wake loop back.
func TestRuntimeDrainAckRefusesASeatHoldingStartedWork(t *testing.T) {
	prevRelease, prevCaller := drainAckReleaseHeldClaims, drainAckCallerSessionID
	t.Cleanup(func() { drainAckReleaseHeldClaims, drainAckCallerSessionID = prevRelease, prevCaller })
	for _, c := range []struct {
		name     string
		operator bool
		callerID string
		refusal  string
		want     int
		passed   bool // the release pass ran
	}{
		{"own ack, started work held", false, "gc-7", "this session holds g-1", drainAckRefused, true},
		{"own ack, nothing held", false, "gc-7", "", 0, true},
		{"operator ack from outside", true, "gc-9", "", 0, true},
		{"operator ack on its own session", true, "gc-7", "", drainAckRefused, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got *drainAckCaller
			drainAckCallerSessionID = func() string { return c.callerID }
			drainAckReleaseHeldClaims = func(_, _ string, caller drainAckCaller, _ io.Writer) string { got = &caller; return c.refusal }
			dops := newFakeDrainOps()
			var stdout, stderr bytes.Buffer
			code := runtimeDrainAck(dops, sessionRuntimeTarget{sessionName: "worker-1", sessionID: "gc-7", display: "worker-1"}, c.operator, "tok-7", false, &stdout, &stderr)
			if code != c.want || dops.acked["worker-1"] != (c.want == 0) || (got != nil) != c.passed {
				t.Fatalf("code %d (want %d), acked %v, pass ran %v; stderr=%s", code, c.want, dops.acked["worker-1"], got != nil, stderr.String())
			}
			if got != nil && *got != (drainAckCaller{operator: c.operator, sessionID: c.callerID, token: "tok-7"}) {
				t.Fatalf("the pass got caller %+v", *got)
			}
			if !c.passed && !strings.Contains(stderr.String(), "--operator acks another session from outside it") {
				t.Fatalf("stderr %q: want the --operator self-ack named", stderr.String())
			}
		})
	}
}

// TestDrainAckPassRefusesBeforeReleasingAnything (should-fix 4, review item
// 2): the started-claim check is the release pass's own read, under its
// budget. A seat's own ack that holds a started claim releases nothing, not
// even its unstarted claims, and is refused with no city config too (the
// refusal releases nothing, so it needs none); an --operator ack whose
// caller's token or session ID names the seat is refused outright (the
// session-ID half catches a name-only target, which runtimeDrainAck's early
// check cannot see); an operator from outside releases the unstarted claim
// and keeps the started one.
func TestDrainAckPassRefusesBeforeReleasingAnything(t *testing.T) {
	for _, c := range []struct {
		name          string
		caller        drainAckCaller
		ownID         bool // the caller's GC_SESSION_ID is the seat's bead ID
		noConfig      bool
		refusal       string
		unstartedOpen bool
	}{
		{"own ack", drainAckCaller{}, false, false, "an operator acks from outside this session with --operator", false},
		{"own ack, no city config", drainAckCaller{}, false, true, "an operator acks from outside this session with --operator", false},
		{"operator, the seat's own token", drainAckCaller{operator: true, token: "tok-1"}, false, false, "--operator acks another session from outside it", false},
		{"operator, the seat's own session ID", drainAckCaller{operator: true, token: "tok-9"}, true, false, "--operator acks another session from outside it", false},
		{"operator from outside", drainAckCaller{operator: true, token: "tok-9", sessionID: "other"}, false, false, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := beads.NewMemStore()
			seat := mustCreateDrainAckBead(t, store, beads.Bead{
				ID: "worker-1", Title: "city/worker-1", Type: session.BeadType,
				Metadata: map[string]string{"session_name": "worker-1", "template": "worker", "instance_token": "tok-1"},
			}, "", "")
			started := mustCreateDrainAckBead(t, store, beads.Bead{Title: "started", Type: "task", Metadata: routedToWorker()}, "in_progress", "worker-1")
			unstarted := mustCreateDrainAckBead(t, store, beads.Bead{Title: "unstarted", Type: "task", Metadata: routedToWorker()}, "in_progress", "worker-1")
			casSetMeta(t, store, seat.ID, session.WorkStartedKey, startedCell(session.WorkStartedEntry{Bead: started.ID}))

			caller, cfg := c.caller, servingCity("worker")
			if c.ownID {
				caller.sessionID = seat.ID
			}
			if c.noConfig {
				cfg = nil
			}
			var stderr bytes.Buffer
			refusal := releaseUnexecutedClaimsForSessionStore("", cfg, store, nil, "worker-1", caller, &stderr)

			if (c.refusal == "") != (refusal == "") || !strings.Contains(refusal, c.refusal) {
				t.Fatalf("refusal %q, want one containing %q", refusal, c.refusal)
			}
			if status, _ := drainAckBeadStatus(t, store, started.ID); status != "in_progress" {
				t.Fatalf("started claim is %s, want kept", status)
			}
			if status, _ := drainAckBeadStatus(t, store, unstarted.ID); (status == "open") != c.unstartedOpen {
				t.Fatalf("unstarted claim is %s; want released=%v", status, c.unstartedOpen)
			}
		})
	}
}

// TestStartedGateBlockedClaimSurvivesTheNudge is mc-ilr3b's scenario end to
// end, over real stores: the seat's hook hands it a claim and records it as
// started; the step then blocks on a gate, so the work query serves it no
// more; the seat is nudged with a --drain-ack nudge and runs the hook. The
// hook answers started_claim_held and acks nothing, a direct
// `gc runtime drain-ack` of its own would be refused (releaseDrainAckClaims), and
// had an ack happened anyway the drain-ack release keeps the claim. It stays
// in_progress on the seat throughout.
func TestStartedGateBlockedClaimSurvivesTheNudge(t *testing.T) {
	work := splittest.NewWorkStore(t, "gc")
	claim := mustCreateDrainAckBead(t, work, beads.Bead{Title: "gate-blocked step", Type: "task", Metadata: routedToWorker()}, "", "")
	row := newStartedRow(t, nil)
	legs := testWorkLegs("", servingCity("worker"), work, nil)
	seatBead := func() beads.Bead {
		b, err := row.store.Get(row.id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	readClaim := func(_ context.Context, _ string, _ []string, beadID, _ string) (beads.Bead, error) {
		return work.Get(beadID)
	}

	// The hand-over: a real claim of the bead, recorded on the row.
	l := &startedLog{}
	ops := l.ops(`[{"id":"`+claim.ID+`","status":"open","metadata":{"gc.routed_to":"worker"}}]`, row, nil)
	ops.Claim = func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
		status := "in_progress"
		if err := work.Update(beadID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			return beads.Bead{}, false, err
		}
		b, err := work.Get(beadID)
		return b, true, err
	}
	ops.ReadWorkMeta = readClaim
	stdout := &orderedWriter{l: l}
	var stderr bytes.Buffer
	if code := doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr); code != 0 || !row.cell(t).Names("", claim.ID, 7) {
		t.Fatalf("hand-over: code %d, cell %+v; stderr=%s", code, row.cell(t), stderr.String())
	}

	// Gate-blocked and nudged: the query serves nothing; the nudge carries
	// --drain-ack; the ack would run the real drain-ack release.
	l = &startedLog{}
	ops = l.ops(`[]`, row, nil)
	ops.ReadWorkStarted = func(string) (string, error) { return row.meta(t, session.WorkStartedKey), nil }
	ops.ReadWorkMeta = readClaim
	ops.DrainAck = func(w io.Writer) error {
		l.acked = true
		releaseUnexecutedClaimsOnDrainAck(legs, seatBead(), drainAckReleaseBudget, w)
		return nil
	}
	stdout = &orderedWriter{l: l}
	doHookClaim("q", "/rig", startedOpts(startedEnv(startedToken, "7")), ops, stdout, &stderr)
	if got := decodeTurnBoundResult(t, stdout.buf.String()); got.Reason != hookClaimWorkReasonStartedClaimHeld || l.acked {
		t.Fatalf("nudged hook: result %+v, acked %v; want started_claim_held and no ack", got, l.acked)
	}

	seat := drainAckSessionBead()
	seat.Metadata[session.WorkStartedKey] = row.meta(t, session.WorkStartedKey)
	if held := releaseDrainAckClaims(legs, seat, true, drainAckReleaseBudget, &stderr); len(held) != 1 || held[0] != claim.ID {
		t.Fatalf("a direct drain-ack's pass found %v; want the claim, so the ack is refused", held)
	}
	releaseUnexecutedClaimsOnDrainAck(legs, seat, drainAckReleaseBudget, &stderr)
	if status, assignee := drainAckBeadStatus(t, work, claim.ID); status != "in_progress" || assignee != "worker-1" {
		t.Fatalf("claim is %s/%q; want it in_progress on the seat", status, assignee)
	}
}

// TestPlainHookPassesIdentitiesToTheDiscoveryDoor (E1, at the call site):
// `gc hook` hands doHookDiscovery the seat's assignee and identities, which
// the drain-ack fence on that door matches a started claim's assignee
// against. (Plain `gc hook` refuses --drain-ack today, "requires --claim", so
// the fence there is defense in depth: it holds the moment that changes.)
func TestPlainHookPassesIdentitiesToTheDiscoveryDoor(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "current-token")
	installFenceWorkQueryProbe(t)
	setFenceClaimEnv(t, cityDir, sessionID, "current-token")
	prev := hookDiscover
	t.Cleanup(func() { hookDiscover = prev })
	var got *hookClaimOptions
	hookDiscover = func(_, _ string, _ bool, opts hookClaimOptions, _ hookClaimOps, _ WorkQueryRunner, _, _ io.Writer, _ hookVisibility) int {
		got = &opts
		return 0
	}

	var stdout, stderr bytes.Buffer
	cmdHookWithOptions(nil, hookCommandOptions{JSON: true}, &stdout, &stderr)

	if got == nil {
		t.Fatalf("plain gc hook never reached the discovery door; stderr=%s", stderr.String())
	}
	if got.Assignee == "" || !slices.Contains(got.IdentityCandidates, sessionID) || !slices.Contains(got.IdentityCandidates, "worker-1") {
		t.Fatalf("discovery door options %+v; want the assignee and the seat's identities (%s, worker-1)", *got, sessionID)
	}
}
