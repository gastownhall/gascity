package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// readinessLeaf is a provider that reports v2 readiness as given.
type readinessLeaf struct {
	runtime.Provider
	missing []string
	err     error
}

func (l readinessLeaf) V2Readiness() ([]string, error) { return l.missing, l.err }

// Kills (PAR-EXEC §5 item 7, C0.8) a boot that trusts the latch's probe over
// the provider the city built: a leaf whose own handshake lacks a v2
// capability refuses the boot, naming it; one whose handshake cannot be
// read is no verdict; a ready leaf, or one that reports no readiness
// (tmux), passes; under a composite every leaf is asked.
func TestC08ReadsTheBuiltProvidersHandshake(t *testing.T) {
	fake := runtime.NewFake()
	ready := readinessLeaf{Provider: fake}
	gapped := readinessLeaf{Provider: fake, missing: []string{runtime.ProtocolCapabilityReportAttachment}}
	unread := readinessLeaf{Provider: fake, err: errors.New("protocol: exit status 1")}
	if err := v2RuntimeReadiness(fake); err != nil {
		t.Fatalf("a provider that reports no readiness: %v, want nil", err)
	}
	if err := v2RuntimeReadiness(ready); err != nil {
		t.Fatalf("a ready leaf: %v, want nil", err)
	}
	err := v2RuntimeReadiness(gapped)
	var u *v2RuntimeUnreadError
	if err == nil || errors.As(err, &u) || !strings.Contains(err.Error(), "C0.8") || !strings.Contains(err.Error(), "report-attachment") {
		t.Fatalf("a gapped leaf: %v, want C0.8's refusal naming report-attachment", err)
	}
	if err := v2RuntimeReadiness(unread); !errors.As(err, &u) || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("an unreadable handshake: %v, want an unread error naming the cause", err)
	}
	if err := v2RuntimeReadiness(auto.New(ready, gapped)); err == nil || errors.As(err, &u) {
		t.Fatalf("a composite with a gapped leaf: %v, want the refusal", err)
	}
	// The registry-built provider over a script that dropped report-attachment.
	script := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	if err := v2RuntimeReadiness(sessionexec.NewSeamBackedForCity(script, "c", t.TempDir())); err == nil || errors.As(err, &u) {
		t.Fatalf("the exec provider over a gapped script: %v, want the refusal", err)
	}
}

// Kills C0.8 skipped at boot, a handshake read failure read as a refusal or
// a pass, or a refusal that does not alert: boot asks runtimeReadiness after
// C0.7, retries an unread handshake with the boot backoff (boot state
// "runtime" meanwhile), and refuses with the boot-refused alert on a gap.
func TestBootRunsC08(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stderr := &synchronizedBuffer{}
		rt := newDefaultPlanner(stderr)
		refusal := errors.New("v2 refuses to boot on an exec runtime that is not v2-ready (C0.8): gapped")
		calls, retryState := 0, ""
		rt.host.runtimeReadiness = func() error {
			calls++
			if calls == 1 {
				return &v2RuntimeUnreadError{Label: "the session runtime", Err: errors.New("timeout")}
			}
			retryState = rt.bootState()
			return refusal
		}
		start := time.Now()
		if err := rt.boot(context.Background()); !errors.Is(err, refusal) || calls != 2 {
			t.Fatalf("boot = %v after %d C0.8 reads, want the read retried once and then the refusal", err, calls)
		}
		// The provider caches a failed handshake for HandshakeRetryAfter: a
		// sooner retry would re-read the cached error.
		if waited := time.Since(start); waited < sessionexec.HandshakeRetryAfter || waited > sessionexec.HandshakeRetryAfter*11/10 {
			t.Errorf("boot retried the read after %s, want the handshake retry window (%s)", waited, sessionexec.HandshakeRetryAfter)
		}
		if retryState != plannerBootRuntime || rt.bootState() != plannerBootRuntime {
			t.Errorf("boot state %q while C0.8 retried, %q once it refused; want %q", retryState, rt.bootState(), plannerBootRuntime)
		}
		if !strings.Contains(stderr.String(), "C0.8 runtime handshake failed (retry in") || !strings.Contains(stderr.String(), alertBootRefused) {
			t.Fatalf("stderr %q, want the retry reported and the boot-refused alert", stderr.String())
		}
	})
}

