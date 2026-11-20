package main

import (
	"context"
	"io"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/k8s"
	"github.com/gastownhall/gascity/internal/runtime/subprocess"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
)

// v2RefusalCases is one config per deferred feature, each hitting exactly one
// refusal (IMPLEMENTATION-PLAN B4). The identity breaker and the two stall
// recyclers are not here: v2 ignores them (v2IgnoredSettings, mc-ymynp).
var v2RefusalCases = []struct {
	name   string
	cfg    func(*config.City)
	config string // the setting the refusal must name
	pr     string
}{
	{"depends_on", func(c *config.City) { c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}} }, "agent depends_on on worker", "PAR-DEP"},
	{"max_session_age", func(c *config.City) {
		c.Agents = []config.Agent{{Name: "a", MaxSessionAge: "5h"}, {Name: "b"}, {Name: "c", MaxSessionAge: "1h"}}
	}, "agent max_session_age on a, c", "PAR-AGE"},
	{"chat idle_timeout", func(c *config.City) { c.ChatSessions.IdleTimeout = "30m" }, "[chat_sessions] idle_timeout", "PAR-CHAT"},
	{"k8s", func(c *config.City) { c.Session.Provider = "k8s" }, `[session] provider = "k8s"`, "PAR-K8S"},
	{"hybrid", func(c *config.City) { c.Session.Provider = "hybrid" }, `[session] provider = "hybrid"`, "PAR-K8S"},
	{"herdr", func(c *config.City) { c.Session.Provider = " herdr " }, `[session] provider = "herdr"`, "PAR-HERDR"},
	{"t3bridge", func(c *config.City) { c.Session.Provider = "t3bridge" }, `[session] provider = "t3bridge"`, "PAR-T3"},
	{"exec t3bridge script", func(c *config.City) { c.Session.Provider = "exec:/opt/bin/gc-session-t3" }, `[session] provider = "exec:/opt/bin/gc-session-t3"`, "PAR-T3"},
	{"ssh", func(c *config.City) { c.Session.Provider = "ssh:ops@box:22" }, `[session] provider = "ssh:ops@box:22"`, "PAR-SSH"},
}

// v2City is an admissible v2 city that mutate turns into the case under test.
func v2City(mutate func(*config.City)) *config.City {
	cfg := &config.City{Daemon: config.DaemonConfig{SessionReconciler: "v2"}}
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// TestV2LatchRefusesEachDeferredFeature pins one refusal per deferred feature:
// each refuses v2 alone, with the setting and parity PR in the latch error, and
// all of them together appear in one error. Kills a refusal silently dropped.
func TestV2LatchRefusesEachDeferredFeature(t *testing.T) {
	for _, tc := range v2RefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := v2City(tc.cfg)
			got := v2LatchRefusals(cfg, "", nil)
			if len(got) != 1 || got[0].Config != tc.config || got[0].ParityPR != tc.pr {
				t.Fatalf("v2LatchRefusals = %+v, want one refusal of %q by %s", got, tc.config, tc.pr)
			}
			mode, err := latchReconcilerMode(cfg, "", noEnv)
			if err == nil || mode != reconcilerLegacy || !strings.Contains(err.Error(), got[0].String()) {
				t.Fatalf("latch = %v, %v; want legacy and an error naming %q", mode, err, got[0])
			}
		})
	}
	all := v2City(func(c *config.City) {
		c.Daemon.SessionCircuitBreaker = true
		c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}, MaxSessionAge: "5h"}}
		c.Session.ProgressStallTimeout, c.Session.ClaimHolderStallTimeout = "30m", "30m"
		c.ChatSessions.IdleTimeout = "30m"
		c.Session.Provider = "k8s"
	})
	if got := len(v2LatchRefusals(all, "", nil)); got != 4 {
		t.Errorf("v2LatchRefusals(every feature) = %d refusals, want 4", got)
	}
	_, err := latchReconcilerMode(all, "", noEnv)
	if err == nil {
		t.Fatal("latch(every feature) admitted v2")
	}
	for _, r := range v2LatchRefusals(all, "", nil) {
		if !strings.Contains(err.Error(), r.String()) {
			t.Errorf("latch(every feature) error lacks %q: %v", r, err)
		}
	}
}

