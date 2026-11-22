package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// evictUncommittedFixture is one row and its occupant runtime at the
// eviction's decision point (#6417 F2): the row is desired, claimless,
// uncommitted and prompt-bearing, and the live runtime under its name reads
// as the row's own. One shared drain tracker and stderr buffer span a case's
// calls, so episode dedup is exercised.
type evictUncommittedFixture struct {
	store     beads.Store
	sp        *runtime.Fake
	row       beads.Bead
	clk       *clock.Fake
	dt        *drainTracker
	stderr    *strings.Builder
	city      string
	tp        TemplateParams
	attached  func() (bool, error)
	holdLease func(t *testing.T) func()
	// beforeStop runs inside the stop, after the decision; stopErr, when set,
	// is what the stop returns instead of acting.
	beforeStop func(t *testing.T)
	stopErr    error
	// created, when set, is the creation time the provider reports for the
	// occupant, as tmux reports #{session_created}: decimal unix seconds.
	created string
}

// createdLivenessProvider is the fake runtime reporting a creation time for
// the session object it observes, as tmux does.
type createdLivenessProvider struct {
	*runtime.Fake
	created string
}

func (p *createdLivenessProvider) ObserveLiveness(name string, _ []string) runtime.Liveness {
	if !p.IsRunning(name) {
		return runtime.Liveness{}
	}
	return runtime.Liveness{Running: true, Alive: true, ObjectID: "$1", ObjectCreated: p.created}
}

func (f *evictUncommittedFixture) sessionName() string { return f.row.Metadata["session_name"] }

func newEvictUncommittedFixture(t *testing.T, meta map[string]string) *evictUncommittedFixture {
	t.Helper()
	f := &evictUncommittedFixture{
		store:  beads.NewMemStore(),
		sp:     runtime.NewFake(),
		clk:    &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		dt:     newDrainTracker(),
		stderr: &strings.Builder{},
		city:   t.TempDir(),
		tp:     TemplateParams{Prompt: "rendered startup prompt"},
	}
	base := map[string]string{
		// The row last woke two hours ago, well past the startup window.
		"last_woke_at": f.clk.Now().Add(-2 * time.Hour).Format(time.RFC3339),
		// The #6417 handoff's stamp: the restart cycle that cleared the start
		// witness committed, so the row sits inside an interrupted start cycle
		// and is the eviction's to complete.
		"reset_committed_at": f.clk.Now().Add(-2 * time.Hour).Format(time.RFC3339),
	}
	for k, v := range meta {
		base[k] = v
	}
	f.row = createUncommittedStartRow(t, f.store, "gc-evict", base)
	if err := f.sp.Start(context.Background(), f.sessionName(), runtime.Config{}); err != nil {
		t.Fatalf("planting the occupant runtime: %v", err)
	}
	if err := f.sp.SetMeta(f.sessionName(), "GC_SESSION_ID", f.row.ID); err != nil {
		t.Fatalf("stamping the occupant's identity: %v", err)
	}
	return f
}

// call runs evictUncommittedLiveStartIfDue once, re-reading the row as the
// reconciler's snapshot does and supplying the baseline closures (the attach
// probe and the legacy stop) the #127 site supplies.
func (f *evictUncommittedFixture) call(t *testing.T) {
	t.Helper()
	row, err := f.store.Get(f.row.ID)
	if err != nil {
		t.Fatalf("Get(row): %v", err)
	}
	info := sessiontest.SeedBead(t, row)
	cfg := &config.City{}
	attached := f.attached
	if attached == nil {
		attached = func() (bool, error) {
			return sessionAttachedForConfigDrift(info.ID, f.sp, f.city, f.store, cfg, f.sessionName())
		}
	}
	if f.holdLease != nil {
		release := f.holdLease(t)
		defer release()
	}
	var sp runtime.Provider = f.sp
	if f.created != "" {
		sp = &createdLivenessProvider{Fake: f.sp, created: f.created}
	}
	evictUncommittedLiveStartIfDue(evictUncommittedStart{
		ctx:      context.Background(),
		cityPath: f.city,
		info:     info,
		name:     f.sessionName(),
		tp:       f.tp,
		cfg:      cfg,
		sp:       sp,
		dt:       f.dt,
		clk:      f.clk,
		startup:  60 * time.Second,
		stderr:   f.stderr,
		attached: attached,
		stop: func(d sessionpkg.Decided) error {
			if f.beforeStop != nil {
				f.beforeStop(t)
			}
			if f.stopErr != nil {
				return f.stopErr
			}
			return (legacyAct{cityPath: f.city, store: f.store, sp: f.sp, cfg: cfg}).Stop(d)
		},
	})
}

