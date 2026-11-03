package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// v2DefaultCity is a city whose unset session_reconciler resolves to v2: the
// sessions and graph classes on a sqlite-beads binding and conditional writes
// at their default (auto). mutate turns it into the case under test.
func v2DefaultCity(mutate func(*config.City)) *config.City {
	cfg := infraSplitConfig(config.DefaultSQLiteStoragePath)
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

// envOf is a lookupEnv over m.
func envOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestResolveReconcilerModeTable pins mc-dpo2b's resolution table: an unset
// session_reconciler is v2 only when config alone shows no true-gap feature,
// conditional writes resolved on (env override applied), and the sessions and
// graph classes on a sqlite-beads binding; else legacy, naming every reason.
// An explicit v2 keeps the latch's hard refusals; explicit legacy and the
// aliases are legacy whatever the config.
func TestResolveReconcilerModeTable(t *testing.T) {
	sqliteSessionsOnly := func(c *config.City) { c.Storage.Classes.Graph = config.StorageWorkBinding }
	for _, tc := range []struct {
		name     string
		cfg      *config.City
		env      map[string]string
		want     reconcilerMode
		reason   reconcilerModeReason
		fallback []string // substrings, one per fallback, in order
		wantErr  string
	}{
		{name: "unset, clean", cfg: v2DefaultCity(nil), want: reconcilerV2, reason: reconcilerDefaultV2},
		{name: "unset, conditional_writes auto", cfg: v2DefaultCity(func(c *config.City) { c.Beads.ConditionalWrites = "auto" }), want: reconcilerV2, reason: reconcilerDefaultV2},
		{name: "unset, conditional_writes require", cfg: v2DefaultCity(func(c *config.City) { c.Beads.ConditionalWrites = "require" }), want: reconcilerV2, reason: reconcilerDefaultV2},
		{
			name: "unset, ignored stall and breaker keys", want: reconcilerV2, reason: reconcilerDefaultV2,
			cfg: v2DefaultCity(func(c *config.City) {
				c.Daemon.SessionCircuitBreaker = true
				c.Session.ProgressStallTimeout, c.Session.ClaimHolderStallTimeout = "30m", "30m"
			}),
		},
		{
			name: "unset, conditional_writes off", cfg: v2DefaultCity(func(c *config.City) { c.Beads.ConditionalWrites = "off" }),
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy, fallback: []string{`[beads] conditional_writes = "off" turns conditional writes off`},
		},
		{
			name: "unset, env override off", cfg: v2DefaultCity(nil), env: map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "off"},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy, fallback: []string{"GC_BEADS_CONDITIONAL_WRITES=off turns conditional writes off"},
		},
		{
			name: "unset, env override auto over config off", cfg: v2DefaultCity(func(c *config.City) { c.Beads.ConditionalWrites = "off" }),
			env: map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "auto"}, want: reconcilerV2, reason: reconcilerDefaultV2,
		},
		{
			name: "unset, no [storage]: the bd work binding", cfg: &config.City{},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`the sessions and graph class stores are on the "work" binding, and v2 needs them on a sqlite-beads binding: moving them there is a storage migration (configure [storage], then run gc storage migrate --from-work)`},
		},
		{
			name: "unset, file store", cfg: &config.City{Beads: config.BeadsConfig{Provider: "file"}},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`on the "work" binding, and v2 needs them on a sqlite-beads binding`},
		},
		{
			name: "unset, exec store", cfg: &config.City{Beads: config.BeadsConfig{Provider: "exec:/opt/bin/beads"}},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`on the "work" binding, and v2 needs them on a sqlite-beads binding`},
		},
		{
			name: "unset, bd with native transport off", cfg: &config.City{Beads: config.BeadsConfig{NativeTransport: "off"}},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`on the "work" binding, and v2 needs them on a sqlite-beads binding`},
		},
		{
			name: "unset, graph left on the work binding", cfg: v2DefaultCity(sqliteSessionsOnly),
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`the graph class store is on the "work" binding, and v2 needs it on a sqlite-beads binding: moving it there is a storage migration`},
		},
		{
			name: "unset, binding kind config cannot classify",
			cfg: v2DefaultCity(func(c *config.City) {
				c.Storage.Bindings["infra"] = config.StorageBindingConfig{Provider: config.StorageProviderBeadsWorkspace, ConfigRef: "infra"}
			}),
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`the sessions and graph class stores are on binding "infra" (provider "beads-workspace"), and v2 needs them on a sqlite-beads binding`},
		},
		{
			name: "unset, undefined binding",
			cfg:  v2DefaultCity(func(c *config.City) { delete(c.Storage.Bindings, "infra") }),
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{`binding "infra", which [storage.bindings] does not define`},
		},
		{
			name: "unset, GC_SESSION names a refused runtime", cfg: v2DefaultCity(nil), env: map[string]string{"GC_SESSION": "herdr"},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy, fallback: []string{`[session] provider = "herdr"`},
		},
		{
			name: "unset, every reason at once",
			cfg: &config.City{
				Beads:        config.BeadsConfig{ConditionalWrites: "off"},
				Agents:       []config.Agent{{Name: "worker", DependsOn: []string{"db"}}},
				ChatSessions: config.ChatSessionsConfig{IdleTimeout: "1h"},
			},
			want: reconcilerLegacy, reason: reconcilerFallbackLegacy,
			fallback: []string{"agent depends_on on worker", "[chat_sessions] idle_timeout", `conditional_writes = "off"`, `the "work" binding`},
		},
		{name: "explicit v2, clean", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "v2" }), want: reconcilerV2, reason: reconcilerExplicit},
		{
			// The store-kind and conditional-writes reasons are not latch
			// refusals: C0.7 refuses an explicit v2 over such stores at boot.
			name: "explicit v2 on the work binding with conditional writes off", want: reconcilerV2, reason: reconcilerExplicit,
			cfg: &config.City{Daemon: config.DaemonConfig{SessionReconciler: "v2"}, Beads: config.BeadsConfig{ConditionalWrites: "off"}},
		},
		{
			name: "explicit v2, true gap", wantErr: `[daemon] session_reconciler = "v2" is refused: agent depends_on on worker`,
			cfg: v2DefaultCity(func(c *config.City) {
				c.Daemon.SessionReconciler = "v2"
				c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}}
			}),
		},
		{name: "explicit legacy", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "legacy" }), want: reconcilerLegacy, reason: reconcilerExplicit},
		{
			name: "explicit legacy, true gap", want: reconcilerLegacy, reason: reconcilerExplicit,
			cfg: &config.City{Daemon: config.DaemonConfig{SessionReconciler: "legacy"}, ChatSessions: config.ChatSessionsConfig{IdleTimeout: "1h"}},
		},
		{name: "alias off", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "off" }), want: reconcilerLegacy, reason: reconcilerExplicit},
		{name: "alias auto", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "auto" }), want: reconcilerLegacy, reason: reconcilerExplicit},
		{name: "alias require", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "require" }), want: reconcilerLegacy, reason: reconcilerExplicit},
		{name: "unknown", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "v3" }), wantErr: "is not a known value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := resolveReconcilerMode(tc.cfg, envOf(tc.env))
			mode, latchErr := latchReconcilerMode(tc.cfg, envOf(tc.env))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || latchErr == nil || mode != reconcilerLegacy {
					t.Fatalf("resolve err = %v, latch = %v, %v; want an error containing %q", err, mode, latchErr, tc.wantErr)
				}
				return
			}
			if err != nil || latchErr != nil || res.Mode != tc.want || mode != tc.want || res.Reason != tc.reason {
				t.Fatalf("resolve = %+v, %v; latch = %v, %v; want %v (reason %d)", res, err, mode, latchErr, tc.want, tc.reason)
			}
			if len(res.Fallbacks) != len(tc.fallback) {
				t.Fatalf("fallbacks = %q, want %d naming %q", res.Fallbacks, len(tc.fallback), tc.fallback)
			}
			for i, want := range tc.fallback {
				if !strings.Contains(res.Fallbacks[i], want) {
					t.Errorf("fallback %d = %q, want it to name %q", i, res.Fallbacks[i], want)
				}
			}
		})
	}
}