// TestV2LatchAdmitsInScopeFeatures pins the features v2 supports, the ones the
// six cities configure (IMPLEMENTATION-PLAN §6.0) among them, so the latch
// never blocks the cutover: agent idle_timeout, sleep_after_idle,
// min_active_sessions, wake_mode and a default scale_check. Also in scope,
// though §6.0 found no city using them: session = "acp" and a custom
// scale_check (PAR-SC, its demand pinned by the allocation differential's
// scaleCheckDiffCities). Disabled spellings ("0", empty) and the allowlisted
// runtimes are admitted too. Kills over-refusal.
func TestV2LatchAdmitsInScopeFeatures(t *testing.T) {
	one := 1
	cfg := v2City(func(c *config.City) {
		c.Agents = []config.Agent{
			{Name: "mayor", IdleTimeout: "10m", SleepAfterIdle: "30m", MinActiveSessions: &one, WakeMode: "fresh"},
			{Name: "polecat", Session: "acp", MaxSessionAge: "0"},
			{Name: "worker", Dir: "rig", ScaleCheck: "echo 2"},
		}
		c.Session.ProgressStallTimeout = "0"
		c.ChatSessions.IdleTimeout = "0"
	})
	for _, provider := range []string{"", "tmux", "acp", "subprocess", "fake", "fail"} {
		cfg.Session.Provider = provider
		if got := v2LatchRefusals(cfg, "", nil); len(got) != 0 {
			t.Errorf("provider %q: v2LatchRefusals = %+v, want none", provider, got)
		}
		if mode, err := latchReconcilerMode(cfg, "", noEnv); err != nil || mode != reconcilerV2 {
			t.Errorf("provider %q: latch = %v, %v; want v2, nil", provider, mode, err)
		}
	}
}

// TestV2LatchRuntimeAllowlist pins the runtime allowlist's two edges. A name
// nothing registers reaches the registry's tmux fallback, so it is admitted as
// tmux. A runtime some registry resolves but the allowlist does not know, such
// as a future builtin or prefix, is refused by default with a generic message.
// Kills an allowlist that fails open on new runtimes, and one that refuses the
// tmux fallback.
func TestV2LatchRuntimeAllowlist(t *testing.T) {
	cfg := v2City(func(c *config.City) { c.Session.Provider = "undeclared-name" })
	if runtimeRegistry.Resolves("undeclared-name") {
		t.Fatal("the builtin registry resolves undeclared-name; pick another name")
	}
	if got := v2LatchRefusals(cfg, "", nil); len(got) != 0 {
		t.Errorf("undeclared provider (tmux fallback): v2LatchRefusals = %+v, want none", got)
	}
	future := runtimeRegistry.Clone()
	nop := func(string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	}
	if err := future.Register("nomad", nop); err != nil {
		t.Fatal(err)
	}
	if err := future.RegisterPrefix("fly:", nop); err != nil {
		t.Fatal(err)
	}
	for _, provider := range []string{"nomad", "fly:app"} {
		cfg.Session.Provider = provider
		r, ok := v2SessionRuntimeRefusal(cfg, "", future, v2SessionRuntimeRawName(cfg, nil), v2SessionRuntimeName(cfg, nil))
		want := `[session] provider = "` + provider + `" (unsupported session runtime) is not available under v2`
		if !ok || r.String() != want {
			t.Errorf("future provider %q: refusal = %q, %v; want %q", provider, r, ok, want)
		}
	}
	cfg.Session.Provider = "tmux"
	if _, ok := v2SessionRuntimeRefusal(cfg, "", future, v2SessionRuntimeRawName(cfg, nil), v2SessionRuntimeName(cfg, nil)); ok {
		t.Error("tmux refused by a registry that resolves it, want admitted")
	}
}

// TestDoctorListsLatchRefusalsUnderLegacy pins the dry run: doctor lists the
// refusals a legacy city would hit on switching to v2, from the composed
// config (a pack agent's max_session_age), without changing the status, and
// no longer lists the pack agent's custom scale_check (PAR-SC).
func TestDoctorListsLatchRefusalsUnderLegacy(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test\"\n\n[daemon]\nsession_reconciler = \"legacy\"\n\n[imports.local]\nsource = \"packs/local\"\n\n[chat_sessions]\nidle_timeout = \"1h\"\n")
	fs.Files["/city/packs/local/pack.toml"] = []byte("[pack]\nname = \"local\"\nschema = 2\n")
	fs.Files["/city/packs/local/agents/witness/agent.toml"] = []byte("max_session_age = \"5h\"\nscale_check = \"echo 2\"\n")
	cfg, _, err := config.LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	r := newSessionReconcilerDoctorCheck(cfg, nil).Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK {
		t.Errorf("status = %v (%s), want OK: the dry run is informational", r.Status, r.Message)
	}
	details := strings.Join(r.Details, "\n")
	for _, want := range []string{"v2 would refuse: agent max_session_age on local.witness", "v2 would refuse: [chat_sessions] idle_timeout"} {
		if !strings.Contains(details, want) {
			t.Errorf("details = %q, want a line containing %q", r.Details, want)
		}
	}
	if len(r.Details) != 2 || strings.Contains(details, "scale_check") {
		t.Errorf("details = %q, want exactly two refusals, none for scale_check", r.Details)
	}
}

