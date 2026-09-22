package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// proxiedScopeFixture is a scope that looks, on disk, exactly like one bd serves
// through its proxy: a proxied-server metadata binding, the proxied sidecar, and
// bd's own proxy.pid record under .beads/dolt.
//
// The files are REAL (proxyendpoint decodes them with production code) and the
// three effects a unit test cannot afford — the process table, the TCP probe and
// the bd fork — are injected. That split is what makes these tests provable on a
// box with no dolt, no bd and no proxy, while still proving gc agrees with the
// document bd writes rather than with a stub.
type proxiedScopeFixture struct {
	t         *testing.T
	scopeRoot string
	root      string
	record    proxyendpoint.Record
	alive     bool
	argv      []string
}

func newProxiedScopeFixture(t *testing.T) *proxiedScopeFixture {
	t.Helper()
	// bd's own environment arms must not decide the root under test.
	t.Setenv(proxyendpoint.RootPathEnv, "")
	t.Setenv(proxyendpoint.DoltDataDirEnv, "")
	t.Setenv(proxyendpoint.SharedServerModeEnv, "")
	t.Setenv(proxyendpoint.SharedServerDirEnv, "")

	scopeRoot := t.TempDir()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	root := filepath.Join(beadsDir, proxyendpoint.DefaultRootDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"),
		[]byte(`{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"beads"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.SidecarPath(beadsDir),
		[]byte(`{"root_path":"dolt","port":44561,"idle_timeout":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	f := &proxiedScopeFixture{t: t, scopeRoot: scopeRoot, root: root, alive: true}
	f.argv = []string{"/opt/beads/bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, root}
	f.writeRecord(7001, "11223344")
	return f
}

func (f *proxiedScopeFixture) writeRecord(pid int, start string) {
	f.t.Helper()
	rootID, err := proxyendpoint.RootID(f.root)
	if err != nil {
		f.t.Fatalf("RootID(%s): %v", f.root, err)
	}
	f.record = proxyendpoint.Record{
		PID:         pid,
		Port:        44561,
		UpstreamID:  "upstream",
		Schema:      proxyendpoint.SchemaV2,
		Kind:        proxyendpoint.RecordKind,
		Birth:       proxyendpoint.BirthToken("boot-fixture", start),
		RootID:      rootID,
		ControlPort: 44562,
	}
	body, err := json.Marshal(f.record)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(f.root), body, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *proxiedScopeFixture) removeRecord() {
	f.t.Helper()
	if err := os.Remove(proxyendpoint.PIDPath(f.root)); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
}

func (f *proxiedScopeFixture) processTable() proxyendpoint.ProcessTable {
	return proxyendpoint.ProcessTable{
		Alive: func(pid int) bool { return f.alive && pid == f.record.PID },
		Argv: func(pid int) ([]string, error) {
			if !f.alive || pid != f.record.PID {
				return nil, errors.New("no such process")
			}
			return f.argv, nil
		},
		Birth: func(pid int) (string, error) {
			if !f.alive || pid != f.record.PID {
				return "", errors.New("no such process")
			}
			return f.record.Birth, nil
		},
	}
}

func (f *proxiedScopeFixture) pinnedCursors() proxyendpoint.Cursors {
	main, ignored := beads.PinnedSchemaCursors()
	return proxyendpoint.Cursors{Main: main, Ignored: ignored}
}

// admit runs a real admission pass against the fixture, which is the only way to
// obtain a beads.Pin outside internal/beads: Pin's fields are unexported and only
// beads.Admit mints an admitted one. That fence is the reason this helper exists
// rather than a hand-built struct.
func (f *proxiedScopeFixture) admit(t *testing.T, longLived bool) beads.Pin {
	t.Helper()
	pin, err := beads.Admit(context.Background(), beads.AdmissionInput{
		ScopeRoot:    f.scopeRoot,
		Database:     "beads",
		ProcessTable: f.processTable(),
		Probe: func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: f.pinnedCursors()}
		},
		LongLived: longLived,
		Observed:  beads.NewGenerationSet(),
		Recovered: beads.NewGenerationSet(),
		Sleep:     func(context.Context, time.Duration) error { return nil },
		SkipMemo:  true,
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return pin
}

// TestProxiedOpenEnvForPinGolden is the whole environment the linked library is
// opened with over bd's proxy, asserted as an exact map.
//
// Exact, not "contains": the window this map is projected through UNSETS every
// listed key the map omits, so a key that quietly appeared here would be a new
// decision gc made about somebody else's database, and a key that quietly
// vanished would be a decision silently handed back to the ambient shell.
//
// Two absences are load-bearing enough to assert by name:
//
//   - BEADS_DOLT_PROXIED_SERVER. beads v1.3.0 reads it only in cmd/bd
//     (init.go:450, :2938, :3501) and nowhere under the library storage path, so
//     scrubbing it cannot change library behavior today. It is belt and braces
//     for the day a future bd moves that read (plan Q3).
//   - every GC_DOLT_* key. Those configure gc's MANAGED Dolt server, which does
//     not exist on a proxied scope; projecting one would point the library at a
//     server bd does not own.
func TestProxiedOpenEnvForPinGolden(t *testing.T) {
	f := newProxiedScopeFixture(t)

	for _, tc := range []struct {
		name      string
		longLived bool
		maxConns  string
	}{
		{"one-shot", false, "1"},
		{"long-lived", true, "4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin := f.admit(t, tc.longLived)
			env := nativeDoltProxiedOpenEnvForPin("demo", pin, tc.longLived)

			want := map[string]string{
				"BEADS_DOLT_SERVER_MODE":     "1",
				"BEADS_DOLT_SERVER_HOST":     "127.0.0.1",
				"BEADS_DOLT_SERVER_PORT":     "44561",
				"BEADS_DOLT_SERVER_USER":     "root",
				"BEADS_DOLT_SERVER_DATABASE": "beads",
				"BEADS_DOLT_AUTO_START":      "0",
				"BEADS_DOLT_MAX_CONNS":       tc.maxConns,
				"GIT_AUTHOR_NAME":            "gc",
				"GIT_AUTHOR_EMAIL":           "gc@demo",
			}
			if !reflect.DeepEqual(env, want) {
				t.Fatalf("proxied open env =\n%s\nwant\n%s", renderEnv(env), renderEnv(want))
			}
			for _, forbidden := range []string{"BEADS_DOLT_PROXIED_SERVER", "GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_DATA_DIR"} {
				if _, present := env[forbidden]; present {
					t.Errorf("the proxied open env names %s; it must decide nothing about a server bd owns", forbidden)
				}
			}
		})
	}

	// The author anchor is the city NAME (decision Q4). A city with none, or one
	// whose "name" is really a path — which is what citylayout's GC_CITY anchor
	// actually holds, the trap this arm exists to keep closed — falls back rather
	// than writing a host path into a Dolt commit that anybody can read.
	t.Run("an unusable city name falls back rather than leaking a path", func(t *testing.T) {
		pin := f.admit(t, false)
		for _, name := range []string{"", "   ", "/home/op/cities/demo", "demo city"} {
			env := nativeDoltProxiedOpenEnvForPin(name, pin, false)
			if got := env["GIT_AUTHOR_EMAIL"]; got != "gc@unknown-city" {
				t.Errorf("GIT_AUTHOR_EMAIL for city name %q = %q, want gc@unknown-city", name, got)
			}
		}
		if got := proxiedNativeAuthorCityName(&config.City{Workspace: config.Workspace{Name: "demo"}}); got != "demo" {
			t.Fatalf("proxiedNativeAuthorCityName = %q, want the configured workspace name", got)
		}
	})
}

func renderEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "  %s=%s\n", key, env[key])
	}
	return b.String()
}

