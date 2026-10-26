package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	gascity "github.com/gastownhall/gascity"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/session"
)

var cliOpT0 = time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)

// cliOpHarness pins the CLI op glue's seams for one test: the env the gates
// and the principal read, the clock, and an op store opened with a fixed id
// prefix (a file or mem store reports none). Tests that use it must not run
// in parallel.
type cliOpHarness struct {
	env   map[string]string
	clk   *clock.Fake
	opens int
	// onOpen, when set, runs as the recorder opens the op store, just
	// before it proposes the op.
	onOpen func()
	// opStore, when set, holds the op rows instead of the command's sessions
	// store: a file-backed city's store mints its own ids, so it cannot pin
	// an op id.
	opStore beads.Store
}

// cliOpsOffEnv turns every ops gate off through the env break-glass: the
// opt-out from the gates' auto default (D-GATES).
func cliOpsOffEnv() map[string]string {
	return map[string]string{"GC_OPS_CONTENT": "off", "GC_OPS_CONTROL": "off", "GC_OPS_LIFECYCLE": "off"}
}

func newCLIOpHarness(t *testing.T, env map[string]string) *cliOpHarness {
	t.Helper()
	h := &cliOpHarness{env: env, clk: &clock.Fake{Time: cliOpT0}}
	oldLookup, oldClock, oldOpen := cliOpLookupEnv, cliOpClock, cliOpenOpStore
	cliOpLookupEnv = func(k string) (string, bool) { v, ok := h.env[k]; return v, ok }
	cliOpClock = h.clk
	cliOpenOpStore = func(s beads.Store) (*opjournal.Store, error) {
		h.opens++
		if h.onOpen != nil {
			h.onOpen()
		}
		if h.opStore != nil {
			s = h.opStore
		}
		return opjournal.NewStore(beads.SessionStore{Store: s}, cliOpClock, opjournal.WithIDPrefix("gc"))
	}
	t.Cleanup(func() { cliOpLookupEnv, cliOpClock, cliOpenOpStore = oldLookup, oldClock, oldOpen })
	return h
}

// cliOpRows lists every op row in store, closed ones included.
func cliOpRows(t *testing.T, store beads.Store) []opjournal.Op {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing op rows: %v", err)
	}
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	var ops []opjournal.Op
	for _, b := range rows {
		if b.Type != opjournal.BeadType {
			continue
		}
		op, err := st.Get(b.ID)
		if err != nil {
			t.Fatalf("reading op %s: %v", b.ID, err)
		}
		ops = append(ops, op)
	}
	return ops
}

// cliOpOnly returns the one op row in store.
func cliOpOnly(t *testing.T, store beads.Store) opjournal.Op {
	t.Helper()
	ops := cliOpRows(t, store)
	if len(ops) != 1 {
		t.Fatalf("op rows = %d (%+v), want 1", len(ops), ops)
	}
	return ops[0]
}

// cliOpWantOutcome fails unless op closed with arm, reason and via.
func cliOpWantOutcome(t *testing.T, op opjournal.Op, arm opjournal.Arm, reason, via string) {
	t.Helper()
	if op.Status != opjournal.StatusClosed {
		t.Fatalf("op %s status = %s, want closed", op.ID, op.Status)
	}
	if op.Outcome.Arm != arm || op.Outcome.Reason != reason || op.Outcome.Result.Via != via {
		t.Fatalf("op %s outcome = %+v, want %s{%s} via %q", op.ID, op.Outcome, arm, reason, via)
	}
	if op.Executor != opjournal.ExecutorLegacyCLI {
		t.Fatalf("op %s executor = %s, want legacy:cli", op.ID, op.Executor)
	}
}

// cliOpJSON decodes one JSON line of a session verb's stdout.
func cliOpJSON(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out), &m); err != nil {
		t.Fatalf("decoding JSON %q: %v", out, err)
	}
	return m
}

