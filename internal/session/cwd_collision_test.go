package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingRecorder is a minimal in-memory events.Recorder fake for
// asserting on emitted events without touching a real event log.
type recordingRecorder struct {
	mu     sync.Mutex
	events []events.Event
}

func (r *recordingRecorder) Record(e events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// refusals returns the recorded session.start_refused_cwd events.
func (r *recordingRecorder) refusals() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []events.Event
	for _, e := range r.events {
		if e.Type == events.SessionStartRefusedCwd {
			out = append(out, e)
		}
	}
	return out
}

// fixedLiveness returns a LivenessScanner stub that always reports the given
// scanned/cwds pair, so tests never depend on this test process's real /proc
// state.
func fixedLiveness(scanned bool, cwds ...string) LivenessScanner {
	return func() pidutil.LiveState {
		return pidutil.LiveState{CWDs: cwds, Scanned: scanned}
	}
}

// fixedLivenessWithPIDs is a completed scan carrying PID-attributed cwd data,
// for the PID cross-reference rather than the directory-wide CWDs signal.
func fixedLivenessWithPIDs(pidCWDs map[int]string, cwds ...string) LivenessScanner {
	return func() pidutil.LiveState {
		return pidutil.LiveState{CWDs: cwds, PIDCWDs: pidCWDs, Scanned: true}
	}
}

// refusedCwdPayload decodes the payload of the only recorded refusal event,
// failing the test unless exactly one was recorded, and returns it with the
// event's subject.
func refusedCwdPayload(t *testing.T, rec *recordingRecorder) (events.SessionStartRefusedCwdPayload, string) {
	t.Helper()
	got := rec.refusals()
	if len(got) != 1 {
		t.Fatalf("recorded %d %s events, want exactly 1: %+v", len(got), events.SessionStartRefusedCwd, got)
	}
	var payload events.SessionStartRefusedCwdPayload
	if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshal %s payload: %v", events.SessionStartRefusedCwd, err)
	}
	if got[0].Actor != "session" {
		t.Errorf("refusal event actor = %q, want session", got[0].Actor)
	}
	return payload, got[0].Subject
}

// openSessionIDs returns the IDs of the session beads that are not closed.
func openSessionIDs(t *testing.T, store beads.Store) []string {
	t.Helper()
	all, err := store.List(beads.ListQuery{Label: LabelSession})
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	var ids []string
	for _, b := range all {
		if b.Status != "closed" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// createIncumbent creates and starts a session in dir on mgr.
func createIncumbent(t *testing.T, mgr *Manager, dir string) Info {
	t.Helper()
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "incumbent", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create incumbent: %v", err)
	}
	return info
}

// suspendedBead seeds a resumable (suspended) session bead recording dir.
func suspendedBead(t *testing.T, store beads.Store, name, dir string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  name,
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": name,
			"state":        string(StateSuspended),
			"template":     "worker",
			"work_dir":     dir,
			"provider":     "claude",
		},
	})
	if err != nil {
		t.Fatalf("seeding suspended bead %s: %v", name, err)
	}
	return b
}

// findsRuntimesFor reports whether the fake was asked to scan for runtimes of
// session id: the orphan cleanup's lookup, which the guard (excluding the
// session being started from its own candidates) never makes for id.
func findsRuntimesFor(sp *runtime.Fake, id string) bool {
	for _, c := range sp.Calls {
		if c.Method == "FindRuntimesBySessionID" && c.Name == id {
			return true
		}
	}
	return false
}

// TestCreateSessionAllowsDistinctWorkDirs: two live sessions in unrelated
// directories never trip the collision guard.
func TestCreateSessionAllowsDistinctWorkDirs(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)))

	first := createIncumbent(t, mgr, t.TempDir())
	second, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "s2", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("second CreateSession with distinct WorkDir: %v", err)
	}
	if !sp.IsRunning(first.SessionName) || !sp.IsRunning(second.SessionName) {
		t.Fatalf("expected both sessions running: %q=%v %q=%v", first.SessionName, sp.IsRunning(first.SessionName), second.SessionName, sp.IsRunning(second.SessionName))
	}
}

