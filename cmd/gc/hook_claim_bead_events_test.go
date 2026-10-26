package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runproj"
)

// hookClaimEventsCity is a city whose run projection is fed by a real event log
// and whose work store is a MemStore served to the hook's bd context through a
// fake bd: the supervisor's CachingStore records the run's creation into the
// log, and `gc hook --claim` then claims a step by writing the store directly,
// exactly as a worker's one-shot process does.
type hookClaimEventsCity struct {
	dir    string
	mem    *beads.MemStore
	cache  *beads.CachingStore
	rootID string
	stepID string

	// afterClaimWrite, when set, runs once the claim CAS has landed in the
	// store and before the hook reads it back: the window in which the
	// supervisor can read the claimed row first.
	afterClaimWrite func()
	// onBd, when set, sees every bd invocation before it is answered.
	onBd func(args []string)

	mu     sync.Mutex
	bdArgs [][]string
}

func newHookClaimEventsCity(t *testing.T) *hookClaimEventsCity {
	t.Helper()
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY", dir)

	// The supervisor's recorder and cache, as wrapWithCachingStore builds them.
	rec, err := openCityEventsLog(dir, io.Discard)
	if err != nil {
		t.Fatalf("opening the city event log: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })
	mem := beads.NewMemStore()
	cache := beads.NewCachingStore(mem, cacheChangeRecorder(rec))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("priming the supervisor cache: %v", err)
	}
	root, err := cache.Create(beads.Bead{Title: "run", Type: "molecule"})
	if err != nil {
		t.Fatalf("creating the run root: %v", err)
	}
	step, err := cache.Create(beads.Bead{
		Title: "implement",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			"gc.routed_to":                 "worker",
		},
	})
	if err != nil {
		t.Fatalf("creating the run step: %v", err)
	}

	c := &hookClaimEventsCity{dir: dir, mem: mem, cache: cache, rootID: root.ID, stepID: step.ID}
	originalRunner := hookClaimCommandRunnerWithEnvContext
	t.Cleanup(func() { hookClaimCommandRunnerWithEnvContext = originalRunner })
	hookClaimCommandRunnerWithEnvContext = func(_ context.Context, env map[string]string) beads.CommandRunner {
		return func(_ string, name string, args ...string) ([]byte, error) {
			if name != "bd" {
				return nil, errors.New("unexpected command " + name)
			}
			return c.fakeBd(t, env["BEADS_ACTOR"], args)
		}
	}
	return c
}

// fakeBd answers the bd verbs the hook claim path issues against the MemStore.
func (c *hookClaimEventsCity) fakeBd(t *testing.T, actor string, args []string) ([]byte, error) {
	c.mu.Lock()
	c.bdArgs = append(c.bdArgs, append([]string(nil), args...))
	c.mu.Unlock()
	if c.onBd != nil {
		c.onBd(args)
	}
	show := func(id string) ([]byte, error) {
		b, err := c.mem.Get(id)
		if err != nil {
			return []byte("Error: no issue found matching " + id), errors.New("exit status 1: no issue found")
		}
		return json.Marshal([]beads.Bead{b})
	}
	switch {
	case len(args) >= 3 && args[0] == "update" && args[2] == "--claim":
		id := args[1]
		b, err := c.mem.Get(id)
		if err != nil {
			return nil, err
		}
		if b.Assignee != "" && b.Assignee != actor {
			return []byte("already claimed by " + b.Assignee), errors.New("exit status 1")
		}
		status, assignee := "in_progress", actor
		if err := c.mem.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			return nil, err
		}
		if c.afterClaimWrite != nil {
			c.afterClaimWrite()
		}
		return show(id)
	case len(args) >= 7 && args[0] == "update" && args[2] == "--if-assignee" && args[4] == "--if-status":
		// The conditional release verb: update <id> --if-assignee A --if-status S
		// --status open --assignee "".
		id := args[1]
		b, err := c.mem.Get(id)
		if err != nil {
			return nil, err
		}
		if b.Assignee != args[3] || b.Status != args[5] {
			return []byte("precondition failed"), errors.New("exit status 13")
		}
		status, assignee := "open", ""
		if err := c.mem.Update(id, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			return nil, err
		}
		return show(id)
	case len(args) >= 3 && args[0] == "update" && args[1] == "--json":
		id := args[2]
		opts := beads.UpdateOpts{Metadata: map[string]string{}}
		for i := 3; i+1 < len(args); i += 2 {
			switch args[i] {
			case "--set-metadata":
				k, v, _ := strings.Cut(args[i+1], "=")
				opts.Metadata[k] = v
			case "--assignee":
				a := args[i+1]
				opts.Assignee = &a
			case "--status":
				s := args[i+1]
				opts.Status = &s
			}
		}
		if err := c.mem.Update(id, opts); err != nil {
			return nil, err
		}
		return show(id)
	case len(args) >= 3 && args[0] == "show" && args[1] == "--json":
		return show(args[len(args)-1])
	case len(args) >= 2 && args[0] == "dep" && args[1] == "list":
		return []byte("[]"), nil
	default:
		t.Errorf("unexpected bd invocation: %q", args)
		return nil, errors.New("unexpected bd invocation")
	}
}