// TestLatchRefusalNamesParityPR pins that every refusal the operator sees names
// the setting to remove and the parity PR that lifts it. Kills a refusal
// whose message loses either.
func TestLatchRefusalNamesParityPR(t *testing.T) {
	parityPR := regexp.MustCompile(`^PAR-[A-Z0-9]+(-[0-9])?$`)
	for _, tc := range v2RefusalCases {
		for _, r := range v2LatchRefusals(v2City(tc.cfg), "", nil) {
			if !parityPR.MatchString(r.ParityPR) || r.Feature == "" {
				t.Errorf("%s: refusal %+v lacks a feature or a PAR- parity PR", tc.name, r)
			}
			if s := r.String(); !strings.Contains(s, r.Config) || !strings.HasSuffix(s, "until "+r.ParityPR) {
				t.Errorf("%s: message %q does not name %q and %s", tc.name, s, r.Config, r.ParityPR)
			}
		}
	}
}

// Pins ruling (c) safe by construction, not by accident: every runtime the
// latch admits reads liveness fresh with errors, attests its listing, reads
// identity and reports attachment, so the plain row writes that stay on the
// pass's cached liveness never run where no fresh read could be made. The
// allowlist is tmux, acp and subprocess (and the test doubles); an exec
// runtime is admitted only when its handshake is v2-ready (PAR-EXEC), and
// for each exec fixture (no handshake, observe.v1 without
// report-attachment, ready, a broken handshake) the latch admits exactly
// the providers the registry builds that read fresh. herdr and k8s, which
// cannot read fresh, refuse. A runtime added to the allowlist must read
// fresh, or this fails.
func TestV2RunsOnlyOnRuntimesThatReadFresh(t *testing.T) {
	want := map[string]bool{"": true, "tmux": true, "acp": true, "subprocess": true, "fake": true, "fail": true}
	if !maps.Equal(v2SessionRuntimesAdmitted, want) {
		t.Fatalf("v2SessionRuntimesAdmitted = %v, want %v; a new runtime must read fresh (ruling (c))", v2SessionRuntimesAdmitted, want)
	}
	for name, p := range map[string]runtime.Provider{"tmux": (*tmux.Provider)(nil), "acp": (*acp.Provider)(nil), "subprocess": (*subprocess.Provider)(nil)} {
		if !runtime.FreshReadable(p) {
			t.Errorf("admitted runtime %s cannot read fresh", name)
		}
	}
	for name, p := range map[string]runtime.Provider{"herdr": (*herdr.Provider)(nil), "k8s": (*k8s.Provider)(nil)} {
		if v2SessionRuntimesAdmitted[name] || runtime.FreshReadable(p) {
			t.Errorf("%s: admitted %t, reads fresh %t; want refused, and not fresh (else revisit ruling (c))", name, v2SessionRuntimesAdmitted[name], runtime.FreshReadable(p))
		}
	}
	for _, f := range execAdmissionFixtures(t) {
		cfg := v2City(func(c *config.City) { c.Session.Provider = "exec:" + f.script })
		admitted := len(v2LatchRefusals(cfg, "", nil)) == 0
		sp, err := runtimeRegistry.New("exec:"+f.script, config.SessionConfig{}, "test-city", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if admitted != f.ready || admitted != runtime.FreshReadable(sp) {
			t.Errorf("exec %s: admitted %t, reads fresh %t; want both %t", f.name, admitted, runtime.FreshReadable(sp), f.ready)
		}
		if !admitted {
			continue
		}
		attested := false
		if a, ok := sp.(runtime.ListingAttestation); ok {
			attested = a.ListRunningComplete()
		}
		identity := false
		if o, ok := sp.(runtime.IdentityObserver); ok {
			identity = o.IdentityObservable()
		}
		if !attested || !identity || !sp.Capabilities().CanReportAttachment {
			t.Errorf("admitted exec %s: listing attested %t, identity observable %t, reports attachment %t; want all", f.name, attested, identity, sp.Capabilities().CanReportAttachment)
		}
	}
}

// execAdmissionFixture is an exec script by its handshake, and whether it is
// v2-ready.
type execAdmissionFixture struct {
	name, script string
	ready        bool
}

// execAdmissionFixtures are a script with no handshake (v0), one declaring
// observe.v1 without report-attachment, a ready one, and one whose
// handshake is broken.
func execAdmissionFixtures(t *testing.T) []execAdmissionFixture {
	t.Helper()
	pack := func(protocol string) string {
		return writeRPPScript(t, `case "$1" in
  protocol) `+protocol+` ;;
  *) exit 2 ;;
esac
`)
	}
	return []execAdmissionFixture{
		{"v0", pack(`exit 2`), false},
		{"observe.v1 without report-attachment", pack(`echo '{"version":0,"capabilities":["observe.v1"]}'`), false},
		{"ready", pack(`echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'`), true},
		{"broken handshake", pack(`echo 'not json'`), false},
	}
}

// Kills an exec refusal that does not tell the operator what to change: a
// refused exec runtime, an exec: script or a pack's, names the setting and
// a Fix (the capabilities its handshake lacks, or the error that kept it
// from being read) and no parity PR, since PAR-EXEC lifted the runtime
// itself; a ready one is admitted.
func TestExecRefusalNamesTheMissingCapabilities(t *testing.T) {
	fixtures := execAdmissionFixtures(t)
	for _, c := range []struct {
		name string
		cfg  func(*config.City)
		fix  []string // "" means admitted
	}{
		{"v0", func(c *config.City) { c.Session.Provider = "exec:" + fixtures[0].script }, []string{"does not declare observe.v1 or report-attachment"}},
		{"observe.v1 alone", func(c *config.City) { c.Session.Provider = "exec:" + fixtures[1].script }, []string{"does not declare report-attachment", "gc runtime conformance"}},
		{"ready", func(c *config.City) { c.Session.Provider = "exec:" + fixtures[2].script }, nil},
		{"broken handshake", func(c *config.City) { c.Session.Provider = "exec:" + fixtures[3].script }, []string{"could not be read", "observe.v1 and report-attachment"}},
		{"missing script", func(c *config.City) { c.Session.Provider = "exec:/nonexistent/gc-runner" }, []string{"could not be read"}},
		{"pack runtime", func(c *config.City) {
			c.Session.Provider = "cloudflare"
			c.Runtimes = map[string]config.DiscoveredRuntime{"cloudflare": {Name: "cloudflare", PackName: "runtime-cloudflare", Command: fixtures[1].script}}
		}, []string{"the pack runtime-cloudflare exec session runtime", "does not declare report-attachment"}},
		{"ready pack runtime", func(c *config.City) {
			c.Session.Provider = "cloudflare"
			c.Runtimes = map[string]config.DiscoveredRuntime{"cloudflare": {Name: "cloudflare", PackName: "runtime-cloudflare", Command: fixtures[2].script}}
		}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := v2LatchRefusals(v2City(c.cfg), "", nil)
			if c.fix == nil {
				if len(got) != 0 {
					t.Fatalf("refusals %v, want a ready exec runtime admitted", got)
				}
				return
			}
			if len(got) != 1 || got[0].ParityPR != "" || got[0].Fix == "" {
				t.Fatalf("refusals %+v, want one with a Fix and no parity PR", got)
			}
			msg := got[0].String()
			for _, part := range append(c.fix, got[0].Config, "is not available under v2: ") {
				if !strings.Contains(msg, part) {
					t.Errorf("refusal %q does not contain %q", msg, part)
				}
			}
		})
	}
}

