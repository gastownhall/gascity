//go:build !windows

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeOwnerStarts are the two Manager verbs that resume a stopped session
// through the pre-start gate.
var runtimeOwnerStarts = []struct {
	name  string
	start func(t *testing.T, mgr *Manager, info Info) error
}{
	{"Start", func(_ *testing.T, mgr *Manager, info Info) error {
		return mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	}},
	{"StartRuntimeOnly", func(t *testing.T, mgr *Manager, info Info) error {
		return startRuntimeOnly(t, mgr, info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	}},
}

// seedRuntimeOwner writes a runtime_owner record as raw JSON, the way the
// row stores it.
func seedRuntimeOwner(t *testing.T, mgr *Manager, id string, uid int, boot string, roots ...map[string]any) {
	t.Helper()
	if roots == nil {
		roots = []map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"uid": uid, "boot": boot, "roots": roots})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.store.SetMetadata(id, "runtime_owner", string(raw)); err != nil {
		t.Fatalf("seeding runtime_owner: %v", err)
	}
}

// liveRoot is the test process itself standing in for a runtime root another
// uid launched: it is alive and its identity can be verified unprivileged.
func liveRoot(t *testing.T) map[string]any {
	t.Helper()
	start, err := pidutil.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("start time of the test process is unreadable here: %v", err)
	}
	return map[string]any{"pid": os.Getpid(), "start": start}
}

// AC-1: a start under euid B over a session whose runtime euid A launched in
// this boot, and whose recorded root is still alive at its recorded start
// time, refuses with ErrRuntimeUnavailable naming the session, uid A, the live
// pid and the remedy, and never reaches the provider's Start, even though B's
// process-table scan (which skips other uids' processes) finds no root.
func TestStartUnderAnotherUIDRefusesWhileRecordedRuntimeLives(t *testing.T) {
	for _, tc := range runtimeOwnerStarts {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp, info := seedSuspendedResumeTarget(t)
			otherUID := os.Geteuid() + 1
			seedRuntimeOwner(t, mgr, info.ID, otherUID, runtimeLeaseBootID(), liveRoot(t))

			err := tc.start(t, mgr, info)
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("%s error = %v, want it to wrap runtime.ErrRuntimeUnavailable", tc.name, err)
			}
			for _, want := range []string{info.ID, "uid " + strconv.Itoa(otherUID), strconv.Itoa(os.Getpid()), "--unset-metadata runtime_owner"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s error = %v, want it to contain %q", tc.name, err, want)
				}
			}
			if hasEventPrefix(sp.events, "start:") || hasEventPrefix(sp.events, "terminate:") {
				t.Fatalf("events = %v, want no start and no terminate", sp.events)
			}
		})
	}
}

// staleKeyScanProvider is an orphanScanProvider whose first failStarts
// Starts die during startup, so a resume takes the stale-key relaunch.
type staleKeyScanProvider struct {
	*orphanScanProvider
	failStarts int
}

func (p *staleKeyScanProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.failStarts > 0 {
		p.failStarts--
		p.events = append(p.events, "start-died:"+cfg.Env["GC_SESSION_ID"])
		return fmt.Errorf("pane exited: %w", runtime.ErrSessionDiedDuringStartup)
	}
	return p.orphanScanProvider.Start(ctx, name, cfg)
}

// setCurrentEUID points the euid seam at uid for the test.
func setCurrentEUID(t *testing.T, uid int) {
	t.Helper()
	prev := currentEUID
	currentEUID = func() int { return uid }
	t.Cleanup(func() { currentEUID = prev })
}

// classifyInfrastructure makes rootIsCityInfrastructure report the pids in
// infra as city infrastructure, and every other root as not.
func classifyInfrastructure(t *testing.T, infra ...int) {
	t.Helper()
	prev := rootIsCityInfrastructure
	rootIsCityInfrastructure = func(live runtime.LiveRuntime) (bool, error) { return slices.Contains(infra, live.PID), nil }
	t.Cleanup(func() { rootIsCityInfrastructure = prev })
}

