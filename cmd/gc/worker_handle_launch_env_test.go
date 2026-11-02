package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// resumeLaunchEnvCity is a city whose sessions were launched with their
// generated launch env and whose runtimes are then gone while the session
// records remain (#8184).
type resumeLaunchEnvCity struct {
	cityDir string
	rigRoot string
	cfg     *config.City
	store   beads.Store
	sp      *runtime.Fake
}

// newResumeLaunchEnvCity writes cityToml (with {{RIG_ROOT}} replaced by a rig
// checkout outside the city directory) and loads it.
func newResumeLaunchEnvCity(t *testing.T, cityToml string) *resumeLaunchEnvCity {
	t.Helper()
	cityDir := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writePhase0InterfaceCity(t, cityDir, strings.ReplaceAll(cityToml, "{{RIG_ROOT}}", rigRoot))
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	return &resumeLaunchEnvCity{cityDir: cityDir, rigRoot: rigRoot, cfg: cfg, store: beads.NewMemStore(), sp: runtime.NewFake()}
}

// stoppedSession creates a session of template for agentName with workDir,
// launched with the env resolveTemplate gives it, then stops only its runtime.
func (c *resumeLaunchEnvCity) stoppedSession(t *testing.T, template, agentName, alias, workDir string) session.Info {
	t.Helper()
	mgr := newSessionManagerWithConfig(c.cityDir, c.store, c.sp, c.cfg)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Alias:    alias,
		Template: template,
		Title:    agentName,
		Command:  "/bin/echo",
		WorkDir:  workDir,
		Provider: "stub",
		Hints:    runtime.Config{Env: c.templateEnv(t, template, agentName)},
		ExtraMeta: map[string]string{
			"session_origin": "ephemeral",
			"agent_name":     agentName,
		},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := c.sp.Stop(info.SessionName); err != nil {
		t.Fatalf("stopping the runtime: %v", err)
	}
	return info
}

// templateEnv is the Env resolveTemplate produces for agentName of template.
func (c *resumeLaunchEnvCity) templateEnv(t *testing.T, template, agentName string) map[string]string {
	t.Helper()
	agentCfg, ok := resolveAgentIdentity(c.cfg, template, "")
	if !ok {
		t.Fatalf("no agent for template %q", template)
	}
	bp := newReadOnlyAgentBuildParams(c.cfg.EffectiveCityName(), c.cityDir, c.cfg, nil, time.Now(), io.Discard)
	tp, err := resolveTemplate(bp, &agentCfg, agentName, nil)
	if err != nil {
		t.Fatalf("resolveTemplate(%q): %v", agentName, err)
	}
	return tp.Env
}

func (c *resumeLaunchEnvCity) handle(t *testing.T, id string) worker.Handle {
	t.Helper()
	handle, err := workerHandleForSessionWithStaleKeyDetectionWaiter(c.cityDir, c.store, c.sp, c.cfg, id,
		func(context.Context, string) error { return nil })
	if err != nil {
		t.Fatalf("building the session handle: %v", err)
	}
	return handle
}

type resumeLaunchEnvVia func(context.Context, worker.Handle, session.Actor) error

var resumeByMessage resumeLaunchEnvVia = func(ctx context.Context, h worker.Handle, by session.Actor) error {
	_, err := h.Message(ctx, by, worker.MessageRequest{Text: "hello"})
	return err
}

// resume resumes info's session through via and returns the replacement's
// Provider.Start config.
func (c *resumeLaunchEnvCity) resume(t *testing.T, info session.Info, via resumeLaunchEnvVia) runtime.Config {
	t.Helper()
	handle := c.handle(t, info.ID)
	c.sp.Calls = nil
	if err := via(context.Background(), handle, testActorIn(t, session.ActorOperator, c.cityDir)); err != nil {
		t.Fatalf("resuming %s: %v", info.ID, err)
	}
	start := c.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("resuming %s started no runtime", info.ID)
	}
	return *start
}

