package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// These tests pin the launch-finalizer contract of the ga-vytkfv ruling: the
// startup prompt is finalized once, at the Manager's launch seams, from the
// config the Manager assembled, so every launch carries it through exactly one
// route AND the delivered marker. Observation point: the runtime.Config the
// Manager hands to Provider.Start.

// finalizerPromptModes are the provider prompt modes a launch can be configured
// for; a fresh create routes by mode, a resume always rides the nudge.
var finalizerPromptModes = []struct {
	name, mode, flag, createRoute string
}{
	{name: "argv", mode: "arg", createRoute: "argv"},
	{name: "flag", mode: "flag", flag: "--prompt", createRoute: "flag"},
	{name: "nudge-only", mode: "none", createRoute: "nudge"},
}

// finalizerCity is newContractCity with the claude provider's prompt mode set.
func finalizerCity(t *testing.T, promptBody, mode, flag string) contractCity {
	t.Helper()
	c := newContractCity(t, promptBody)
	spec := c.cfg.Providers["claude"]
	spec.PromptMode = mode
	spec.PromptFlag = flag
	c.cfg.Providers["claude"] = spec
	resolved, err := config.ResolveProvider(&c.cfg.Agents[0], &c.cfg.Workspace, c.cfg.Providers, func(name string) (string, error) { return name, nil })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	c.resolved = resolved
	return c
}

// launchRig is a real Manager over a fake runtime, with the real finalizer.
type launchRig struct {
	city  contractCity
	store *beads.MemStore
	sp    *runtime.Fake
	mgr   *session.Manager
	calls *atomic.Int32
}

func newLaunchRig(t *testing.T, city contractCity) launchRig {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	prod := startupPromptLaunchFinalizer(city.path, city.cfg, nil)
	calls := &atomic.Int32{}
	counting := func(lc session.LaunchContext) (runtime.Config, error) {
		calls.Add(1)
		if prod == nil {
			return lc.Config, nil
		}
		return prod(lc)
	}
	mgr := session.NewManagerWithOptions(beadstest.Sessions(store), sp,
		session.WithCityPath(city.path), session.WithLaunchFinalizer(counting))
	return launchRig{city: city, store: store, sp: sp, mgr: mgr, calls: calls}
}

func (r launchRig) actor(t *testing.T) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(r.city.path)
	if err != nil {
		t.Fatalf("NewCityDir: %v", err)
	}
	return session.Actor{Kind: session.ActorOperator, City: dir}
}

// startedConfig is the config the runtime was started with for sessionName.
func (r launchRig) startedConfig(t *testing.T, sessionName string) runtime.Config {
	t.Helper()
	cfg := r.sp.LastStartConfig(sessionName)
	if cfg == nil {
		t.Fatalf("no Start recorded for %q: %#v", sessionName, r.sp.Calls)
	}
	return *cfg
}

const rigSessionName = "worker-sess"

// create is a started create of the city's worker agent.
func (r launchRig) create(t *testing.T, alias string, env map[string]string) (session.Info, error) {
	t.Helper()
	hints := workerSessionCreateHints(r.city.resolved)
	hints.Env = env
	return r.mgr.CreateSession(context.Background(), session.CreateOptions{
		Alias: alias, Template: "worker", Title: "worker", Command: "claude", WorkDir: r.city.path,
		Provider: "claude", Hints: hints, ExtraMeta: map[string]string{"session_origin": "ephemeral"},
	})
}

