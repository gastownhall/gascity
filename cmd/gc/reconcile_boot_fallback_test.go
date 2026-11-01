package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
)

// stubV2BootProbe replaces the probe with one that returns refusal and counts
// its calls.
func stubV2BootProbe(t *testing.T, refusal error) *int {
	t.Helper()
	calls := new(int)
	prev := v2DefaultBootProbe
	t.Cleanup(func() { v2DefaultBootProbe = prev })
	v2DefaultBootProbe = func(string, string, *config.City, events.Recorder) error {
		*calls++
		return refusal
	}
	return calls
}

func fallbackCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return city
}

var errCensusRefusal = errors.New("enterprise-era session rows: 2 open row(s) in a state main does not know")

// TestDefaultV2FallsBackToLegacyOnABootRefusal is O6: an unset
// session_reconciler resolves to v2, the v2 boot would refuse (C11 here),
// and the city runs legacy instead of not starting, loudly and with a
// record doctor reports.
func TestDefaultV2FallsBackToLegacyOnABootRefusal(t *testing.T) {
	city := fallbackCity(t)
	cfg := v2DefaultCity(nil)
	calls := stubV2BootProbe(t, errCensusRefusal)
	w, err := newControllerWiringLoggingTo(cfg, envOf(nil), io.Discard, io.Discard)
	if err != nil || w.mode != reconcilerV2 || w.reason != reconcilerDefaultV2 {
		t.Fatalf("fixture wiring = (%+v, %v), want the v2 default", w, err)
	}

	var log bytes.Buffer
	refusal := applyV2DefaultBootFallback(w, city, "fb-city", cfg, nil, &log)
	if *calls != 1 || !errors.Is(refusal, errCensusRefusal) {
		t.Fatalf("probe calls=%d refusal=%v, want one probe and its refusal", *calls, refusal)
	}
	if w.mode != reconcilerLegacy || w.v2 != nil {
		t.Fatalf("after the fallback the wiring is mode=%v v2=%v, want legacy with no planner", w.mode, w.v2)
	}
	if reply, ok := reconcilerModeReplyFor(w.wake); !ok || reply.SessionReconciler != config.SessionReconcilerLegacy {
		t.Errorf("the controller socket would report %+v (ok=%v), want legacy", reply, ok)
	}
	if err := checkReconcilerWiring(w.runtimeParams(CityRuntimeParams{})); err != nil {
		t.Errorf("the demoted wiring is not a valid legacy wiring: %v", err)
	}
	if !strings.Contains(log.String(), "FALLING BACK TO LEGACY") || !strings.Contains(log.String(), errCensusRefusal.Error()) {
		t.Errorf("boot log %q is not a loud warning naming the refusal", log.String())
	}
	rec, ok := readReconcilerV2FallbackRecord(city)
	if !ok || !strings.Contains(rec.Refusal, "enterprise-era") {
		t.Fatalf("fallback record = (%+v, %v), want the refusal recorded for doctor", rec, ok)
	}
}

// TestExplicitV2IsNeverProbedAndStillRefuses: an operator who asked for v2
// gets v2, and its boot refuses as before.
func TestExplicitV2IsNeverProbedAndStillRefuses(t *testing.T) {
	city := fallbackCity(t)
	cfg := v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = config.SessionReconcilerV2 })
	calls := stubV2BootProbe(t, errCensusRefusal)
	w, err := newControllerWiringLoggingTo(cfg, envOf(nil), io.Discard, io.Discard)
	if err != nil || w.mode != reconcilerV2 || w.reason != reconcilerExplicit {
		t.Fatalf("fixture wiring = (%+v, %v), want an explicit v2", w, err)
	}
	if refusal := applyV2DefaultBootFallback(w, city, "fb-city", cfg, nil, io.Discard); refusal != nil || *calls != 0 {
		t.Fatalf("explicit v2: refusal=%v probe calls=%d, want no probe", refusal, *calls)
	}
	if w.mode != reconcilerV2 || w.v2 == nil {
		t.Fatal("an explicit v2 was demoted")
	}
	if _, ok := readReconcilerV2FallbackRecord(city); ok {
		t.Error("an explicit v2 wrote a fallback record")
	}
}

// TestLegacyIsNeverProbed: nothing to fall back from.
func TestLegacyIsNeverProbed(t *testing.T) {
	cfg := v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = config.SessionReconcilerLegacy })
	calls := stubV2BootProbe(t, errCensusRefusal)
	w, err := newControllerWiringLoggingTo(cfg, envOf(nil), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_ = applyV2DefaultBootFallback(w, fallbackCity(t), "fb-city", cfg, nil, io.Discard)
	if *calls != 0 || w.mode != reconcilerLegacy {
		t.Fatalf("legacy: probe calls=%d mode=%v", *calls, w.mode)
	}
}