// cliOpFailureCode is the error code of a failure envelope, which must
// validate against schemas/failure.schema.json.
func cliOpFailureCode(t *testing.T, out []byte) string {
	t.Helper()
	raw, err := gascity.BuiltinSchemas.ReadFile("schemas/failure.schema.json")
	if err != nil {
		t.Fatalf("reading the failure schema: %v", err)
	}
	var doc any
	if err := json.Unmarshal(bytes.TrimSpace(out), &doc); err != nil {
		t.Fatalf("decoding JSON %q: %v", out, err)
	}
	if err := compileJSONSchema(t, "gc://schemas/failure.schema.json", raw).Validate(doc); err != nil {
		t.Fatalf("failure envelope %s does not match the failure schema: %v", out, err)
	}
	m := cliOpJSON(t, out)
	if m["ok"] != false {
		t.Fatalf("failure envelope %s, want ok:false", out)
	}
	e, _ := m["error"].(map[string]any)
	code, _ := e["code"].(string)
	return code
}

// cliOpFirstIndex is the index of the first recorded op matching match, or -1.
func cliOpFirstIndex(ops []beadstest.RecordedOp, match func(beadstest.RecordedOp) bool) int {
	for i, op := range ops {
		if match(op) {
			return i
		}
	}
	return -1
}

// cliOpProposeOpen writes an open legacy:cli op for conv under the operator
// principal and key, as another CLI process mid-run would leave it.
func cliOpProposeOpen(t *testing.T, store beads.Store, kind opjournal.Kind, conv, key string, scope opjournal.Scope, at time.Time, policy opjournal.Policy) opjournal.Op {
	t.Helper()
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, &clock.Fake{Time: at}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	op, created, err := st.Propose(opjournal.Request{
		Conversation: conv, Kind: kind, Executor: opjournal.ExecutorLegacyCLI, ClientKey: key,
		Producer: opjournal.Producer{Principal: legacyop.PrincipalOperator, Actor: opjournal.ActorOperator},
		Policy:   policy, Scope: scope,
		StartBy: at.Add(legacyop.ExecutionBound), FinishBy: at.Add(legacyop.ExecutionBound),
	})
	if err != nil || !created {
		t.Fatalf("proposing open op: created=%t err=%v", created, err)
	}
	return op
}

// cliOpPlantClosed writes a legacy:cli op for conv under the operator
// principal and key, closed with outcome, as an earlier CLI run would leave
// it.
func cliOpPlantClosed(t *testing.T, store beads.Store, kind opjournal.Kind, conv, key string, scope opjournal.Scope, outcome opjournal.Outcome) opjournal.Op {
	t.Helper()
	op := cliOpProposeOpen(t, store, kind, conv, key, scope, cliOpT0, opjournal.Policy{})
	st, err := opjournal.NewStore(beads.SessionStore{Store: store}, &clock.Fake{Time: cliOpT0}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatalf("opening op store: %v", err)
	}
	closed, err := st.Close(op.ID, outcome, legacyop.PrincipalOperator)
	if err != nil {
		t.Fatalf("closing op %s: %v", op.ID, err)
	}
	return closed
}

// cliOpIncapableStore hides the conditional-write family, as an old bd does.
type cliOpIncapableStore struct{ beads.Store }

func TestCLIPrincipal(t *testing.T) {
	for _, tc := range []struct {
		name          string
		env           map[string]string
		wantPrincipal string
		wantActor     string
	}{
		{"no session", nil, "operator", "operator"},
		{"agent session", map[string]string{"GC_SESSION_ID": "gc-42"}, "conversation:gc-42", "agent"},
		{"invalid session id reads absent", map[string]string{"GC_SESSION_ID": "gc 42"}, "operator", "operator"},
		{"empty session id reads absent", map[string]string{"GC_SESSION_ID": ""}, "operator", "operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, a := cliPrincipal(func(k string) (string, bool) { v, ok := tc.env[k]; return v, ok })
			if p != tc.wantPrincipal || a != tc.wantActor {
				t.Fatalf("cliPrincipal = (%q, %q), want (%q, %q)", p, a, tc.wantPrincipal, tc.wantActor)
			}
		})
	}
}