// TestCreateSessionRefusesLiveWorkDirCollision: a second live session in the
// same directory as a running incumbent is refused, the incumbent is left
// untouched, the refused create leaves no live bead behind, the error names
// the incumbent, and no unrelated process's cwd leaks into the error or event.
func TestCreateSessionRefusesLiveWorkDirCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	unrelated := t.TempDir() // a live cwd with no known session recorded there
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true, unrelated)), WithEventRecorder(rec))

	incumbent := createIncumbent(t, mgr, dir)

	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err == nil {
		t.Fatal("second CreateSession with colliding live WorkDir succeeded, want refusal")
	}
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("second CreateSession error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if !strings.Contains(err.Error(), incumbent.ID) {
		t.Fatalf("collision error %q does not identify the incumbent session %q", err.Error(), incumbent.ID)
	}
	if strings.Contains(err.Error(), unrelated) {
		t.Fatalf("collision error %q leaks an unrelated live cwd %q", err.Error(), unrelated)
	}
	if !sp.IsRunning(incumbent.SessionName) {
		t.Fatal("incumbent session was stopped by a refused challenger")
	}
	if ids := openSessionIDs(t, store); len(ids) != 1 || ids[0] != incumbent.ID {
		t.Fatalf("open session beads after a refused create = %v, want only the incumbent %s", ids, incumbent.ID)
	}

	payload, subject := refusedCwdPayload(t, rec)
	if payload.Reason != events.SessionStartRefusedReasonCollision {
		t.Fatalf("payload.Reason = %q, want %q", payload.Reason, events.SessionStartRefusedReasonCollision)
	}
	if payload.CollidingSessionID != incumbent.ID {
		t.Fatalf("payload.CollidingSessionID = %q, want %q", payload.CollidingSessionID, incumbent.ID)
	}
	if subject == "" || subject == incumbent.ID {
		t.Fatalf("event subject = %q, want the refused session's own id (not the incumbent %s)", subject, incumbent.ID)
	}
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), unrelated) {
		t.Fatalf("event payload %s leaks an unrelated live cwd %q", raw, unrelated)
	}
}

// TestCreateSessionRefusesWhenSessionOwnedPIDLiveAtKnownDir: the guard still
// refuses through its unattributed fallback when a session-owned process (one
// the process table attributes to some session ID) is live at the directory
// of a known, never-started bead: no single candidate can be named, yet the
// collision is real.
func TestCreateSessionRefusesWhenSessionOwnedPIDLiveAtKnownDir(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	const elsewherePID = 5151
	sp.OrphanedRuntimes = map[string]runtime.LiveRuntime{
		"elsewhere-session": {SessionID: "elsewhere-session", PID: elsewherePID},
	}
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLivenessWithPIDs(map[int]string{elsewherePID: pathutil.NormalizePathForCompare(dir)}, dir)), WithEventRecorder(rec))

	if _, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "known", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
		t.Fatalf("create bead-only known session: %v", err)
	}

	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("CreateSession at a dir with a session-owned live PID: error = %v, want wrapping ErrWorkDirCollision", err)
	}
	payload, _ := refusedCwdPayload(t, rec)
	if payload.CollidingSessionID != "" {
		t.Fatalf("payload.CollidingSessionID = %q, want empty: the live PID belongs to another session, not a candidate on record", payload.CollidingSessionID)
	}
}

// TestCreateSessionAllowsWorkDirWhenLiveProcessIsUnattributedSquatter: a live
// process with no relationship to any Gas City session (the supervisor, the
// controller, an operator's shell) merely having its cwd at a directory, plus
// a never-started session bead recording that directory, is not enough to
// refuse a start. The scan is injected: the process appears in both the
// directory-wide and the PID-attributed views, but no runtime owns its PID.
func TestCreateSessionAllowsWorkDirWhenLiveProcessIsUnattributedSquatter(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	const squatterPID = 777001
	live := fixedLivenessWithPIDs(map[int]string{squatterPID: pathutil.NormalizePathForCompare(dir)}, dir)
	mgr := newTestManager(t, store, sp, WithLivenessScanner(live))

	known, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "known", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("creating bead-only known session: %v", err)
	}
	if sp.IsRunning(known.SessionName) {
		t.Fatal("precondition violated: bead-only session must not be running")
	}

	if _, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
		t.Fatalf("CreateSession refused at %s: pid %d is not a session-owned process and %s was never started, so no live session shares this directory: %v", dir, squatterPID, known.ID, err)
	}
}