// TestResolveReconcilerModeEachTrueGapFallsBack pins that every feature the
// latch refuses an explicit v2 for turns an unset key into the legacy
// fallback, naming the refusal, rather than a refused start.
func TestResolveReconcilerModeEachTrueGapFallsBack(t *testing.T) {
	for _, tc := range v2RefusalCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := v2DefaultCity(tc.cfg)
			res, err := resolveReconcilerMode(cfg, noEnv)
			if err != nil || res.Mode != reconcilerLegacy || res.Reason != reconcilerFallbackLegacy {
				t.Fatalf("resolve(unset, %s) = %+v, %v; want the legacy fallback", tc.name, res, err)
			}
			if len(res.Fallbacks) != 1 || !strings.Contains(res.Fallbacks[0], tc.config) || !strings.Contains(res.Fallbacks[0], tc.pr) {
				t.Fatalf("fallbacks = %q, want one naming %q and %s", res.Fallbacks, tc.config, tc.pr)
			}
			boot := reconcilerBootWarnings(cfg, res)
			if len(boot) != 1 || !strings.Contains(boot[0], "legacy (default fallback: ") || !strings.Contains(boot[0], tc.config) || !strings.Contains(boot[0], `set session_reconciler = "legacy"`) {
				t.Errorf("boot warnings = %q, want the fallback naming %q and the fix", boot, tc.config)
			}
			cfg.Daemon.SessionReconciler = "v2"
			if _, err := latchReconcilerMode(cfg, noEnv); err == nil || !strings.Contains(err.Error(), tc.config) {
				t.Errorf("latch(explicit v2, %s) err = %v, want the hard refusal naming %q", tc.name, err, tc.config)
			}
		})
	}
}