const rigPoolLaunchEnvCity = `[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = "{{RIG_ROOT}}"

[[agent]]
name = "worker"
dir = "myrig"
provider = "stub"
max_active_sessions = 3

[agent.env]
AGENT_ENV = "agent"

[providers.stub]
command = "/bin/echo"

[providers.stub.env]
PROVIDER_ENV = "provider"
`

// generatedLaunchEnvKeys are the generated scope keys AC-1 names, beyond the
// backend projection.
var generatedLaunchEnvKeys = []string{"GC_BIN", "GC_RIG", "GC_RIG_ROOT", "BEADS_DIR", "GC_BEADS", "GC_BEADS_SCOPE_ROOT", "GT_ROOT"}

func assertResumedGeneratedLaunchEnv(t *testing.T, c *resumeLaunchEnvCity, got, want map[string]string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got["GC_BIN"] != exe {
		t.Errorf("GC_BIN = %q, want the running binary %q", got["GC_BIN"], exe)
	}
	if prefix := filepath.Dir(exe) + string(os.PathListSeparator); !strings.HasPrefix(got["PATH"], prefix) {
		t.Errorf("PATH = %q, want it to begin with GC_BIN's directory %q", got["PATH"], prefix)
	}
	keys := append([]string(nil), generatedLaunchEnvKeys...)
	backend, err := sessionBackendEnvWithError(c.cityDir, c.rigRoot, c.cfg.Rigs)
	if err != nil {
		t.Fatalf("sessionBackendEnvWithError: %v", err)
	}
	for key := range backend {
		keys = append(keys, key)
	}
	for _, key := range keys {
		wantValue, wantOK := want[key]
		gotValue, gotOK := got[key]
		if gotOK != wantOK || gotValue != wantValue {
			t.Errorf("start env[%s] = %q (present %v), want resolveTemplate's %q (present %v)", key, gotValue, gotOK, wantValue, wantOK)
		}
	}
}

// AC-1: a message that resumes a reconciler-launched rig pool session whose
// runtime is gone starts the replacement with the generated launch env.
func TestResumeByMessageRegeneratesRigScopedLaunchEnv(t *testing.T) {
	c := newResumeLaunchEnvCity(t, rigPoolLaunchEnvCity)
	info := c.stoppedSession(t, "myrig/worker", "myrig/worker-1", "", filepath.Join(c.cityDir, ".gc", "worktrees", "task"))

	start := c.resume(t, info, resumeByMessage)

	want := c.templateEnv(t, "myrig/worker", "myrig/worker-1")
	if want["GC_RIG"] != "myrig" || want["GC_RIG_ROOT"] != c.rigRoot {
		t.Fatalf("resolveTemplate rig scope = %q/%q, want myrig/%q", want["GC_RIG"], want["GC_RIG_ROOT"], c.rigRoot)
	}
	assertResumedGeneratedLaunchEnv(t, c, start.Env, want)
}

var (
	resumeByAttach resumeLaunchEnvVia = func(ctx context.Context, h worker.Handle, by session.Actor) error {
		return h.Attach(ctx, by)
	}
	resumeByStart resumeLaunchEnvVia = func(ctx context.Context, h worker.Handle, by session.Actor) error {
		return h.Start(ctx, by)
	}
)

// AC-2: Attach and Start resume the stopped runtime with the same generated
// launch env a message does.
func TestResumeByAttachOrStartRegeneratesRigScopedLaunchEnv(t *testing.T) {
	for _, tc := range []struct {
		name string
		via  resumeLaunchEnvVia
	}{
		{"attach", resumeByAttach},
		{"start", resumeByStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newResumeLaunchEnvCity(t, rigPoolLaunchEnvCity)
			info := c.stoppedSession(t, "myrig/worker", "myrig/worker-1", "", filepath.Join(c.cityDir, ".gc", "worktrees", "task"))

			start := c.resume(t, info, tc.via)

			assertResumedGeneratedLaunchEnv(t, c, start.Env, c.templateEnv(t, "myrig/worker", "myrig/worker-1"))
		})
	}
}

