//go:build acceptance_c

package workerinference_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	workerpkg "github.com/gastownhall/gascity/internal/worker"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// profileUsesHookSessionKeyPersistence reports whether a profile's provider
// resume key is persisted by the provider hook plugin at runtime (plugin →
// `gc prime --hook` → city store metadata) instead of being generated and
// passed by the session manager at create time. Those profiles need a real
// on-disk city behind the live handle harness so the gc child spawned by the
// plugin and the in-process session manager share the same bead store.
//
// The set mirrors the hook-managed providers in
// internal/worker/provider_resume_test.go (hook-time GC_PROVIDER_SESSION_ID
// is authoritative; no derived resume keys): opencode, mimocode, kimi, pi,
// and antigravity. omp has no live harness profile today.
func profileUsesHookSessionKeyPersistence(profile workerpkg.Profile) bool {
	switch profile {
	case workerpkg.ProfileOpenCodeTmuxCLI,
		workerpkg.ProfileMimoCodeTmuxCLI,
		workerpkg.ProfileKimiTmuxCLI,
		workerpkg.ProfilePiTmuxCLI,
		workerpkg.ProfileAntigravityTmuxCLI:
		return true
	default:
		return false
	}
}

// stageLiveHandleCity turns the harness root into a real city using the
// staged gc binary (`gc init --file`, the documented non-interactive path
// for pre-authored configs), then opens the city's bead store in-process for
// the harness session manager.
//
// The init template pins [beads] provider = "file" so every gc child invoked
// by the provider hook plugin (`gc prime --hook`, `gc hook --inject`, ...)
// resolves the exact same scope-local file store the harness opens here. It
// declares the probe agent (no [[named_session]] entry) so `gc prime --hook`
// renders the probe instructions instead of the generic claim-work prompt,
// without giving the supervisor anything to spawn.
//
// `gc init` registers the city with a supervisor; the harness drives its
// session itself, so the supervisor-managed runtime is torn down immediately,
// leaving only the on-disk city and its store.
func stageLiveHandleCity(env *helpers.Env, cityDir, provider string) (beads.Store, error) {
	gcHome := strings.TrimSpace(env.Get("GC_HOME"))
	if gcHome == "" {
		return nil, fmt.Errorf("staging live handle city: GC_HOME is not set")
	}
	// gc init registers the city with the (bare-forked) supervisor; reserve
	// an isolated port so it never collides with a host supervisor.
	if err := helpers.WriteSupervisorConfig(gcHome); err != nil {
		return nil, fmt.Errorf("staging live handle city: writing supervisor config: %w", err)
	}

	cityToml := fmt.Sprintf(`[workspace]
name = %q
provider = %q

[beads]
provider = "file"

[providers.%s]
base = "builtin:%s"

[[agent]]
name = %q
prompt_template = "agents/%s/prompt.template.md"
`, filepath.Base(cityDir), provider, provider, provider, inferenceProbeTemplate, inferenceProbeTemplate)
	// The template lives outside the city: gc init --file splits it into
	// pack.toml ([[agent]]) and city.toml itself; a pre-authored city.toml
	// would leave the probe agent declared twice.
	cityTomlPath := filepath.Join(gcHome, "live-handle-city.toml")
	if err := os.WriteFile(cityTomlPath, []byte(cityToml), 0o644); err != nil {
		return nil, fmt.Errorf("staging live handle city: writing init template: %w", err)
	}
	promptPath := filepath.Join(cityDir, "agents", inferenceProbeTemplate, "prompt.template.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		return nil, fmt.Errorf("staging live handle city: %w", err)
	}
	if err := os.WriteFile(promptPath, []byte(strings.TrimSpace(workerHandleProbeInstructions)+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("staging live handle city: writing probe prompt: %w", err)
	}

	restoreHome := restoreRealHomeDuring(env)
	defer restoreHome()
	initOut, initErr := runGCWithTimeout(liveBootstrapTimeout, env, "",
		"init", "--file", cityTomlPath, "--skip-provider-readiness", cityDir)
	if initErr != nil {
		return nil, fmt.Errorf("staging live handle city: gc init: %w\n%s", initErr, strings.TrimSpace(initOut))
	}
	teardownLiveHandleCityRuntime(env, cityDir)

	return openLiveHandleCityStore(cityDir)
}

// restoreRealHomeDuring temporarily points the env's HOME back at the real
// user home and returns the undo func. gc's platform supervisor refuses HOME
// overrides ("use GC_HOME for isolated runs"), but some provider harness envs
// (antigravity) override HOME for the provider session itself — supervisor
// lifecycle calls must run with the real HOME while the session keeps the
// override.
func restoreRealHomeDuring(env *helpers.Env) func() {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return func() {}
	}
	current := env.Get("HOME")
	if current == "" || current == realHome {
		return func() {}
	}
	env.With("HOME", realHome)
	return func() { env.With("HOME", current) }
}

// teardownLiveHandleCityRuntime best-effort stops the supervisor-managed
// runtime that gc init started for the harness city. The harness owns the
// session lifecycle; a reconciling controller sharing the same bead store
// would fight the test's stop/restart sequence.
func teardownLiveHandleCityRuntime(env *helpers.Env, cityDir string) {
	restoreHome := restoreRealHomeDuring(env)
	defer restoreHome()
	_, _ = runGCWithTimeout(liveShutdownTimeout, env, cityDir, "stop", cityDir)
	_, _ = runGCWithTimeout(liveShutdownTimeout, env, cityDir, "unregister", cityDir)
	_, _ = runGCWithTimeout(liveShutdownTimeout, env, "", "supervisor", "stop", "--wait")
}

// openLiveHandleCityStore opens the same scope-local file store gc itself
// opens for this city: cmd/gc's openStoreResultAtForCity resolves the
// city.toml [beads] provider ("file"), and its file branch opens
// <city>/.gc/beads.json guarded by a flock sidecar (openScopeLocalFileStore).
// Mirroring that here means the harness manager and every gc child operate
// on one store, which is the normal multi-process production mode.
func openLiveHandleCityStore(cityDir string) (beads.Store, error) {
	beadsPath := filepath.Join(cityDir, ".gc", "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		return nil, fmt.Errorf("opening live handle city store: %w", err)
	}
	store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))
	return store, nil
}

// closeLiveHandleStore releases in-process store resources when the store
// implementation exposes a store-level closer (the beads package convention
// is Shutdown; beads.Store's own Close(id) closes a bead, not the store).
// The scope-local file store holds no long-lived handles, so this is a
// no-op for it today.
func closeLiveHandleStore(store beads.Store) {
	if s, ok := store.(interface{ Shutdown() error }); ok {
		_ = s.Shutdown()
	}
}