// TestEvictUncommittedLiveStart_Guards is AC-7 of
// gastownhall/gascity#6417: with exactly one precondition of the AC-6
// eviction broken, the reconciler stops nothing — and a foreign or unreadable
// identity logs exactly one warning per episode. The control case (no
// condition broken) evicts on the tick after the startup window.
func TestEvictUncommittedLiveStart_Guards(t *testing.T) {
	// The control first: the row is sighted inside the window on the first
	// call, and the second call, one startup window later, evicts.
	t.Run("control evicts after the window", func(t *testing.T) {
		f := newEvictUncommittedFixture(t, nil)
		f.call(t)
		if !f.sp.IsRunning(f.sessionName()) {
			t.Fatalf("the runtime was stopped inside the startup window\nstderr=%s", f.stderr.String())
		}
		f.clk.Time = f.clk.Time.Add(90 * time.Second)
		f.call(t)
		if f.sp.IsRunning(f.sessionName()) {
			t.Fatalf("the control case did not evict\nstderr=%s", f.stderr.String())
		}
		if !strings.Contains(f.stderr.String(), "evicted "+f.sessionName()) {
			t.Fatalf("the eviction was not logged\nstderr=%s", f.stderr.String())
		}
	})

	// Every other case breaks exactly one condition and expects the runtime
	// to survive two ticks that would otherwise evict.
	cases := []struct {
		name     string
		meta     map[string]string
		prepare  func(t *testing.T, f *evictUncommittedFixture)
		advance  time.Duration // the clock step between the two calls
		wantWarn int
	}{
		{
			name:    "startup window not passed",
			advance: 0,
		},
		{
			name: "client attached",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.attached = func() (bool, error) { return true, nil }
			},
			advance: 90 * time.Second,
		},
		{
			name: "attach probe fails",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.attached = func() (bool, error) { return false, errors.New("probe gave no answer") }
			},
			advance: 90 * time.Second,
		},
		{
			name: "identity foreign",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				if err := f.sp.SetMeta(f.sessionName(), "GC_SESSION_ID", "gc-somebody-else"); err != nil {
					t.Fatalf("restamping the occupant: %v", err)
				}
			},
			advance:  90 * time.Second,
			wantWarn: 1,
		},
		{
			name: "identity unreadable",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				if f.sp.GetMetaErrors == nil {
					f.sp.GetMetaErrors = map[string]map[string]error{}
				}
				f.sp.GetMetaErrors[f.sessionName()] = map[string]error{"GC_SESSION_ID": errors.New("no answer")}
			},
			advance:  90 * time.Second,
			wantWarn: 1,
		},
		{
			name: "template renders no prompt",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.tp = TemplateParams{}
			},
			advance: 90 * time.Second,
		},
		{
			name:    "pending-create claim",
			meta:    map[string]string{"pending_create_claim": "true", "state": "creating"},
			advance: 90 * time.Second,
		},
		{
			name:    "operator hold",
			meta:    map[string]string{"sleep_intent": "user-hold"},
			advance: 90 * time.Second,
		},
		{
			// A steady row that simply never committed a config hash (a live
			// seat outside any restart cycle) is not the eviction's to
			// replace: the wake arms own it.
			name:    "no reset cycle committed",
			meta:    map[string]string{"reset_committed_at": ""},
			advance: 90 * time.Second,
		},
		{
			name:    "start committed",
			meta:    map[string]string{"started_config_hash": "h-1"},
			advance: 90 * time.Second,
		},
		{
			name:    "live hold",
			meta:    map[string]string{"held_until": "2026-03-08T14:00:00Z"},
			advance: 90 * time.Second,
		},
		{
			name: "identity unstamped",
			prepare: func(t *testing.T, f *evictUncommittedFixture) {
				if err := f.sp.RemoveMeta(f.sessionName(), "GC_SESSION_ID"); err != nil {
					t.Fatalf("unstamping the occupant: %v", err)
				}
			},
			advance:  90 * time.Second,
			wantWarn: 1,
		},
		{
			name: "a start is in flight",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.stopErr = sessionpkg.ErrSessionStarting
			},
			advance: 90 * time.Second,
		},
		{
			// A newer incarnation landing between the decision and the kill
			// refuses it under FactsLegacyUncommittedEvict.
			name: "premise moved after the decision",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.beforeStop = func(t *testing.T) {
					if err := f.store.SetMetadata(f.row.ID, "generation", "3"); err != nil {
						t.Fatalf("moving the row's incarnation: %v", err)
					}
				}
			},
			advance: 90 * time.Second,
		},
		{
			name: "a start holds the lease",
			prepare: func(_ *testing.T, f *evictUncommittedFixture) {
				f.holdLease = func(t *testing.T) func() {
					lease, release, err := tryRuntimeLease(nil, f.city, f.sessionName(), "", 0)
					if err != nil {
						t.Fatalf("holding the runtime lease: %v", err)
					}
					return func() { lease.Release(); release() }
				}
			},
			advance: 90 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictUncommittedFixture(t, tc.meta)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			f.call(t) // inside the window (or refused by the broken condition)
			f.clk.Time = f.clk.Time.Add(tc.advance)
			f.call(t)
			if f.sp.IsRunning(f.sessionName()) == false {
				t.Fatalf("the runtime was stopped although %s", tc.name)
			}
			if got := strings.Count(f.stderr.String(), "leaving the live runtime"); got != tc.wantWarn {
				t.Fatalf("identity warnings = %d, want %d\nstderr=%s", got, tc.wantWarn, f.stderr.String())
			}
			if strings.Contains(f.stderr.String(), "evicting the uncommitted live start of") {
				t.Fatalf("a benign refusal was logged as an eviction error\nstderr=%s", f.stderr.String())
			}
		})
	}
}