// Kills an admission probe that forks on every latch, or remembers what it
// should not: the decision reads config and, outside it, only the exec
// handshake, one probe per version of the script file and city. The latch,
// the default resolution, the swap check and doctor's detail all reuse it;
// a rewrite of the script probes again; a handshake that cannot be read is
// remembered only for its retry window. Not parallel: it replaces
// probeExecHandshake.
func TestExecAdmissionProbeIsRemembered(t *testing.T) {
	saved := probeExecHandshake
	probes := 0
	probeExecHandshake = func(path, cityName, cityPath string) (runtime.ProtocolInfo, []string, error) {
		probes++
		return saved(path, cityName, cityPath)
	}
	t.Cleanup(func() { probeExecHandshake = saved })
	script := writeRPPScript(t, `[ "$1" = protocol ] && { echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}'; exit 0; }; exit 2
`)
	cfg := v2City(func(c *config.City) { c.Session.Provider = "exec:" + script })
	for range 3 {
		if got := v2LatchRefusals(cfg, "", nil); len(got) != 0 {
			t.Fatalf("refusals %v, want the ready script admitted", got)
		}
	}
	cfg.Daemon.SessionReconciler = ""
	for _, f := range v2DefaultFallbacks(cfg, "", nil) {
		if strings.Contains(f, script) {
			t.Fatalf("default fallback %q, want the ready script admitted", f)
		}
	}
	if _, refused := v2ProviderSwapRefusal(cfg, "", func(string) (string, bool) { return "", false }); refused {
		t.Fatal("the swap check refused a ready script")
	}
	execRuntimeDoctorDetail(cfg, "", nil, reconcilerV2)
	if probes != 1 {
		t.Fatalf("%d probes for one version of the script, want 1", probes)
	}
	// A rewrite is a new version: probed again, and now refused.
	if err := os.WriteFile(script, []byte("#!/bin/sh\n[ \"$1\" = protocol ] && { echo '{\"version\":0,\"capabilities\":[\"observe.v1\"]}'; exit 0; }; exit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := v2LatchRefusals(cfg, "", nil); len(got) != 1 || probes != 2 {
		t.Fatalf("after a rewrite: refusals %v, %d probes; want refused on a second probe", got, probes)
	}
	// An error is remembered for the handshake's retry window
	// (TestExecAdmissionProbeIsBoundedAndShared), not for good.
	broken := writeRPPScript(t, "exit 1\n")
	bcfg := v2City(func(c *config.City) { c.Session.Provider = "exec:" + broken })
	v2LatchRefusals(bcfg, "", nil)
	v2LatchRefusals(bcfg, "", nil)
	if probes != 3 {
		t.Fatalf("%d probes after two latches over a broken handshake, want 3 (its error remembered in its window)", probes)
	}
}

// Kills a locality read that ignores the handshake (owner decision D4,
// #8509's seam): an exec: runtime whose handshake declares locality=local
// is local; one that declares none, another, or whose handshake cannot be
// read is remote.
func TestExecRuntimeLocalityComesFromTheHandshake(t *testing.T) {
	for _, c := range []struct {
		protocol string
		remote   bool
	}{
		{`echo '{"version":0,"capabilities":[],"locality":"local"}'`, false},
		{`echo '{"version":0,"capabilities":[]}'`, true},
		{`echo '{"version":0,"capabilities":[],"locality":"remote"}'`, true},
		{`exit 1`, true},
	} {
		script := writeRPPScript(t, `[ "$1" = protocol ] && { `+c.protocol+`; exit $?; }; exit 2
`)
		if got := sessionRuntimeIsRemote(&config.City{}, "", "exec:"+script); got != c.remote {
			t.Errorf("handshake %s: remote %t, want %t", c.protocol, got, c.remote)
		}
	}
}

// Kills a latch that reads only the config's provider, and a v2 reload that
// swaps onto a runtime the latch refuses (review item 1): GC_SESSION, the
// provider the city runs, is what the latch judges, and a v2 controller's
// provider swap to a refused runtime is itself refused before it pauses
// anything. Not parallel: it replaces reconcilerModeLookupEnv.
func TestV2LatchJudgesTheEffectiveProvider(t *testing.T) {
	env := map[string]string{"GC_SESSION": "herdr"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cfg := v2City(func(c *config.City) { c.Session.Provider = "tmux" })
	if got := v2LatchRefusals(cfg, "", lookup); len(got) != 1 || !strings.Contains(got[0].String(), "herdr") {
		t.Fatalf("GC_SESSION=herdr over tmux: refusals %v, want herdr's", got)
	}
	if got := v2LatchRefusals(cfg, "", nil); len(got) != 0 {
		t.Fatalf("tmux, no override: refusals %v, want none", got)
	}
	saved := reconcilerModeLookupEnv
	reconcilerModeLookupEnv = lookup
	t.Cleanup(func() { reconcilerModeLookupEnv = saved })
	cr := &CityRuntime{v2: newDefaultPlanner(io.Discard)}
	if _, err := cr.beforeProviderSwap(context.Background(), cfg, nil); err == nil || cr.v2.planner.paused.Load() {
		t.Fatalf("swap onto herdr under v2: err %v, paused %t; want refused before any pause", err, cr.v2.planner.paused.Load())
	}
	env = map[string]string{}
	if resume, err := cr.beforeProviderSwap(context.Background(), cfg, nil); err != nil {
		t.Fatalf("swap onto tmux under v2: %v, want it held and applied", err)
	} else {
		resume()
	}
}