// seedResumable is a suspended worker row, with a conversation when key is set.
func (r launchRig) seedResumable(t *testing.T, key string) beads.Bead {
	t.Helper()
	meta := map[string]string{
		"session_name": rigSessionName, "state": string(session.StateSuspended), "template": "worker",
		"provider": "claude", "work_dir": r.city.path, "session_origin": "ephemeral",
	}
	if key != "" {
		meta["session_key"] = key
		meta["started_config_hash"] = "cfg"
	}
	b, err := r.store.Create(beads.Bead{Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	return b
}

// resume resumes b's runtime as an operator, with env in the launch hints.
func (r launchRig) resume(t *testing.T, b beads.Bead, env map[string]string) error {
	t.Helper()
	return r.mgr.Attach(context.Background(), r.actor(t), b.ID, "claude --resume warm",
		runtime.Config{WorkDir: r.city.path, Env: env})
}

// payloadRoutes names the routes cfg carries prompt through.
func payloadRoutes(cfg runtime.Config, prompt string) []string {
	var routes []string
	if carriedThroughArgv(cfg, prompt) {
		if cfg.PromptFlag != "" {
			routes = append(routes, "flag")
		} else {
			routes = append(routes, "argv")
		}
	}
	if strings.Contains(cfg.Nudge, prompt) {
		routes = append(routes, "nudge")
	}
	return routes
}

// finalizerLaunchRow is one way gc brings a session's runtime up through the
// Manager. It returns the config the runtime was started with and whether the
// launch resumed a conversation.
type finalizerLaunchRow struct {
	name   string
	resume bool
	launch func(t *testing.T, r launchRig) runtime.Config
}

var finalizerLaunchRows = []finalizerLaunchRow{
	{name: "create named/aliased", launch: func(t *testing.T, r launchRig) runtime.Config {
		info, err := r.create(t, "alice", nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return r.startedConfig(t, info.SessionName)
	}},
	{name: "create unaliased pool-managed", launch: func(t *testing.T, r launchRig) runtime.Config {
		info, err := r.create(t, "", nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return r.startedConfig(t, info.SessionName)
	}},
	{name: "ensureRunning first start", resume: true, launch: func(t *testing.T, r launchRig) runtime.Config {
		if err := r.resume(t, r.seedResumable(t, ""), nil); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		return r.startedConfig(t, rigSessionName)
	}},
	{name: "ensureRunning resume with key", resume: true, launch: func(t *testing.T, r launchRig) runtime.Config {
		if err := r.resume(t, r.seedResumable(t, "warm"), nil); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		return r.startedConfig(t, rigSessionName)
	}},
}

// TestLaunchFinalizerSeamContract (T1) is the guard against the
// launch-population gap (ga-4k4zfk), observed where it matters: at the config a
// Manager launch hands the runtime. For a prompted agent every launch row, in
// every prompt mode, carries the prompt through exactly one route AND the
// delivered marker. (non-empty prompt, no payload, no marker) must never occur:
// that launch leaves the SessionStart hook the sole, size-limited carrier.
// The create rows are the ones that were hook-only before the finalizer.
//
// The interrupting-submit relaunch of a running session shares the
// ensureRunning seam; its seam test lives in internal/session
// (TestLaunchFinalizerInterruptRelaunchOfRunningSession).
func TestLaunchFinalizerSeamContract(t *testing.T) {
	for _, pm := range finalizerPromptModes {
		for _, row := range finalizerLaunchRows {
			t.Run(pm.name+"/"+row.name, func(t *testing.T) {
				rig := newLaunchRig(t, finalizerCity(t, contractPromptBody, pm.mode, pm.flag))
				cfg := row.launch(t, rig)

				routes := payloadRoutes(cfg, contractPromptBody)
				marked := cfg.Env[startupPromptDeliveredEnv] == "1"
				if len(routes) != 1 {
					t.Errorf("prompt must ride exactly one route, got %v (PromptSuffix=%q PromptFlag=%q Nudge=%q)",
						routes, abbreviate(cfg.PromptSuffix), cfg.PromptFlag, abbreviate(cfg.Nudge))
				}
				if !marked {
					t.Errorf("a launch that carries the prompt must set %s=1, got %q", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
				}
				want := pm.createRoute
				if row.resume {
					// A resume never rides argv: it would be submitted to the
					// continued conversation as a new turn.
					want = "nudge"
				}
				if len(routes) == 1 && routes[0] != want {
					t.Errorf("route = %s, want %s", routes[0], want)
				}
			})
		}
	}
}

// TestLaunchFinalizerRendersLaunchIdentity (T2): the prompt is rendered for the
// identity the launch env carries (GC_ALIAS, else GC_AGENT), for a create as
// for a resume. An unaliased pool-managed create has no agent_name yet, so
// rendering from the session's stored agent name would show the template name
// to an agent whose env says it is a session id.
func TestLaunchFinalizerRendersLaunchIdentity(t *testing.T) {
	for _, row := range finalizerLaunchRows {
		t.Run(row.name, func(t *testing.T) {
			rig := newLaunchRig(t, finalizerCity(t, "AGENT={{.AgentName}}|", "none", ""))
			cfg := row.launch(t, rig)

			id := cfg.Env["GC_ALIAS"]
			if id == "" {
				id = cfg.Env["GC_AGENT"]
			}
			if id == "" {
				t.Fatalf("launch env carries no identity: %v", cfg.Env)
			}
			if want := "AGENT=" + id + "|"; !strings.Contains(cfg.Nudge, want) {
				t.Errorf("rendered prompt = %q, want it to contain %q (the launch env's identity)", abbreviate(cfg.Nudge), want)
			}
		})
	}
}

// TestLaunchFinalizerNotInvokedOverRunningRuntime (T3): reading, and
// ensureRunning over a live runtime, never reach the finalizer, so nothing
// renders without a launch.
func TestLaunchFinalizerNotInvokedOverRunningRuntime(t *testing.T) {
	rig := newLaunchRig(t, finalizerCity(t, contractPromptBody, "arg", ""))
	b := rig.seedResumable(t, "warm")
	if err := rig.sp.Start(context.Background(), rigSessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.mgr.Get(b.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := rig.resume(t, b, nil); err != nil {
		t.Fatalf("Attach over a live runtime: %v", err)
	}
	if got := rig.calls.Load(); got != 0 {
		t.Fatalf("finalizer invoked %d times without a launch, want 0", got)
	}
}

// TestWorkerRuntimeResolversRenderNoPrompt (T3): the CLI resolver builds hints
// without rendering or delivering a startup prompt; delivery belongs to the
// launch seam.
func TestWorkerRuntimeResolversRenderNoPrompt(t *testing.T) {
	city := newContractCity(t, contractPromptBody)
	for _, mode := range launchModes {
		t.Run(mode.name, func(t *testing.T) {
			resolved, err := resolvedWorkerRuntimeWithConfig(city.path, city.cfg, city.sessionInfo(mode), "")
			if err != nil || resolved == nil {
				t.Fatalf("resolvedWorkerRuntimeWithConfig = %v, %v", resolved, err)
			}
			h := resolved.Hints
			if h.PromptSuffix != "" || h.PromptFlag != "" || h.Nudge != "" {
				t.Errorf("resolver hints carry a prompt: PromptSuffix=%q PromptFlag=%q Nudge=%q", abbreviate(h.PromptSuffix), h.PromptFlag, abbreviate(h.Nudge))
			}
			if v, ok := h.Env[startupPromptDeliveredEnv]; ok {
				t.Errorf("resolver hints set %s=%q", startupPromptDeliveredEnv, v)
			}
		})
	}
}

// TestLaunchFinalizerErasesStaleMarker (T5): an inherited delivered marker
// never survives a launch that delivers nothing, for a create and a resume —
// the hook must stay free to carry the prompt.
func TestLaunchFinalizerErasesStaleMarker(t *testing.T) {
	stale := map[string]string{startupPromptDeliveredEnv: "1", "KEEP": "me"}
	cases := []struct {
		name string
		city func(t *testing.T) contractCity
	}{
		{"empty prompt", func(t *testing.T) contractCity { return newContractCity(t, "") }},
		{"nudge-incapable runtime", func(t *testing.T) contractCity {
			c := newContractCity(t, contractPromptBody)
			c.cfg.Session.Provider = "subprocess"
			return c
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/create", func(t *testing.T) {
			rig := newLaunchRig(t, tc.city(t))
			info, err := rig.create(t, "alice", stale)
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			assertNoMarker(t, rig.startedConfig(t, info.SessionName))
		})
		t.Run(tc.name+"/resume", func(t *testing.T) {
			rig := newLaunchRig(t, tc.city(t))
			if err := rig.resume(t, rig.seedResumable(t, "warm"), stale); err != nil {
				t.Fatalf("Attach: %v", err)
			}
			assertNoMarker(t, rig.startedConfig(t, rigSessionName))
		})
	}
}

func assertNoMarker(t *testing.T, cfg runtime.Config) {
	t.Helper()
	if v, ok := cfg.Env[startupPromptDeliveredEnv]; ok {
		t.Errorf("stale %s=%q survived a launch that delivers nothing", startupPromptDeliveredEnv, v)
	}
	if cfg.Env["KEEP"] != "me" {
		t.Errorf("unrelated env dropped: %v", cfg.Env)
	}
	if cfg.PromptSuffix != "" || cfg.PromptFlag != "" || strings.Contains(cfg.Nudge, contractPromptBody) {
		t.Errorf("a launch that delivers nothing carries a payload: PromptSuffix=%q PromptFlag=%q Nudge=%q", abbreviate(cfg.PromptSuffix), cfg.PromptFlag, abbreviate(cfg.Nudge))
	}
}

// TestStartupPromptPlanErasesStaleMarkerForBothKinds (T5): the plan, not the
// caller, decides the marker. A fresh plan that delivers nothing strips an
// inherited marker exactly as a resume plan does, and never aliases the
// caller's env map.
func TestStartupPromptPlanErasesStaleMarkerForBothKinds(t *testing.T) {
	for _, launch := range []startupPromptLaunch{startupPromptFreshLaunch, startupPromptResumeLaunch} {
		plan, err := planStartupPrompt(startupPromptSource{TemplateName: "worker", SessionName: "s"}, launch)
		if err != nil {
			t.Fatal(err)
		}
		inherited := map[string]string{startupPromptDeliveredEnv: "1", "KEEP": "me"}
		cfg := runtime.Config{Env: inherited}
		plan.applyTo(&cfg)
		if _, ok := cfg.Env[startupPromptDeliveredEnv]; ok {
			t.Errorf("launch kind %d: stale marker survived a plan that delivers nothing: %v", launch, cfg.Env)
		}
		if cfg.Env["KEEP"] != "me" {
			t.Errorf("launch kind %d: unrelated env dropped: %v", launch, cfg.Env)
		}
	}
}

// TestLaunchFinalizerOversizedPrompt (T8): a prompt too large for argv rides
// the nudge on a nudge-capable runtime, for a create (where the mode would
// have chosen argv) as for a resume, and is marked delivered.
func TestLaunchFinalizerOversizedPrompt(t *testing.T) {
	big := strings.Repeat("x", maxPromptSuffixRawBytes+1)
	for _, row := range finalizerLaunchRows {
		t.Run(row.name, func(t *testing.T) {
			rig := newLaunchRig(t, finalizerCity(t, big, "arg", ""))
			cfg := row.launch(t, rig)
			if cfg.PromptSuffix != "" || cfg.PromptFlag != "" {
				t.Errorf("an oversized prompt must not ride argv, got PromptSuffix of %d bytes", len(cfg.PromptSuffix))
			}
			if !strings.Contains(cfg.Nudge, big) {
				t.Errorf("an oversized prompt must ride the nudge, got a nudge of %d bytes", len(cfg.Nudge))
			}
			if cfg.Env[startupPromptDeliveredEnv] != "1" {
				t.Errorf("oversized prompt launch must set %s=1, got %q", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
			}
		})
	}
}

// TestLaunchFinalizerEmptyPromptAgent (T9): an agent with no prompt gets no
// payload and no marker from any launch row.
func TestLaunchFinalizerEmptyPromptAgent(t *testing.T) {
	for _, row := range finalizerLaunchRows {
		t.Run(row.name, func(t *testing.T) {
			rig := newLaunchRig(t, newContractCity(t, ""))
			cfg := row.launch(t, rig)
			if cfg.PromptSuffix != "" || cfg.PromptFlag != "" || cfg.Nudge != "" {
				t.Errorf("empty prompt must deliver no payload, got PromptSuffix=%q PromptFlag=%q Nudge=%q", cfg.PromptSuffix, cfg.PromptFlag, cfg.Nudge)
			}
			if v, ok := cfg.Env[startupPromptDeliveredEnv]; ok {
				t.Errorf("empty prompt must not set %s, got %q", startupPromptDeliveredEnv, v)
			}
		})
	}
}

// TestLaunchFinalizerLeavesCallerEnvUntouched: the marker belongs to the launch
// only; the env map a caller passed in hints is never written.
func TestLaunchFinalizerLeavesCallerEnvUntouched(t *testing.T) {
	rig := newLaunchRig(t, finalizerCity(t, contractPromptBody, "arg", ""))
	env := map[string]string{"KEEP": "me"}
	if _, err := rig.create(t, "alice", env); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(env) != 1 || env["KEEP"] != "me" {
		t.Errorf("caller env mutated by the launch: %v", env)
	}
}

// TestLaunchFinalizerNilConfigIsNil: with no city config there is no agent to
// resolve, so the factory installs no finalizer and launches are unchanged.
func TestLaunchFinalizerNilConfigIsNil(t *testing.T) {
	if f := startupPromptLaunchFinalizer(t.TempDir(), nil, nil); f != nil {
		t.Fatal("startupPromptLaunchFinalizer(nil config) must be nil")
	}
}

// captureSlog routes slog.Default() to a buffer for the test's duration.
func captureSlog(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			rec := map[string]any{}
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatalf("slog line %q: %v", line, err)
			}
			out = append(out, rec)
		}
		return out
	}
}

// TestLaunchFinalizerLaunchRecord (AC4): one structured record per launch that
// found a prompt, naming origin, session, template, route, marker and size —
// never the prompt text — and a WARN with a reason when an expected prompt
// could not be delivered.
func TestLaunchFinalizerLaunchRecord(t *testing.T) {
	t.Run("delivered", func(t *testing.T) {
		records := captureSlog(t)
		rig := newLaunchRig(t, finalizerCity(t, contractPromptBody, "arg", ""))
		info, err := rig.create(t, "alice", nil)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		var launch []map[string]any
		for _, rec := range records() {
			if _, ok := rec["route"]; ok {
				launch = append(launch, rec)
			}
		}
		if len(launch) != 1 {
			t.Fatalf("got %d launch records, want exactly 1: %v", len(launch), records())
		}
		rec := launch[0]
		for key, want := range map[string]any{"origin": "create", "session_id": info.ID, "template": "worker", "route": "argv", "marker": true} {
			if rec[key] != want {
				t.Errorf("launch record %s = %v, want %v (record %v)", key, rec[key], want, rec)
			}
		}
		if n, _ := rec["prompt_bytes"].(float64); int(n) < len(contractPromptBody) {
			t.Errorf("launch record prompt_bytes = %v, want at least %d", rec["prompt_bytes"], len(contractPromptBody))
		}
		for _, r := range records() {
			raw, _ := json.Marshal(r)
			if strings.Contains(string(raw), contractPromptBody) {
				t.Errorf("a log record carries prompt text: %s", raw)
			}
		}
	})
	t.Run("not delivered", func(t *testing.T) {
		records := captureSlog(t)
		city := newContractCity(t, contractPromptBody)
		city.cfg.Session.Provider = "subprocess"
		rig := newLaunchRig(t, city)
		if _, err := rig.create(t, "alice", nil); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		var warns int
		for _, rec := range records() {
			if rec["level"] == "WARN" && rec["reason"] != nil && rec["reason"] != "" {
				warns++
			}
		}
		if warns != 1 {
			t.Fatalf("got %d WARN records with a reason for an undeliverable prompt, want 1: %v", warns, records())
		}
	})
}
