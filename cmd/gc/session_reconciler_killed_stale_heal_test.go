package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// staleListingProvider answers a plain liveness read for its stale names from
// a listing taken before their runtime died, as the tmux state cache does
// until it lists again after another process (`gc session kill`) stopped
// them. A fresh read (ObserveLivenessSince) sees the runtime as it is.
type staleListingProvider struct {
	*runtime.Fake
	stale map[string]bool
}

func (p staleListingProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if p.stale[name] {
		return runtime.Liveness{Running: true, Alive: true}, nil
	}
	return runtime.ObserveLivenessWithError(p.Fake, name, processNames)
}

func (p staleListingProvider) ObserveLivenessSince(name string, processNames []string, _ time.Time) (runtime.Liveness, error) {
	return runtime.ObserveLivenessWithError(p.Fake, name, processNames)
}

// TestLegacyHealNeverWakesAKilledSeatOnAStaleListing (mc-pvonx, staging J39 C
// on legacy dolt). Kills legacy's state heal writing state=awake over a seat
// `gc session kill` just stopped because the controller's liveness read was
// served from a listing taken before the kill: the row read awake with
// sleep_reason=killed, and the next pass restarted the seat in place on its
// claim instead of freeing the slot. The heal confirms a runtime that would
// wake an asleep row on a fresh read, so the killed row stays asleep.
func TestLegacyHealNeverWakesAKilledSeatOnAStaleListing(t *testing.T) {
	for _, c := range []struct {
		name      string
		live      bool // the runtime is really up (a fresh read sees it)
		wantAwake bool
	}{
		{"killed, the listing stale", false, false},
		{"its runtime really up", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}}}
			env.addDesired("worker", "worker", c.live)
			session := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&session, map[string]string{
				"state": string(sessionpkg.StateAsleep), "sleep_reason": string(sessionpkg.SleepReasonKilled),
				"slept_at": env.clk.Now().Add(-time.Second).UTC().Format(time.RFC3339), "pool_slot": "1",
			})
			gen := session.Metadata["generation"]
			sp := staleListingProvider{Fake: env.sp, stale: map[string]bool{"worker": !c.live}}
			pass := func() beads.Bead {
				t.Helper()
				cur, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				reconcileSessionBeadsAtPath(
					context.Background(), env.city, []beads.Bead{cur}, env.desiredState, configuredSessionNames(env.cfg, "", env.store), env.cfg, sp,
					env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
					nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
					env.startOptions...,
				)
				got, err := env.store.Get(session.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			got := pass()
			if awake := got.Metadata["state"] == string(sessionpkg.StateAwake); awake != c.wantAwake {
				t.Fatalf("after one pass: state=%q sleep_reason=%q; want awake %v", got.Metadata["state"], got.Metadata["sleep_reason"], c.wantAwake)
			}
			if c.live {
				return
			}
			// The listing catches up; the killed seat is not restarted in place.
			sp.stale["worker"] = false
			env.clk.Time = env.clk.Time.Add(time.Minute)
			got = pass()
			if got.Metadata["generation"] != gen || got.Metadata["state"] == string(sessionpkg.StateCreating) || env.sp.IsRunning("worker") {
				t.Fatalf("after the listing caught up: state=%q generation=%q (was %q), running %v; want the killed seat not restarted in place", got.Metadata["state"], got.Metadata["generation"], gen, env.sp.IsRunning("worker"))
			}
		})
	}
}