func TestCLIOpWakeConflictReason(t *testing.T) {
	for in, want := range map[string]string{"closed": "closed", "Archived": "archived", "drain-acked": "drain_acked"} {
		if got := cliOpWakeConflictReason(in); got != want {
			t.Errorf("cliOpWakeConflictReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionVerbSentinelsMapLegacyErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		kind   opjournal.Kind
		err    error
		arm    opjournal.Arm
		reason string
	}{
		{"success", opjournal.KindSuspend, nil, opjournal.ArmSucceeded, ""},
		{"kill of an inactive session", opjournal.KindKill, session.ErrSessionInactive, opjournal.ArmSkipped, opjournal.ReasonNotNeeded},
		{"not a session", opjournal.KindWake, session.ErrNotSessionBead, opjournal.ArmFailed, "refused:not_a_session"},
		{"lease busy", opjournal.KindKill, session.ErrRuntimeLeaseBusy, opjournal.ArmFailed, "refused:session_starting"},
		{"starting", opjournal.KindRestart, session.ErrSessionStarting, opjournal.ArmFailed, "refused:session_starting"},
		{"stopping", opjournal.KindSuspend, session.ErrSessionStopping, opjournal.ArmFailed, "refused:session_stopping"},
		{"illegal transition", opjournal.KindClose, session.ErrIllegalTransition, opjournal.ArmFailed, "refused:illegal_transition"},
		{"unknown", opjournal.KindClose, errors.New("boom"), opjournal.ArmFailed, opjournal.ReasonOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := sessionVerbSentinels.SyncResult(tc.kind, tc.err, true)
			if res.Outcome.Arm != tc.arm || res.Outcome.Reason != tc.reason || res.LeaveOpen {
				t.Fatalf("SyncResult = %+v, want %s{%s}, closed", res, tc.arm, tc.reason)
			}
		})
	}
}

// cliOpWakeFixture is a suspended session a wake starts, over a recording
// store, with its deps.
type cliOpWakeFixture struct {
	rec  *beadstest.OpRecordingStore
	id   string
	deps sessionWakeDeps
}

func newCLIOpWakeFixture(t *testing.T) *cliOpWakeFixture {
	t.Helper()
	rec := beadstest.NewOpRecordingStore(&beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true})
	b, err := rec.Create(beads.Bead{
		Title:  "wake op session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-wake-op",
			"template":     "worker",
			"state":        "suspended",
			"held_until":   cliOpT0.Add(time.Hour).Format(time.RFC3339),
			"sleep_reason": "user-hold",
			"generation":   "3",
		},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return &cliOpWakeFixture{rec: rec, id: b.ID, deps: sessionWakeDeps{
		store:                     rec,
		cfg:                       &config.City{Agents: []config.Agent{{Name: "worker"}}},
		cityPath:                  "/city",
		cityResolved:              true,
		now:                       func() time.Time { return cliOpT0 },
		withdrawQueuedWaitNudges:  func(string, []string) error { return nil },
		cityUsesManagedReconciler: func(string) bool { return false },
		pokeController:            func(string, reconcilekey.Key) error { return nil },
	}}
}

// sessionWrites counts writes to the session bead.
func (f *cliOpWakeFixture) sessionWrites() int {
	n := 0
	for _, op := range f.rec.Ops() {
		if op.ID == f.id && op.Kind != beadstest.OpGet && op.Kind != beadstest.OpList {
			n++
		}
	}
	return n
}

func (f *cliOpWakeFixture) run(t *testing.T, key string, asJSON bool) (int, string, string) {
	t.Helper()
	deps := f.deps
	deps.opKey = key
	var stdout, stderr bytes.Buffer
	code := doSessionWake(f.id, &stdout, &stderr, asJSON, deps)
	return code, stdout.String(), stderr.String()
}

func TestSessionWakeOpOffWritesNothing(t *testing.T) {
	h := newCLIOpHarness(t, cliOpsOffEnv())
	f := newCLIOpWakeFixture(t)
	code, stdout, stderr := f.run(t, "", true)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	if ops := cliOpRows(t, f.rec); len(ops) != 0 {
		t.Fatalf("op rows = %+v, want none with the gate off", ops)
	}
	if h.opens != 0 {
		t.Fatalf("op store opened %d times with the gate off, want 0", h.opens)
	}
	m := cliOpJSON(t, []byte(stdout))
	for _, k := range []string{"op_id", "key", "replayed"} {
		if _, ok := m[k]; ok {
			t.Fatalf("JSON %v carries %q with the gate off", m, k)
		}
	}
}

func TestSessionWakeOpRecordsBeforeTheWakeWrite(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	code, stdout, stderr := f.run(t, "", true)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, f.rec)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
	if op.Kind != opjournal.KindWake || op.Conversation != f.id || op.Producer.Principal != "operator" {
		t.Fatalf("op = %+v, want a wake of %s by the operator", op, f.id)
	}
	ops := f.rec.Ops()
	created := cliOpFirstIndex(ops, func(r beadstest.RecordedOp) bool { return r.Kind == beadstest.OpCreate && r.ID == op.ID })
	wrote := cliOpFirstIndex(ops, func(r beadstest.RecordedOp) bool {
		return r.ID == f.id && r.Kind != beadstest.OpGet && r.Kind != beadstest.OpList && r.Kind != beadstest.OpCreate
	})
	if created < 0 || wrote < 0 || created > wrote {
		t.Fatalf("op create at %d, first session write at %d: the op must be proposed first", created, wrote)
	}
	m := cliOpJSON(t, []byte(stdout))
	key, _ := m["key"].(string)
	if m["op_id"] != op.ID || !strings.HasPrefix(key, "k-") || key != op.ClientKey {
		t.Fatalf("JSON %v, want op_id %s and the minted key %s", m, op.ID, op.ClientKey)
	}
	if _, ok := m["replayed"]; ok {
		t.Fatalf("JSON %v carries replayed on a first run", m)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "wake"}, []byte(stdout))

	// A plain-text success prints no key.
	code, stdout, _ = f.run(t, "", false)
	if code != 0 || strings.Contains(stdout, "k-") || strings.Contains(stdout, "op") {
		t.Fatalf("plain wake = %d, stdout %q: want the unchanged success line", code, stdout)
	}
}

