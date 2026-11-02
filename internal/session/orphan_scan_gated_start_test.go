package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// gatedScanStartWait is how long a gated start is given to refuse once its
// own context has ended, before the test calls it stuck.
const gatedScanStartWait = 5 * time.Second

// gatedScanFake is a Fake whose process-table scans answer nothing at once
// until it is armed, and block once it is: until the test ends. It counts the
// provider Starts made while armed.
type gatedScanFake struct {
	*runtime.Fake
	armOnStart bool

	mu          sync.Mutex
	gate        chan struct{}
	opened      bool
	armedStarts int
}

// newGatedScanFake returns an unarmed gated scanner whose gate opens when the
// test ends, so no scan outlives it.
func newGatedScanFake(t *testing.T) *gatedScanFake {
	f := &gatedScanFake{Fake: runtime.NewFake()}
	t.Cleanup(f.open)
	return f
}

// arm makes every later scan block.
func (f *gatedScanFake) arm() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		f.gate = make(chan struct{})
	}
}

func (f *gatedScanFake) open() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil && !f.opened {
		close(f.gate)
		f.opened = true
	}
}

func (f *gatedScanFake) FindRuntimesBySessionID(string) ([]runtime.LiveRuntime, error) {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return nil, nil
}

// Start runs the Fake's Start, arming the gate after it when armOnStart is
// set.
func (f *gatedScanFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	f.mu.Lock()
	if f.gate != nil {
		f.armedStarts++
	}
	f.mu.Unlock()
	err := f.Fake.Start(ctx, name, cfg)
	if f.armOnStart {
		f.arm()
	}
	return err
}

func (f *gatedScanFake) startsWhileArmed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.armedStarts
}

// startUnderShortDeadline runs start with a context that ends well inside the
// scan budget and returns its error, failing if it is still waiting long
// after that context has ended.
func startUnderShortDeadline(t *testing.T, start func(ctx context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- start(ctx) }()
	select {
	case err := <-done:
		return err
	case <-time.After(gatedScanStartWait):
		t.Fatalf("start still waiting on its blocked orphan scan %s after its context ended", gatedScanStartWait)
		return nil
	}
}

// seedSuspendedGatedTarget creates a session through an unarmed gated scanner
// and suspends it, so the next Start or StartRuntimeOnly resumes it through
// the pre-start orphan gate.
func seedSuspendedGatedTarget(t *testing.T) (*Manager, *gatedScanFake, Info) {
	t.Helper()
	sp := newGatedScanFake(t)
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithCityPath(t.TempDir()))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	return mgr, sp, info
}

// seedStaleKeyGatedTarget stores a suspended, resume-capable session whose
// resume dies during startup, and arms the gate at that Start, so only the
// stale-key fresh relaunch's orphan scan blocks.
func seedStaleKeyGatedTarget(t *testing.T) (*Manager, *gatedScanFake, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	sp := newGatedScanFake(t)
	sp.armOnStart = true
	mgr := newTestManager(t, store, sp, WithCityPath(t.TempDir()), WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	b, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state":               string(StateSuspended),
			"provider":            "claude",
			"work_dir":            "/tmp",
			"command":             "claude --dangerously",
			"resume_flag":         "--resume",
			"session_id_flag":     "--session-id",
			"session_key":         capacityTestSessionKey,
			"started_config_hash": capacityTestConfigHash,
			PrimedAtMetadataKey:   capacityTestPrimedAt,
		},
	})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	sessName := sessionNameFor(b.ID)
	if err := store.SetMetadata(b.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	sp.StartErrors = map[string]error{sessName: fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, sessName)}
	return mgr, sp, b.ID, sessName
}

// Every start gated on the pre-start orphan scan refuses as unavailable when
// the scan cannot answer in time, and makes no provider Start after it: a
// resume (Start and StartRuntimeOnly), the stale-key fresh relaunch, and a
// create, which rolls its new row and explicit name back.
func TestPreStartOrphanScanDefersEveryGatedStart(t *testing.T) {
	requireDeferred := func(t *testing.T, what string, err error, sp *gatedScanFake) {
		t.Helper()
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("%s error = %v, want one wrapping runtime.ErrRuntimeUnavailable", what, err)
		}
		if got := sp.startsWhileArmed(); got != 0 {
			t.Fatalf("%s made %d provider Start(s) after its orphan scan went unanswered", what, got)
		}
	}

	t.Run("Start", func(t *testing.T) {
		mgr, sp, info := seedSuspendedGatedTarget(t)
		sp.arm()
		err := startUnderShortDeadline(t, func(ctx context.Context) error {
			return mgr.Start(ctx, testActor(mgr, ActorOperator), info.ID, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
		})
		requireDeferred(t, "Start", err, sp)
	})

	t.Run("StartRuntimeOnly", func(t *testing.T) {
		mgr, sp, info := seedSuspendedGatedTarget(t)
		by := runtimeOnlyActor(t, mgr, info.ID)
		inc := mintUnder(t, mgr, by.Lease, info.ID)
		sp.arm()
		err := startUnderShortDeadline(t, func(ctx context.Context) error {
			return mgr.StartRuntimeOnly(ctx, by, info.ID, inc, BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir})
		})
		requireDeferred(t, "StartRuntimeOnly", err, sp)
	})

	t.Run("stale-key fresh relaunch", func(t *testing.T) {
		mgr, sp, id, sessName := seedStaleKeyGatedTarget(t)
		err := startUnderShortDeadline(t, func(ctx context.Context) error {
			return mgr.Start(ctx, testActor(mgr, ActorOperator), id, capacityTestResumeCmd, runtime.Config{WorkDir: "/tmp"})
		})
		if got := sp.CountCalls("Start", sessName); got != 1 {
			t.Fatalf("Start called %d times, want only the resume that died", got)
		}
		requireDeferred(t, "stale-key relaunch", err, sp)
	})

	t.Run("CreateSession", func(t *testing.T) {
		store := beads.NewMemStore()
		sp := newGatedScanFake(t)
		mgr := newTestManager(t, store, sp, WithCityPath(t.TempDir()))
		sp.arm()
		err := startUnderShortDeadline(t, func(ctx context.Context) error {
			_, err := mgr.CreateSession(ctx, CreateOptions{ExplicitName: "orphan-scan-worker", Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude"})
			return err
		})
		requireDeferred(t, "CreateSession", err, sp)
		rows, err := store.List(beads.ListQuery{Type: BeadType, IncludeClosed: true})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("session rows = %d, want the one the create made", len(rows))
		}
		row := rows[0]
		if row.Status != "closed" || row.Metadata["session_name"] != "" || row.Metadata["session_name_explicit"] != "" {
			t.Fatalf("row %s left status=%s session_name=%q session_name_explicit=%q, want it closed with its explicit name released",
				row.ID, row.Status, row.Metadata["session_name"], row.Metadata["session_name_explicit"])
		}
	})
}