// TestLegacyUndesiredHealNeverWakesAKilledRowOnAStaleListing (the #7885
// review). Kills the undesired-row heal site's spurious awake over a stale
// listing: a killed row that left the desired set stays asleep; one whose
// runtime is really up still heals awake.
func TestLegacyUndesiredHealNeverWakesAKilledRowOnAStaleListing(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "the listing stale", true: "its runtime really up"}[live], func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true"}}}
			if live {
				if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
					t.Fatal(err)
				}
			}
			session := env.createSessionBead("worker", "worker")
			env.setSessionMetadata(&session, map[string]string{
				"state": string(sessionpkg.StateAsleep), "sleep_reason": string(sessionpkg.SleepReasonKilled),
				"slept_at": env.clk.Now().Add(-time.Second).UTC().Format(time.RFC3339),
			})
			sp := staleListingProvider{Fake: env.sp, stale: map[string]bool{"worker": !live}}
			cur, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			reconcileSessionBeadsAtPath(
				context.Background(), env.city, []beads.Bead{cur}, map[string]TemplateParams{}, configuredSessionNames(env.cfg, "", env.store), env.cfg, sp,
				env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
				nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
				env.startOptions...,
			)
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if awake := got.Metadata["state"] == string(sessionpkg.StateAwake); awake != live {
				t.Fatalf("undesired row after one pass: state=%q sleep_reason=%q; want awake %v", got.Metadata["state"], got.Metadata["sleep_reason"], live)
			}
		})
	}
}

// failingFreshProvider is staleListingProvider whose fresh read fails.
type failingFreshProvider struct{ staleListingProvider }

func (failingFreshProvider) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return runtime.Liveness{}, errors.New("fresh read failed")
}

// TestLegacyHealWakesNothingOnAFailedFreshRead (the #7885 review, M4). Kills
// a confirmation that falls back to the stale read when the fresh read
// fails: the asleep row stays asleep.
func TestLegacyHealWakesNothingOnAFailedFreshRead(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{
		"state": string(sessionpkg.StateAsleep), "sleep_reason": string(sessionpkg.SleepReasonKilled),
		"slept_at": env.clk.Now().Add(-time.Second).UTC().Format(time.RFC3339), "pool_slot": "1",
	})
	sp := failingFreshProvider{staleListingProvider{Fake: env.sp, stale: map[string]bool{"worker": true}}}
	cur, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{cur}, env.desiredState, configuredSessionNames(env.cfg, "", env.store), env.cfg, sp,
		env.store, nil, nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		env.startOptions...,
	)
	if got, err := env.store.Get(session.ID); err != nil || got.Metadata["state"] != string(sessionpkg.StateAsleep) {
		t.Fatalf("after a failed fresh read: row %v (%v); want it left asleep", got.Metadata, err)
	}
}

// sinceRecordingProvider records the since of every fresh read.
type sinceRecordingProvider struct {
	*runtime.Fake
	sinces []time.Time
}

func (p *sinceRecordingProvider) ObserveLivenessSince(name string, processNames []string, since time.Time) (runtime.Liveness, error) {
	p.sinces = append(p.sinces, since)
	return runtime.ObserveLivenessWithError(p.Fake, name, processNames)
}

// TestHealAliveConfirmerReusesOneSincePerPass (the #7885 review). Kills a
// confirmation that pays a fresh listing and process snapshot per row: every
// confirmation of a pass reads at or after the pass's first one, so the
// provider serves them from one fresh listing; a row not asleep, or read
// dead, asks nothing.
func TestHealAliveConfirmerReusesOneSincePerPass(t *testing.T) {
	sp := &sinceRecordingProvider{Fake: runtime.NewFake()}
	c := &healAliveConfirmer{sp: sp}
	asleep := sessionpkg.Info{MetadataState: string(sessionpkg.StateAsleep)}
	c.alive(asleep, "a", nil, true)
	c.alive(sessionpkg.Info{MetadataState: string(sessionpkg.StateAwake)}, "b", nil, true)
	c.alive(asleep, "c", nil, false)
	c.alive(asleep, "d", nil, true)
	if len(sp.sinces) != 2 || !sp.sinces[0].Equal(sp.sinces[1]) || sp.sinces[0].IsZero() {
		t.Fatalf("fresh reads at %v; want two (the asleep rows read alive), sharing the pass's first since", sp.sinces)
	}
}