// readRuntimeOwner decodes the row's runtime_owner record.
func readRuntimeOwner(t *testing.T, mgr *Manager, id string) (runtimeOwner, bool) {
	t.Helper()
	b, err := mgr.store.Get(id)
	if err != nil {
		t.Fatalf("reading row %s: %v", id, err)
	}
	raw := b.Metadata["runtime_owner"]
	if raw == "" {
		return runtimeOwner{}, false
	}
	owner, err := parseRuntimeOwner(raw)
	if err != nil {
		t.Fatalf("runtime_owner %q: %v", raw, err)
	}
	return owner, true
}

// ownerScanResults are what the scan reports right after a start: the
// session's own (tracked) runtime root, the same session's root in another
// city, another session's root, and a city-infrastructure root carrying the
// session's identity. Only the first is the session's runtime.
func ownerScanResults(city string) []runtime.LiveRuntime {
	return []runtime.LiveRuntime{
		{PID: os.Getpid(), PPID: os.Getppid(), IsTracked: true},
		{PID: os.Getpid() + 100001, IsTracked: true, City: city + "-elsewhere"},
		{PID: os.Getpid() + 100002, IsTracked: true, SessionID: "other-session"},
		{PID: os.Getpid() + 100003, IsTracked: true},
	}
}

// AC-2: every gated start path whose provider Start succeeds (CreateSession,
// Start, StartRuntimeOnly, and the stale-key relaunch) records the Manager's
// euid, the current boot id, and the pid and start time of each root the
// scan reports for that session and city that is not city infrastructure.
func TestStartRecordsRuntimeOwnerAfterStart(t *testing.T) {
	const euid = 4242
	setCurrentEUID(t, euid)
	classifyInfrastructure(t, os.Getpid()+100003)
	selfStart, err := pidutil.StartTime(os.Getpid())
	if err != nil {
		t.Skipf("start time of the test process is unreadable here: %v", err)
	}
	want := runtimeOwner{UID: euid, Boot: runtimeLeaseBootID(), Roots: []runtimeOwnerRoot{{PID: os.Getpid(), Start: selfStart}}}

	newTarget := func(t *testing.T) (*Manager, *staleKeyScanProvider) {
		store := beads.NewMemStore()
		sp := &staleKeyScanProvider{orphanScanProvider: &orphanScanProvider{Fake: runtime.NewFake(), city: t.TempDir()}}
		sp.results = ownerScanResults(sp.city)
		return newTestManager(t, store, sp, WithCityPath(sp.city), WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter)), sp
	}
	create := func(t *testing.T, mgr *Manager) Info {
		info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return info
	}
	// suspended creates a session, suspends it and clears its record, so the
	// resume under test is the only writer of the record it reads.
	suspended := func(t *testing.T, mgr *Manager, meta map[string]string) Info {
		info := create(t, mgr)
		for k, v := range meta {
			if err := mgr.store.SetMetadata(info.ID, k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
			t.Fatalf("Suspend: %v", err)
		}
		if err := mgr.store.SetMetadata(info.ID, "runtime_owner", ""); err != nil {
			t.Fatal(err)
		}
		return info
	}
	staleKey := map[string]string{"session_key": "k-stale", "resume_flag": "--resume"}

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, mgr *Manager) string
	}{
		{"CreateSession", func(t *testing.T, mgr *Manager) string { return create(t, mgr).ID }},
		{"Start", func(t *testing.T, mgr *Manager) string {
			info := suspended(t, mgr, nil)
			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			return info.ID
		}},
		{"StartRuntimeOnly", func(t *testing.T, mgr *Manager) string {
			info := suspended(t, mgr, nil)
			if err := startRuntimeOnly(t, mgr, info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}); err != nil {
				t.Fatalf("StartRuntimeOnly: %v", err)
			}
			return info.ID
		}},
		{"Start stale-key relaunch", func(t *testing.T, mgr *Manager) string {
			info := suspended(t, mgr, staleKey)
			mgr.sp.(*staleKeyScanProvider).failStarts = 1
			if err := mgr.Start(context.Background(), testActor(mgr, ActorOperator), info.ID, "claude --resume k-stale", runtime.Config{WorkDir: info.WorkDir}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			return info.ID
		}},
		{"StartRuntimeOnly stale-key relaunch", func(t *testing.T, mgr *Manager) string {
			info := suspended(t, mgr, staleKey)
			mgr.sp.(*staleKeyScanProvider).failStarts = 1
			if err := startRuntimeOnly(t, mgr, info.ID, "claude --resume k-stale", runtime.Config{WorkDir: info.WorkDir}); err != nil {
				t.Fatalf("StartRuntimeOnly: %v", err)
			}
			return info.ID
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp := newTarget(t)
			id := tc.run(t, mgr)
			if strings.Contains(tc.name, "stale-key") && !hasEventPrefix(sp.events, "start-died:") {
				t.Fatalf("events = %v, want the resume to die and relaunch", sp.events)
			}
			got, ok := readRuntimeOwner(t, mgr, id)
			if !ok {
				t.Fatalf("row %s has no runtime_owner after a successful start", id)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("runtime_owner = %+v, want %+v", got, want)
			}
		})
	}
}