// TestEvictUncommittedLiveStart_FirstSightingBasis pins the age basis when the
// provider reports no creation time (#6417 F2): the startup window runs from
// the first tick that saw the row's incarnation live and uncommitted, a new
// drain tracker starts it again, and so does a new incarnation of the row.
func TestEvictUncommittedLiveStart_FirstSightingBasis(t *testing.T) {
	f := newEvictUncommittedFixture(t, nil)
	sighted := f.dt
	f.call(t)
	if !f.sp.IsRunning(f.sessionName()) {
		t.Fatalf("evicted on the first sighting\nstderr=%s", f.stderr.String())
	}

	f.clk.Time = f.clk.Time.Add(90 * time.Second)
	f.dt = newDrainTracker()
	f.call(t)
	if !f.sp.IsRunning(f.sessionName()) {
		t.Fatalf("evicted under a new tracker, want its first sighting to restart the wait\nstderr=%s", f.stderr.String())
	}

	f.dt = sighted
	if err := f.store.SetMetadata(f.row.ID, "generation", "3"); err != nil {
		t.Fatalf("moving the row's incarnation: %v", err)
	}
	f.call(t)
	if !f.sp.IsRunning(f.sessionName()) {
		t.Fatalf("evicted a new incarnation on the old incarnation's sighting\nstderr=%s", f.stderr.String())
	}

	f.clk.Time = f.clk.Time.Add(90 * time.Second)
	f.call(t)
	if f.sp.IsRunning(f.sessionName()) {
		t.Fatalf("not evicted past the startup window from the new incarnation's first sighting\nstderr=%s", f.stderr.String())
	}
}