// TestControllerWiringLogsTheResolution pins the boot log: the wiring both
// controller entry points share logs the fallback once with its reasons, and
// a default v2 says it is the default and names each setting v2 ignores.
func TestControllerWiringLogsTheResolution(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", DependsOn: []string{"db"}}}}
	w, err := newControllerWiring(cfg, noEnv, &stderr)
	if err != nil || w.mode != reconcilerLegacy || w.v2 != nil {
		t.Fatalf("wiring(unset, depends_on) = %+v, %v; want legacy", w, err)
	}
	if got := stderr.String(); strings.Count(got, "warning: session reconciler: legacy (default fallback: ") != 1 || !strings.Contains(got, "agent depends_on on worker") {
		t.Errorf("stderr = %q, want one fallback warning naming depends_on", got)
	}

	stderr.Reset()
	if w, err = newControllerWiring(&config.City{}, noEnv, &stderr); err != nil || w.mode != reconcilerLegacy {
		t.Fatalf("wiring(empty config) = %+v, %v; want the legacy fallback", w, err)
	}
	if got := stderr.String(); strings.Count(got, "warning: session reconciler: legacy (default fallback: ") != 1 {
		t.Errorf("stderr = %q, want one fallback warning for an empty config", got)
	}

	stderr.Reset()
	cfg = v2DefaultCity(func(c *config.City) { c.Session.ProgressStallTimeout = "30m" })
	w, err = newControllerWiring(cfg, noEnv, &stderr)
	if err != nil || w.mode != reconcilerV2 || w.v2 == nil {
		t.Fatalf("wiring(unset, clean) = %+v, %v; want v2", w, err)
	}
	t.Cleanup(w.v2.stop)
	got := stderr.String()
	for _, want := range []string{
		"warning: [session] progress_stall_timeout is deprecated under v2 and ignored: the work ladder replaces it",
		"session reconciler: v2 (default: session_reconciler is unset)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "fallback") {
		t.Errorf("stderr = %q, want no fallback for a default v2", got)
	}
}