// goneRootPID is above Linux's largest pid_max, so no process ever has it.
const goneRootPID = 1 << 30

// AC-3: a start whose runtime-owner record cannot be completed (the scan
// fails, finds no root, cannot read a root's start time, or the write fails)
// still succeeds, and a log line names the session and the failure.
func TestRuntimeOwnerRecordFailureDoesNotFailStart(t *testing.T) {
	classifyInfrastructure(t)
	tracked := func(pid int) []runtime.LiveRuntime { return []runtime.LiveRuntime{{PID: pid, IsTracked: true}} }
	for _, tc := range []struct {
		name    string
		results []runtime.LiveRuntime
		findErr error
		store   func(*beads.MemStore) beads.Store
		want    string
	}{
		{name: "scan error", findErr: errors.New("proc entry unreadable"), want: "scanning for its runtime roots: proc entry unreadable"},
		{name: "no root", want: "no runtime root found"},
		{name: "unreadable start time", results: tracked(goneRootPID), want: fmt.Sprintf("leaving root pid=%d out: reading its start time", goneRootPID)},
		{
			name: "failed write", results: tracked(os.Getpid()),
			store: func(s *beads.MemStore) beads.Store { return failMetadataKeyStore{MemStore: s, key: "runtime_owner"} },
			want:  "writing runtime_owner: set metadata failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLog(t)
			var store beads.Store = beads.NewMemStore()
			if tc.store != nil {
				store = tc.store(store.(*beads.MemStore))
			}
			sp := &orphanScanProvider{Fake: runtime.NewFake(), city: t.TempDir(), results: tc.results, findErr: tc.findErr}
			mgr := newTestManager(t, store, sp, WithCityPath(sp.city))

			info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
			if err != nil {
				t.Fatalf("CreateSession: %v, want the start to succeed", err)
			}
			if !hasEventPrefix(sp.events, "start:") {
				t.Fatalf("events = %v, want a start", sp.events)
			}
			var line string
			for _, l := range strings.Split(logs.String(), "\n") {
				if strings.Contains(l, "recording runtime owner of "+info.ID) && strings.Contains(l, tc.want) {
					line = l
				}
			}
			if line == "" {
				t.Fatalf("log = %q, want a line naming %s and %q", logs.String(), info.ID, tc.want)
			}
		})
	}
}