func TestSessionWakeOpKeyReplaysWithoutActing(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	code, first, stderr := f.run(t, "wake-once", false)
	if code != 0 {
		t.Fatalf("first wake = %d; stderr=%s", code, stderr)
	}
	writes := f.sessionWrites()
	code, second, stderr := f.run(t, "wake-once", false)
	if code != 0 || second != first {
		t.Fatalf("replay = %d, stdout %q, want 0 and %q", code, second, first)
	}
	op := cliOpOnly(t, f.rec)
	if !strings.Contains(stderr, "replayed op "+op.ID) {
		t.Fatalf("replay stderr = %q, want it to name op %s", stderr, op.ID)
	}
	if got := f.sessionWrites(); got != writes {
		t.Fatalf("session writes %d -> %d: a replay must not act again", writes, got)
	}
	code, out, _ := f.run(t, "wake-once", true)
	m := cliOpJSON(t, []byte(out))
	if code != 0 || m["replayed"] != true || m["op_id"] != op.ID || m["key"] != "wake-once" {
		t.Fatalf("JSON replay = %d %v, want replayed op %s under key wake-once", code, m, op.ID)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "wake"}, []byte(out))
}

func TestSessionWakeOpInProgressExitsOne(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	open := cliOpProposeOpen(t, f.rec, opjournal.KindWake, f.id, "busy-key", opjournal.Scope{}, cliOpT0.Add(-time.Minute), opjournal.Policy{})
	writes := f.sessionWrites()
	code, stdout, stderr := f.run(t, "busy-key", true)
	if code != 1 {
		t.Fatalf("wake = %d, want 1 for an op in progress", code)
	}
	if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeInProgress {
		t.Fatalf("failure code = %q, want %s", got, cliOpCodeInProgress)
	}
	if m := cliOpJSON(t, []byte(stdout)); m["op_id"] != open.ID || m["key"] != "busy-key" {
		t.Fatalf("failure envelope %v, want op_id %s and key busy-key", m, open.ID)
	}
	if !strings.Contains(stderr, open.ID) || !strings.Contains(stderr, "busy-key") || !strings.Contains(stderr, "rerun later or without --key") {
		t.Fatalf("stderr = %q, want the op id, the key and the remedy", stderr)
	}
	if got := f.sessionWrites(); got != writes {
		t.Fatalf("session writes %d -> %d: an op in progress must not act", writes, got)
	}
	if op := cliOpOnly(t, f.rec); op.Status == opjournal.StatusClosed {
		t.Fatalf("op %s closed; the CLI must leave another process's op in its bound alone", op.ID)
	}
}

