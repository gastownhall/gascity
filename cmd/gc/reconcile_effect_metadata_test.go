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
// not synced to one that is (v5 M1; SESS-716): an alias written on a row
// whose runtime reads Current is set on that runtime; the same runtime read
// fresh as another incarnation's (its token not the row's) is not.
func TestSimRowMetadataSyncsRuntimeAliasOnCurrentOnly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  bool
	}{{"current", "", true}, {"another incarnation", "tok-0", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reread := simOpts{specs: func(m map[string]effectSpec) {
				spec := m[intentRowMetadata]
				spec.sections = []section{called(0, probed(aliasAvailability, func(v txView, chk aliasCheck, err error) txStep {
					if tc.token != "" && v.RT != nil {
						rt := *v.RT
						rt.Identity.Token = tc.token
						v.RT = &rt
					}
					return rowMetadataStep(v, chk, err)
				}), syncRuntimeAlias)}
				m[intentRowMetadata] = spec
			}}
			s := bossSim(t, reread, []string{"gc-1"}, bossRow("state", "active", "last_woke_at", rowAt(-time.Hour)))
			s.settle()
			if m := s.gc1(); m["alias"] != "boss" {
				t.Fatalf("gc-1 %v, want the alias written", m)
			}
			got := slices.ContainsFunc(s.sp.Calls, func(c runtime.Call) bool {
				return c.Method == "SetMeta" && c.Name == "s-gc-1" && c.Key == "GC_ALIAS" && c.Value == "boss"
			})
			if got != tc.want {
				t.Fatalf("runtime alias synced %v, want %v (calls %v)", got, tc.want, s.sp.Calls)
			}
			s.noViolations(t)
		})
	}
}
