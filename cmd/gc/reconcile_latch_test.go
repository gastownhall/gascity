package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// v2RefusalCases is one config per deferred feature, each hitting exactly one
// refusal (IMPLEMENTATION-PLAN B4).
var v2RefusalCases = []struct {
	name   string
	cfg    func(*config.City)
	config string // the setting the refusal must name
	pr     string
}{
	{"breaker", func(c *config.City) { c.Daemon.SessionCircuitBreaker = true }, "[daemon] session_circuit_breaker = true", "PAR-BRK"},
	{"depends_on", func(c *config.City) { c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}} }, "agent depends_on on worker", "PAR-DEP"},
	{"scale_check", func(c *config.City) { c.Agents = []config.Agent{{Name: "worker", Dir: "rig", ScaleCheck: "echo 2"}} }, "agent scale_check on rig/worker", "PAR-SC"},
	{"max_session_age", func(c *config.City) {
		c.Agents = []config.Agent{{Name: "a", MaxSessionAge: "5h"}, {Name: "b"}, {Name: "c", MaxSessionAge: "1h"}}
	}, "agent max_session_age on a, c", "PAR-AGE"},
	{"progress_stall", func(c *config.City) { c.Session.ProgressStallTimeout = "30m" }, "[session] progress_stall_timeout", "PAR-STALL-1"},
	{"claim_holder_stall", func(c *config.City) { c.Session.ClaimHolderStallTimeout = "30m" }, "[session] claim_holder_stall_timeout", "PAR-STALL-2"},
	{"chat idle_timeout", func(c *config.City) { c.ChatSessions.IdleTimeout = "30m" }, "[chat_sessions] idle_timeout", "PAR-CHAT"},
	{"k8s", func(c *config.City) { c.Session.Provider = "k8s" }, `[session] provider = "k8s"`, "PAR-K8S"},
	{"hybrid", func(c *config.City) { c.Session.Provider = "hybrid" }, `[session] provider = "hybrid"`, "PAR-K8S"},
	{"herdr", func(c *config.City) { c.Session.Provider = " herdr " }, `[session] provider = "herdr"`, "PAR-HERDR"},
	{"t3bridge", func(c *config.City) { c.Session.Provider = "t3bridge" }, `[session] provider = "t3bridge"`, "PAR-T3"},
	{"exec t3bridge script", func(c *config.City) { c.Session.Provider = "exec:/opt/bin/gc-session-t3" }, `[session] provider = "exec:/opt/bin/gc-session-t3"`, "PAR-T3"},
	{"exec", func(c *config.City) { c.Session.Provider = "exec:/opt/bin/runner" }, `[session] provider = "exec:/opt/bin/runner"`, "PAR-EXEC"},
	{"ssh", func(c *config.City) { c.Session.Provider = "ssh:ops@box:22" }, `[session] provider = "ssh:ops@box:22"`, "PAR-SSH"},
	{"pack runtime", func(c *config.City) {
		c.Session.Provider = "cloudflare"
		c.Runtimes = map[string]config.DiscoveredRuntime{"cloudflare": {Name: "cloudflare", PackName: "runtime-cloudflare"}}
	}, `[session] provider = "cloudflare"`, "PAR-EXEC"},
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
			got := v2LatchRefusals(cfg)
			if len(got) != 1 || got[0].Config != tc.config || got[0].ParityPR != tc.pr {
				t.Fatalf("v2LatchRefusals = %+v, want one refusal of %q by %s", got, tc.config, tc.pr)
			}
			mode, err := latchReconcilerMode(cfg, overrideEnv("1"))
			if err == nil || mode != reconcilerLegacy || !strings.Contains(err.Error(), got[0].String()) {
				t.Fatalf("latch = %v, %v; want legacy and an error naming %q", mode, err, got[0])
			}
		})
	}
	all := v2City(func(c *config.City) {
		c.Daemon.SessionCircuitBreaker = true
		c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}, ScaleCheck: "echo 2", MaxSessionAge: "5h"}}
		c.Session.ProgressStallTimeout, c.Session.ClaimHolderStallTimeout = "30m", "30m"
		c.ChatSessions.IdleTimeout = "30m"
		c.Session.Provider = "k8s"
	})
	if got := len(v2LatchRefusals(all)); got != 8 {
		t.Errorf("v2LatchRefusals(every feature) = %d refusals, want 8", got)
	}
	_, err := latchReconcilerMode(all, overrideEnv("1"))
	if err == nil {
		t.Fatal("latch(every feature) admitted v2")
	}
	for _, r := range v2LatchRefusals(all) {
		if !strings.Contains(err.Error(), r.String()) {
			t.Errorf("latch(every feature) error lacks %q: %v", r, err)
		}
	}
}

