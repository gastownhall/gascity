package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// The tests in this file cover gastownhall/gascity#8184: a configured pool
// session first launched by the reconciler carries a generated launch
// environment (GC_BIN and its PATH entry, rig scope, backend projection,
// upstream serving env, the pins); a runtime recreated through a message,
// attach or start must regenerate that layer instead of starting with the
// reduced handle env.

// rigEnvReproCity is the shared harness: a written city, its loaded config,
// its store and a fake runtime.
type rigEnvReproCity struct {
	cityDir string
	cfg     *config.City
	store   beads.Store
	sp      *runtime.Fake
}

func newRigEnvReproCity(t *testing.T, cityTOML string) *rigEnvReproCity {
	t.Helper()
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, cityTOML)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return &rigEnvReproCity{cityDir: cityDir, cfg: cfg, store: store, sp: runtime.NewFake()}
}

// rigScopedCityTOML is a rig-scoped pool city: agent worker lives in rig
// myrig whose checkout is outside the city directory.
func rigScopedCityTOML(rigRoot string) string {
	return fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "myrig"
path = %q

[[agent]]
name = "worker"
dir = "myrig"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`, rigRoot)
}

// resumeRigEnvSession creates a started session for template whose runtime is
// then stopped while its record remains — the #8184 scenario — and resumes it
// through op ("message", "attach" or "start"). It returns the session record
// and the replacement start's env.
func resumeRigEnvSession(t *testing.T, c *rigEnvReproCity, template, workDir, op string) (session.Info, map[string]string) {
	t.Helper()
	ctx := context.Background()
	mgr := newSessionManagerWithConfig(c.cityDir, c.store, c.sp, c.cfg)
	info, err := mgr.CreateSession(ctx, session.CreateOptions{
		Template: template,
		Title:    "Probe",
		Command:  "stub-agent",
		WorkDir:  workDir,
		Provider: "stub",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		ExtraMeta: map[string]string{"session_origin": "ephemeral"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(ctx, testActorIn(t, session.ActorOperator, c.cityDir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	handle, err := workerHandleForSessionWithConfig(c.cityDir, c.store, c.sp, c.cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}
	switch op {
	case "message":
		_, err = handle.Message(ctx, testActorIn(t, session.ActorOperator, c.cityDir), worker.MessageRequest{Text: "hello"})
	case "attach":
		err = handle.Attach(ctx, testActorIn(t, session.ActorOperator, c.cityDir))
	case "start":
		err = handle.Start(ctx, testActorIn(t, session.ActorOperator, c.cityDir))
	default:
		t.Fatalf("unknown resume op %q", op)
	}
	if err != nil {
		t.Fatalf("resume through %s: %v", op, err)
	}
	start := c.sp.LastStartConfig(info.SessionName)
	if start == nil {
		t.Fatalf("LastStartConfig(%q) = nil, want a replacement start", info.SessionName)
	}
	return info, start.Env
}

// TestResolvedWorkerRuntimeRegeneratesRigScopedLaunchEnv is AC-1: a
// rig-scoped pool session the reconciler launched, whose runtime is gone
// while its record remains, is messaged; the replacement start's env carries
// the generated launch layer.
func TestResolvedWorkerRuntimeRegeneratesRigScopedLaunchEnv(t *testing.T) {
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	c := newRigEnvReproCity(t, rigScopedCityTOML(rigRoot))
	workDir := filepath.Join(c.cityDir, ".gc", "worktrees", "task")

	info, env := resumeRigEnvSession(t, c, "myrig/worker", workDir, "message")

	if got := env["GC_SESSION_NAME"]; got != info.SessionName {
		t.Errorf("env[GC_SESSION_NAME] = %q, want the persisted %q", got, info.SessionName)
	}
	for key, want := range map[string]string{
		"GC_DIR":              workDir,
		"GC_RIG":              "myrig",
		"GC_RIG_ROOT":         rigRoot,
		"BEADS_DIR":           filepath.Join(rigRoot, ".beads"),
		"GC_BEADS_SCOPE_ROOT": rigRoot,
		"GT_ROOT":             c.cityDir,
		// The backend projection: bd's Dolt auto-start stays suppressed.
		"BEADS_DOLT_AUTO_START": "0",
	} {
		if got := env[key]; got != want {
			t.Errorf("env[%s] = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"GC_BIN", "GC_BEADS"} {
		if env[key] == "" {
			t.Errorf("env[%s] missing/empty, want the generated launch value", key)
		}
	}
	if gcBin, path := env["GC_BIN"], env["PATH"]; !strings.HasPrefix(path, filepath.Dir(gcBin)) {
		t.Errorf("env[PATH] = %q, want it to begin with GC_BIN's dir %q", path, filepath.Dir(gcBin))
	}
	if got, ok := env["GC_DOLT_MANAGED_LOCAL"]; !ok || got != "" {
		t.Errorf("env[GC_DOLT_MANAGED_LOCAL] = %q (present %v), want present-empty", got, ok)
	}
}

// TestResumedSessionAttachAndStartCarryGeneratedLaunchEnv is AC-2: the same
// stopped-runtime session resumed through Attach and through Start starts
// with the same generated env as the message path.
func TestResumedSessionAttachAndStartCarryGeneratedLaunchEnv(t *testing.T) {
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	c := newRigEnvReproCity(t, rigScopedCityTOML(rigRoot))
	workDir := filepath.Join(c.cityDir, ".gc", "worktrees", "task")

	for _, op := range []string{"attach", "start"} {
		_, env := resumeRigEnvSession(t, c, "myrig/worker", workDir, op)
		for key, want := range map[string]string{
			"GC_RIG":              "myrig",
			"GC_RIG_ROOT":         rigRoot,
			"GC_BEADS_SCOPE_ROOT": rigRoot,
			"BEADS_DIR":           filepath.Join(rigRoot, ".beads"),
			"GT_ROOT":             c.cityDir,
		} {
			if got := env[key]; got != want {
				t.Errorf("%s: env[%s] = %q, want %q", op, key, got, want)
			}
		}
		if env["GC_BIN"] == "" || env["GC_BEADS"] == "" {
			t.Errorf("%s: env GC_BIN/GC_BEADS missing, want the generated launch values", op)
		}
	}
}

// TestResumedSessionRigIdentityComesFromConfiguredAgent is AC-3: the rig's
// checkout lives outside the city, the session's recorded work_dir outside
// the rig checkout, and the caller environment carries a different rig —
// the resumed start's rig scope comes from the configured agent and the
// city's rig config, never from the caller.
func TestResumedSessionRigIdentityComesFromConfiguredAgent(t *testing.T) {
	rigRoot := filepath.Join(t.TempDir(), "myrig-checkout")
	c := newRigEnvReproCity(t, rigScopedCityTOML(rigRoot))
	workDir := filepath.Join(t.TempDir(), "task-worktree")

	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_RIG_ROOT", filepath.Join(t.TempDir(), "caller-rig-root"))
	t.Setenv("BEADS_DIR", filepath.Join(t.TempDir(), "caller-beads"))
	t.Setenv("GC_BEADS_SCOPE_ROOT", filepath.Join(t.TempDir(), "caller-scope"))

	_, env := resumeRigEnvSession(t, c, "myrig/worker", workDir, "message")

	for key, want := range map[string]string{
		"GC_DIR":              workDir,
		"GC_RIG":              "myrig",
		"GC_RIG_ROOT":         rigRoot,
		"BEADS_DIR":           filepath.Join(rigRoot, ".beads"),
		"GC_BEADS_SCOPE_ROOT": rigRoot,
	} {
		if got := env[key]; got != want {
			t.Errorf("env[%s] = %q, want %q from the configured agent and rig config", key, got, want)
		}
	}
	for _, key := range []string{"GC_RIG", "GC_RIG_ROOT", "BEADS_DIR", "GC_BEADS_SCOPE_ROOT"} {
		if strings.Contains(env[key], "caller") {
			t.Errorf("env[%s] = %q, want no caller environment value", key, env[key])
		}
	}
}

// TestResumedCityScopedSessionClearsStaleRigEnv is AC-4: a city-scoped
// agent session resumed with a stale caller GC_RIG / GC_RIG_ROOT / BEADS_DIR
// starts with those keys present but empty (so the runtime unsets them) and
// the scope root pinned to the city.
func TestResumedCityScopedSessionClearsStaleRigEnv(t *testing.T) {
	c := newRigEnvReproCity(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)

	t.Setenv("GC_RIG", "caller-rig")
	t.Setenv("GC_RIG_ROOT", "/tmp/caller-rig-root")
	t.Setenv("BEADS_DIR", "/tmp/caller-beads")

	workDir := filepath.Join(c.cityDir, ".gc", "worktrees", "task")
	_, env := resumeRigEnvSession(t, c, "worker", workDir, "message")

	for _, key := range []string{"GC_RIG", "GC_RIG_ROOT", "BEADS_DIR"} {
		got, ok := env[key]
		if !ok || got != "" {
			t.Errorf("env[%s] = %q (present %v), want present-empty so the runtime unsets it", key, got, ok)
		}
	}
	if got := env["GC_BEADS_SCOPE_ROOT"]; got != c.cityDir {
		t.Errorf("env[GC_BEADS_SCOPE_ROOT] = %q, want the city path %q", got, c.cityDir)
	}
	if got := env["GT_ROOT"]; got != c.cityDir {
		t.Errorf("env[GT_ROOT] = %q, want the city path %q", got, c.cityDir)
	}
}

// TestResumedDispatcherSessionKeepsPerDispatcherTracePath is AC-5: a
// rig-qualified control-dispatcher session and an ordinary agent session
// resumed in the same city each get their own trace default — the
// dispatcher's per-dispatcher-qualified path (the rig-qualified name maps to
// its own trace file), the ordinary agent's the city-uniform value
// resolveTemplate gives it.
func TestResumedDispatcherSessionKeepsPerDispatcherTracePath(t *testing.T) {
	appRig := filepath.Join(t.TempDir(), "app-checkout")
	c := newRigEnvReproCity(t, fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "app"
path = %q

[[agent]]
name = "control-dispatcher"
dir = "app"
provider = "stub"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`, appRig))
	cityEnv := cityRuntimeEnvMapForCity(c.cityDir)
	runtimeDir := cityEnv["GC_CITY_RUNTIME_DIR"]
	uniform := cityEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]
	if uniform == "" {
		t.Fatal("cityRuntimeEnvMapForCity carries no GC_CONTROL_DISPATCHER_TRACE_DEFAULT to compare against")
	}

	workDir := filepath.Join(c.cityDir, ".gc", "worktrees", "task")
	_, dispatcherEnv := resumeRigEnvSession(t, c, "app/control-dispatcher", workDir, "message")

	wantDispatcher := citylayout.ControlDispatcherTraceDefaultPathForRuntimeDirAndName(c.cityDir, runtimeDir, "app/control-dispatcher")
	if got := dispatcherEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != wantDispatcher {
		t.Errorf("dispatcher env[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q, want the per-dispatcher path %q", got, wantDispatcher)
	}
	if got := dispatcherEnv["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got == uniform {
		t.Errorf("dispatcher trace default is the city-uniform %q, want per-dispatcher qualified", got)
	}

	_, env := resumeRigEnvSession(t, c, "worker", workDir, "message")
	if got := env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"]; got != uniform {
		t.Errorf("ordinary env[GC_CONTROL_DISPATCHER_TRACE_DEFAULT] = %q, want the city-uniform %q resolveTemplate gives it", got, uniform)
	}
}

