package main

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The row-metadata verb's effect tests (CONTRACT v5 M1; C7d).

// bossRow is gc-1, the named session "boss" of the worker template, asleep
// and on demand, with no alias yet.
func bossRow(meta ...string) beads.Bead {
	base := []string{
		"template", "worker", "state", "asleep", "session_name", "s-gc-1", "generation", "1", "instance_token", "tok-1",
		"session_origin", "named", "configured_named_session", "true", "configured_named_identity", "boss", "configured_named_mode", "on_demand",
	}
	return sessionRow("gc-1", append(base, meta...)...)
}

// bossSim is a sim over rows with the named session "boss" configured.
func bossSim(t *testing.T, o simOpts, live []string, rows ...beads.Bead) *sim {
	s := scriptedWith(t, o, live, rows...)
	s.cfg.NamedSessions = []config.NamedSession{{Name: "boss", Template: "worker", Mode: "on_demand"}}
	return s
}

// Kills a conflict recorded every pass, a retry while the alias is still
// held, and no retry once it is free (v5 M1; SESS-717): a named row whose
// alias another row holds records the conflict once and holds its alias
// work, taking no lock, while the census shows the alias taken; freed, the
// alias lands and the conflict clears.
func TestSimRowMetadataConflictRecordedOnceRetriedWhenFree(t *testing.T) {
	saved := withCityAliasLock
	t.Cleanup(func() { withCityAliasLock = saved })
	locks := 0
	withCityAliasLock = func(city, alias string, fn func() error) error {
		locks++
		return saved(city, alias, fn)
	}
	s := bossSim(t, simOpts{}, nil, bossRow(), sessionRow("gc-9", "template", "worker", "state", "asleep", "session_name", "s-gc-9", "generation", "1", "session_origin", "manual", "alias", "boss"))
	s.settle()
	first := s.gc1()
	if first[poolAliasConflictMetadataKey] != "boss" || first[poolAliasConflictCountMetadataKey] != "1" || first["alias"] != "" {
		t.Fatalf("gc-1 %v, want the conflict recorded and no alias", first)
	}
	recorded := locks
	for range 3 {
		s.advance(time.Minute)
		s.settle()
	}
	if m := s.gc1(); m[poolAliasConflictCountMetadataKey] != "1" || m[poolAliasConflictAtMetadataKey] != first[poolAliasConflictAtMetadataKey] || locks != recorded {
		t.Fatalf("gc-1 %v after %d more locks, want the conflict recorded once and no retry", m, locks-recorded)
	}
	s.legs[0].cache.ApplyEvent("bead.updated", s.operator("gc-9", "alias", ""))
	for range 2 {
		s.settle()
	}
	if m := s.gc1(); m["alias"] != "boss" || m[poolAliasConflictMetadataKey] != "" {
		t.Fatalf("gc-1 %v, want the freed alias written and the conflict cleared", m)
	}
	s.noViolations(t)
}

// Kills alias work written unlocked (v5 M1): a city alias lock that cannot
// be taken writes nothing, neither the alias nor a conflict.
func TestSimRowMetadataAliasLockErrorWritesNothing(t *testing.T) {
	saved := withCityAliasLock
	t.Cleanup(func() { withCityAliasLock = saved })
	var locked []string
	withCityAliasLock = func(_ string, alias string, _ func() error) error {
		locked = append(locked, alias)
		return errors.New("lock unavailable")
	}
	s := bossSim(t, simOpts{}, nil, bossRow())
	for range 2 {
		s.settle()
	}
	if m := s.gc1(); len(locked) == 0 || locked[0] != "boss" || m["alias"] != "" || m[poolAliasConflictMetadataKey] != "" || m["synced_at"] != "" {
		t.Fatalf("locked %v, gc-1 %v: want the lock tried on boss and nothing written", locked, m)
	}
	s.noViolations(t)
}

// Kills the runtime alias synced to a runtime that is not the row's, or
// not synced to one that is, and provider I/O under the alias lock (v5 M1;
// SESS-716; C7d-1 review should-fix 3 and 5): the alias write holds the
// city alias lock across its read and write alone, then its own effect
// syncs the alias to the row's runtime when that reads Current, once; the
// same runtime read fresh as another incarnation's (its token not the
// row's) is not synced, and the debt is paid either way.
func TestSimRowMetadataSyncsRuntimeAliasOnCurrentOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{{"current", "", true}, {"another incarnation", "tok-0", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reread := simOpts{specs: func(m map[string]effectSpec) {
				spec := m[intentRuntimeAlias]
				spec.sections = []section{called(0, section{Decide: func(v txView) txStep {
					if tc.token != "" && v.RT != nil {
						rt := *v.RT
						rt.Identity.Token = tc.token
						v.RT = &rt
					}
					return runtimeAliasStep(v)
				}}, syncRuntimeAlias)}
				m[intentRuntimeAlias] = spec
			}}
			s := bossSim(t, reread, []string{"gc-1"}, bossRow("state", "active", "last_woke_at", rowAt(-time.Hour)))
			saved := withCityAliasLock
			t.Cleanup(func() { withCityAliasLock = saved })
			underLock := 0
			withCityAliasLock = func(city, alias string, fn func() error) error {
				return saved(city, alias, func() error {
					before := len(s.sp.Calls)
					err := fn()
					underLock += len(s.sp.Calls) - before
					return err
				})
			}
			synced := func() int {
				return len(slices.DeleteFunc(slices.Clone(s.sp.Calls), func(c runtime.Call) bool {
					return c.Method != "SetMeta" || c.Name != "s-gc-1" || c.Key != "GC_ALIAS" || c.Value != "boss"
				}))
			}
			s.settle()
			if m := s.gc1(); m["alias"] != "boss" || synced() != 0 || underLock != 0 {
				t.Fatalf("after the write: gc-1 %v, %d syncs, %d provider calls under the alias lock; want the alias, no sync yet, none", m, synced(), underLock)
			}
			for range 2 {
				s.settle()
			}
			if got := synced(); got != map[bool]int{true: 1, false: 0}[tc.want] {
				t.Fatalf("runtime alias synced %d times, want synced %v once at most (calls %v)", got, tc.want, s.sp.Calls)
			}
			if rec := s.p.out.record.Load(); slices.ContainsFunc(rec.Admitted, func(it intent) bool { return it.Kind == intentRuntimeAlias }) {
				t.Fatal("the paid sync was proposed again")
			}
			s.noViolations(t)
		})
	}
}