// TestReconcilerDriftKeepsTheBootLatchAcrossReload pins the boot latch for an
// unset key: a reload never re-latches. A default-v2 controller whose
// candidate gains a true gap keeps running v2 and warns once that the next
// start falls back; a fallback-legacy controller whose candidate becomes
// eligible keeps running legacy and warns once that the next start runs v2.
func TestReconcilerDriftKeepsTheBootLatchAcrossReload(t *testing.T) {
	clean := v2DefaultCity(nil)
	gap := v2DefaultCity(func(c *config.City) { c.Agents = []config.Agent{{Name: "worker", DependsOn: []string{"db"}}} })

	d := reconcilerModeDrift{running: reconcilerV2, lookupEnv: noEnv}
	if got := d.observe(clean); got != "" {
		t.Fatalf("default v2, clean candidate: warned %q", got)
	}
	got := d.observe(gap)
	if !strings.HasPrefix(got, "pending restart: session_reconciler on disk is unset, which resolves to legacy (default fallback: ") ||
		!strings.Contains(got, "agent depends_on on worker") || !strings.HasSuffix(got, "; this controller runs v2; "+reconcilerDriftCLINote) {
		t.Fatalf("default v2, candidate with depends_on: warning %q, want the pending fallback against the running v2", got)
	}
	if got := d.observe(gap); got != "" {
		t.Errorf("same drift again: warned %q, want once per transition", got)
	}
	if d.running != reconcilerV2 {
		t.Fatalf("reload re-latched the running mode to %v", d.running)
	}
	if got := d.observe(clean); got != "" {
		t.Errorf("back to clean: warned %q, want in sync", got)
	}

	d = reconcilerModeDrift{running: reconcilerLegacy, lookupEnv: noEnv}
	if got := d.observe(gap); got != "" {
		t.Fatalf("fallback legacy, candidate still falls back: warned %q", got)
	}
	if got := d.observe(clean); got != "pending restart: session_reconciler on disk is unset, which resolves to v2 (default: session_reconciler is unset); this controller runs legacy; "+reconcilerDriftCLINote {
		t.Errorf("fallback legacy, eligible candidate: warning %q", got)
	}
	if d.running != reconcilerLegacy {
		t.Fatalf("reload re-latched the running mode to %v", d.running)
	}

	// An explicit v2 whose candidate gains a refused feature keeps running
	// and says the next start will refuse it.
	d = reconcilerModeDrift{running: reconcilerV2, lookupEnv: noEnv}
	gap.Daemon.SessionReconciler = "v2"
	if got := d.observe(gap); !strings.Contains(got, "on disk is v2; this controller runs v2; the next controller start will refuse it: ") {
		t.Errorf("explicit v2 gaining depends_on: warning %q, want the coming refusal named", got)
	}
}

// TestDoctorSessionReconcilerReportsTheResolution pins doctor's
// daemon-session-reconciler output: the resolved mode and why for each
// reason, the fallback a warning naming every reason with the fix, and the
// explicit-v2 refusal still an error.
func TestDoctorSessionReconcilerReportsTheResolution(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *config.City
		status  doctor.CheckStatus
		message []string
		fix     string
	}{
		{name: "default v2", cfg: v2DefaultCity(nil), status: doctor.StatusOK, message: []string{"session reconciler: v2 (default: session_reconciler is unset)"}},
		{name: "explicit v2", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "v2" }), status: doctor.StatusOK, message: []string{"session reconciler: v2 (set by [daemon] session_reconciler)"}},
		{name: "explicit legacy", cfg: v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "legacy" }), status: doctor.StatusOK, message: []string{"session reconciler: legacy (set by [daemon] session_reconciler)"}},
		{
			name:   "fallback",
			cfg:    &config.City{Beads: config.BeadsConfig{ConditionalWrites: "off"}, ChatSessions: config.ChatSessionsConfig{IdleTimeout: "1h"}},
			status: doctor.StatusWarning,
			message: []string{
				"session reconciler: legacy (default fallback: session_reconciler is unset and v2 is not available: ",
				"[chat_sessions] idle_timeout (chat auto-suspend) is not available under v2 until PAR-CHAT",
				`[beads] conditional_writes = "off" turns conditional writes off`,
				`the sessions and graph class stores are on the "work" binding`,
			},
			fix: `set session_reconciler = "legacy" to keep legacy and silence this warning`,
		},
		{
			name:    "explicit v2 refused",
			cfg:     v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "v2"; c.ChatSessions.IdleTimeout = "1h" }),
			status:  doctor.StatusError,
			message: []string{`[daemon] session_reconciler = "v2" is refused: [chat_sessions] idle_timeout`, "the controller refuses to start"},
			fix:     `set session_reconciler = "legacy"`,
		},
		{
			name:    "alias",
			cfg:     v2DefaultCity(func(c *config.City) { c.Daemon.SessionReconciler = "auto" }),
			status:  doctor.StatusWarning,
			message: []string{`[daemon] session_reconciler = "auto" is a deprecated alias for legacy; session reconciler: legacy (set by [daemon] session_reconciler)`},
			fix:     `set session_reconciler = "legacy"`,
		},
		{
			name:    "default v2 with ignored keys",
			cfg:     v2DefaultCity(func(c *config.City) { c.Daemon.SessionCircuitBreaker = true }),
			status:  doctor.StatusWarning,
			message: []string{"session reconciler: v2 (default: session_reconciler is unset)", "[daemon] session_circuit_breaker is deprecated under v2 and ignored: the work ladder replaces it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSessionReconcilerDoctorCheck(tc.cfg, noEnv).Run(&doctor.CheckContext{})
			if r.Status != tc.status {
				t.Errorf("status = %v (%s), want %v", r.Status, r.Message, tc.status)
			}
			for _, want := range tc.message {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message = %q, want %q", r.Message, want)
				}
			}
			if tc.fix != "" && !strings.Contains(r.FixHint, tc.fix) {
				t.Errorf("fix hint = %q, want %q", r.FixHint, tc.fix)
			}
		})
	}
}