// scriptedProviderOps records every provider verb the ladder spent, in order.
type scriptedProviderOps struct {
	mu   sync.Mutex
	ops  []string
	fail map[string]error
}

func (s *scriptedProviderOps) run(op string) error {
	s.mu.Lock()
	s.ops = append(s.ops, op)
	err := s.fail[op]
	s.mu.Unlock()
	return err
}

func (s *scriptedProviderOps) spent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

// TestProxiedReopenEscalationLadder is the bd-verb budget of the whole lane.
//
// The lane's reason to exist is that a healthy proxied scope costs ZERO forks,
// and its safety argument is that an unhealthy one costs a BOUNDED number: at
// most one probe and one recover per proxy generation, and never a spawn. Both
// halves are asserted here on the sequence of verbs, not on the outcome — a
// ladder that reached the right verdict by forking bd three times would pass
// every verdict-only assertion while destroying the property.
func TestProxiedReopenEscalationLadder(t *testing.T) {
	newOpener := func(t *testing.T, f *proxiedScopeFixture, ops *scriptedProviderOps, probe func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult) *proxiedNativeOpener {
		t.Helper()
		observed := beads.NewGenerationSet()
		restore := providerOwnedScopeLifecycleOp
		providerOwnedScopeLifecycleOp = func(_ context.Context, _, _, op string) error { return ops.run(op) }
		t.Cleanup(func() { providerOwnedScopeLifecycleOp = restore })
		return &proxiedNativeOpener{
			cityPath:     t.TempDir(),
			scopeRoot:    f.scopeRoot,
			database:     "beads",
			ops:          proxiedProviderOps{cityPath: t.TempDir(), observed: observed},
			processTable: f.processTable(),
			probe:        probe,
			observed:     observed,
			recovered:    beads.NewGenerationSet(),
			sleep:        func(context.Context, time.Duration) error { return nil },
		}
	}
	servedProbe := func(f *proxiedScopeFixture) func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		return func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: f.pinnedCursors()}
		}
	}

	t.Run("a healthy proxy spends nothing", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		pin, err := newOpener(t, f, ops, servedProbe(f)).admit(context.Background(), true)
		if err != nil {
			t.Fatalf("admit: %v", err)
		}
		if !pin.Admitted() {
			t.Fatal("admit returned an unadmitted pin")
		}
		if spent := ops.spent(); len(spent) != 0 {
			t.Fatalf("a healthy proxy cost %v; the lane exists because it costs nothing", spent)
		}
	})

	t.Run("a stopped proxy costs exactly one probe", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		opener := newOpener(t, f, ops, servedProbe(f))
		// bd removed the record on an orderly stop. The verb brings it back.
		f.removeRecord()
		restore := providerOwnedScopeLifecycleOp
		providerOwnedScopeLifecycleOp = func(_ context.Context, _, _, op string) error {
			err := ops.run(op)
			f.writeRecord(7002, "55667788")
			return err
		}
		t.Cleanup(func() { providerOwnedScopeLifecycleOp = restore })

		pin, err := opener.admit(context.Background(), true)
		if err != nil {
			t.Fatalf("admit after a provider probe: %v", err)
		}
		if pin.PoolKey().PID != 7002 {
			t.Fatalf("admitted pid %d, want the proxy the probe produced (7002)", pin.PoolKey().PID)
		}
		if spent := strings.Join(ops.spent(), ","); spent != proxiedProviderProbeOp {
			t.Fatalf("verbs spent = [%s], want exactly [probe]", spent)
		}
	})

	t.Run("a zombie costs one probe then one recover, and then stops", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		// The endpoint accepts and never greets, forever: the worst case, where
		// every rung is spent and the ladder must still terminate.
		silent := func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeAcceptedNoGreeting, Err: errors.New("no greeting")}
		}
		opener := newOpener(t, f, ops, silent)

		_, err := opener.admit(context.Background(), true)
		verdict, typed := beads.ProxiedVerdictOf(err)
		if !typed || verdict.Verdict != beads.ProxiedVerdictProxyZombie {
			t.Fatalf("admit = %v, want the proxy_zombie verdict", err)
		}
		if !verdict.Terminal() {
			t.Error("a zombie that survived the whole ladder must be terminal; gc has asked bd everything it may ask")
		}
		if spent := strings.Join(ops.spent(), ","); spent != proxiedProviderProbeOp+","+proxiedProviderRecoverOp {
			t.Fatalf("verbs spent = [%s], want exactly [probe recover]", spent)
		}

		// A second open in the same process against the SAME generation spends
		// nothing more: the rungs are per generation, ever.
		before := len(ops.spent())
		if _, err := opener.admit(context.Background(), true); err == nil {
			t.Fatal("the second admission of a zombie succeeded")
		}
		if after := len(ops.spent()); after != before {
			t.Fatalf("the second admission spent %d more verb(s) on the same generation: %v", after-before, ops.spent())
		}
	})

	t.Run("a one-shot never waits out a drain", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		refused := func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
			return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeRefused, Err: errors.New("connection refused")}
		}
		opener := newOpener(t, f, ops, refused)

		start := time.Now()
		_, err := opener.admit(context.Background(), false)
		verdict, typed := beads.ProxiedVerdictOf(err)
		if !typed || verdict.Verdict != beads.ProxiedVerdictDraining {
			t.Fatalf("admit = %v, want the draining verdict", err)
		}
		if verdict.Terminal() {
			t.Error("draining must be non-terminal: the next open re-admits against whatever bd produced")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("a one-shot waited %s for a draining proxy; it should take the bd front door immediately", elapsed)
		}
		if spent := ops.spent(); len(spent) != 0 {
			t.Fatalf("a drain cost %v; a proxy shutting down is not something to ask bd about", spent)
		}
	})

	t.Run("the readiness memo skips a second probe on a generation bd just produced", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		observed := beads.NewGenerationSet()
		restore := providerOwnedScopeLifecycleOp
		providerOwnedScopeLifecycleOp = func(_ context.Context, _, _, op string) error { return ops.run(op) }
		t.Cleanup(func() { providerOwnedScopeLifecycleOp = restore })

		provider := proxiedProviderOps{cityPath: t.TempDir(), observed: observed}
		if err := provider.Ping(context.Background(), f.scopeRoot); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		generation := proxyendpoint.NewPoolKey(f.record, "").Generation()
		if !observed.Has(generation) {
			t.Fatalf("the readiness memo did not record generation %s after a successful probe", generation)
		}
		// A later open that finds THAT generation silent goes straight to the
		// recover rung instead of re-asking for a ping gc itself produced.
		if observed.Add(generation) {
			t.Fatal("the memo let a second ping be spent on a generation gc had just asked bd for")
		}
	})
}