// Kills a default v2 an exec handshake chose in silence (owner decision D6):
// with session_reconciler unset, a v2-ready exec runtime resolves to v2,
// the boot log says the handshake opted the city in, and doctor warns
// until the key is set; an explicit v2, or another runtime, says nothing.
// Doctor's detail names the runtime v2-ready, and warns when v2 runs on it
// without input-fence (D2), or names why it is not v2-ready.
func TestExecOptInIsAnnounced(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	fenced := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment","proc.exec","input-fence"]}'; exit 0; }; exit 2
`)
	gapped := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1"]}'; exit 0; }; exit 2
`)
	city := func(provider, mode string) *config.City {
		return v2DefaultCity(func(c *config.City) { c.Session.Provider, c.Daemon.SessionReconciler = provider, mode })
	}
	cfg := city("exec:"+ready, "")
	res, err := resolveReconcilerMode(cfg, "", nil)
	if err != nil || res.Mode != reconcilerV2 || res.Reason != reconcilerDefaultV2 {
		t.Fatalf("resolution %+v, %v; want the default v2", res, err)
	}
	boot := strings.Join(reconcilerBootWarnings(cfg, nil, res), "\n")
	if !strings.Contains(boot, "v2 by default on the exec session runtime") || !strings.Contains(boot, `session_reconciler = "v2"`) {
		t.Fatalf("boot warnings %q, want the opt-in announced", boot)
	}
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "v2 by default on the exec session runtime") {
		t.Fatalf("doctor %v %q, want a warning naming the opt-in", r.Status, r.Message)
	}
	if !strings.Contains(strings.Join(r.Details, "\n"), "v2-ready (observe.v1, report-attachment)") || !strings.Contains(r.Message, "input-fence") {
		t.Fatalf("doctor details %q message %q, want the runtime v2-ready and the missing input fence", r.Details, r.Message)
	}
	explicit := city("exec:"+fenced, "v2")
	res, _ = resolveReconcilerMode(explicit, "", nil)
	if boot := reconcilerBootWarnings(explicit, nil, res); len(boot) != 0 {
		t.Fatalf("explicit v2 on a fenced exec runtime: boot warnings %q, want none", boot)
	}
	if r := newSessionReconcilerDoctorCheck(explicit, nil).Run(&doctor.CheckContext{}); r.Status != doctor.StatusOK {
		t.Fatalf("explicit v2 on a fenced exec runtime: doctor %v %q, want OK", r.Status, r.Message)
	}
	notReady := city("exec:"+gapped, "")
	r = newSessionReconcilerDoctorCheck(notReady, nil).Run(&doctor.CheckContext{})
	if got := strings.Join(r.Details, "\n"); strings.Count(got, "does not declare report-attachment") != 1 || r.Status != doctor.StatusWarning {
		t.Fatalf("a gapped exec runtime: doctor %v %q %q, want the fallback warning and the gap named once", r.Status, r.Message, r.Details)
	}
}

// countProbes counts probeExecHandshake calls until the test ends. Not
// parallel-safe: it replaces the variable.
func countProbes(t *testing.T) *atomic.Int64 {
	t.Helper()
	saved := probeExecHandshake
	var n atomic.Int64
	probeExecHandshake = func(path, cityName, cityPath string) (runtime.ProtocolInfo, []string, error) {
		n.Add(1)
		return saved(path, cityName, cityPath)
	}
	t.Cleanup(func() { probeExecHandshake = saved })
	return &n
}