// TestDoctorV2SessionMigrationWarnsAheadOfADefaultV2 pins item 3 of
// mc-dpo2b: the boot census stays a hard refusal for an unset key that
// resolves to v2, so doctor warns about the refused rows ahead of time with
// the fix, as it does for an explicit v2; a city that falls back to legacy
// only notes them.
func TestDoctorV2SessionMigrationWarnsAheadOfADefaultV2(t *testing.T) {
	cfg := v2DefaultCity(func(c *config.City) {
		chat := chatCity("always")
		chat.Agents = append(chat.Agents, allocPoolAgent("worker", 3))
		c.Workspace, c.Agents = chat.Workspace, chat.Agents
	})
	legs := func() []v2SessionLeg {
		return []v2SessionLeg{{ref: "city", store: censusStore(poolRow("gc-a1", "worker", 1, "archived"))}}
	}
	check := &v2SessionMigrationCheck{cfg: cfg, cityName: "test-city", openLegs: legs, lookupEnv: noEnv}
	r := check.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, "v2 refuses to boot over them (session reconciler: v2 (default: session_reconciler is unset))") {
		t.Fatalf("default v2 over a refused row: %v %q, want a warning naming the default v2", r.Status, r.Message)
	}
	if !strings.Contains(r.FixHint, "close or repair the rows listed") || !strings.Contains(r.FixHint, `set session_reconciler = "legacy"`) {
		t.Errorf("fix hint = %q, want the repair and the legacy opt-out", r.FixHint)
	}

	fallback := *cfg
	fallback.Storage = nil
	check = &v2SessionMigrationCheck{cfg: &fallback, cityName: "test-city", openLegs: legs, lookupEnv: noEnv}
	r = check.Run(&doctor.CheckContext{})
	if r.Status != doctor.StatusOK || !strings.Contains(r.Message, "(legacy runs over these; v2 would refuse to boot)") {
		t.Errorf("fallback legacy over a refused row: %v %q, want OK with the note", r.Status, r.Message)
	}
	if last := r.Details[len(r.Details)-1]; !strings.HasPrefix(last, "session reconciler: legacy (default fallback: ") {
		t.Errorf("last detail = %q, want the resolved mode and why", last)
	}
}