// The API half of AC-1: the controller hands the API the resolver the gc
// worker handles carry, built from the config snapshot the API's worker
// factory passes in, so an API resume of a rig pool session whose runtime is
// gone regenerates the same launch env.
func TestControllerStateLaunchEnvResolverRegeneratesRigScopedLaunchEnv(t *testing.T) {
	c := newResumeLaunchEnvCity(t, rigPoolLaunchEnvCity)
	stopped := c.stoppedSession(t, "myrig/worker", "myrig/worker-1", "", filepath.Join(c.cityDir, ".gc", "worktrees", "task"))
	info, err := session.NewStore(beads.SessionStore{Store: c.store}).Get(stopped.ID)
	if err != nil {
		t.Fatalf("reading the stopped session: %v", err)
	}
	b, err := c.store.Get(stopped.ID)
	if err != nil {
		t.Fatal(err)
	}

	var state api.State = &controllerState{cityPath: c.cityDir}
	provider, ok := state.(api.LaunchEnvResolverProvider)
	if !ok {
		t.Fatal("controllerState does not implement api.LaunchEnvResolverProvider")
	}
	resolve := provider.LaunchEnvResolver(c.cfg)
	if resolve == nil {
		t.Fatal("LaunchEnvResolver returned no resolver for a configured city")
	}
	env, err := resolve(info, "", b.Metadata)
	if err != nil {
		t.Fatalf("resolving the launch env: %v", err)
	}

	assertResumedGeneratedLaunchEnv(t, c, env, c.templateEnv(t, "myrig/worker", "myrig/worker-1"))
}

// AC-3: rig scope comes from the configured agent and the city's rig config,
// not from the recorded work_dir or the caller's environment.
func TestResumeDerivesRigScopeFromConfiguredAgent(t *testing.T) {
	callerRoot := t.TempDir()
	caller := map[string]string{
		"GC_RIG":              "caller-rig",
		"GC_RIG_ROOT":         callerRoot,
		"BEADS_DIR":           filepath.Join(callerRoot, ".beads"),
		"GC_BEADS_SCOPE_ROOT": callerRoot,
	}
	for key, value := range caller {
		t.Setenv(key, value)
	}
	c := newResumeLaunchEnvCity(t, rigPoolLaunchEnvCity)
	workDir := t.TempDir()
	info := c.stoppedSession(t, "myrig/worker", "myrig/worker-1", "", workDir)

	env := c.resume(t, info, resumeByMessage).Env

	for key, want := range map[string]string{
		"GC_RIG":              "myrig",
		"GC_RIG_ROOT":         c.rigRoot,
		"BEADS_DIR":           filepath.Join(c.rigRoot, ".beads"),
		"GC_BEADS_SCOPE_ROOT": c.rigRoot,
		"GC_DIR":              workDir,
	} {
		if got := env[key]; got != want {
			t.Errorf("start env[%s] = %q, want %q", key, got, want)
		}
	}
	for key, value := range env {
		for callerKey, callerValue := range caller {
			if value == callerValue {
				t.Errorf("start env[%s] = %q carries the caller's %s", key, value, callerKey)
			}
		}
	}
}

// AC-4: a city-scoped session clears stale rig scope with present-empty
// values and scopes beads to the city.
func TestResumeCityScopedSessionClearsRigScope(t *testing.T) {
	t.Setenv("GC_RIG", "stale-rig")
	t.Setenv("GC_RIG_ROOT", "/stale/rig")
	t.Setenv("BEADS_DIR", "/stale/rig/.beads")
	c := newResumeLaunchEnvCity(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "helper"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)
	info := c.stoppedSession(t, "helper", "helper", "helper-alias", t.TempDir())

	env := c.resume(t, info, resumeByMessage).Env

	for _, key := range []string{"GC_RIG", "GC_RIG_ROOT", "BEADS_DIR"} {
		value, ok := env[key]
		if !ok || value != "" {
			t.Errorf("start env[%s] = %q (present %v), want present and empty", key, value, ok)
		}
	}
	if got := env["GC_BEADS_SCOPE_ROOT"]; got != c.cityDir {
		t.Errorf("start env[GC_BEADS_SCOPE_ROOT] = %q, want the city path %q", got, c.cityDir)
	}
}