func TestSessionWakeOpExpiredOpenOpClosesAndReplays(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	open := cliOpProposeOpen(t, f.rec, opjournal.KindWake, f.id, "stale-key", opjournal.Scope{}, cliOpT0, opjournal.Policy{})
	h.clk.Time = cliOpT0.Add(legacyop.ExecutionBound + time.Second)
	writes := f.sessionWrites()
	code, stdout, stderr := f.run(t, "stale-key", true)
	if code != 1 {
		t.Fatalf("wake = %d, want 1 for an op closed failed", code)
	}
	if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeAlreadyFailed {
		t.Fatalf("failure code = %q, want %s", got, cliOpCodeAlreadyFailed)
	}
	if !strings.Contains(stderr, "failed{deadline_in_flight}") || !strings.Contains(stderr, "rerun without --key") {
		t.Fatalf("stderr = %q, want the reason and the remedy", stderr)
	}
	op := cliOpOnly(t, f.rec)
	if op.ID != open.ID {
		t.Fatalf("op %s, want %s", op.ID, open.ID)
	}
	if op.Status != opjournal.StatusClosed || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
		t.Fatalf("op = %+v, want closed failed{deadline_in_flight}", op)
	}
	if got := f.sessionWrites(); got != writes {
		t.Fatalf("session writes %d -> %d: a replay must not act", writes, got)
	}
}

func TestSessionWakeOpRequireRefusesAnIncapableStore(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
	f := newCLIOpWakeFixture(t)
	f.deps.store = cliOpIncapableStore{f.rec}
	writes := f.sessionWrites()
	code, stdout, stderr := f.run(t, "", true)
	if code != 1 {
		t.Fatalf("wake = %d, want 1 when require cannot record", code)
	}
	if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeUnavailable {
		t.Fatalf("failure code = %q, want %s; stderr=%s", got, cliOpCodeUnavailable, stderr)
	}
	if got := f.sessionWrites(); got != writes {
		t.Fatalf("session writes %d -> %d: require must refuse before any effect", writes, got)
	}
	b, _ := f.rec.Get(f.id)
	if b.Metadata["state"] != "suspended" {
		t.Fatalf("state = %q, want suspended (no wake)", b.Metadata["state"])
	}
}

func TestSessionWakeOpAutoWarnsAndRunsOnAnIncapableStore(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	f.deps.store = cliOpIncapableStore{f.rec}
	code, stdout, stderr := f.run(t, "", true)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "warning: op journal open failed") || strings.Count(stderr, "warning: op journal") != 1 {
		t.Fatalf("stderr = %q, want one warning naming the open stage", stderr)
	}
	if _, ok := cliOpJSON(t, []byte(stdout))["op_id"]; ok {
		t.Fatalf("JSON %s carries op_id for an unrecorded run", stdout)
	}
}

func TestSessionWakeOpAgentPrincipal(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_SESSION_ID": "gc-agent-7"})
	f := newCLIOpWakeFixture(t)
	if code, _, stderr := f.run(t, "", false); code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, f.rec)
	if op.Producer.Principal != "conversation:gc-agent-7" || op.Producer.Actor != opjournal.ActorAgent {
		t.Fatalf("producer = %+v, want conversation:gc-agent-7 / agent", op.Producer)
	}
}

