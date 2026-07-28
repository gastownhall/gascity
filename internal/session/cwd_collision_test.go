package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	events []events.Event
}

func (r *recordingRecorder) Record(e events.Event) {
	r.events = append(r.events, e)
}

// fixedLiveness returns a LivenessScanner stub that always reports the given
// scanned/cwds pair, so tests never depend on this test process's real /proc
// state (ga-ighomh.1 acceptance criterion 1/3 need deterministic inputs).
func fixedLiveness(scanned bool, cwds ...string) LivenessScanner {
	return func() pidutil.LiveState {
		return pidutil.LiveState{CWDs: cwds, Scanned: scanned}
	}
}

// fixedLivenessWithPIDs extends fixedLiveness with PID-attributed cwd data,
// for tests proving the PID cross-reference in candidateConfirmedLiveByPID
// (ga-9x4z1g.1 FR3) rather than just the directory-wide CWDs signal.
func fixedLivenessWithPIDs(scanned bool, pidCWDs map[int]string, cwds ...string) LivenessScanner {
	return func() pidutil.LiveState {
		return pidutil.LiveState{CWDs: cwds, PIDCWDs: pidCWDs, Scanned: scanned}
	}
}

// refusedCwdPayload decodes the SessionStartRefusedCwd payload of the first
// matching event, failing the test if none is found.
func refusedCwdPayload(t *testing.T, rec *recordingRecorder) events.SessionStartRefusedCwdPayload {
	t.Helper()
	for _, e := range rec.events {
		if e.Type != events.SessionStartRefusedCwd {
			continue
		}
		var payload events.SessionStartRefusedCwdPayload
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("unmarshal %s payload: %v", events.SessionStartRefusedCwd, err)
		}
		return payload
	}
	t.Fatalf("no %s event recorded; events = %+v", events.SessionStartRefusedCwd, rec.events)
	return events.SessionStartRefusedCwdPayload{}
}

// TestCreateSessionAllowsDistinctWorkDirs covers acceptance criterion 6's
// "distinct directories allowed" case: two live sessions in unrelated
// directories must never trip the collision guard.
func TestCreateSessionAllowsDistinctWorkDirs(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true)))

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	first, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "s1", Command: "claude", WorkDir: dir1, Provider: "claude"})
	if err != nil {
		t.Fatalf("first CreateSession: %v", err)
	}
	second, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "s2", Command: "claude", WorkDir: dir2, Provider: "claude"})
	if err != nil {
		t.Fatalf("second CreateSession with distinct WorkDir: %v", err)
	}
	if !sp.IsRunning(first.SessionName) || !sp.IsRunning(second.SessionName) {
		t.Fatalf("expected both sessions running: %q=%v %q=%v", first.SessionName, sp.IsRunning(first.SessionName), second.SessionName, sp.IsRunning(second.SessionName))
	}
}

// TestCreateSessionRefusesLiveWorkDirCollision covers acceptance criteria
// 1-2: a second live session in the same directory as a running incumbent is
// refused, the incumbent is left untouched, the error identifies the
// collision, and no unrelated process's cwd leaks into the error or event.
func TestCreateSessionRefusesLiveWorkDirCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	unrelated := t.TempDir() // a live cwd with no known session recorded there
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true, unrelated)), WithEventRecorder(rec))

	incumbent, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "incumbent", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create incumbent: %v", err)
	}
	if !sp.IsRunning(incumbent.SessionName) {
		t.Fatalf("incumbent session %q not running after create", incumbent.SessionName)
	}

	_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
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
	again, err := mgr.Get(incumbent.ID)
	if err != nil {
		t.Fatalf("Get incumbent after refusal: %v", err)
	}
	if again.WorkDir != incumbent.WorkDir {
		t.Fatalf("incumbent WorkDir changed after refused challenger: got %q want %q", again.WorkDir, incumbent.WorkDir)
	}

	payload := refusedCwdPayload(t, rec)
	if payload.Reason != events.SessionStartRefusedReasonCollision {
		t.Fatalf("payload.Reason = %q, want %q", payload.Reason, events.SessionStartRefusedReasonCollision)
	}
	if payload.CollidingSessionID != incumbent.ID {
		t.Fatalf("payload.CollidingSessionID = %q, want %q", payload.CollidingSessionID, incumbent.ID)
	}
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), unrelated) {
		t.Fatalf("event payload %s leaks an unrelated live cwd %q", raw, unrelated)
	}
}

