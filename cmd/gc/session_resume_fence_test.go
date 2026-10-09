package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/testutil"
)

// startHookProvider runs hooks around a runtime Start and gives the started
// runtime the GC_INSTANCE_TOKEN it was launched with, as tmux's environment
// does.
type startHookProvider struct {
	*runtime.Fake
	beforeStart, afterStart func()
}

func (p *startHookProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.beforeStart != nil {
		p.beforeStart()
	}
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if err := p.SetMeta(name, "GC_INSTANCE_TOKEN", cfg.Env["GC_INSTANCE_TOKEN"]); err != nil {
		return err
	}
	if p.afterStart != nil {
		p.afterStart()
	}
	return nil
}

// preWakeRefusingStore refuses, and records, a PreWake commit (a write that
// moves the row to creating), so a tick that would start the row stops
// before it waits on the resume's in-process session lock.
type preWakeRefusingStore struct {
	beads.Store
	preWakes *[]string
}

func (s preWakeRefusingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if kvs["state"] == string(sessionpkg.StateCreating) {
		*s.preWakes = append(*s.preWakes, id)
		return errors.New("test: PreWake refused")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// resumeTickEnv is an always-on named session (legacy wants it awake unless
// held) and a legacy tick with a persistent drain tracker and real drainOps.
type resumeTickEnv struct {
	store    beads.Store
	bead     beads.Bead
	cfg      *config.City
	fake     *runtime.Fake
	dt       *drainTracker
	tickLog  bytes.Buffer
	preWakes []string
}

const resumeTickName = "s-gc-resume-tick"

func newResumeTickEnv(t *testing.T, meta map[string]string) *resumeTickEnv {
	t.Helper()
	store, bead, cityDir := newKillPokeSession(t, resumeTickName)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "session-a"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "session-a"
mode = "always"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityDir, io.Discard)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	row := map[string]string{"generation": "2", "instance_token": "tok-2", "work_dir": cityDir, "provider": "claude"}
	maps.Copy(row, meta)
	setKillFixtureMetadata(t, store, bead.ID, row)
	return &resumeTickEnv{store: store, bead: bead, cfg: cfg, fake: runtime.NewFake(), dt: newDrainTracker()}
}

func (e *resumeTickEnv) tick(t *testing.T) {
	t.Helper()
	current := mustGetBead(t, e.store, e.bead.ID)
	desired := map[string]TemplateParams{resumeTickName: {Command: "true", SessionName: resumeTickName, TemplateName: killPokeSessionIdentity}}
	reconcileSessionBeads(
		context.Background(), []beads.Bead{current}, desired,
		configuredSessionNames(e.cfg, "", e.store), e.cfg, e.fake, preWakeRefusingStore{Store: e.store, preWakes: &e.preWakes},
		newDrainOps(e.fake), nil, nil, e.dt, map[string]int{killPokeSessionIdentity: 1}, false, nil, "",
		nil, clock.Real{}, events.Discard, 0, 0, &e.tickLog, &e.tickLog,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	)
}

// resumeHoldShape is an operator hold a resume consumes (review finding 4)
// on a row whose runtime is down. legacyStarts marks one legacy does not
// read as a hold: its tick plans a start, and only the locked re-read before
// PreWake (the resume's fence or runtime) defers it.
type resumeHoldShape struct {
	meta         map[string]string
	legacyStarts bool
}

func resumeHoldShapes() map[string]resumeHoldShape {
	hour := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	asleep := map[string]string{"state": string(sessionpkg.StateAsleep), "slept_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}
	with := func(kv ...string) map[string]string {
		m := maps.Clone(asleep)
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	return map[string]resumeHoldShape{
		"bare suspended":  {meta: map[string]string{"state": string(sessionpkg.StateSuspended), "suspended_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, legacyStarts: true},
		"user-hold only":  {meta: with("sleep_intent", "user-hold"), legacyStarts: true},
		"heartbeat hold":  {meta: with("held_until", hour)},
		"managed suspend": {meta: map[string]string{"state": string(sessionpkg.StateSuspended), "held_until": hour, "sleep_intent": "user-hold", "sleep_reason": "drained"}},
		"quarantine":      {meta: with("quarantined_until", hour, "sleep_reason", "quarantine")},
		"wait hold":       {meta: with("sleep_intent", "wait-hold", "wait_hold", "true", "sleep_reason", "wait-hold")},
	}
}

// TestOperatorResumeWithLegacyTickMidResume is D8's controller race
// (CONTRACT v5.9 D8 rule 2; review B1), for each hold shape: legacy ticks at
// both edges of an operator resume's Start see the row still held, so they
// neither PreWake it nor start a second runtime, and do not stop the
// resume's runtime; the resume consumes the hold at the generation and token
// it started with; and a tick after the consume, with the same drain
// tracker and real drainOps, leaves the session running. For the shapes
// legacy does not read as a hold, the tick's locked re-read defers its
// planned start. Kills a resume that clears the hold before its runtime
// exists (the tick plans a start and PreWakes over the operator's runtime),
// and a PreWake without the re-read's recheck.
func TestOperatorResumeWithLegacyTickMidResume(t *testing.T) {
	for name, shape := range resumeHoldShapes() {
		t.Run(name, func(t *testing.T) {
			e := newResumeTickEnv(t, shape.meta)
			var violations []string
			// The controller is another process; in-process, a tick that plans
			// a start blocks on the resume's session lock, so the hook waits
			// for it only boundedly and records that as a planned start.
			var blocked []chan struct{}
			runTick := func(edge string) {
				done := make(chan struct{})
				go func() { defer close(done); e.tick(t) }()
				select {
				case <-done:
				case <-time.After(testutil.GoroutineRaceTimeout):
					if !shape.legacyStarts {
						violations = append(violations, edge+": the tick planned a start of the held row")
					}
					blocked = append(blocked, done)
					return
				}
				if after := mustGetBead(t, e.store, e.bead.ID); after.Metadata["instance_token"] != "tok-2" || after.Metadata["generation"] != "2" {
					violations = append(violations, edge+": the tick re-keyed the row")
				}
				if edge == "after" && !e.fake.IsRunning(resumeTickName) {
					violations = append(violations, edge+": the tick stopped the resume's runtime")
				}
			}
			sp := &startHookProvider{Fake: e.fake}
			sp.beforeStart = func() { runTick("before") }
			sp.afterStart = func() { runTick("after") }

			mgr := sessionpkg.NewManagerWithOptions(e.store, sp)
			attachErr := mgr.Attach(context.Background(), e.bead.ID, "true", runtime.Config{})
			for _, done := range blocked {
				<-done
			}
			if attachErr != nil {
				t.Fatalf("Attach: %v; violations %v; tick log:\n%s", attachErr, violations, e.tickLog.String())
			}
			e.tick(t)
			if !e.fake.IsRunning(resumeTickName) {
				violations = append(violations, "the tick after the consume stopped the session")
			}
			for _, v := range violations {
				t.Error(v)
			}
			if len(e.preWakes) != 0 || e.fake.CountCalls("Start", resumeTickName) != 1 {
				t.Errorf("PreWakes = %d, Starts = %d; want none and one", len(e.preWakes), e.fake.CountCalls("Start", resumeTickName))
			}
			final := mustGetBead(t, e.store, e.bead.ID)
			if state := final.Metadata["state"]; state != string(sessionpkg.StateActive) && state != string(sessionpkg.StateAwake) {
				t.Errorf("final state = %q, want active (or legacy's awake); row %v; tick log:\n%s", state, final.Metadata, e.tickLog.String())
			}
			for key, want := range map[string]string{
				"held_until": "", "sleep_intent": "", "wait_hold": "", "quarantined_until": "",
				"generation": "2", "instance_token": "tok-2", sessionpkg.ResumePendingAtKey: "",
			} {
				if got := final.Metadata[key]; got != want {
					t.Errorf("final %s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// TestResumeVoidsLegacyHoldDrainAck is review finding 2: legacy drains a
// managed-suspended session whose runtime is still up and acks the drain for
// its user-hold; an operator attaches to the live runtime and the resume
// consumes the hold. Whether the ack landed before the resume (the resume
// voids the reconciler's ack on its runtime) or after it (a tick acting on a
// snapshot from before the resume; the drain-ack handler finds the fresh row
// unheld), the next tick, with the same tracker and real drainOps, keeps the
// session running and drops the ack. Kills either guard.
func TestResumeVoidsLegacyHoldDrainAck(t *testing.T) {
	hour := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for name, tc := range map[string]struct {
		ackReason      string // the ack's reason when not the drain's own
		ackAfterResume bool
	}{
		"ack before resume": {},
		"ack after resume":  {ackAfterResume: true},
		// An ack minted while the row was held but for a reason that is not
		// the hold (a no-wake-reason drain of a held row): only the resume's
		// void on its own runtime clears it.
		"non-hold ack before resume": {ackReason: "no-wake-reason"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newResumeTickEnv(t, map[string]string{"state": string(sessionpkg.StateSuspended), "sleep_intent": "user-hold", "held_until": hour})
			if err := e.fake.Start(context.Background(), resumeTickName, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := e.fake.SetMeta(resumeTickName, "GC_INSTANCE_TOKEN", "tok-2"); err != nil {
				t.Fatal(err)
			}
			e.tick(t) // begins the hold's drain
			ds := e.dt.get(e.bead.ID)
			if ds == nil {
				t.Fatalf("test premise: legacy did not begin a drain; tick log:\n%s", e.tickLog.String())
			}
			// The drain advance's ack, as setReconcilerDrainAckMetadata writes
			// it; the stop a tick would schedule on it is not this test's race.
			ack := func() {
				minted := *ds
				if tc.ackReason != "" {
					minted.reason = tc.ackReason
				}
				if err := setReconcilerDrainAckMetadata(e.fake, resumeTickName, &minted); err != nil {
					t.Fatal(err)
				}
				ds.ackSet = true
			}
			if !tc.ackAfterResume {
				ack()
			}
			mgr := sessionpkg.NewManagerWithOptions(e.store, e.fake)
			if err := mgr.Attach(context.Background(), e.bead.ID, "true", runtime.Config{}); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if tc.ackAfterResume {
				ack()
			}
			e.tick(t)
			if !e.fake.IsRunning(resumeTickName) {
				t.Fatalf("the tick after the resume stopped the session on its stale %s ack; tick log:\n%s", ds.reason, e.tickLog.String())
			}
			if acked, _ := newDrainOps(e.fake).isDrainAcked(resumeTickName); acked {
				t.Fatal("the stale hold ack was kept")
			}
		})
	}
}

// TestPrepareStartDefersChangedCandidate is the legacy PreWake recheck
// (review finding 4): after its locked re-read, a start whose row moved its
// lifecycle facts since the tick's snapshot, carries a live resume fence, or
// whose runtime came up since writes no PreWake and defers. An unchanged row
// proceeds. Kills a recheck missing any of the three.
func TestPrepareStartDefersChangedCandidate(t *testing.T) {
	for name, tc := range map[string]struct {
		change   func(store beads.Store, sp *runtime.Fake, id string)
		deferred bool
	}{
		"unchanged": {change: func(beads.Store, *runtime.Fake, string) {}},
		"lifecycle moved": {change: func(store beads.Store, _ *runtime.Fake, id string) {
			_ = store.SetMetadataBatch(id, map[string]string{"held_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		}, deferred: true},
		"live resume fence": {change: func(store beads.Store, _ *runtime.Fake, id string) {
			_ = store.SetMetadataBatch(id, map[string]string{sessionpkg.ResumePendingAtKey: time.Now().UTC().Format(time.RFC3339Nano)})
		}, deferred: true},
		"runtime up": {change: func(_ beads.Store, sp *runtime.Fake, _ string) {
			_ = sp.Start(context.Background(), "worker", runtime.Config{})
		}, deferred: true},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			b, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel, "agent:worker"}, Metadata: map[string]string{
				"template": "worker", "session_name": "worker", "state": "asleep", "generation": "2", "instance_token": "tok-2",
			}})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := mustGetBead(t, store, b.ID)
			sp := runtime.NewFake()
			tc.change(store, sp, b.ID)
			_, err = prepareStartCandidateForCity(startCandidate{
				info: sessiontest.SeedBead(t, snapshot),
				tp:   TemplateParams{TemplateName: "worker", SessionName: "worker", Command: "true"},
			}, "", "", nil, sp, store, clock.Real{}, io.Discard, nil)
			if got := errors.Is(err, errStartCandidateChanged); got != tc.deferred {
				t.Fatalf("prepare = %v, want deferred = %v", err, tc.deferred)
			}
			if tc.deferred && mustGetBead(t, store, b.ID).Metadata["state"] == string(sessionpkg.StateCreating) {
				t.Fatal("a deferred start wrote its PreWake")
			}
		})
	}
}