// TestResumedSessionEnvPrecedenceAndPins is AC-6: a key set at each of the
// workspace, provider, agent, generated and upstream layers resolves to the
// highest layer in the order baseline < workspace < provider < agent <
// generated < upstream < pinned overlays; the controller token stays
// empty, the usage-metrics opt-out is set, and the identity keys equal the
// persisted record's values.
func TestResumedSessionEnvPrecedenceAndPins(t *testing.T) {
	c := newRigEnvReproCity(t, `[workspace]
name = "test-city"

[workspace.env]
SHARED = "ws"
WS_ONLY = "ws"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
upstream = "local"
env = { SHARED = "agent", AGENT_ONLY = "agent", GT_ROOT = "agent-gt" }

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"

[providers.stub.env]
SHARED = "provider"
PROVIDER_ONLY = "provider"

[upstreams.local]

[upstreams.local.env]
SHARED = "upstream"
GC_CONTROLLER_TOKEN = "leaked-secret"
`)
	t.Setenv("GC_CONTROLLER_TOKEN", "caller-controller-token")

	workDir := filepath.Join(c.cityDir, ".gc", "worktrees", "task")
	info, env := resumeRigEnvSession(t, c, "worker", workDir, "message")

	for key, want := range map[string]string{
		// baseline < workspace < provider < agent < generated < upstream:
		// the upstream layer wins the collision chain.
		"SHARED":        "upstream",
		"WS_ONLY":       "ws",
		"PROVIDER_ONLY": "provider",
		"AGENT_ONLY":    "agent",
		// The generated layer outranks agent-authored env.
		"GT_ROOT": c.cityDir,
		// Pinned overlays: the caller's and the upstream's controller
		// token never reach the runtime.
		"GC_CONTROLLER_TOKEN": "",
		// The usage-metrics opt-out is part of the generated layer.
		execenv.UsageMetricsDisableEnv: execenv.UsageMetricsDisableValue,
		// Identity keys come from the persisted record.
		"GC_SESSION_NAME": info.SessionName,
		"GC_SESSION_ID":   info.ID,
		"GC_TEMPLATE":     info.Template,
		"GC_ALIAS":        info.Alias,
	} {
		if got := env[key]; got != want {
			t.Errorf("env[%s] = %q, want %q", key, got, want)
		}
	}
}