// TestProxiedGuardRecoveryAdmitsWithoutForkingBd is council A-F1's production
// half: the guard tick now has a recovery path for a non-terminally demoted
// handle, and that path must not break the tick's "never forks bd" invariant.
//
// The invariant is stated in proxied_guard_tick.go's header ("It holds NO
// ProviderOps: a tick never forks bd. An escalation rung is the read path's to
// spend, through the reopen hook, where a caller is waiting for an answer and
// the cost is attributable"). The recovery runs on the tick's goroutine, so it
// admits with nil Ops. The control is the same scope admitted through the
// ordinary path, which DOES spend the probe — without it this test would pass
// against a fixture that simply had nothing to escalate.
func TestProxiedGuardRecoveryAdmitsWithoutForkingBd(t *testing.T) {
	newOpener := func(t *testing.T, f *proxiedScopeFixture, ops *scriptedProviderOps) *proxiedNativeOpener {
		t.Helper()
		observed := beads.NewGenerationSet()
		restore := providerOwnedScopeLifecycleOp
		providerOwnedScopeLifecycleOp = func(_ context.Context, _, _, op string) error {
			err := ops.run(op)
			f.writeRecord(7104, "1a2b3c4d")
			return err
		}
		t.Cleanup(func() { providerOwnedScopeLifecycleOp = restore })
		return &proxiedNativeOpener{
			cityPath:     t.TempDir(),
			scopeRoot:    f.scopeRoot,
			database:     "beads",
			ops:          proxiedProviderOps{cityPath: t.TempDir(), observed: observed},
			processTable: f.processTable(),
			probe: func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
				return proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed, Cursors: f.pinnedCursors()}
			},
			observed:  observed,
			recovered: beads.NewGenerationSet(),
			sleep:     func(context.Context, time.Duration) error { return nil },
			openNative: func(context.Context, string, map[string]string, ...beads.NativeDoltStoreOption) (*beads.NativeDoltStore, error) {
				return nil, errors.New("this test never reaches the library open")
			},
		}
	}

	t.Run("the ordinary admission path spends the probe", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		opener := newOpener(t, f, ops)
		f.removeRecord()

		if _, err := opener.admit(context.Background(), true); err != nil {
			t.Fatalf("admit: %v", err)
		}
		if spent := strings.Join(ops.spent(), ","); spent != proxiedProviderProbeOp {
			t.Fatalf("verbs spent = [%s], want exactly [probe]: without this control the assertion below is vacuous", spent)
		}
	})

	t.Run("the guard recovery spends nothing", func(t *testing.T) {
		f := newProxiedScopeFixture(t)
		ops := &scriptedProviderOps{}
		opener := newOpener(t, f, ops)
		f.removeRecord()

		_, _, err := opener.recoverNativeLeaf()(context.Background())
		if err == nil {
			t.Fatal("the recovery admitted a scope whose proxy record is gone")
		}
		verdict, typed := beads.ProxiedVerdictOf(err)
		if !typed {
			t.Fatalf("the recovery returned an untyped error the tick cannot classify: %v", err)
		}
		if verdict.Terminal() {
			t.Errorf("a stopped proxy is not a fact about the database, so the refusal must be non-terminal: %v", verdict)
		}
		if spent := ops.spent(); len(spent) != 0 {
			t.Fatalf("the guard recovery forked bd for %v; a tick never forks bd, and the rung belongs to the read path", spent)
		}
	})
}