// AC-5: a resumed control-dispatcher keeps its per-dispatcher trace path; an
// ordinary session gets the value resolveTemplate gives it.
func TestResumeKeepsPerDispatcherTracePath(t *testing.T) {
	c := newResumeLaunchEnvCity(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "`+config.ControlDispatcherAgentName+`"
provider = "stub"

[[agent]]
name = "helper"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)
	dispatcher := c.stoppedSession(t, config.ControlDispatcherAgentName, config.ControlDispatcherAgentName, "", t.TempDir())
	ordinary := c.stoppedSession(t, "helper", "helper", "", t.TempDir())

	dispatcherEnv := c.resume(t, dispatcher, resumeByMessage).Env
	ordinaryEnv := c.resume(t, ordinary, resumeByMessage).Env

	wantDispatcher := citylayout.ControlDispatcherTraceDefaultPathForRuntimeDirAndName(
		c.cityDir, cityRuntimeEnvMapForCity(c.cityDir)["GC_CITY_RUNTIME_DIR"], config.ControlDispatcherAgentName)
	if got := dispatcherEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != wantDispatcher {
		t.Errorf("dispatcher GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want %q", got, wantDispatcher)
	}
	wantOrdinary := c.templateEnv(t, "helper", "helper")["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]
	if wantOrdinary == "" {
		t.Fatal("resolveTemplate gives the ordinary session no GC_CONTROL_DISPATCHER_TRACE_DEFAULT")
	}
	if got := ordinaryEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != wantOrdinary {
		t.Errorf("ordinary GC_CONTROL_DISPATCHER_TRACE_DEFAULT = %q, want resolveTemplate's %q", got, wantOrdinary)
	}
}

// AC-6: the resumed env layers baseline < workspace < provider < agent <
// generated < upstream < pinned overlays, and the persisted identity wins.
func TestResumeLaunchEnvLayerPrecedence(t *testing.T) {
	t.Setenv("LANG", "caller-lang")
	t.Setenv("GC_CONTROLLER_TOKEN", "caller-token")
	c := newResumeLaunchEnvCity(t, `[workspace]
name = "test-city"

[workspace.env]
LANG = "workspace"
L_WS_PROV = "workspace"
GC_CONTROLLER_TOKEN = "workspace-token"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = "{{RIG_ROOT}}"

[upstreams.serving.env]
L_AGENT_UP = "upstream"
GT_ROOT = "upstream-root"
`+execenv.UsageMetricsDisableEnv+` = "upstream-metrics"

[[agent]]
name = "worker"
dir = "myrig"
provider = "stub"
upstream = "serving"
max_active_sessions = 3

[agent.env]
L_PROV_AGENT = "agent"
L_AGENT_UP = "agent"
GC_RIG = "agent-rig"
GC_SESSION_ID = "agent-forged"
GC_SESSION_NAME = "agent-forged"
GC_ALIAS = "agent-forged"
GC_TEMPLATE = "agent-forged"

[providers.stub]
command = "/bin/echo"

[providers.stub.env]
L_WS_PROV = "provider"
L_PROV_AGENT = "provider"
`)
	info := c.stoppedSession(t, "myrig/worker", "myrig/worker-1", "probe-alias", t.TempDir())

	env := c.resume(t, info, resumeByMessage).Env

	for key, want := range map[string]string{
		"LANG":                         "workspace",
		"L_WS_PROV":                    "provider",
		"L_PROV_AGENT":                 "agent",
		"GC_RIG":                       "myrig",
		"L_AGENT_UP":                   "upstream",
		"GT_ROOT":                      "upstream-root",
		execenv.UsageMetricsDisableEnv: execenv.UsageMetricsDisableValue,
		"GC_SESSION_ID":                info.ID,
		"GC_SESSION_NAME":              info.SessionName,
		"GC_ALIAS":                     "probe-alias",
		"GC_TEMPLATE":                  "myrig/worker",
	} {
		if got := env[key]; got != want {
			t.Errorf("start env[%s] = %q, want %q", key, got, want)
		}
	}
	if value, ok := env["GC_CONTROLLER_TOKEN"]; !ok || value != "" {
		t.Errorf("start env[GC_CONTROLLER_TOKEN] = %q (present %v), want present and empty", value, ok)
	}
}

func (c *resumeLaunchEnvCity) startCalls() int {
	n := 0
	for _, call := range c.sp.Calls {
		if call.Method == "Start" {
			n++
		}
	}
	return n
}

// AC-7: a launch env that cannot be built fails the resume before anything
// starts or mints, while the handle and its non-start reads keep working.
func TestResumeLaunchEnvFailureRefusesStartOnly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		city   string
		setup  func(t *testing.T, c *resumeLaunchEnvCity)
		target func(c *resumeLaunchEnvCity) (template, agentName string)
		cause  func(t *testing.T, err error)
	}{
		{
			name: "unregistered beads backend",
			city: `[workspace]
name = "test-city"

[[agent]]
name = "helper"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`,
			setup: func(t *testing.T, c *resumeLaunchEnvCity) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(c.cityDir, ".beads"), 0o700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_BEADS", "")
				config := "issue_prefix: demo\ngc.endpoint_origin: managed_city\ngc.endpoint_status: verified\ndolt.auto-start: false\n"
				if err := os.WriteFile(filepath.Join(c.cityDir, ".beads", "config.yaml"), []byte(config), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(c.cityDir, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"mysql"}`), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			cause: func(t *testing.T, err error) {
				t.Helper()
				if !errors.Is(err, contract.ErrUnknownBackend) {
					t.Errorf("resume error = %v, want it to wrap ErrUnknownBackend", err)
				}
			},
		},
		{
			name: "undeclared upstream",
			city: `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "helper"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`,
			setup: func(t *testing.T, c *resumeLaunchEnvCity) {
				t.Helper()
				c.cfg.Agents[0].Upstream = "undeclared"
			},
			cause: func(t *testing.T, err error) {
				t.Helper()
				if !strings.Contains(err.Error(), `upstream "undeclared"`) {
					t.Errorf("resume error = %v, want it to name the undeclared upstream", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newResumeLaunchEnvCity(t, tc.city)
			info := c.stoppedSession(t, "helper", "helper", "", t.TempDir())
			tc.setup(t, c)
			before, err := c.store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}

			handle := c.handle(t, info.ID)
			if _, err := handle.State(context.Background()); err != nil {
				t.Fatalf("State on a session whose launch env cannot be built: %v", err)
			}
			c.sp.Calls = nil
			_, err = handle.Message(context.Background(), testActorIn(t, session.ActorOperator, c.cityDir), worker.MessageRequest{Text: "hello"})
			if err == nil {
				t.Fatal("Message resumed a session whose launch env cannot be built")
			}
			if !strings.Contains(err.Error(), info.ID) {
				t.Errorf("resume error = %v, want it to name session %s", err, info.ID)
			}
			tc.cause(t, err)
			if n := c.startCalls(); n != 0 {
				t.Errorf("Provider.Start called %d times, want none", n)
			}
			after, err := c.store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"generation", "instance_token"} {
				if after.Metadata[key] != before.Metadata[key] {
					t.Errorf("row %s = %q, want the unchanged %q", key, after.Metadata[key], before.Metadata[key])
				}
			}
		})
	}
}