// TestResumedSessionLaunchEnvErrorRefusesStart is AC-7: an agent that
// selects an undeclared upstream makes the launch-env build fail; the
// message refuses with an error naming the session and wrapping the cause,
// Provider.Start is never called, the row's incarnation is unchanged, and
// building the handle plus a non-start read (State) still succeed.
func TestResumedSessionLaunchEnvErrorRefusesStart(t *testing.T) {
	c := newRigEnvReproCity(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"
upstream = "missing"

[providers.stub]
command = "/bin/echo"
resume_flag = "--resume"
resume_style = "flag"
session_id_flag = "--session-id"
`)
	ctx := context.Background()
	mgr := newSessionManagerWithConfig(c.cityDir, c.store, c.sp, c.cfg)
	info, err := mgr.CreateSession(ctx, session.CreateOptions{
		Template: "worker",
		Title:    "Probe",
		Command:  "stub-agent",
		WorkDir:  filepath.Join(c.cityDir, ".gc", "worktrees", "task"),
		Provider: "stub",
		Resume: session.ProviderResume{
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
		ExtraMeta: map[string]string{"session_origin": "ephemeral"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(ctx, testActorIn(t, session.ActorOperator, c.cityDir), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	rowBefore, err := c.store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get before the message: %v", err)
	}

	c.sp.Calls = nil
	handle, err := workerHandleForSessionWithConfig(c.cityDir, c.store, c.sp, c.cfg, info.ID)
	if err != nil {
		t.Fatalf("workerHandleForSessionWithConfig: %v", err)
	}
	_, err = handle.Message(ctx, testActorIn(t, session.ActorOperator, c.cityDir), worker.MessageRequest{Text: "hello"})
	if err == nil {
		t.Fatal("Message = nil error, want the refused start")
	}
	if !strings.Contains(err.Error(), info.ID) {
		t.Errorf("Message err = %q, want it to name session %q", err, info.ID)
	}
	if !strings.Contains(err.Error(), "not declared in [upstreams]") {
		t.Errorf("Message err = %q, want it to wrap the undeclared-upstream cause", err)
	}
	for _, call := range c.sp.Calls {
		if call.Method == "Start" {
			t.Fatalf("provider calls = %#v, want no Start after the refused resume", c.sp.Calls)
		}
	}
	rowAfter, err := c.store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get after the message: %v", err)
	}
	for _, key := range []string{"generation", "instance_token"} {
		if got, want := rowAfter.Metadata[key], rowBefore.Metadata[key]; got != want {
			t.Errorf("row %s = %q after the refused start, want unchanged %q", key, got, want)
		}
	}

	// Handle building and a non-start read still succeed: the launch-env
	// build runs only when a start must launch a runtime.
	freshHandle, err := workerHandleForSessionWithConfig(c.cityDir, c.store, c.sp, c.cfg, info.ID)
	if err != nil {
		t.Fatalf("rebuild workerHandleForSessionWithConfig: %v", err)
	}
	if _, err := freshHandle.State(ctx); err != nil {
		t.Errorf("handle.State: %v, want a non-start read to succeed", err)
	}
}