// claim runs the production `gc hook --claim` entry point against the city's
// work store, with a work query that offers the step.
func (c *hookClaimEventsCity) claim(t *testing.T) {
	t.Helper()
	step, err := c.mem.Get(c.stepID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal([]beads.Bead{step})
	if err != nil {
		t.Fatal(err)
	}
	workQuery := "printf '%s' '" + string(out) + "'"
	env := []string{"BEADS_DIR=" + filepath.Join(c.dir, ".beads")}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                env,
		JSON:               true,
	}
	cfg, err := loadCityConfig(c.dir, io.Discard)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := claimHookWork(newHookCity(c.dir, cfg), workQuery, c.dir, env, []hookStore{{dir: c.dir, env: env}}, opts,
		func(string, error) {}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("claimHookWork = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

// projectedStep folds the city event log the way the supervisor's run tailer
// does and returns the step as the run views would serve it.
func (c *hookClaimEventsCity) projectedStep(t *testing.T) beads.Bead {
	t.Helper()
	p := runproj.NewProjector()
	if err := p.ColdLoad(filepath.Join(c.dir, ".gc", "events.jsonl")); err != nil {
		t.Fatalf("folding the city event log: %v", err)
	}
	if misses := p.DecodeMisses(); misses != 0 {
		t.Fatalf("fold decode misses = %d, want 0", misses)
	}
	for _, m := range runproj.RunMembers(p.Beads(), c.rootID) {
		if m.ID == c.stepID {
			return m
		}
	}
	t.Fatalf("step %s is not a member of run %s in the projection", c.stepID, c.rootID)
	return beads.Bead{}
}

// stepUpdates returns the step's bead.updated events in log order.
func (c *hookClaimEventsCity) stepUpdates(t *testing.T) []events.Event {
	t.Helper()
	evts, err := events.ReadFiltered(filepath.Join(c.dir, ".gc", "events.jsonl"),
		events.Filter{Type: events.BeadUpdated, Subject: c.stepID})
	if err != nil {
		t.Fatalf("reading the city event log: %v", err)
	}
	return evts
}

// A step claimed by `gc hook --claim` writes the store directly from a one-shot
// worker process. The run views fold only the city event log, so unless the
// claim announces itself there, /runs/{id}/steps keeps the step pending with no
// assignee until the supervisor happens to read the row back (30-120 s).
func TestHookClaimAnnouncesClaimToRunProjection(t *testing.T) {
	city := newHookClaimEventsCity(t)
	if got := city.projectedStep(t); got.Status != "open" || got.Assignee != "" {
		t.Fatalf("before the claim the projection shows status=%q assignee=%q, want open and unassigned", got.Status, got.Assignee)
	}

	city.claim(t)

	got := city.projectedStep(t)
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("after `gc hook --claim` the run projection shows step %s status=%q assignee=%q, want in_progress owned by worker-1",
			city.stepID, got.Status, got.Assignee)
	}
	if got.Metadata[beadmeta.RootBeadIDMetadataKey] != city.rootID {
		t.Fatalf("projected step lost its run membership: metadata=%v", got.Metadata)
	}
	// The identity stamp (gc.claimed_at) is derived onto the CAS readback
	// rather than read back, and must still match what the store holds.
	stored, err := city.mem.Get(city.stepID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Metadata, stored.Metadata) {
		t.Fatalf("projected metadata = %v, want the stored row's %v", got.Metadata, stored.Metadata)
	}
	// The announcement reads nothing it can derive: after the hook's last
	// write (the stamp), the only bd call is the one edge confirmation.
	var afterWrites []string
	for _, args := range city.bdArgs {
		if args[0] == "update" {
			afterWrites = nil
			continue
		}
		afterWrites = append(afterWrites, strings.Join(args, " "))
	}
	if len(afterWrites) != 1 || !strings.HasPrefix(afterWrites[0], "dep list ") {
		t.Fatalf("bd calls after the hook's last write = %q, want only one edge confirmation", afterWrites)
	}

	// The announcement is the supervisor's own bead.updated shape: a canonical
	// snapshot with the run correlation on the envelope and an explicit edge set.
	updated := city.stepUpdates(t)
	if len(updated) == 0 {
		t.Fatal("no bead.updated recorded for the claimed step")
	}
	first := updated[0]
	if first.RunID != city.rootID {
		t.Fatalf("bead.updated run_id = %q, want %q", first.RunID, city.rootID)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(first.Payload, &fields); err != nil {
		t.Fatalf("decoding the bead.updated payload: %v", err)
	}
	if _, ok := fields["dependencies"]; !ok {
		t.Fatalf("bead.updated payload %s carries no dependencies key; the supervisor cache would read it as edge coverage unknown", first.Payload)
	}
}

// The supervisor may also announce the same claim: its read-announce when a
// read refetches the row, or its reconcile. Folding both announcements, in
// either order, must leave the step exactly where the store has it.
func TestHookClaimAnnouncementFoldsIdempotentlyWithSupervisorAnnouncement(t *testing.T) {
	for _, supervisorFirst := range []bool{false, true} {
		name := "hook-then-supervisor"
		if supervisorFirst {
			name = "supervisor-then-hook"
		}
		t.Run(name, func(t *testing.T) {
			city := newHookClaimEventsCity(t)
			if supervisorFirst {
				// The supervisor refetches the claimed row before the hook has
				// read it back, so its announcement lands first.
				city.afterClaimWrite = func() {
					city.afterClaimWrite = nil
					if _, err := city.cache.RefreshRow(city.stepID); err != nil {
						t.Errorf("supervisor read-back: %v", err)
					}
				}
			}
			city.claim(t)
			afterHook := city.projectedStep(t)

			// The supervisor reads the row back once more after the hook is done
			// and announces whatever it finds that differs from its cache.
			if _, err := city.cache.RefreshRow(city.stepID); err != nil {
				t.Fatalf("supervisor read-back: %v", err)
			}
			afterBoth := city.projectedStep(t)

			var announcers []string
			for _, e := range city.stepUpdates(t) {
				b, ok := beads.DecodeBeadEventPayload(e.Payload)
				if !ok || b.Status != "in_progress" || b.Assignee != "worker-1" {
					t.Fatalf("bead.updated seq %d by %s carries %s, want the claimed row", e.Seq, e.Actor, e.Payload)
				}
				who := "hook"
				if isCacheActor(e.Actor) {
					who = "supervisor"
				}
				if len(announcers) == 0 || announcers[len(announcers)-1] != who {
					announcers = append(announcers, who)
				}
			}
			want := []string{"hook", "supervisor"}
			if supervisorFirst {
				want = []string{"supervisor", "hook", "supervisor"}
			}
			if !reflect.DeepEqual(announcers, want) {
				t.Fatalf("bead.updated announcers in log order = %v, want %v", announcers, want)
			}

			stored, err := city.mem.Get(city.stepID)
			if err != nil {
				t.Fatal(err)
			}
			for _, got := range []beads.Bead{afterHook, afterBoth} {
				if got.Status != "in_progress" || got.Assignee != "worker-1" {
					t.Fatalf("projection shows status=%q assignee=%q, want in_progress owned by worker-1", got.Status, got.Assignee)
				}
				if !reflect.DeepEqual(got.Metadata, stored.Metadata) {
					t.Fatalf("projected metadata = %v, want the stored row's %v", got.Metadata, stored.Metadata)
				}
			}
			if hook, both := projectedForm(t, afterHook), projectedForm(t, afterBoth); hook != both {
				t.Fatalf("the supervisor re-announcing the claim changed the projected step:\n hook: %s\n both: %s", hook, both)
			}
		})
	}
}

// projectedForm is a projected bead as the run views serialize it, with its
// creation time cut to whole seconds (bd stamps whole seconds, so the hook's
// snapshot, read through BdStore, carries them, while the test's supervisor
// cache reads its MemStore directly, which does not) and its update time left
// out. updated_at is the write clock, not claim state: the hook derives its
// snapshot onto the claim's CAS readback, which precedes its gc.claimed_at
// stamp write, so whenever the stamp lands in the next wall-clock second the
// hook's updated_at trails the supervisor's by one. The fold's idempotence is
// about status, assignee, metadata and run membership, all still compared.
func projectedForm(t *testing.T, b beads.Bead) string {
	t.Helper()
	b.CreatedAt = b.CreatedAt.UTC().Truncate(time.Second)
	b.UpdatedAt = time.Time{}
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// Every work-store write the hook makes after (or instead of) the claim CAS is
// announced once it lands — preassigning a continuation sibling, re-stamping an
// adopted bead, giving back an undelivered claim, reverting a stale lease — and
// a write that did not land announces nothing.
func TestHookClaimWorkStoreWritesAnnounceOnlyWhatLanded(t *testing.T) {
	ctx := context.Background()
	inProgress, open, empty := "in_progress", "open", ""
	set := func(city *hookClaimEventsCity, status, assignee *string) {
		t.Helper()
		if err := city.mem.Update(city.stepID, beads.UpdateOpts{Status: status, Assignee: assignee}); err != nil {
			t.Fatal(err)
		}
	}
	claimedBy := func(who string) func(*hookClaimEventsCity) {
		return func(city *hookClaimEventsCity) { set(city, &inProgress, &who) }
	}
	cases := []struct {
		name         string
		setup        func(*hookClaimEventsCity)
		run          func(*hookClaimEventsCity, hookClaimOps) error
		wantStatus   string
		wantAssignee string
		wantAnnounce bool
	}{{
		name: "preassign continuation sibling",
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			return ops.AssignContinuation(ctx, city.dir, nil, city.stepID, "worker-1")
		},
		wantStatus: "open", wantAssignee: "worker-1", wantAnnounce: true,
	}, {
		name:  "adoption re-stamp",
		setup: claimedBy("worker-legacy"),
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			moved, err := ops.RestampAdopted(ctx, city.dir, nil, city.stepID, "worker-legacy", "worker-1")
			if !moved {
				return errors.New("re-stamp did not move")
			}
			return err
		},
		wantStatus: "in_progress", wantAssignee: "worker-1", wantAnnounce: true,
	}, {
		name:  "undelivered claim release",
		setup: claimedBy("worker-1"),
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			released, err := ops.Release(ctx, city.dir, nil, city.stepID, "worker-1")
			if !released {
				return errors.New("release did not land")
			}
			return err
		},
		wantStatus: "open", wantAssignee: "", wantAnnounce: true,
	}, {
		name:  "stale lease reclaim",
		setup: claimedBy("worker-dead"),
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			reclaimed, _, err := ops.ReclaimStale(ctx, city.dir, nil, city.stepID)
			if !reclaimed {
				return errors.New("reclaim did not land")
			}
			return err
		},
		wantStatus: "open", wantAssignee: "", wantAnnounce: true,
	}, {
		name:  "lost release",
		setup: claimedBy("worker-2"),
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			released, err := ops.Release(ctx, city.dir, nil, city.stepID, "worker-1")
			if released {
				return errors.New("release of another worker's claim landed")
			}
			return err
		},
		wantStatus: "in_progress", wantAssignee: "worker-2", wantAnnounce: false,
	}, {
		name: "failed preassignment",
		run: func(city *hookClaimEventsCity, ops hookClaimOps) error {
			if err := ops.AssignContinuation(ctx, city.dir, nil, "gc-missing", "worker-1"); err == nil {
				return errors.New("assignment of a missing bead succeeded")
			}
			return nil
		},
		wantStatus: "open", wantAssignee: "", wantAnnounce: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := newHookClaimEventsCity(t)
			if tc.setup != nil {
				tc.setup(city)
			}
			// The base seams write the MemStore directly, as the bd context
			// would; the wrapper under test reads back through the fake bd.
			assign := func(id, who string) error {
				return city.mem.Update(id, beads.UpdateOpts{Assignee: &who})
			}
			ifAssigned := func(who string, write func() error) (bool, error) {
				b, err := city.mem.Get(city.stepID)
				if err != nil || b.Assignee != who {
					return false, err
				}
				return true, write()
			}
			ops := withHookClaimBeadEvents(hookClaimOps{
				AssignContinuation: func(_ context.Context, _ string, _ []string, id, who string) error {
					return assign(id, who)
				},
				RestampAdopted: func(_ context.Context, _ string, _ []string, id, from, to string) (bool, error) {
					return ifAssigned(from, func() error { return assign(id, to) })
				},
				Release: func(_ context.Context, _ string, _ []string, id, who string) (bool, error) {
					return ifAssigned(who, func() error {
						return city.mem.Update(id, beads.UpdateOpts{Status: &open, Assignee: &empty})
					})
				},
				ReclaimStale: func(_ context.Context, _ string, _ []string, id string) (bool, string, error) {
					b, err := city.mem.Get(id)
					if err != nil {
						return false, "", err
					}
					return true, b.Assignee, city.mem.Update(id, beads.UpdateOpts{Status: &open, Assignee: &empty})
				},
			}, city.dir)
			before := len(city.stepUpdates(t))

			if err := tc.run(city, ops); err != nil {
				t.Fatal(err)
			}
			// The write only notes itself; nothing reaches the log (or reads
			// the store) until the announcement point.
			if n := len(city.stepUpdates(t)); n != before {
				t.Fatalf("the write announced itself (%d new bead.updated) before the announcement point", n-before)
			}
			ops.announceBeadEvents()

			announced := len(city.stepUpdates(t)) > before
			if announced != tc.wantAnnounce {
				t.Fatalf("announced = %v, want %v", announced, tc.wantAnnounce)
			}
			if !tc.wantAnnounce {
				return
			}
			got := city.projectedStep(t)
			if got.Status != tc.wantStatus || got.Assignee != tc.wantAssignee {
				t.Fatalf("projection shows status=%q assignee=%q, want %q/%q", got.Status, got.Assignee, tc.wantStatus, tc.wantAssignee)
			}
		})
	}
}