// Kills a memo key that leaves out a part of the script's version or the
// city (review S-2, S-8): the same size with a new modification time, the
// same modification time with a new size, a replaced file (new inode, same
// size and time), another city name and another city path each probe again;
// the same everything does not. The probe carries the city's path.
func TestExecAdmissionMemoKeysEveryPart(t *testing.T) {
	probes := countProbes(t)
	body := func(caps string) string {
		return `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":[` + caps + `]}'; exit 0; }; exit 2
`
	}
	script := writeRPPScript(t, body(`"observe.v1"`))
	read := func(name, path string) { execReadiness(script, name, path) }
	read("c", "/city")
	read("c", "/city")
	if probes.Load() != 1 {
		t.Fatalf("%d probes for one version and city, want 1", probes.Load())
	}
	stat := func() os.FileInfo {
		fi, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	fi := stat()
	if err := os.Chtimes(script, fi.ModTime(), fi.ModTime().Add(time.Second)); err != nil { // same size, new mtime
		t.Fatal(err)
	}
	read("c", "/city")
	fi = stat()
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body(`"observe.v1","x"`)), 0o755); err != nil { // new size
		t.Fatal(err)
	}
	if err := os.Chtimes(script, fi.ModTime(), fi.ModTime()); err != nil { // same mtime
		t.Fatal(err)
	}
	read("c", "/city")
	fi = stat()
	data, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script+".new", data, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(script+".new", fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(script+".new", script); err != nil { // new inode, same size and mtime
		t.Fatal(err)
	}
	read("c", "/city")
	read("d", "/city")  // another city name
	read("d", "/other") // another city path
	if got := probes.Load(); got != 6 {
		t.Fatalf("%d probes, want 6: one per version (4) and per city (2)", got)
	}
	// The probe carries the city's path, as the provider it admits runs.
	env := writeRPPScript(t, `[ "$1" = protocol ] && { if [ "$GC_EXEC_CITY_PATH" = /the/city ]; then echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; else echo '{"version":0,"capabilities":[]}'; fi; exit 0; }; exit 2
`)
	if !execReadiness(env, "c", "/the/city").ready() || execReadiness(env, "c", "/elsewhere").ready() {
		t.Fatal("the probe did not carry the city's path")
	}
}

// Kills a hung or failing handshake probed on every latch (review S-3,
// S-8): the probe is bounded by execReadinessProbeTimeout; its error is
// remembered for sessionexec.HandshakeRetryAfter, then probed again; and
// callers that miss while a probe runs wait for its one answer.
func TestExecAdmissionProbeIsBoundedAndShared(t *testing.T) {
	probes := countProbes(t)
	savedTimeout, savedNow := execReadinessProbeTimeout, execReadinessNow
	t.Cleanup(func() { execReadinessProbeTimeout, execReadinessNow = savedTimeout, savedNow })
	execReadinessProbeTimeout = 300 * time.Millisecond
	now := time.Now()
	execReadinessNow = func() time.Time { return now }
	hung := writeRPPScript(t, "[ \"$1\" = protocol ] && exec sleep 30; exit 2\n")
	start := time.Now()
	if h := execReadiness(hung, "c", ""); h.err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("a hung protocol: %+v after %s, want an error within the bound", h, time.Since(start))
	}
	execReadiness(hung, "c", "")
	if probes.Load() != 1 {
		t.Fatalf("%d probes inside the retry window, want 1 (the error remembered)", probes.Load())
	}
	now = now.Add(sessionexec.HandshakeRetryAfter)
	execReadiness(hung, "c", "")
	if probes.Load() != 2 {
		t.Fatalf("%d probes once the window ran out, want 2", probes.Load())
	}

	slow := writeRPPScript(t, "[ \"$1\" = protocol ] && { sleep 0.3; echo '{\"version\":0,\"capabilities\":[]}'; exit 0; }; exit 2\n")
	execReadinessProbeTimeout = 10 * time.Second
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); execReadiness(slow, "c", "") }()
	}
	wg.Wait()
	if probes.Load() != 3 {
		t.Fatalf("%d probes for 8 concurrent callers, want one more (single flight)", probes.Load()-2)
	}
}