// TestCreateSessionRefusesAttributingCorrectCandidateAmongMultiple: with two
// known beads sharing a directory, only the one whose PID the scan found
// there may be named as the collision.
func TestCreateSessionRefusesAttributingCorrectCandidateAmongMultiple(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	const rightPID = 4242
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLivenessWithPIDs(map[int]string{rightPID: pathutil.NormalizePathForCompare(dir)}, dir)), WithEventRecorder(rec))

	wrong, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "wrong", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only wrong candidate: %v", err)
	}
	right, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "right", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only right candidate: %v", err)
	}
	sp.OrphanedRuntimes = map[string]runtime.LiveRuntime{
		right.ID: {SessionID: right.ID, PID: rightPID},
	}

	_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if !strings.Contains(err.Error(), right.ID) {
		t.Fatalf("collision error %q does not name the PID-attributed candidate %q", err.Error(), right.ID)
	}
	if strings.Contains(err.Error(), wrong.ID) {
		t.Fatalf("collision error %q names the unattributed candidate %q", err.Error(), wrong.ID)
	}
	if payload, _ := refusedCwdPayload(t, rec); payload.CollidingSessionID != right.ID {
		t.Fatalf("payload.CollidingSessionID = %q, want %q", payload.CollidingSessionID, right.ID)
	}
}

// TestCreateSessionIgnoresZeroAndElsewherePIDs: a candidate whose runtime
// reports no PID, or a PID live at a different directory, is not attributed.
func TestCreateSessionIgnoresZeroAndElsewherePIDs(t *testing.T) {
	dir := t.TempDir()
	elsewhere := pathutil.NormalizePathForCompare(t.TempDir())
	for name, pid := range map[string]int{"zero pid": 0, "pid live elsewhere": 4343} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLivenessWithPIDs(map[int]string{4343: elsewhere}, dir)))
			known, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "candidate", Command: "claude", WorkDir: dir, Provider: "claude"})
			if err != nil {
				t.Fatalf("create bead-only candidate: %v", err)
			}
			sp.OrphanedRuntimes = map[string]runtime.LiveRuntime{known.ID: {SessionID: known.ID, PID: pid}}
			if _, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
				t.Fatalf("CreateSession refused although the candidate's runtime is not live at %s: %v", dir, err)
			}
		})
	}
}

// TestCreateSessionAllowsWorkDirWhenOtherSessionNotLive: a known session bead
// recorded at a directory that neither the runtime provider nor the scan
// reports live does not block reuse of that directory.
func TestCreateSessionAllowsWorkDirWhenOtherSessionNotLive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)))

	stale, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "stale", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only stale session: %v", err)
	}
	if sp.IsRunning(stale.SessionName) {
		t.Fatal("bead-only session must not be running")
	}
	if _, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "newcomer", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
		t.Fatalf("CreateSession at a dir with only a non-live known session: %v", err)
	}
}

// TestCreateSessionAllowsReuseAfterIncumbentCloses: a directory becomes
// reusable once its incumbent is closed. Name identity persists after Close;
// working-directory liveness does not.
func TestCreateSessionAllowsReuseAfterIncumbentCloses(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)))

	incumbent := createIncumbent(t, mgr, dir)
	if err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), incumbent.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "second", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
		t.Fatalf("CreateSession reusing a closed incumbent's WorkDir: %v", err)
	}
}

// TestCreateSessionRefusesSpellingVariantsOfOccupiedWorkDir: the candidate
// match and the liveness match normalize both sides the same way, so a
// trailing slash, a "." segment or a symlinked spelling of the occupied
// directory cannot evade the guard, whichever side carries the variant.
func TestCreateSessionRefusesSpellingVariantsOfOccupiedWorkDir(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tests := []struct {
		name       string
		incumbent  string
		challenger string
	}{
		{name: "trailing slash on the challenger", incumbent: target, challenger: target + string(filepath.Separator)},
		{name: "dot segment on the challenger", incumbent: target, challenger: filepath.Join(target, ".") + string(filepath.Separator) + "."},
		{name: "symlink on the challenger", incumbent: target, challenger: link},
		{name: "symlink on the incumbent", incumbent: link, challenger: target},
		{name: "trailing slash on the incumbent", incumbent: target + string(filepath.Separator), challenger: target},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)))
			incumbent := createIncumbent(t, mgr, tt.incumbent)
			_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: tt.challenger, Provider: "claude"})
			if !errors.Is(err, ErrWorkDirCollision) {
				t.Fatalf("CreateSession in %q while %q is occupied: error = %v, want wrapping ErrWorkDirCollision", tt.challenger, tt.incumbent, err)
			}
			if !sp.IsRunning(incumbent.SessionName) {
				t.Fatal("incumbent was stopped by a refused challenger")
			}
		})
	}
}