// Announcing is best-effort: the claim has already committed when the event is
// appended, so a city event log that cannot be written leaves the claim in
// place, delivered, and reported once on stderr.
func TestHookClaimSucceedsWhenTheEventLogCannotBeWritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only mode this test relies on")
	}
	city := newHookClaimEventsCity(t)
	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })
	eventsPath := filepath.Join(city.dir, ".gc", "events.jsonl")
	if err := os.Chmod(eventsPath, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(eventsPath, 0o644) })

	city.claim(t)

	stored, err := city.mem.Get(city.stepID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != "in_progress" || stored.Assignee != "worker-1" {
		t.Fatalf("stored step = status %q assignee %q, want the claim to stand", stored.Status, stored.Assignee)
	}
	if len(warned) == 0 {
		t.Fatal("a claim whose announcement could not be appended reported nothing")
	}
}

// claimWithClock runs the claim tier through the announcing ops on a fake clock:
// the claim CAS lands casAge into the invocation, and every bd read made after
// the CAS's own readback costs two seconds, as on a loaded store.
func claimWithClock(t *testing.T, city *hookClaimEventsCity, window, casAge time.Duration) (hookClaimOps, int, string, string) {
	t.Helper()
	step, err := city.mem.Get(city.stepID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal([]beads.Bead{step})
	if err != nil {
		t.Fatal(err)
	}
	invokedAt := time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	now := invokedAt
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	claimed, readbackSeen := false, false
	city.onBd = func(args []string) {
		clockMu.Lock()
		defer clockMu.Unlock()
		switch {
		case len(args) >= 3 && args[0] == "update" && args[2] == "--claim":
			now = invokedAt.Add(casAge)
			claimed = true
		case claimed && args[0] == "show" && !readbackSeen:
			readbackSeen = true // the CAS's own canonical readback
		case claimed && (args[0] == "show" || args[0] == "dep"):
			now = now.Add(2 * time.Second)
		}
	}

	env := []string{"BEADS_DIR=" + filepath.Join(city.dir, ".beads")}
	ops := withHookClaimBeadEvents(hookClaimOps{Now: clock, InvokedAt: invokedAt, ClaimWindow: window}, city.dir)
	var stdout, stderr bytes.Buffer
	code := claimHookWorkWithRunner("printf '%s' '"+string(out)+"'", city.dir, env, []hookStore{{dir: city.dir, env: env}},
		hookClaimOptions{
			Assignee:           "worker-1",
			IdentityCandidates: []string{"worker-1"},
			RouteTargets:       []string{"worker"},
			Env:                env,
			JSON:               true,
		}, ops, shellWorkQueryWithEnv, func(string, error) {}, &stdout, &stderr)
	return ops, code, stdout.String(), stderr.String()
}

// The announcement must never decide whether a claim is kept. A claim CAS that
// lands inside the claim window plus its reserved mutation budget is delivered;
// the store reads the announcement makes come after that decision, so a slow
// store spending them cannot turn the claim into a straddle unwind.
func TestHookClaimAnnouncementReadsDoNotSpendTheLandingWindow(t *testing.T) {
	city := newHookClaimEventsCity(t)
	const window = time.Minute
	_, code, stdout, stderr := claimWithClock(t, city, window, window+hookClaimMutationTimeout-time.Second)

	if code != 0 || !strings.Contains(stdout, city.stepID) {
		t.Fatalf("claim = %d, stdout=%q, want the claim delivered; stderr=%s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "claim window") {
		t.Fatalf("a claim that landed inside window + reserve was unwound: %s", stderr)
	}
	got := city.projectedStep(t)
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("projection shows status=%q assignee=%q, want the kept claim announced", got.Status, got.Assignee)
	}
}

// A claim that lands too late is unwound, and what reaches the run views is
// the release, never the claim it gave back.
func TestHookClaimUnwoundClaimAnnouncesOnlyItsRelease(t *testing.T) {
	city := newHookClaimEventsCity(t)
	const window = time.Minute
	ops, code, _, stderr := claimWithClock(t, city, window, window+hookClaimMutationTimeout+time.Second)
	if code != 1 || !strings.Contains(stderr, "claim window") {
		t.Fatalf("claim = %d, stderr=%s, want the straddle unwind", code, stderr)
	}
	// claimHookWork announces what is left on its way out.
	ops.announceBeadEvents()

	for _, e := range city.stepUpdates(t) {
		if b, ok := beads.DecodeBeadEventPayload(e.Payload); ok && b.Status == "in_progress" {
			t.Fatalf("the unwound claim was announced: %s", e.Payload)
		}
	}
	got := city.projectedStep(t)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("projection shows status=%q assignee=%q, want the released step", got.Status, got.Assignee)
	}
	if len(city.stepUpdates(t)) == 0 {
		t.Fatal("the release was not announced")
	}
}