// AC-4: a record under another uid whose roots are all gone, or carry another
// start time, or that names another boot, lets the start proceed, and the start
// rewrites the record under its own euid.
func TestStartUnderAnotherUIDProceedsWhenRecordedRootsAreGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(t *testing.T) (boot string, root map[string]any)
	}{
		{"root gone", func(*testing.T) (string, map[string]any) {
			return runtimeLeaseBootID(), map[string]any{"pid": goneRootPID, "start": "1"}
		}},
		{"root pid reused", func(t *testing.T) (string, map[string]any) {
			root := liveRoot(t)
			root["start"] = root["start"].(string) + "0"
			return runtimeLeaseBootID(), root
		}},
		{"other boot", func(t *testing.T) (string, map[string]any) {
			if runtimeLeaseBootID() == "" {
				t.Skip("no boot id is readable here")
			}
			return "other-boot", liveRoot(t)
		}},
	} {
		for _, start := range runtimeOwnerStarts {
			t.Run(tc.name+"/"+start.name, func(t *testing.T) {
				mgr, sp, info := seedSuspendedResumeTarget(t)
				boot, root := tc.seed(t)
				seedRuntimeOwner(t, mgr, info.ID, os.Geteuid()+1, boot, root)

				if err := start.start(t, mgr, info); err != nil {
					t.Fatalf("%s: %v, want the start to proceed", start.name, err)
				}
				if !hasEventPrefix(sp.events, "start:") {
					t.Fatalf("events = %v, want a start", sp.events)
				}
				got, ok := readRuntimeOwner(t, mgr, info.ID)
				if !ok || got.UID != os.Geteuid() || got.Boot != runtimeLeaseBootID() {
					t.Fatalf("runtime_owner = %+v (present %v), want it rewritten under uid %d in boot %q", got, ok, os.Geteuid(), runtimeLeaseBootID())
				}
			})
		}
	}
}

// AC-4, end to end: one Manager records the fence under one euid, a second
// Manager over the same store, as a restart under another uid is, is refused
// while that record's root is alive and never reaches its provider's Start,
// and starts once the root is gone, rewriting the record under its own euid.
// The currentEUID seam stands in for the first Manager's uid; it is a
// package global, so this test never runs in parallel (#5870).
func TestRuntimeOwnerRecordedUnderOneUIDFencesAnother(t *testing.T) {
	firstUID := os.Geteuid() + 1
	store := beads.NewMemStore()
	city := t.TempDir()
	spA := &orphanScanProvider{Fake: runtime.NewFake(), city: city}
	spA.results = []runtime.LiveRuntime{{PID: os.Getpid(), IsTracked: true}}
	setCurrentEUID(t, firstUID)
	mgrA := newTestManager(t, store, spA, WithCityPath(city))

	info, err := mgrA.CreateSession(context.Background(), CreateOptions{Template: "helper", Title: "", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Env: nil, Resume: ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession under uid %d: %v", firstUID, err)
	}
	owner, ok := readRuntimeOwner(t, mgrA, info.ID)
	if !ok || owner.UID != firstUID || len(owner.Roots) != 1 {
		t.Fatalf("runtime_owner after the first create = %+v (present %v), want uid %d with the one root it launched", owner, ok, firstUID)
	}
	if err := mgrA.Suspend(context.Background(), testActor(mgrA, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend under uid %d: %v", firstUID, err)
	}
	setCurrentEUID(t, os.Geteuid())

	// The second Manager's scan finds nothing: exactly what the Linux
	// process-table scan reports for a root another uid owns.
	spB := &orphanScanProvider{Fake: runtime.NewFake(), city: city}
	mgrB := newTestManager(t, store, spB, WithCityPath(city))
	start := func() error {
		return mgrB.Start(context.Background(), testActor(mgrB, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
	}
	if err := start(); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Start under the second uid: %v, want a refusal wrapping runtime.ErrRuntimeUnavailable while the recorded root is alive", err)
	}
	if hasEventPrefix(spB.events, "start:") {
		t.Fatalf("the second uid started a runtime beside the recorded one; events = %v", spB.events)
	}

	// The recorded runtime exits: the fence opens, and the second uid's start
	// proceeds and rewrites the record under its own euid.
	seedRuntimeOwner(t, mgrB, info.ID, firstUID, owner.Boot, map[string]any{"pid": goneRootPID, "start": owner.Roots[0].Start})
	spB.results = []runtime.LiveRuntime{{PID: os.Getpid(), IsTracked: true}}
	if err := start(); err != nil {
		t.Fatalf("Start under the second uid after the recorded root exited: %v", err)
	}
	got, ok := readRuntimeOwner(t, mgrB, info.ID)
	if !ok || got.UID != os.Geteuid() || got.Boot != runtimeLeaseBootID() || len(got.Roots) != 1 || got.Roots[0].PID != os.Getpid() {
		t.Fatalf("runtime_owner = %+v (present %v), want it rewritten under uid %d with the test process as its one root", got, ok, os.Geteuid())
	}
}
