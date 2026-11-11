package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
)

// orphanRuntime starts name on sp with the identity env a real runtime
// carries: its GC_SESSION_ID and GC_INSTANCE_TOKEN (either may be "" for a
// runtime that never had one).
func orphanRuntime(t *testing.T, sp *runtime.Fake, name, sessionID, token string) {
	t.Helper()
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"GC_SESSION_ID": sessionID, "GC_INSTANCE_TOKEN": token} {
		if v == "" {
			continue
		}
		if err := sp.SetMeta(name, k, v); err != nil {
			t.Fatal(err)
		}
	}
}

// orphanRow is an open session row bound to runtime name with token.
func orphanRow(id, name, token string) beads.Bead {
	meta := map[string]string{"session_name": name, "state": "active"}
	if token != "" {
		meta["instance_token"] = token
	}
	return censusSession(id, meta)
}

// ownerSourceOver is the orphan-sessions owner source over cityPath, as the
// registration builds it from the ownerless-runtimes check's store access.
func ownerSourceOver(cityPath string, cfg *config.City, open func(string) (beads.Store, error), sp runtime.Provider) doctor.RuntimeOwnerSource {
	return newOwnerlessRuntimesCheck(cfg, cityPath, open, sp, nil).runtimeOwnerSource()
}

func ownerOf(t *testing.T, resolve doctor.RuntimeOwnerResolver, name string) doctor.RuntimeOwnership {
	t.Helper()
	got, _ := resolve(name)
	return got
}

// TestRuntimeOwnerSourceDecidesByRuntimeIdentity (ga-cm3cfp): ownership is
// the runtime's own identity read against the session rows, never its name
// against a configured agent's. Every case here is a runtime no configured
// agent's name matches except the one that is named like one.
func TestRuntimeOwnerSourceDecidesByRuntimeIdentity(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}, Agents: []config.Agent{{Name: "deployer"}}}

	// A pool instance and a session-id-named runtime: GC_SESSION_ID resolves
	// to an open row.
	orphanRuntime(t, sp, "c--builder-3", "gc-pool3", "tok-pool3")
	orphanRuntime(t, sp, "s-gc-sid", "gc-sid", "tok-sid")
	// An adopted runtime: its env id predates the row minted for it, but the
	// open row named for it carries the runtime's own token.
	orphanRuntime(t, sp, "c--adopted", "gc-predates", "tok-adopted")
	// A runtime named like a configured named session whose bead is gone:
	// the stale env id resolves to nothing, and the open same-name row holds
	// a different token, so that row belongs to another incarnation.
	orphanRuntime(t, sp, "c--deployer", "gc-gone", "tok-old")
	// A runtime with no identity env at all, and no row naming it.
	orphanRuntime(t, sp, "c--anon", "", "")

	store := beads.NewMemStoreFrom(0, []beads.Bead{
		orphanRow("gc-pool3", "c--builder-3", "tok-pool3"),
		orphanRow("gc-sid", "s-gc-sid", "tok-sid"),
		orphanRow("gc-adopted", "c--adopted", "tok-adopted"),
		orphanRow("gc-deployer", "c--deployer", "tok-new"),
	}, nil)
	source := ownerSourceOver(city, cfg, func(string) (beads.Store, error) { return store, nil }, sp)
	resolve, err := source()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]doctor.RuntimeOwnership{
		"c--builder-3": doctor.RuntimeOwned,
		"s-gc-sid":     doctor.RuntimeOwned,
		"c--adopted":   doctor.RuntimeOwned,
		"c--deployer":  doctor.RuntimeOrphaned,
		"c--anon":      doctor.RuntimeOrphaned,
	} {
		if got := ownerOf(t, resolve, name); got != want {
			t.Errorf("ownership of %q = %v, want %v", name, got, want)
		}
	}
}

// The same fixtures, through the doctor check: the tracked runtimes are not
// listed and Fix stops only the untracked ones.
func TestOrphanSessionsDoctorCheckListsOnlyUntrackedRuntimes(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	orphanRuntime(t, sp, "c--builder-3", "gc-pool3", "tok-pool3")
	orphanRuntime(t, sp, "c--adopted", "gc-predates", "tok-adopted")
	orphanRuntime(t, sp, "c--deployer", "gc-gone", "tok-old")
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		orphanRow("gc-pool3", "c--builder-3", "tok-pool3"),
		orphanRow("gc-adopted", "c--adopted", "tok-adopted"),
	}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "c"}}
	check := doctor.NewOrphanSessionsCheck(sp, ownerSourceOver(city, cfg, func(string) (beads.Store, error) { return store, nil }, sp))

	res := check.Run(&doctor.CheckContext{CityPath: city})
	if res.Status != doctor.StatusWarning || strings.Join(res.Details, "\n") != "c--deployer" {
		t.Fatalf("result %+v, want a warning naming only the untracked c--deployer", res)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: city}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	if sp.IsRunning("c--deployer") || !sp.IsRunning("c--builder-3") || !sp.IsRunning("c--adopted") {
		t.Fatalf("running after Fix: deployer=%v builder-3=%v adopted=%v, want only the untracked one stopped",
			sp.IsRunning("c--deployer"), sp.IsRunning("c--builder-3"), sp.IsRunning("c--adopted"))
	}
}