// TestV2LatchAdmitsInScopeFeatures pins the features v2 supports, the ones the
// six cities configure (IMPLEMENTATION-PLAN §6.0) among them, so the latch
// never blocks the cutover: agent idle_timeout, sleep_after_idle,
// min_active_sessions, wake_mode and a default scale_check. Also in scope,
// though §6.0 found no city using it: session = "acp". Disabled spellings
// ("0", empty) and the allowlisted runtimes are admitted too. Kills
// over-refusal.
func TestV2LatchAdmitsInScopeFeatures(t *testing.T) {
	one := 1
	cfg := v2City(func(c *config.City) {
		c.Agents = []config.Agent{
			{Name: "mayor", IdleTimeout: "10m", SleepAfterIdle: "30m", MinActiveSessions: &one, WakeMode: "fresh"},
			{Name: "polecat", Session: "acp", MaxSessionAge: "0"},
		}
		c.Session.ProgressStallTimeout = "0"
		c.ChatSessions.IdleTimeout = "0"
	})
	for _, provider := range []string{"", "tmux", "acp", "subprocess", "fake", "fail"} {
		cfg.Session.Provider = provider
		if got := v2LatchRefusals(cfg); len(got) != 0 {
			t.Errorf("provider %q: v2LatchRefusals = %+v, want none", provider, got)
		}
		if mode, err := latchReconcilerMode(cfg, overrideEnv("1")); err != nil || mode != reconcilerV2 {
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
	if got := v2LatchRefusals(cfg); len(got) != 0 {
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
		r, ok := v2SessionRuntimeRefusal(cfg, future)
		want := `[session] provider = "` + provider + `" (unsupported session runtime) is not available under v2`
		if !ok || r.String() != want {
			t.Errorf("future provider %q: refusal = %q, %v; want %q", provider, r, ok, want)
		}
	}
	cfg.Session.Provider = "tmux"
	if _, ok := v2SessionRuntimeRefusal(cfg, future); ok {
		t.Error("tmux refused by a registry that resolves it, want admitted")
	}
}

// TestDoctorListsLatchRefusalsUnderLegacy pins the dry run: doctor lists the
// refusals a legacy city would hit on switching to v2, from the composed
// config (a pack agent's max_session_age), without changing the status.
func TestDoctorListsLatchRefusalsUnderLegacy(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test\"\n\n[imports.local]\nsource = \"packs/local\"\n\n[chat_sessions]\nidle_timeout = \"1h\"\n")
	fs.Files["/city/packs/local/pack.toml"] = []byte("[pack]\nname = \"local\"\nschema = 2\n")
	fs.Files["/city/packs/local/agents/witness/agent.toml"] = []byte("max_session_age = \"5h\"\n")
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
	if len(r.Details) != 2 {
		t.Errorf("details = %q, want exactly two refusals", r.Details)
	}
}

// TestLatchRefusalNamesParityPR pins that every refusal the operator sees names
// the setting to remove and the parity PR that lifts it. Kills a refusal
// whose message loses either.
func TestLatchRefusalNamesParityPR(t *testing.T) {
	parityPR := regexp.MustCompile(`^PAR-[A-Z0-9]+(-[0-9])?$`)
	for _, tc := range v2RefusalCases {
		for _, r := range v2LatchRefusals(v2City(tc.cfg)) {
			if !parityPR.MatchString(r.ParityPR) || r.Feature == "" {
				t.Errorf("%s: refusal %+v lacks a feature or a PAR- parity PR", tc.name, r)
			}
			if s := r.String(); !strings.Contains(s, r.Config) || !strings.HasSuffix(s, "until "+r.ParityPR) {
				t.Errorf("%s: message %q does not name %q and %s", tc.name, s, r.Config, r.ParityPR)
			}
		}
	}
}