// Kills (review S-1) a C0.8 gap that leaves the stale admission answer in
// place: the refusal forgets every remembered handshake, so the next latch
// probes again.
func TestC08GapForgetsTheAdmissionProbe(t *testing.T) {
	probes := countProbes(t)
	script := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	execReadiness(script, "c", "")
	_ = v2RuntimeReadiness(readinessLeaf{Provider: runtime.NewFake(), missing: []string{runtime.ProtocolCapabilityReportAttachment}})
	execReadiness(script, "c", "")
	if probes.Load() != 2 {
		t.Fatalf("%d probes, want the admission probed again after a C0.8 gap", probes.Load())
	}
}

// Kills an unreadable leaf masking a gapped one (review M19): the gap
// refuses, not a retry.
func TestC08GapWinsOverAnUnreadLeaf(t *testing.T) {
	fake := runtime.NewFake()
	err := v2RuntimeReadiness(auto.New(readinessLeaf{Provider: fake, err: errors.New("timeout")}, readinessLeaf{Provider: fake, missing: []string{"observe.v1"}}))
	var u *v2RuntimeUnreadError
	if err == nil || errors.As(err, &u) {
		t.Fatalf("an unread leaf beside a gapped one: %v, want the refusal", err)
	}
}

// Kills a latch that probes a trimmed name the session provider never runs
// (review M20): an exec provider name with surrounding whitespace is
// refused, naming it, since the registry builds the provider from it raw.
func TestExecProviderNameWithWhitespaceIsRefused(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	got := v2LatchRefusals(v2City(func(c *config.City) { c.Session.Provider = "exec:" + ready + " " }), "", nil)
	if len(got) != 1 || !strings.Contains(got[0].String(), "surrounding whitespace") {
		t.Fatalf("refusals %v, want the whitespace refusal", got)
	}
}