// TestProxiedNativeOpenerIsWiredAtEveryCompositionRoot is the wiring assertion.
//
// Every earlier group built machinery that nothing calls; this is the commit
// where a proxied city can actually take the native lane, and the only thing
// standing between "the code exists" and "the city uses it" is whether each
// composition root passes the opener and the long-lived shape. Neither can be
// proven by opening a real store in a unit test (that needs Dolt), so both are
// proven at the factory boundary.
func TestProxiedNativeOpenerIsWiredAtEveryCompositionRoot(t *testing.T) {
	t.Run("the CLI and controller city store", func(t *testing.T) {
		cityDir := t.TempDir()
		writeMinimalCityToml(t, cityDir)

		var captured beads.StoreOpenOptions
		restore := openStoreFactoryForCity
		openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
			captured = opts
			return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
		}
		t.Cleanup(func() { openStoreFactoryForCity = restore })

		for _, longLived := range []bool{false, true} {
			if _, err := openStoreResultAtForCityWithConfig(
				cityDir, cityDir, &config.City{}, gate.ModeUnset, false, false, longLived); err != nil {
				t.Fatalf("openStoreResultAtForCityWithConfig(longLived=%v): %v", longLived, err)
			}
			if captured.LongLived != longLived {
				t.Errorf("LongLived = %v, want %v; the idle rule and the pool shape both turn on it",
					captured.LongLived, longLived)
			}
			if captured.OpenProxiedStore == nil {
				t.Fatal("the city store open passes no proxied opener; the lane can never be reached")
			}
			if captured.OpenBdStore == nil {
				t.Fatal("the city store open passes no bd opener")
			}
		}
	})

	t.Run("the controller rig store", func(t *testing.T) {
		cityDir := t.TempDir()
		writeMinimalCityToml(t, cityDir)
		rigDir := filepath.Join(cityDir, "rigs", "repo")
		if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}

		var captured beads.StoreOpenOptions
		restore := controllerStateOpenRigStoreAtForCity
		controllerStateOpenRigStoreAtForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
			captured = opts
			return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
		}
		t.Cleanup(func() { controllerStateOpenRigStoreAtForCity = restore })

		cs := &controllerState{cityPath: cityDir}
		cs.openRigStore("bd", "repo", rigDir, "rp", &config.City{})
		if !captured.LongLived {
			t.Error("the controller rig store is not marked long-lived; it is held for the process lifetime")
		}
		if captured.OpenProxiedStore == nil {
			t.Fatal("the rig store open passes no proxied opener; a migrated rig can never take the lane")
		}
	})

	t.Run("a scope with no canonical database refuses with a typed verdict", func(t *testing.T) {
		// A scope the contract cannot resolve AT ALL — the error return, not a
		// missing dolt_database key — declines, and declines VISIBLY: a nil
		// opener would make "no database" indistinguishable from "the lane was
		// never wired here", and the cursors admission gates on are
		// DATABASE()-scoped, so a probe with none selected would read zeros and
		// call them evidence. The missing-KEY case is a different answer and is
		// pinned in TestProxiedScopeDatabaseResolvesTheBdShapedProxiedCity.
		ops := &scriptedProviderOps{}
		restore := providerOwnedScopeLifecycleOp
		providerOwnedScopeLifecycleOp = func(_ context.Context, _, _, op string) error { return ops.run(op) }
		t.Cleanup(func() { providerOwnedScopeLifecycleOp = restore })

		opener := proxiedNativeStoreOpenerForScope(t.TempDir(), t.TempDir(), &config.City{}, func() (beads.Store, error) {
			t.Fatal("the bd write leaf was opened for a scope that has no database")
			return nil, nil
		})
		if opener == nil {
			t.Fatal("no opener was built at all; the factory would never even consult the lane")
		}
		_, _, err := opener(context.Background(), false)
		if verdict, typed := beads.ProxiedVerdictOf(err); !typed || verdict.Verdict != beads.ProxiedVerdictNoOwnershipRecord {
			t.Fatalf("open = %v, want the no_ownership_record verdict", err)
		}
		if spent := ops.spent(); len(spent) != 0 {
			t.Fatalf("an unresolvable scope cost %v bd verb(s)", spent)
		}
	})
}