// TestCreateSessionRefusesWhenScannerReportsKnownSessionDirLive proves the
// guard also honors the /proc-derived liveness signal for a known session's
// recorded directory, not just the runtime provider's own IsRunning
// bookkeeping — the bead-only session below is never started via the fake
// provider, so only the injected scanner can be the source of the refusal.
func TestCreateSessionRefusesWhenScannerReportsKnownSessionDirLive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true, dir)))

	known, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "known", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only known session: %v", err)
	}
	if sp.IsRunning(known.SessionName) {
		t.Fatal("bead-only session must not be running yet")
	}

	_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err == nil {
		t.Fatal("CreateSession at a dir the scanner reports live for a known session succeeded, want refusal")
	}
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirCollision", err)
	}
}

// TestCandidateConfirmedLiveByPID exercises the PID-attribution helper in
// isolation from the store/scanner integration, pinning edge cases a
// CreateSession-level test would only hit nondeterministically (multiple
// candidates iterate in the store's unordered map order): a PID the scan
// found at the target directory, a zero PID (Fake's shape for its own
// tracked, never-orphaned sessions), and a PID that is live but at a
// different directory (ga-9x4z1g.1 FR3).
func TestCandidateConfirmedLiveByPID(t *testing.T) {
	dir := t.TempDir()
	normalizedDir := pathutil.NormalizePathForCompare(dir)
	elsewhere := pathutil.NormalizePathForCompare(t.TempDir())

	tests := []struct {
		name string
		rt   runtime.LiveRuntime
		want bool
	}{
		{name: "pid live at the target work dir attributes", rt: runtime.LiveRuntime{SessionID: "candidate", PID: 4242}, want: true},
		{name: "zero pid never attributes", rt: runtime.LiveRuntime{SessionID: "candidate", PID: 0}, want: false},
		{name: "pid live at an unrelated dir does not attribute", rt: runtime.LiveRuntime{SessionID: "candidate", PID: 4343}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := beads.NewMemStore()
			sp := runtime.NewFake()
			live := pidutil.LiveState{Scanned: true, PIDCWDs: map[int]string{4242: normalizedDir, 4343: elsewhere}}
			if tt.rt.PID != 0 {
				sp.OrphanedRuntimes = map[string]runtime.LiveRuntime{"candidate": tt.rt}
			}
			mgr := NewManagerWithOptions(store, sp)
			other := beads.Bead{ID: "candidate"}
			if got := mgr.candidateConfirmedLiveByPID(other, live, normalizedDir); got != tt.want {
				t.Fatalf("candidateConfirmedLiveByPID(%+v) = %v, want %v", tt.rt, got, tt.want)
			}
		})
	}
}

// TestCandidateConfirmedLiveByPID_NoRuntimeFound proves a candidate with no
// runtime reported at all (never started, or started by a since-terminated
// process) is never attributed — Fake's FindRuntimesBySessionID returns an
// empty slice, not an error, for an unknown session ID.
func TestCandidateConfirmedLiveByPID_NoRuntimeFound(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	normalizedDir := pathutil.NormalizePathForCompare(dir)
	live := pidutil.LiveState{Scanned: true, PIDCWDs: map[int]string{4242: normalizedDir}}
	mgr := NewManagerWithOptions(store, sp)
	other := beads.Bead{ID: "never-started"}
	if mgr.candidateConfirmedLiveByPID(other, live, normalizedDir) {
		t.Fatal("candidateConfirmedLiveByPID with no matching runtime = true, want false")
	}
}

// TestCreateSessionRefusesWithoutFalseAttributionWhenUnattributable proves
// the FR3 "honesty" property: when the scanner confirms some live process
// occupies a directory but the sole candidate session bead cannot be
// positively attributed to it (not IsRunning, no matching live PID), the
// guard still refuses fail-closed but must not name that candidate as the
// occupant — the prior implementation blamed whichever bead was the only (or
// first-iterated) candidate on the scanner signal alone, without evidence it
// was actually the live one (ga-9x4z1g.1 FR3).
func TestCreateSessionRefusesWithoutFalseAttributionWhenUnattributable(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true, dir)), WithEventRecorder(rec))

	known, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "known", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only known session: %v", err)
	}

	_, err = mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err == nil {
		t.Fatal("CreateSession at a dir the scanner reports live succeeded, want refusal")
	}
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirCollision", err)
	}

	payload := refusedCwdPayload(t, rec)
	if payload.CollidingSessionID != "" {
		t.Fatalf("payload.CollidingSessionID = %q, want empty: %s has no evidence (not IsRunning, no matching live PID) of being the live occupant", payload.CollidingSessionID, known.ID)
	}
}