// startsWithin runs up to n patrols, each a pass whose effects but gc-1's
// start are released, and reports whether gc-1's start was admitted and
// how many of those passes admitted its row-metadata verb.
func startsWithin(s *sim, n int) (started bool, a7 int) {
	for i := 0; i < n && !started; i++ {
		s.advance(simPatrol)
		s.inventory()
		s.pass()
		for _, it := range s.p.out.record.Load().Admitted {
			switch {
			case it.Key.ID != "gc-1":
			case it.Kind == intentStart:
				started = true
			case it.Kind == intentRowMetadata:
				a7++
			}
		}
		for j := 0; j < len(s.parked); {
			if e := s.parked[j]; e.it.Key.ID == "gc-1" && e.it.Kind == intentStart {
				j++
				continue
			}
			s.release(j)
		}
	}
	return started, a7
}

// Kills a conflict the census cannot see starving the row (C7d-1 review
// must-fix 2): a wanted named row whose alias another row holds as its
// session name records the conflict once, then starts with its alias
// deferred.
func TestSimRowMetadataConflictBySessionNameDefersTheAlias(t *testing.T) {
	holder := sessionRow("gc-9", "template", "worker", "state", "asleep", "session_name", "boss", "generation", "1", "session_origin", "manual")
	s := bossSim(t, simOpts{}, nil, bossRow("configured_named_mode", "always"), holder)
	s.cfg.NamedSessions[0].Mode = "always"
	started, a7 := startsWithin(s, 6)
	if m := s.gc1(); !started || a7 != 1 || m[poolAliasConflictMetadataKey] != "boss" || m["alias"] != "" {
		t.Fatalf("started %v after %d row-metadata passes, gc-1 %v: want the conflict recorded once, then the start with no alias", started, a7, m)
	}
	s.noViolations(t)
}

// Kills a persistent refusal winning the row every pass (C7d-1 review
// should-fix 4): a city alias lock that never comes refuses A7's alias
// work, which then defers, so the wanted row starts without its alias.
func TestSimRowMetadataLockErrorDefersTheAlias(t *testing.T) {
	saved := withCityAliasLock
	t.Cleanup(func() { withCityAliasLock = saved })
	withCityAliasLock = func(string, string, func() error) error { return errors.New("lock unavailable") }
	s := bossSim(t, simOpts{}, nil, bossRow("configured_named_mode", "always"))
	s.cfg.NamedSessions[0].Mode = "always"
	if started, a7 := startsWithin(s, 6); !started || a7 == 0 || s.gc1()["alias"] != "" {
		t.Fatalf("started %v after %d row-metadata passes, gc-1 %v: want the alias deferred and the row started", started, a7, s.gc1())
	}
	s.noViolations(t)
}

// Kills the write guard dropped (C7d-1 review F11): a row whose alias
// conflict is recorded and held gets the verb for a plain key alone, with
// no lock; an operator's clear of the conflict count landing between the
// pass and the verb gives the fresh row alias work, under a lock the verb
// does not hold, so it refuses and writes nothing.
func TestSimRowMetadataRefusesALockTheFreshRowDoesNotTake(t *testing.T) {
	recorded := []string{"session_origin", "", poolAliasConflictMetadataKey, "boss", poolAliasConflictCountMetadataKey, "1"}
	s := bossSim(t, simOpts{}, nil, bossRow(recorded...), sessionRow("gc-9", "template", "worker", "state", "asleep", "session_name", "s-gc-9", "generation", "1", "session_origin", "manual", "alias", "boss"))
	s.inventory()
	s.pass()
	var e *simEffect
	for _, p := range s.parked {
		if p.it.Key.ID == "gc-1" {
			e = p
		}
	}
	if e == nil || e.it.Kind != intentRowMetadata || e.it.AliasLock != "" {
		t.Fatalf("parked %d effects, want gc-1's verb with no lock", len(s.parked))
	}
	b, _ := s.legs[0].backing.Get("gc-1")
	s.outside("operator", s.legs[0], b, func(l *simLeg, b beads.Bead) {
		s.setMeta(l, b.ID, poolAliasConflictCountMetadataKey, "") // the count alone: the row's resolution stands
	})
	for len(s.parked) > 0 {
		s.release(0)
	}
	s.audit("v2")
	if m := s.gc1(); m["session_origin"] != "" || m[poolAliasConflictCountMetadataKey] != "" || m["synced_at"] != "" {
		t.Fatalf("gc-1 %v: want the verb refused, none of its keys written", m)
	}
	s.noViolations(t)
}