// Kills a D2 warning that tests one capability of the two input fencing
// needs (review M8, M9): v2 on an exec runtime warns unless the handshake
// declares both proc.exec and input-fence; legacy never warns.
func TestExecInputFenceWarningNeedsBoth(t *testing.T) {
	for caps, want := range map[string]bool{
		``:                           true,
		`,"input-fence"`:             true,
		`,"proc.exec"`:               true,
		`,"proc.exec","input-fence"`: false,
	} {
		script := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"`+caps+`]}'; exit 0; }; exit 2
`)
		cfg := &config.City{Session: config.SessionConfig{Provider: "exec:" + script}}
		if _, warn := execRuntimeDoctorDetail(cfg, "", nil, reconcilerV2); (warn != "") != want {
			t.Errorf("capabilities %q under v2: warn %q, want %v", caps, warn, want)
		}
		if _, warn := execRuntimeDoctorDetail(cfg, "", nil, reconcilerLegacy); warn != "" {
			t.Errorf("capabilities %q under legacy: warn %q, want none", caps, warn)
		}
	}
}

// Kills a boot that skips C0.8 or runs it out of order (review M5): C0.8
// runs right after C0.7, and its refusal stops boot before the env is
// published or the census runs.
func TestBootRunsC08AfterC07BeforeTheEnv(t *testing.T) {
	var seq []string
	rt := newDefaultPlanner(&synchronizedBuffer{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refusal := errors.New("C0.8: gapped")
	rt.host.capabilities = func() error { seq = append(seq, "C0.7"); return nil }
	rt.host.runtimeReadiness = func() error { seq = append(seq, "C0.8"); return refusal }
	rt.host.snapshotEnv = func() (*config.City, runtime.Provider, string) { seq = append(seq, "env"); return nil, nil, "" }
	rt.host.bootCensus = func() (v2SessionMigration, error) {
		seq = append(seq, "C11")
		cancel()
		return v2SessionMigration{}, errors.New("unreadable")
	}
	if err := rt.boot(ctx); !errors.Is(err, refusal) || strings.Join(seq, ",") != "C0.7,C0.8" {
		t.Fatalf("boot = %v after %v; want the C0.8 refusal after C0.7, no env, no census", err, seq)
	}
}

// Kills (review: C0.8's production wiring) a city runtime whose planner host
// drops C0.8 or reads another provider: booted through the city runtime's own
// host (attachTestV2 binds newPlannerHost), a provider the city built whose
// handshake lacks report-attachment refuses the boot with the boot-refused
// alert, before the env is published or the planner starts, nothing is
// written, the running session is not stopped, and the boot state reads
// runtime.
func TestBootRunsC08OnTheCityRuntimesProvider(t *testing.T) {
	cr, store := newPhaseFixtureRuntime(t, false, true)
	stderr := &lockedBuffer{}
	cr.stderr = stderr
	cr.sp = readinessLeaf{Provider: cr.sp, missing: []string{runtime.ProtocolCapabilityReportAttachment}}
	rt := attachTestV2(t, cr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if cr.bootV2(ctx) {
		t.Fatal("bootV2 reached ready on a provider whose handshake lacks report-attachment")
	}
	if ctx.Err() != nil {
		t.Fatal("bootV2 waited out the deadline instead of refusing at C0.8")
	}
	if got := stderr.String(); !strings.Contains(got, "(C0.8)") || !strings.Contains(got, runtime.ProtocolCapabilityReportAttachment) || !strings.Contains(got, "alert "+alertBootRefused) {
		t.Errorf("stderr = %q, want C0.8's refusal naming %s and the %s alert", got, runtime.ProtocolCapabilityReportAttachment, alertBootRefused)
	}
	if got := rt.bootState(); got != plannerBootRuntime {
		t.Errorf("boot state %q after C0.8 refused, want %q", got, plannerBootRuntime)
	}
	rt.mu.Lock()
	started := rt.started
	rt.mu.Unlock()
	if started || rt.env.Load() != nil {
		t.Errorf("started=%v env=%v: C0.8's refusal published the env or started the planner", started, rt.env.Load())
	}
	for _, op := range store.recorded() {
		if !strings.HasPrefix(op, "List") && !strings.HasPrefix(op, "Get") {
			t.Errorf("store op %q during a refused boot", op)
		}
	}
	if !cr.sp.IsRunning("worker") {
		t.Error("a refused boot stopped the running session")
	}
}

// Kills a swap that trusts the latch's probe over the provider it builds
// (review S-4): under v2, a swap onto a provider whose own handshake has a
// gap, or cannot be read, is refused before anything pauses.
func TestV2SwapChecksTheBuiltProvider(t *testing.T) {
	saved := reconcilerModeLookupEnv
	reconcilerModeLookupEnv = func(string) (string, bool) { return "", false }
	t.Cleanup(func() { reconcilerModeLookupEnv = saved })
	cfg := v2City(func(c *config.City) { c.Session.Provider = "tmux" })
	fake := runtime.NewFake()
	for name, next := range map[string]runtime.Provider{
		"gapped":     readinessLeaf{Provider: fake, missing: []string{"report-attachment"}},
		"unreadable": readinessLeaf{Provider: fake, err: errors.New("timeout")},
	} {
		cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
		if _, err := cr.beforeProviderSwap(context.Background(), cfg, next); err == nil || !strings.Contains(err.Error(), "C0.8") || cr.v2.planner.paused.Load() {
			t.Errorf("%s: swap err %v, paused %t; want refused by C0.8 before any pause", name, err, cr.v2.planner.paused.Load())
		}
	}
	cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
	resume, err := cr.beforeProviderSwap(context.Background(), cfg, readinessLeaf{Provider: fake})
	if err != nil {
		t.Fatalf("a ready provider: %v, want the swap held", err)
	}
	resume()
}

// Kills (review S-5) doctor findings after an O6 fallback that would make
// the next boot refuse: with a fallback record, the exec opt-in and fence
// findings are not made, and the repair hint stands.
func TestDoctorKeepsTheO6HintOnAnExecCity(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	cityPath := t.TempDir()
	record := reconcilerV2FallbackRecordPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(record), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, []byte(`{"refusal":"C0.7 refused","at":"2026-11-04T00:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := v2DefaultCity(func(c *config.City) { c.Session.Provider = "exec:" + ready })
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{CityPath: cityPath})
	if !strings.Contains(r.FixHint, "repair what the refusal names") || strings.Contains(r.Message, "v2 by default on the exec") || strings.Contains(r.Message, "input-fence") {
		t.Fatalf("doctor %q hint %q; want the O6 finding and its repair hint only", r.Message, r.FixHint)
	}
}

// Kills a D6 flip announced only at boot (review NIT): a reload whose unset
// session_reconciler now resolves to v2 on a v2-ready exec runtime says,
// in its pending-restart note, that the handshake opted the city in.
func TestReloadNamesTheExecOptIn(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	d := reconcilerModeDrift{running: reconcilerLegacy, lookupEnv: noEnv}
	cfg := v2DefaultCity(func(c *config.City) { c.Session.Provider = "exec:" + ready })
	if got := d.observe(cfg); !strings.Contains(got, "pending restart") || !strings.Contains(got, "v2 by default on the exec session runtime") {
		t.Fatalf("drift note %q, want the pending restart to name the exec opt-in", got)
	}
}

// Kills a later doctor finding replacing an earlier one's fix (review S-5,
// M19): on a default-v2 exec city that also sets a setting v2 ignores, the
// ignored-settings hint stands after the exec opt-in finding.
func TestDoctorKeepsTheFirstFixHint(t *testing.T) {
	ready := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment","proc.exec","input-fence"]}'; exit 0; }; exit 2
`)
	cfg := v2DefaultCity(func(c *config.City) {
		c.Session.Provider = "exec:" + ready
		c.Daemon.SessionCircuitBreaker = true
	})
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{})
	if r.FixHint != "remove the settings v2 ignores" || !strings.Contains(r.Message, "v2 by default on the exec") {
		t.Fatalf("doctor %q hint %q; want both findings, the first one's hint kept", r.Message, r.FixHint)
	}
}

// Pins (review N7) the swap's C0.8 check below the legacy branch: a legacy
// controller's swap onto any provider is not refused by it.
func TestLegacySwapIsNotCheckedByC08(t *testing.T) {
	cfg := &config.City{}
	cr := &CityRuntime{}
	resume, err := cr.beforeProviderSwap(context.Background(), cfg, readinessLeaf{Provider: runtime.NewFake(), missing: []string{"observe.v1"}})
	if err != nil {
		t.Fatalf("a legacy swap onto a gapped provider: %v, want held as legacy holds it", err)
	}
	resume()
}

// Kills (review N-A) a probe panic that strands its waiters: the entry is
// released with an error, the panic reaches the caller, and the next call
// probes again rather than waiting forever.
func TestExecProbePanicReleasesTheEntry(t *testing.T) {
	saved := probeExecHandshake
	t.Cleanup(func() { probeExecHandshake = saved })
	calls := 0
	probeExecHandshake = func(string, string, string) (runtime.ProtocolInfo, []string, error) {
		calls++
		if calls == 1 {
			panic("probe")
		}
		return runtime.ProtocolInfo{}, nil, nil
	}
	script := writeRPPScript(t, "exit 2\n")
	func() {
		defer func() { _ = recover() }()
		execReadiness(script, "c", "")
	}()
	done := make(chan execHandshake, 1)
	go func() { done <- execReadiness(script, "c", "") }()
	select {
	case h := <-done:
		if calls != 2 || h.err != nil {
			t.Fatalf("after a panicked probe: %+v, %d probes; want a fresh probe", h, calls)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the call after a panicked probe never returned")
	}
}