// TestCreateSessionRefusesAttributingCorrectCandidateAmongMultiple proves
// the FR3 precision property with two known candidate beads sharing a
// directory: only the one whose PID the scan actually found at that
// directory may be named as the collision. The prior implementation blamed
// the first candidate iterated from the store regardless of which one (if
// either) the scan actually confirmed, so a wrong bead could be blamed while
// the true live occupant went unmentioned (ga-9x4z1g.1 FR3).
func TestCreateSessionRefusesAttributingCorrectCandidateAmongMultiple(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	dir := t.TempDir()
	normalizedDir := pathutil.NormalizePathForCompare(dir)
	const rightPID = 4242
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLivenessWithPIDs(true, map[int]string{rightPID: normalizedDir}, dir)), WithEventRecorder(rec))

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
	if err == nil {
		t.Fatal("CreateSession at a dir with a PID-confirmed live candidate succeeded, want refusal")
	}
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if !strings.Contains(err.Error(), right.ID) {
		t.Fatalf("collision error %q does not name the PID-attributed candidate %q", err.Error(), right.ID)
	}
	if strings.Contains(err.Error(), wrong.ID) {
		t.Fatalf("collision error %q names the unattributed candidate %q instead of the PID-confirmed one %q", err.Error(), wrong.ID, right.ID)
	}

	payload := refusedCwdPayload(t, rec)
	if payload.CollidingSessionID != right.ID {
		t.Fatalf("payload.CollidingSessionID = %q, want %q (the PID-confirmed candidate, not an arbitrary one)", payload.CollidingSessionID, right.ID)
	}
}

// TestCreateSessionAllowsWorkDirWhenOtherSessionNotLive covers the
// "stale/non-live PIDs handled" case from acceptance criterion 6: a known
// session bead recorded at a directory that neither the runtime provider nor
// the liveness scan reports as live must not block reuse of that directory.
func TestCreateSessionAllowsWorkDirWhenOtherSessionNotLive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true))) // scanned, nothing live

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

// TestCreateSessionAllowsReuseAfterIncumbentCloses proves a directory
// becomes reusable once its incumbent session is explicitly closed,
// deliberately diverging from ensureSessionNameAvailable's name-reservation
// semantics (TestCreateSessionNamedWithTransport_ClosedSessionStillReservesName):
// name identity persists after Close, but working-directory liveness does not.
func TestCreateSessionAllowsReuseAfterIncumbentCloses(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true)))

	incumbent, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "first", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create incumbent: %v", err)
	}
	if err := mgr.Close(incumbent.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "second", Command: "claude", WorkDir: dir, Provider: "claude"}); err != nil {
		t.Fatalf("CreateSession reusing a closed incumbent's WorkDir: %v", err)
	}
}

// TestStartRuntimeOnlyRefusesLiveWorkDirCollision proves the respawn bridge
// (used by legacy reconciler callers) enforces the same guard as
// CreateSession, satisfying acceptance criterion 6's "start and respawn
// paths" coverage.
func TestStartRuntimeOnlyRefusesLiveWorkDirCollision(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true)))

	incumbent, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "incumbent", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create incumbent: %v", err)
	}
	challenger, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "challenger", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only challenger: %v", err)
	}

	err = mgr.StartRuntimeOnly(context.Background(), challenger.ID, "", runtime.Config{})
	if err == nil {
		t.Fatal("StartRuntimeOnly on a colliding WorkDir succeeded, want refusal")
	}
	if !errors.Is(err, ErrWorkDirCollision) {
		t.Fatalf("StartRuntimeOnly error = %v, want wrapping ErrWorkDirCollision", err)
	}
	if sp.IsRunning(challenger.SessionName) {
		t.Fatal("challenger runtime should not have started")
	}
	if !sp.IsRunning(incumbent.SessionName) {
		t.Fatal("incumbent must remain running after a refused respawn collision")
	}
}