// TestDefaultV2ThatBootsClearsAStaleFallbackRecord: once the refusal is
// repaired the next boot runs v2 and doctor stops reporting the fallback.
func TestDefaultV2ThatBootsClearsAStaleFallbackRecord(t *testing.T) {
	city := fallbackCity(t)
	cfg := v2DefaultCity(nil)
	if err := os.WriteFile(reconcilerV2FallbackRecordPath(city), []byte(`{"refusal":"old","at":"2026-10-01T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	stubV2BootProbe(t, nil)
	w, err := newControllerWiringLoggingTo(cfg, envOf(nil), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if refusal := applyV2DefaultBootFallback(w, city, "fb-city", cfg, nil, io.Discard); refusal != nil || w.mode != reconcilerV2 || w.v2 == nil {
		t.Fatalf("a clean default v2: refusal=%v mode=%v", refusal, w.mode)
	}
	if _, ok := readReconcilerV2FallbackRecord(city); ok {
		t.Error("a default v2 that boots left the stale fallback record behind")
	}
}

// TestDoctorReportsTheV2Fallback is the doctor finding.
func TestDoctorReportsTheV2Fallback(t *testing.T) {
	city := fallbackCity(t)
	cfg := v2DefaultCity(nil)
	check := newSessionReconcilerDoctorCheck(cfg, envOf(nil))
	check.queryPass = nil
	if r := check.Run(&doctor.CheckContext{CityPath: city}); r.Status != doctor.StatusOK {
		t.Fatalf("without a fallback record: %v %q, want OK", r.Status, r.Message)
	}
	if err := os.WriteFile(reconcilerV2FallbackRecordPath(city), []byte(`{"refusal":"enterprise-era session rows","at":"2026-10-29T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := check.Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "FELL BACK TO LEGACY") || !strings.Contains(r.Message, "enterprise-era") {
		t.Fatalf("with a fallback record: %v %q, want a warning naming the refusal", r.Status, r.Message)
	}
	explicit := newSessionReconcilerDoctorCheck(v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = config.SessionReconcilerLegacy }), envOf(nil))
	explicit.queryPass = nil
	if r := explicit.Run(&doctor.CheckContext{CityPath: city}); strings.Contains(r.Message, "FELL BACK") {
		t.Errorf("an explicit legacy city reported a v2 fallback: %q", r.Message)
	}
}

// stubV2ProbeStores points the real probe at an in-memory work store and
// fixes its C0.7 verdict.
func stubV2ProbeStores(t *testing.T, work beads.Store, capabilities error) {
	t.Helper()
	prevOpen, prevVerdict := openInfraMigrationSource, v2BootProbeVerdict
	t.Cleanup(func() { openInfraMigrationSource, v2BootProbeVerdict = prevOpen, prevVerdict })
	openInfraMigrationSource = func(string) (beads.Store, error) { return work, nil }
	v2BootProbeVerdict = func([]v2StoreCapability) error { return capabilities }
}

// TestV2DefaultBootRefusalReadsTheSameChecksTheBootDoes drives the real
// probe: a C0.7 refusal and a C11 refusal are verdicts; an unread C0.7, a
// clean census and a store that will not open are not.
func TestV2DefaultBootRefusalReadsTheSameChecksTheBootDoes(t *testing.T) {
	cfg := chatCity("always")
	cfg.Agents = append(cfg.Agents, allocPoolAgent("worker", 3))
	enterprise := func() beads.Store {
		return censusStore(poolRow("gc-1", "worker", 1, "archived"), poolRow("gc-2", "worker", 2, "archived"))
	}
	clean := func() beads.Store { return censusStore(poolRow("gc-1", "worker", 1, "active")) }
	capRefusal := errors.New("C0.7: the sessions store cannot fence")

	t.Run("capability refusal", func(t *testing.T) {
		stubV2ProbeStores(t, clean(), capRefusal)
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); !errors.Is(got, capRefusal) {
			t.Fatalf("probe = %v, want the C0.7 refusal", got)
		}
	})
	t.Run("census refusal", func(t *testing.T) {
		stubV2ProbeStores(t, enterprise(), nil)
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); got == nil || !strings.Contains(got.Error(), "enterprise-era") {
			t.Fatalf("probe = %v, want the C11 refusal", got)
		}
	})
	t.Run("unread capability still censuses", func(t *testing.T) {
		stubV2ProbeStores(t, enterprise(), &v2CapabilityUnreadError{Class: "sessions", Err: errors.New("timeout")})
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); got == nil || !strings.Contains(got.Error(), "enterprise-era") {
			t.Fatalf("probe = %v, want the census refusal after an unread C0.7", got)
		}
	})
	t.Run("clean", func(t *testing.T) {
		stubV2ProbeStores(t, clean(), nil)
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); got != nil {
			t.Fatalf("probe over a clean city = %v, want nil", got)
		}
	})
	t.Run("work store will not open", func(t *testing.T) {
		prev := openInfraMigrationSource
		t.Cleanup(func() { openInfraMigrationSource = prev })
		openInfraMigrationSource = func(string) (beads.Store, error) { return nil, errors.New("dolt down") }
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); got != nil {
			t.Fatalf("probe with no store = %v, want no verdict", got)
		}
	})
}