// TestBuildDoctorChecksResolvesTheReconcilerUnderTheProcessEnv pins doctor's
// production wiring: the daemon-session-reconciler check resolves under
// reconcilerModeLookupEnv, as gc start latches, so a GC_BEADS_CONDITIONAL_WRITES
// =off in the caller's environment reads as the legacy fallback. Kills:
// registering the check with a nil env.
func TestBuildDoctorChecksResolvesTheReconcilerUnderTheProcessEnv(t *testing.T) {
	original := reconcilerModeLookupEnv
	t.Cleanup(func() { reconcilerModeLookupEnv = original })
	reconcilerModeLookupEnv = envOf(map[string]string{"GC_BEADS_CONDITIONAL_WRITES": "off"})
	cfg := v2DefaultCity(nil)
	cfg.Workspace.Name = "demo"
	checks := buildDoctorChecks(t.TempDir(), cfg, nil, buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	var check *sessionReconcilerDoctorCheck
	for _, c := range checks {
		if rc, ok := c.(*sessionReconcilerDoctorCheck); ok {
			check = rc
		}
	}
	if check == nil {
		t.Fatal("buildDoctorChecks registered no daemon-session-reconciler check")
	}
	check.capabilities, check.queryPass = nil, nil
	if r := check.Run(nil); !strings.Contains(r.Message, "legacy (default fallback: ") || !strings.Contains(r.Message, "GC_BEADS_CONDITIONAL_WRITES=off") {
		t.Errorf("message = %q, want the legacy fallback naming GC_BEADS_CONDITIONAL_WRITES=off", r.Message)
	}
}

// TestDefaultV2CityPassesC07OnTheRoutesItOpens is the end-to-end half of the
// default: a city whose unset session_reconciler resolves to v2 opens its
// storage routes as the controller does (the binding stamped by its
// conditional_writes default, served through the controller cache), and C0.7
// admits the sessions and graph class stores it resolves. Kills a resolver
// that calls a city v2 whose opened stores C0.7 then refuses at boot.
func TestDefaultV2CityPassesC07OnTheRoutesItOpens(t *testing.T) {
	root := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(root, "store"))
	if res, err := resolveReconcilerMode(cfg, noEnv); err != nil || res.Mode != reconcilerV2 || res.Reason != reconcilerDefaultV2 {
		t.Fatalf("resolveReconcilerMode = %+v, %v; want the v2 default", res, err)
	}
	plan, err := resolveCityStoragePlan(root, cfg)
	if err != nil {
		t.Fatalf("resolving the storage plan: %v", err)
	}
	routes, err := openStorageRoutes(plan, mustResolveInfraTarget(t, root, cfg), cfg, root, nil)
	if err != nil {
		t.Fatalf("openStorageRoutes: %v", err)
	}
	t.Cleanup(func() { _ = routes.close() })
	cached := routes.withControllerCache(context.Background(), nil)
	work := beads.NewMemStore()
	caps := v2ClassStoreCapabilities(resolveSessionStore(cached, work, cfg, root, nil), resolveGraphStore(cached, work, cfg, root, nil), false)
	if err := v2CapabilityVerdict(caps); err != nil {
		t.Fatalf("C0.7 refused the default-v2 city's opened routes: %v (capabilities %+v)", err, caps)
	}
	for _, c := range caps {
		if !c.Writer || !c.Closer || c.Mode != gate.Auto {
			t.Errorf("%s capability = %+v, want a fenced writer and closer stamped auto", c.Class, c)
		}
	}
}

// TestSupervisorReconcilerBootLogNamesTheCityOnce pins the supervisor's
// reconciler boot log: each line names the city, an init retry with the same
// lines logs nothing, lines that change (a config edit) log again, and so
// does the city's next boot once it has started.
func TestSupervisorReconcilerBootLogNamesTheCityOnce(t *testing.T) {
	path := t.TempDir()
	t.Cleanup(func() { supervisorReconcilerBootLogged.Delete(path) })
	var stderr bytes.Buffer
	lines := "warning: session reconciler: legacy (default fallback: x)\n"
	logSupervisorReconcilerBoot(&stderr, path, "demo", lines)
	if got, want := stderr.String(), "gc supervisor: city 'demo': warning: session reconciler: legacy (default fallback: x)\n"; got != want {
		t.Fatalf("first log = %q, want %q", got, want)
	}
	stderr.Reset()
	logSupervisorReconcilerBoot(&stderr, path, "demo", lines)
	if stderr.Len() != 0 {
		t.Fatalf("init retry logged %q again", stderr.String())
	}
	logSupervisorReconcilerBoot(&stderr, path, "demo", "session reconciler: v2 (default: session_reconciler is unset)\n")
	if got := stderr.String(); got != "gc supervisor: city 'demo': session reconciler: v2 (default: session_reconciler is unset)\n" {
		t.Fatalf("changed lines logged %q, want the new line naming the city", got)
	}
	// The city started; restarting it is a new boot, which logs again.
	forgetSupervisorReconcilerBoot(path)
	stderr.Reset()
	logSupervisorReconcilerBoot(&stderr, path, "demo", "session reconciler: v2 (default: session_reconciler is unset)\n")
	if stderr.Len() == 0 {
		t.Fatal("a new boot of the city logged nothing")
	}
}