// TestStartRuntimeOnlyExcludesSelfFromCollisionCheck covers acceptance
// criterion 1's "excludes the session being started" requirement: the
// scanner reports the target directory itself as live (as it plausibly
// would be, mid-respawn), but the only known session recorded there is the
// one being respawned, so the guard must not self-block.
func TestStartRuntimeOnlyExcludesSelfFromCollisionCheck(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	dir := t.TempDir()
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(true, dir)))

	info, err := mgr.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", Title: "respawn-me", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err != nil {
		t.Fatalf("create bead-only session: %v", err)
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatal("bead-only session must not be running yet")
	}

	if err := mgr.StartRuntimeOnly(context.Background(), info.ID, "", runtime.Config{}); err != nil {
		t.Fatalf("StartRuntimeOnly on self at a dir the scanner reports live: %v", err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("expected runtime to be started after StartRuntimeOnly")
	}
}

// TestCreateSessionRefusesWhenLivenessScanUnavailable covers acceptance
// criterion 3: when the known-session process/cwd state cannot be
// enumerated, start is refused even for an otherwise uncontested directory —
// never assumed safe.
func TestCreateSessionRefusesWhenLivenessScanUnavailable(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	rec := &recordingRecorder{}
	mgr := NewManagerWithOptions(store, sp, WithLivenessScanner(fixedLiveness(false)), WithEventRecorder(rec))

	dir := t.TempDir() // uncontested: no other session has ever used it
	_, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "solo", Command: "claude", WorkDir: dir, Provider: "claude"})
	if err == nil {
		t.Fatal("CreateSession succeeded despite an unavailable liveness scan, want fail-closed refusal")
	}
	if !errors.Is(err, ErrWorkDirLivenessUnavailable) {
		t.Fatalf("error = %v, want wrapping ErrWorkDirLivenessUnavailable", err)
	}

	payload := refusedCwdPayload(t, rec)
	if payload.Reason != events.SessionStartRefusedReasonLivenessUnavailable {
		t.Fatalf("payload.Reason = %q, want %q", payload.Reason, events.SessionStartRefusedReasonLivenessUnavailable)
	}
}

// TestRuntimeStartCallSitesCheckCwdCollisionFirst mirrors
// TestRuntimeStartCallSitesCleanOrphansFirst: every m.sp.Start(ctx,
// sessName, cfg) call site in manager.go/chat.go must be preceded by a
// checkNoCWDCollision call using that file's id expression, so no start or
// respawn path can bypass the guard (acceptance criterion 1).
func TestRuntimeStartCallSitesCheckCwdCollisionFirst(t *testing.T) {
	tests := []struct {
		file   string
		idExpr string
	}{
		{file: "manager.go", idExpr: "b.ID"},
		{file: "chat.go", idExpr: "id"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(".", tt.file))
			if err != nil {
				t.Fatalf("read %s: %v", tt.file, err)
			}
			lines := strings.Split(string(data), "\n")
			starts := 0
			for i, line := range lines {
				if !strings.Contains(line, "m.sp.Start(ctx, sessName, cfg)") {
					continue
				}
				starts++
				if !cwdCollisionCheckPrecedes(lines, i, tt.idExpr) {
					t.Errorf("%s:%d Start is not preceded by a cwd collision check using %s", tt.file, i+1, tt.idExpr)
				}
			}
			if starts == 0 {
				t.Fatalf("%s contains no m.sp.Start(ctx, sessName, cfg) call sites", tt.file)
			}
		})
	}
}

// cwdCollisionCheckPrecedes reports whether m.checkNoCWDCollision(ctx,
// idExpr, ...) appears within the short window of non-blank lines preceding
// the Start at index before, mirroring orphanCleanupPrecedes's tolerance for
// an error-gate wrapper between the check and the Start it guards.
func cwdCollisionCheckPrecedes(lines []string, before int, idExpr string) bool {
	needle := "m.checkNoCWDCollision(ctx, " + idExpr + ", "
	const window = 12
	seen := 0
	for i := before - 1; i >= 0 && seen < window; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		seen++
		if strings.Contains(lines[i], needle) {
			return true
		}
	}
	return false
}