func TestSessionWakeOpGateFromConfig(t *testing.T) {
	newCLIOpHarness(t, nil)
	f := newCLIOpWakeFixture(t)
	f.deps.cfg.Ops.LifecycleMode = "auto"
	if code, _, stderr := f.run(t, "", false); code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	cliOpWantOutcome(t, cliOpOnly(t, f.rec), opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
}

// A wake the session's lifecycle refuses (a row on its way to closed) closes
// its op refused with the conflicting state. A closed row never gets that
// far: TestSessionWakeOpByIDOfAClosedRowWritesNoOp.
func TestSessionWakeOpConflictClosesRefused(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	if err := f.rec.SetMetadataBatch(f.id, map[string]string{"state": "closing"}); err != nil {
		t.Fatalf("marking the session closing: %v", err)
	}
	code, _, stderr := f.run(t, "", false)
	if code != 1 {
		t.Fatalf("wake of a closing session = %d, want 1; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, f.rec)
	cliOpWantOutcome(t, op, opjournal.ArmFailed, "refused:closing", "")
}

func TestSessionWakeOpWillNotStartClosesRefused(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	// A create that never completed and lost its lease: the wake is recorded
	// but cannot complete.
	if err := f.rec.SetMetadataBatch(f.id, map[string]string{
		"state":                     "creating",
		"pending_create_started_at": cliOpT0.Add(-time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding a stuck create: %v", err)
	}
	code, _, stderr := f.run(t, "", false)
	if code != 1 {
		t.Fatalf("wake = %d, want 1; stderr=%s", code, stderr)
	}
	cliOpWantOutcome(t, cliOpOnly(t, f.rec), opjournal.ArmFailed, "refused:will_not_start", "")
}

func TestSessionWakeOpInvalidKey(t *testing.T) {
	t.Run("require refuses", func(t *testing.T) {
		newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
		f := newCLIOpWakeFixture(t)
		writes := f.sessionWrites()
		code, stdout, _ := f.run(t, " padded", true)
		if code != 1 {
			t.Fatalf("wake = %d, want 1 for an invalid key in require", code)
		}
		if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeInvalidKey {
			t.Fatalf("failure code = %q, want %s", got, cliOpCodeInvalidKey)
		}
		if got := f.sessionWrites(); got != writes {
			t.Fatalf("session writes %d -> %d: require must refuse before any effect", writes, got)
		}
	})
	t.Run("auto warns and runs unrecorded", func(t *testing.T) {
		newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		f := newCLIOpWakeFixture(t)
		code, _, stderr := f.run(t, " padded", false)
		if code != 0 {
			t.Fatalf("wake = %d; stderr=%s", code, stderr)
		}
		if !strings.Contains(stderr, "warning: invalid --key") {
			t.Fatalf("stderr = %q, want an invalid-key warning", stderr)
		}
		if ops := cliOpRows(t, f.rec); len(ops) != 0 {
			t.Fatalf("op rows = %+v, want none for an invalid key", ops)
		}
	})
}

func TestCLIOpWakeConflictReasonIsAlwaysAValidToken(t *testing.T) {
	for _, state := range []string{"", strings.Repeat("Very-Long State ", 20)} {
		why := cliOpWakeConflictReason(state)
		if why == "" || len(why) > opjournal.MaxRefusedWhyLen {
			t.Errorf("cliOpWakeConflictReason(%q) = %q, want a non-empty reason that fits a token", state, why)
		}
	}
}

// Under the builtin default (no [ops] gate, no GC_OPS_* override) the gate is
// auto, but a store that cannot record ops is treated as off: the wake runs
// unrecorded and prints nothing, as every released bd lacks --if-revision.
func TestSessionWakeOpDefaultAutoIsQuietOnAnIncapableStore(t *testing.T) {
	newCLIOpHarness(t, nil)
	f := newCLIOpWakeFixture(t)
	f.deps.store = cliOpIncapableStore{f.rec}
	code, stdout, stderr := f.run(t, "", true)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want nothing under the builtin default over an incapable store", stderr)
	}
	if _, ok := cliOpJSON(t, []byte(stdout))["op_id"]; ok {
		t.Fatalf("JSON %s carries op_id for an unrecorded run", stdout)
	}
	b, _ := f.rec.Get(f.id)
	if b.Metadata["state"] == "suspended" {
		t.Fatal("the wake did not run")
	}
}

// A --key under the builtin default still warns over a store that cannot
// record ops: the run asked for a dedup the journal cannot give.
func TestSessionWakeOpDefaultAutoWarnsForAKeyOnAnIncapableStore(t *testing.T) {
	newCLIOpHarness(t, nil)
	f := newCLIOpWakeFixture(t)
	f.deps.store = cliOpIncapableStore{f.rec}
	code, _, stderr := f.run(t, "k-1", false)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "warning: op journal open failed (conditional_writes_unsupported)") {
		t.Fatalf("stderr = %q, want the open warning for a --key run", stderr)
	}
}

// The builtin default records on a store that can record ops.
func TestSessionWakeOpDefaultAutoRecordsOnACapableStore(t *testing.T) {
	newCLIOpHarness(t, nil)
	f := newCLIOpWakeFixture(t)
	if code, _, stderr := f.run(t, "", false); code != 0 || stderr != "" {
		t.Fatalf("wake = %d, stderr %q; want 0 and nothing on stderr", code, stderr)
	}
	op := cliOpOnly(t, f.rec)
	if op.Status != opjournal.StatusClosed {
		t.Fatalf("op = %+v, want closed", op)
	}
}

// cliOpBdLeafProxy stands in for the default topology's proxied sessions
// store: its conditional writes, and so the op journal's open, resolve to its
// bd write leaf.
type cliOpBdLeafProxy struct {
	beads.Store
	leaf beads.Store
}

func (p cliOpBdLeafProxy) ConditionalWritesResolveTarget() beads.Store { return p.leaf }

// The builtin default over a proxied sessions store follows the bd behind
// it, as the CHANGELOG's upgrading note says: a bd whose update, close,
// assign and delete all advertise --if-revision opens the op journal, so the
// default records through bd; a released bd, which lacks the flag, fails the
// open as a store that cannot record ops, which the default treats as off.
func TestCLIOpDefaultAutoOverAProxiedBdStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		help    string
		records bool
		probes  []string
	}{
		{"bd with --if-revision", "Flags:\n      --if-revision int   fence\n", true, []string{"update", "close", "assign", "delete"}},
		{"released bd", "Flags:\n      --json   JSON output\n", false, []string{"update"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldLookup := cliOpLookupEnv
			cliOpLookupEnv = func(string) (string, bool) { return "", false }
			t.Cleanup(func() { cliOpLookupEnv = oldLookup })
			var probes []string
			bd := beads.NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
				if len(args) == 2 && args[1] == "--help" {
					probes = append(probes, args[0])
					return []byte(tc.help), nil
				}
				return nil, fmt.Errorf("unexpected bd %v", args)
			}, "gc")
			var stderr bytes.Buffer
			op := newCLIOp(&config.City{}, cliOpBdLeafProxy{Store: beads.NewMemStore(), leaf: bd},
				cliOpRequest{verb: "gc session wake", kind: opjournal.KindWake, stderr: &stderr})
			if op.rec.Mode != rollout.Auto || !op.rec.QuietCannotRecord {
				t.Fatalf("recorder mode=%s quiet=%t, want the builtin auto default, quiet over a store that cannot record", op.rec.Mode, op.rec.QuietCannotRecord)
			}
			st, err := op.rec.Open()
			if tc.records {
				if err != nil || st.Prefix() != "gc" {
					t.Fatalf("Open() = %v, %v; want the op journal over the bd leaf's gc namespace", st, err)
				}
			} else if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || !legacyop.CannotRecord(legacyop.DegradeReason(err)) {
				t.Fatalf("Open() = %v, want a store that cannot record ops (conditional_writes_unsupported)", err)
			}
			if !slices.Equal(probes, tc.probes) {
				t.Fatalf("bd probes = %v, want %v", probes, tc.probes)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want nothing", stderr.String())
			}
		})
	}
}