// A closed row owns nothing, and an adopted row's token must be the
// runtime's own: a row that merely shares the name does not prove ownership.
func TestRuntimeOwnerSourceClosedAndForeignTokenRowsOwnNothing(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	orphanRuntime(t, sp, "c--closed", "gc-closed", "tok-closed")
	orphanRuntime(t, sp, "c--tokenless", "gc-gone", "")
	closed := orphanRow("gc-closed", "c--closed", "tok-closed")
	closed.Status = "closed"
	store := beads.NewMemStoreFrom(0, []beads.Bead{closed, orphanRow("gc-row", "c--tokenless", "tok-row")}, nil)
	resolve, err := ownerSourceOver(city, &config.City{}, func(string) (beads.Store, error) { return store, nil }, sp)()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c--closed", "c--tokenless"} {
		if got := ownerOf(t, resolve, name); got != doctor.RuntimeOrphaned {
			t.Errorf("ownership of %q = %v, want orphaned", name, got)
		}
	}
}

// An identity read that fails is unproven, not an orphan: neither a
// GC_SESSION_ID nor a GC_INSTANCE_TOKEN that cannot be read authorizes a Stop.
func TestRuntimeOwnerSourceUnreadableIdentityIsUnproven(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	orphanRuntime(t, sp, "c--noid", "gc-x", "tok-x")
	orphanRuntime(t, sp, "c--notoken", "gc-gone", "tok-y")
	sp.GetMetaErrors["c--noid"] = map[string]error{"GC_SESSION_ID": errors.New("pane unreachable")}
	sp.GetMetaErrors["c--notoken"] = map[string]error{"GC_INSTANCE_TOKEN": errors.New("pane unreachable")}
	store := beads.NewMemStore()
	resolve, err := ownerSourceOver(city, &config.City{}, func(string) (beads.Store, error) { return store, nil }, sp)()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c--noid", "c--notoken"} {
		got, why := resolve(name)
		if got != doctor.RuntimeUnproven || !strings.Contains(why, "pane unreachable") {
			t.Errorf("ownership of %q = %v (%q), want unproven naming the read error", name, got, why)
		}
	}
}

// A store the census cannot read leaves no identity provable: opening the
// city store, opening a configured rig's store, and a hard-failed census
// each fail the source, and Fix then stops nothing.
func TestRuntimeOwnerSourceUnreadableStoreFailsTheSource(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	orphanRuntime(t, sp, "c--stale", "gc-gone", "tok")
	rigCfg := &config.City{Workspace: config.Workspace{Name: "c"}, Rigs: []config.Rig{{Name: "r", Path: "/rigs/r"}}}
	for name, tc := range map[string]struct {
		cfg  *config.City
		open func(string) (beads.Store, error)
	}{
		"city store will not open": {&config.City{}, func(string) (beads.Store, error) { return nil, errors.New("store locked") }},
		"rig store will not open": {rigCfg, func(path string) (beads.Store, error) {
			if path == "/rigs/r" {
				return nil, errors.New("store locked")
			}
			return beads.NewMemStore(), nil
		}},
		"census hard error": {&config.City{}, func(string) (beads.Store, error) {
			return censusErrStore{beads.NewMemStore(), errors.New("store locked")}, nil
		}},
	} {
		t.Run(name, func(t *testing.T) {
			source := ownerSourceOver(city, tc.cfg, tc.open, sp)
			if _, err := source(); err == nil || !strings.Contains(err.Error(), "store locked") {
				t.Fatalf("source error = %v, want the store's cause", err)
			}
			check := doctor.NewOrphanSessionsCheck(sp, source)
			if err := check.Fix(&doctor.CheckContext{CityPath: city}); err == nil {
				t.Fatal("Fix() error = nil, want a refusal without an identity read")
			}
			if n := sp.CountCalls("Stop", "c--stale"); n != 0 {
				t.Fatalf("Stop called %d times without an identity read", n)
			}
		})
	}
}

// A partial census still proves what it holds owned, but an absence in it
// proves nothing: a runtime no returned row owns is unproven, never an orphan.
func TestRuntimeOwnerResolverPartialCensusProvesNoAbsence(t *testing.T) {
	sp := runtime.NewFake()
	orphanRuntime(t, sp, "c--held", "gc-held", "tok-held")
	orphanRuntime(t, sp, "c--missing", "gc-gone", "tok-gone")
	partial := censusErrStore{censusStore(orphanRow("gc-held", "c--held", "tok-held")), &beads.PartialResultError{Op: "list", Err: errors.New("down")}}
	census := readCensus(t, censusNow, censusLegs("class:sessions", partial))
	resolve := runtimeOwnerResolver(census, sp)
	if got := ownerOf(t, resolve, "c--held"); got != doctor.RuntimeOwned {
		t.Errorf("ownership of c--held = %v, want owned by the row the partial read returned", got)
	}
	if got := ownerOf(t, resolve, "c--missing"); got != doctor.RuntimeUnproven {
		t.Errorf("ownership of c--missing = %v, want unproven on a partial census", got)
	}
}

// Nothing here names a role: a runtime is as owned under one name as under
// any other, so renaming every runtime changes no answer.
func TestRuntimeOwnerResolverIgnoresNames(t *testing.T) {
	for _, name := range []string{"mayor", "c--mayor", "deployer", "x"} {
		sp := runtime.NewFake()
		orphanRuntime(t, sp, name, "gc-1", "tok-1")
		census := readCensus(t, censusNow, censusLegs("class:sessions", censusStore(orphanRow("gc-1", "elsewhere", "tok-1"))))
		if got := ownerOf(t, runtimeOwnerResolver(census, sp), name); got != doctor.RuntimeOwned {
			t.Errorf("ownership of %q = %v, want owned by its GC_SESSION_ID row whatever its name", name, got)
		}
	}
}