// TestCreateSessionRefusesWhenLivenessScanUnavailable: when the host's process
// state cannot be enumerated, a start is refused even for an uncontested
// directory, never assumed safe.
func TestCreateSessionRefusesWhenLivenessScanUnavailable(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(false)), WithEventRecorder(rec))

	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "solo", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if !errors.Is(err, ErrWorkDirLivenessUnavailable) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirLivenessUnavailable", err)
	}
	if payload, _ := refusedCwdPayload(t, rec); payload.Reason != events.SessionStartRefusedReasonLivenessUnavailable {
		t.Fatalf("payload.Reason = %q, want %q", payload.Reason, events.SessionStartRefusedReasonLivenessUnavailable)
	}
	if ids := openSessionIDs(t, store); len(ids) != 0 {
		t.Fatalf("a refused create left open session beads %v", ids)
	}
}

// sessionListPartialStore returns a beads.PartialResultError, with no rows,
// from a session-labeled List once armed. It is armed only after the test has
// seeded its beads, so the failure lands on the guard's own read.
type sessionListPartialStore struct {
	beads.Store
	armed *bool
}

func (s sessionListPartialStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if *s.armed && q.Label == LabelSession {
		return nil, &beads.PartialResultError{Op: "bd list", Err: errors.New("skipped 1 corrupt row")}
	}
	return s.Store.List(q)
}

// TestAttachRefusesWhenSessionBeadListIsPartial: a partial session-bead read
// must not be scanned as "no other candidates": the dropped rows may include
// the live occupant's own bead. The guard fails closed like an unavailable
// scan, and the event says so.
func TestAttachRefusesWhenSessionBeadListIsPartial(t *testing.T) {
	armed := false
	store := sessionListPartialStore{Store: beads.NewMemStore(), armed: &armed}
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)), WithEventRecorder(rec))
	challenger := suspendedBead(t, store, "test-city--challenger", dir)

	armed = true
	err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), challenger.ID, "claude --resume", runtime.Config{})
	if !errors.Is(err, ErrWorkDirLivenessUnavailable) {
		t.Fatalf("Attach with a partial session list: error = %v, want wrapping ErrWorkDirLivenessUnavailable", err)
	}
	if payload, _ := refusedCwdPayload(t, rec); payload.Reason != events.SessionStartRefusedReasonLivenessUnavailable {
		t.Fatalf("payload.Reason = %q, want %q", payload.Reason, events.SessionStartRefusedReasonLivenessUnavailable)
	}
	if sp.IsRunning(challenger.Metadata["session_name"]) {
		t.Fatal("the session started despite a partial session-bead read")
	}
}

// TestAttachRefusesLiveWorkDirCollisionBeforeAnyStartStep: a resume into a
// directory a live session occupies is refused before the orphan cleanup, the
// incarnation mint or the runtime start, leaving the suspended row as it was.
func TestAttachRefusesLiveWorkDirCollisionBeforeAnyStartStep(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)), WithEventRecorder(rec))

	incumbent := createIncumbent(t, mgr, dir)
	challenger := suspendedBead(t, store, "test-city--challenger", dir)
	before, err := store.Get(challenger.ID)
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.Attach(context.Background(), testActor(mgr, ActorOperator), challenger.ID, "claude --resume", runtime.Config{})
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("Attach into an occupied dir: error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if !strings.Contains(err.Error(), incumbent.ID) {
		t.Fatalf("collision error %q does not name the incumbent %q", err.Error(), incumbent.ID)
	}
	if sp.IsRunning("test-city--challenger") {
		t.Fatal("the refused resume started a runtime")
	}
	if !sp.IsRunning(incumbent.SessionName) {
		t.Fatal("incumbent was stopped by a refused resume")
	}
	if findsRuntimesFor(sp, challenger.ID) {
		t.Error("the orphan cleanup ran for the refused session: the guard must come first")
	}
	after, err := store.Get(challenger.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"state", "session_key", "generation", "instance_token", "continuation_epoch", "work_dir"} {
		if after.Metadata[key] != before.Metadata[key] {
			t.Errorf("metadata[%s] = %q after a refused resume, want unchanged %q", key, after.Metadata[key], before.Metadata[key])
		}
	}
	if payload, subject := refusedCwdPayload(t, rec); payload.CollidingSessionID != incumbent.ID || subject != challenger.ID {
		t.Fatalf("refusal event = subject %q payload %+v, want subject %s colliding %s", subject, payload, challenger.ID, incumbent.ID)
	}
}