// TestProxiedScopeDatabaseResolvesTheBdShapedProxiedCity pins the database
// resolution against the on-disk shape a real `gc init` leaves behind.
//
// This is the defect the acceptance fork gate found on its first run, and it is
// worth a named test rather than a one-line change, because the mechanism is
// counter-intuitive: the lane's own resolution asked for an AUTHORITATIVE scope
// config, and the one shape it exists to serve — a gc-initialized proxied city —
// deliberately has none. gc retires its canonical Dolt config for a scope bd
// owns, so what is left is bd's `issue_prefix:`-only config.yaml, which the
// contract classifies ScopeConfigLegacyMinimal. Every proxied open on such a
// city therefore resolved an empty database and refused with
// no_ownership_record, with the flag on, silently.
//
// The negative half is asserted too. Without it the test would pass just as
// happily against the old resolution if some later change made proxied cities
// authoritative again, and the reason this function does not use
// canonicalScopeDoltTarget would quietly stop being true.
func TestProxiedScopeDatabaseResolvesTheBdShapedProxiedCity(t *testing.T) {
	f := newProxiedScopeFixture(t)
	// bd's own config.yaml, as `bd init --proxied-server` writes it and as gc
	// leaves it on a scope bd owns: the issue prefix and nothing else. No
	// gc.endpoint_origin, no dolt.mode, no host or port.
	if err := os.WriteFile(filepath.Join(f.scopeRoot, ".beads", "config.yaml"),
		[]byte("issue_prefix: hq\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := proxiedScopeDatabase(f.scopeRoot, f.scopeRoot); got != "beads" {
		t.Fatalf("proxiedScopeDatabase = %q, want %q — admission refuses an empty database with a terminal verdict, so an unresolved name turns the whole lane off on the shape it exists for",
			got, "beads")
	}

	if _, ok, err := canonicalScopeDoltTarget(f.scopeRoot, f.scopeRoot); err != nil || ok {
		t.Fatalf("canonicalScopeDoltTarget(bd-shaped proxied city) = ok %v, err %v; want ok=false — if this starts answering, the comment on proxiedScopeDatabase needs rewriting, not deleting",
			ok, err)
	}

	// Council C-F13. The missing-KEY case is a GUESS, not a decline:
	// ResolveDoltConnectionTarget initializes Database: "beads" unconditionally
	// and overwrites it only when metadata.json names one. The old doc said the
	// lane "declines rather than guessing", which is true only of the total
	// resolution failure below. The guess is fenced — a wrong database fails
	// the probe, and a shared proxy root serving a differently-prefixed
	// database is caught by proxiedPrefixAgreement — so this pins the behavior
	// rather than changing it, and pins it where a reader looking for the
	// decline will find it.
	t.Run("metadata with no dolt_database yields beads' own default", func(t *testing.T) {
		metadata := filepath.Join(f.scopeRoot, ".beads", "metadata.json")
		if err := os.WriteFile(metadata, []byte(`{"backend":"dolt","dolt_mode":"proxied-server"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := proxiedScopeDatabase(f.scopeRoot, f.scopeRoot); got != "beads" {
			t.Fatalf("proxiedScopeDatabase with no dolt_database = %q, want %q: the contract defaults "+
				"the name rather than declining, and the doc must say which it does", got, "beads")
		}
	})

	// And the lane still declines for a scope the contract cannot resolve at
	// all, rather than inventing beads' default database name for it.
	empty := t.TempDir()
	if got := proxiedScopeDatabase(empty, empty); got != "" {
		t.Errorf("proxiedScopeDatabase(a directory with no beads scope) = %q, want \"\"", got)
	}
}

// TestProxiedOpenArmsTheGuardForALongLivedStore is the cmd/gc half of council
// C-F8.
//
// `if longLived { store.StartGuard() }` is the sole production call site, and
// deleting it leaves every controller store holding a native leaf with no
// generation or cursor watch for the process lifetime — the moved-root hazard
// proxied_guard_tick.go's header says no read can detect. Nothing failed:
// TestProxiedNativeOpenerIsWiredAtEveryCompositionRoot asserts only that
// OpenProxiedStore is non-nil, and P2-15 states the tick is undrivable from a
// one-shot CLI.
//
// Driving open() to that line needs a real library open, i.e. Dolt, so the
// behavioral proof is the acceptance lifecycle row (now wired into CI by the
// C-F1 fix) and beads' own TestStartGuardArmsARealTicker, which drives the
// exported entry point with a real ticker. What is left for a unit test is the
// WIRING, and this asserts it on the source: the long-lived arm arms the guard,
// and the one-shot arm does not.
//
// A source assertion is the weaker instrument and it is chosen deliberately
// over adding an exported accessor to ProxiedStore purely so a test could see
// the ticker — the finding is about a line that can be deleted unnoticed, and
// this notices.
func TestProxiedOpenArmsTheGuardForALongLivedStore(t *testing.T) {
	body, err := os.ReadFile("beads_proxied_native.go")
	if err != nil {
		t.Fatalf("read beads_proxied_native.go: %v", err)
	}
	source := string(body)

	openBody, ok := functionBody(source, "func (o *proxiedNativeOpener) open(")
	if !ok {
		t.Fatal("proxiedNativeOpener.open not found; this guard cannot see what it is guarding")
	}
	if !strings.Contains(openBody, "store.StartGuard()") {
		t.Fatal("proxiedNativeOpener.open no longer calls store.StartGuard(): every long-lived store " +
			"would hold a native leaf with no generation or cursor watch for the process lifetime, " +
			"including the moved-root hazard no read can detect")
	}
	guardLine := strings.Index(openBody, "store.StartGuard()")
	longLivedArm := strings.LastIndex(openBody[:guardLine], "if longLived {")
	if longLivedArm < 0 {
		t.Fatal("store.StartGuard() is no longer inside an `if longLived` arm: a one-shot store lives " +
			"for 40ms, and a ticker plus a probe session on it is bought for nothing")
	}
}

// functionBody returns the body of the first function whose declaration starts
// with prefix, by brace balance from the opening brace of the declaration.
func functionBody(source, prefix string) (string, bool) {
	start := strings.Index(source, prefix)
	if start < 0 {
		return "", false
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		return "", false
	}
	depth := 0
	for i := start + open; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start+open : i], true
			}
		}
	}
	return "", false
}