// TestV2BootProbeGivesNoVerdictOnReadFailures (M21b, M21c): a storage boot
// gate that refuses, or a census that cannot read its legs, is no verdict;
// the probe leaves v2 to its own boot, which reports the real failure.
func TestV2BootProbeGivesNoVerdictOnReadFailures(t *testing.T) {
	cfg := chatCity("always")
	cfg.Agents = append(cfg.Agents, allocPoolAgent("worker", 3))
	t.Run("boot gate refuses", func(t *testing.T) {
		stubV2ProbeStores(t, censusStore(poolRow("gc-1", "worker", 1, "archived")), errors.New("C0.7 would refuse"))
		broken := *cfg
		broken.Storage = &config.StorageConfig{Classes: config.StorageClasses{Work: "elsewhere"}}
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", &broken, nil); got != nil {
			t.Fatalf("probe over a refusing boot gate = %v, want no verdict", got)
		}
	})
	t.Run("census read fails", func(t *testing.T) {
		// A partial read: some enterprise-era rows came back with the error.
		// Even rows that would refuse are no verdict while the read failed.
		partial := &beads.PartialResultError{Op: "bd list", Err: errors.New("2 rows unparseable")}
		stubV2ProbeStores(t, censusErrStore{Store: censusStore(poolRow("gc-1", "worker", 1, "archived"), poolRow("gc-2", "worker", 2, "archived")), err: partial}, nil)
		if got := v2DefaultBootRefusal(t.TempDir(), "test-city", cfg, nil); got != nil {
			t.Fatalf("probe over a census that failed to read = %v, want no verdict", got)
		}
	})
}

// TestStandaloneControllerWiringAppliesTheFallback (M27): gc start
// --foreground's latch applies the O6 fallback before its socket.
func TestStandaloneControllerWiringAppliesTheFallback(t *testing.T) {
	city := fallbackCity(t)
	cfg := v2DefaultCity(nil)
	prevEnv := reconcilerModeLookupEnv
	t.Cleanup(func() { reconcilerModeLookupEnv = prevEnv })
	reconcilerModeLookupEnv = envOf(nil)
	calls := stubV2BootProbe(t, errCensusRefusal)
	var stderr bytes.Buffer
	w, err := newStandaloneControllerWiring(city, cfg, nil, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || w.mode != reconcilerLegacy || !w.fellBack || !strings.Contains(stderr.String(), "FALLING BACK TO LEGACY") {
		t.Fatalf("standalone latch: probe calls=%d mode=%v fellBack=%v stderr=%q", *calls, w.mode, w.fellBack, stderr.String())
	}
	if p := w.runtimeParams(CityRuntimeParams{}); !p.ReconcilerV2FellBack || p.ReconcilerMode != reconcilerLegacy {
		t.Errorf("runtime params = mode %v fellBack %v", p.ReconcilerMode, p.ReconcilerV2FellBack)
	}
}

// TestFallbackDriftIsNotAPendingRestartToV2: after a fallback, a reload of the
// same unset key must not claim a restart would run v2 (it would fall back
// again); it says why legacy runs, once.
func TestFallbackDriftIsNotAPendingRestartToV2(t *testing.T) {
	cfg := v2DefaultCity(nil)
	d := reconcilerModeDrift{running: reconcilerLegacy, lookupEnv: envOf(nil), fellBack: true}
	got := d.observe(cfg)
	if strings.Contains(got, "pending restart") || !strings.Contains(got, "boot check refused") {
		t.Fatalf("fallback drift warning = %q", got)
	}
	if again := d.observe(cfg); again != "" {
		t.Errorf("the fallback note repeated: %q", again)
	}
	plain := reconcilerModeDrift{running: reconcilerLegacy, lookupEnv: envOf(nil)}
	if got := plain.observe(cfg); !strings.Contains(got, "pending restart") {
		t.Errorf("a legacy controller that did not fall back lost its pending-restart warning: %q", got)
	}
}

// TestV2BootProbeAndRuntimeAgreeOnC07: the probe reads C0.7 over the plain
// routes the boot gate opens; the runtime reads it over the same routes
// behind its controller cache. They must give the same verdict, or the probe
// would fall back over a city the runtime would boot (or the reverse).
func TestV2BootProbeAndRuntimeAgreeOnC07(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	stubInfraMigrationSource(t)
	routes, err := storageBootGate(root, cfg, "gc v2 boot probe", nil, io.Discard)
	if err != nil {
		t.Fatalf("boot gate: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	work := beads.NewMemStore()
	plain := v2CapabilityVerdict(v2ClassStoreCapabilities(resolveSessionStore(routes, work, cfg, root, nil), resolveGraphStore(routes, work, cfg, root, nil), false))
	cached := routes.withControllerCache(context.Background(), nil)
	runtime := v2CapabilityVerdict(v2ClassStoreCapabilities(resolveSessionStore(cached, work, cfg, root, nil), resolveGraphStore(cached, work, cfg, root, nil), false))
	if (plain == nil) != (runtime == nil) {
		t.Fatalf("probe C0.7 = %v, runtime C0.7 = %v; they must agree", plain, runtime)
	}
}