// TestStartRuntimeOnlyRefusesLiveWorkDirCollision: the reconciler's
// runtime-only start (legacy path) enforces the same guard as CreateSession,
// before the orphan cleanup.
func TestStartRuntimeOnlyRefusesLiveWorkDirCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)), WithEventRecorder(rec))

	incumbent := createIncumbent(t, mgr, dir)
	challenger, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only challenger: %v", err)
	}

	err = startRuntimeOnly(t, mgr, challenger.ID, BuildResumeCommand(challenger), runtime.Config{WorkDir: challenger.WorkDir})
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("StartRuntimeOnly error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if sp.IsRunning(challenger.SessionName) {
		t.Fatal("challenger runtime should not have started")
	}
	if !sp.IsRunning(incumbent.SessionName) {
		t.Fatal("incumbent must remain running after a refused respawn collision")
	}
	if findsRuntimesFor(sp, challenger.ID) {
		t.Error("the orphan cleanup ran for the refused session: the guard must come first")
	}
	if payload, subject := refusedCwdPayload(t, rec); payload.CollidingSessionID != incumbent.ID || subject != challenger.ID {
		t.Fatalf("refusal event = subject %q payload %+v, want subject %s colliding %s", subject, payload, challenger.ID, incumbent.ID)
	}
}

// TestStartRuntimeOnlyUsesLaunchDirNotRecordedDir: the guard checks the
// directory the runtime will actually launch in (the start's hints), which can
// differ from the bead's recorded work_dir.
func TestStartRuntimeOnlyUsesLaunchDirNotRecordedDir(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	occupied := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true)))

	incumbent := createIncumbent(t, mgr, occupied)
	challenger, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "challenger", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only challenger: %v", err)
	}

	err = startRuntimeOnly(t, mgr, challenger.ID, BuildResumeCommand(challenger), runtime.Config{WorkDir: occupied})
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("StartRuntimeOnly into the occupied launch dir: error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if !strings.Contains(err.Error(), incumbent.ID) {
		t.Fatalf("collision error %q does not name the incumbent %q", err.Error(), incumbent.ID)
	}
}

// TestStartRuntimeOnlyExcludesSelfFromCollisionCheck: the scanner reports the
// target directory live (as it plausibly is mid-respawn), but the only known
// session recorded there is the one being respawned, so the guard must not
// block it on itself.
func TestStartRuntimeOnlyExcludesSelfFromCollisionCheck(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := newTestManager(t, store, sp, WithLivenessScanner(fixedLiveness(true, dir)))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "respawn-me", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only session: %v", err)
	}
	if err := startRuntimeOnly(t, mgr, info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
		t.Fatalf("StartRuntimeOnly on self at a dir the scanner reports live: %v", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("expected runtime to be started after StartRuntimeOnly")
	}
}

// TestRuntimeStartCallSitesCheckCwdCollisionBeforeOrphanCleanup: every orphan
// cleanup that gates a runtime start in manager.go and chat.go is preceded, in
// the same block, by the working-directory guard, so no start path reaches the
// kill of a prior process (or the incarnation mint after it) for a directory
// another session occupies. The four sites are the create, the resume, the
// runtime-only respawn and the fresh retry after a stale resume key.
func TestRuntimeStartCallSitesCheckCwdCollisionBeforeOrphanCleanup(t *testing.T) {
	const wantSites = 4
	sites := 0
	for _, file := range []string{"manager.go", "chat.go"} {
		data, err := os.ReadFile(filepath.Join(".", file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if !strings.Contains(line, "m.killExistingOrphans(ctx, ") {
				continue
			}
			sites++
			if !cwdGuardPrecedes(lines, i) {
				t.Errorf("%s:%d orphan cleanup is not preceded by m.checkNoCWDCollision(ctx, ...)", file, i+1)
			}
		}
	}
	if sites != wantSites {
		t.Fatalf("found %d orphan-cleanup call sites, want %d: a new start path must be guarded and counted here", sites, wantSites)
	}
}

// cwdGuardPrecedes reports whether m.checkNoCWDCollision(ctx, appears within
// the short window of non-blank lines preceding the line at index before.
func cwdGuardPrecedes(lines []string, before int) bool {
	const window = 12
	seen := 0
	for i := before - 1; i >= 0 && seen < window; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if strings.Contains(lines[i], "m.checkNoCWDCollision(ctx, ") {
			return true
		}
	}
	return false
}