// TestEvictUncommittedLiveStart_RowGuardsReadNoRuntime: a row that fails a
// row guard, the cycle guard included, is refused before any provider call
// (no identity read, liveness read or attach probe), so a row that can never
// qualify costs a tick nothing, and a foreign identity under it logs no
// warning. Kills a cycle guard ordered after the identity read.
func TestEvictUncommittedLiveStart_RowGuardsReadNoRuntime(t *testing.T) {
	for _, tc := range []struct {
		name     string
		meta     map[string]string
		noPrompt bool
	}{
		{name: "no reset cycle committed", meta: map[string]string{"reset_committed_at": ""}},
		{name: "start committed", meta: map[string]string{"started_config_hash": "h-1"}},
		{name: "pending-create claim", meta: map[string]string{"pending_create_claim": "true", "state": "creating"}},
		{name: "operator hold", meta: map[string]string{"sleep_intent": "user-hold"}},
		{name: "template renders no prompt", noPrompt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEvictUncommittedFixture(t, tc.meta)
			if tc.noPrompt {
				f.tp = TemplateParams{}
			}
			if err := f.sp.SetMeta(f.sessionName(), "GC_SESSION_ID", "gc-somebody-else"); err != nil {
				t.Fatalf("restamping the occupant: %v", err)
			}
			probed := false
			f.attached = func() (bool, error) {
				probed = true
				return false, nil
			}
			f.clk.Time = f.clk.Time.Add(90 * time.Second)
			before := len(f.sp.Calls)
			f.call(t)
			if got := f.sp.Calls[before:]; len(got) != 0 || probed {
				t.Fatalf("provider calls = %v (attach probed: %v), want none for a row the guards refuse", got, probed)
			}
			if f.stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want nothing logged", f.stderr.String())
			}
		})
	}
}

// TestEvictUncommittedLiveStart_CreationTimeBasis pins the age basis when the
// provider reports the runtime's creation time as tmux does
// (#{session_created}, decimal unix seconds; #6417 F2): the startup window
// runs from that time, so a runtime created past the window is evicted on the
// first sighting (a new drain tracker, as after a controller restart, does
// not restart the wait), and one created inside the window survives until the
// window has passed from its creation.
func TestEvictUncommittedLiveStart_CreationTimeBasis(t *testing.T) {
	t.Run("created past the window", func(t *testing.T) {
		f := newEvictUncommittedFixture(t, nil)
		f.created = strconv.FormatInt(f.clk.Now().Add(-2*time.Hour).Unix(), 10)
		f.call(t)
		if f.sp.IsRunning(f.sessionName()) {
			t.Fatalf("a runtime created two hours ago survived its first sighting: its creation time was not the age basis\nstderr=%s", f.stderr.String())
		}
	})
	t.Run("created inside the window", func(t *testing.T) {
		f := newEvictUncommittedFixture(t, nil)
		f.created = strconv.FormatInt(f.clk.Now().Add(-10*time.Second).Unix(), 10)
		f.call(t)
		f.clk.Time = f.clk.Time.Add(40 * time.Second)
		f.call(t)
		if !f.sp.IsRunning(f.sessionName()) {
			t.Fatalf("evicted 50s after its creation, inside the startup window\nstderr=%s", f.stderr.String())
		}
		f.clk.Time = f.clk.Time.Add(30 * time.Second)
		f.call(t)
		if f.sp.IsRunning(f.sessionName()) {
			t.Fatalf("not evicted 80s after its creation, past the startup window\nstderr=%s", f.stderr.String())
		}
	})
}
