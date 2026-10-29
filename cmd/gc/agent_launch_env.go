// agent_launch_env.go builds the generated agent launch environment — the
// layer the create-time resolver (resolveTemplate) composes and a resume
// start must regenerate identically when it launches a missing runtime
// (#8184). The two halves mirror resolveTemplate's steps:
// agentScopeLaunchEnv is Step 8's agent-scope keys (everything except the six
// session identity keys, which a resume layers over it from the persisted
// record), and composeAgentLaunchEnv is Step 10's layer merge plus the Step
// 10b upstream axis and the usage-metrics and controller-only pins.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/processenv"
)

// agentLaunchEnvInput describes the agent the generated launch env is built
// for. resolveTemplate fills it from its build params; the resume-time
// launch-env resolver recomputes the same fields from the persisted session
// record and the city config, so both paths generate the layer from one
// description of the agent.
type agentLaunchEnvInput struct {
	CityPath      string
	Rigs          []config.Rig
	Workspace     *config.Workspace
	Upstreams     map[string]config.UpstreamSpec
	Agent         *config.Agent
	Resolved      *config.ResolvedProvider
	QualifiedName string
	WorkDir       string
	Rig, RigRoot  string
}

// agentScopeLaunchEnv builds the generated agent-scope launch layer: GC_DIR,
// the scope roots, the city runtime env, the per-dispatcher trace default,
// GC_BEADS, GC_BIN, the session backend projection, and the rig overrides.
// It is resolveTemplate's Step 8 without the six session identity keys
// (GC_SESSION_NAME, GC_SESSION_ID, GC_TEMPLATE, GC_SESSION_ORIGIN, GC_AGENT,
// GC_ALIAS), which a resume start layers over this from the persisted record
// via startEnv. The backend projection error is returned unwrapped; each
// caller adds its context.
func agentScopeLaunchEnv(in agentLaunchEnvInput) (map[string]string, error) {
	env := map[string]string{
		"GC_DIR":              in.WorkDir,
		"GC_BEADS_SCOPE_ROOT": in.CityPath,
		// Explicit empty values matter here. tmux session creation uses `env -u`
		// only for keys present with empty strings, which prevents stale rig
		// scope from leaking out of the tmux server's inherited environment.
		"GC_RIG":      "",
		"GC_RIG_ROOT": "",
		"BEADS_DIR":   "",
		// GT_ROOT stays city-scoped by default. bd formula discovery falls back
		// to $GT_ROOT/.beads/formulas when agents run outside the city/rig repo
		// roots (for example under .gc/agents/... or .gc/worktrees/...).
		// Rig-scoped agents override the rig-specific keys below.
		"GT_ROOT": in.CityPath,
	}
	for key, value := range cityRuntimeEnvMapForCity(in.CityPath) {
		env[key] = value
	}
	// Override the city-uniform GC_CONTROL_DISPATCHER_TRACE_DEFAULT with a
	// per-dispatcher path so each control-dispatcher writes to its own
	// trace file (closes #1650). Goes last in the merge order so it wins
	// over both the baseline and the uniform city default seeded by
	// cityRuntimeEnvMapForCity above.
	if in.Agent != nil && in.Agent.Name == config.ControlDispatcherAgentName {
		env["GC_CONTROL_DISPATCHER_TRACE_DEFAULT"] = citylayout.ControlDispatcherTraceDefaultPathForRuntimeDirAndName(
			in.CityPath,
			env["GC_CITY_RUNTIME_DIR"],
			in.QualifiedName,
		)
	}
	env["GC_BEADS"] = rawBeadsProviderForScope(in.RigRoot, in.CityPath)
	if exe, err := os.Executable(); err == nil && exe != "" {
		env["GC_BIN"] = exe
	}
	sessionBackendEnv, err := sessionBackendEnvWithError(in.CityPath, in.RigRoot, in.Rigs)
	if err != nil {
		return nil, err
	}
	for key, value := range sessionBackendEnv {
		env[key] = value
	}
	if in.Rig != "" {
		env["GC_RIG"] = in.Rig
		env["GC_RIG_ROOT"] = in.RigRoot
		env["BEADS_DIR"] = filepath.Join(in.RigRoot, ".beads")
		env["GC_BEADS_SCOPE_ROOT"] = in.RigRoot
	}
	return env, nil
}

// composeAgentLaunchEnv merges the generated layer over a baseline with the
// operator-authored layers between them — baseline < workspace < provider <
// agent < generated — then applies the upstream axis and the pinned
// overlays. It is resolveTemplate's Step 10 merge, Step 10b upstream block,
// the usage-metrics pin and the controller-only re-pin, moved verbatim;
// create passes passthroughEnv() as the baseline (the controller GC_* sweep),
// a resume start passes providerProcessPassthroughEnv() so the caller's GC_*
// context is not swept into the session. Returns the session env and the
// operator-authored overlay (workspace, provider, agent — token-scrubbed)
// that fingerprints config env changes.
func composeAgentLaunchEnv(baseline, generated map[string]string, in agentLaunchEnvInput) (env, operatorEnv map[string]string, err error) {
	var workspaceEnv map[string]string
	if in.Workspace != nil {
		workspaceEnv = in.Workspace.Env
	}
	var providerEnv map[string]string
	if in.Resolved != nil {
		providerEnv = in.Resolved.Env
	}
	var agentCfgEnv map[string]string
	if in.Agent != nil {
		agentCfgEnv = in.Agent.Env
	}
	env = mergeEnv(baseline, expandEnvMap(workspaceEnv), expandEnvMap(providerEnv), expandEnvMap(agentCfgEnv), generated)
	processenv.PrependGCBinDirToPATH(env, env["GC_BIN"])
	env = convergence.ScrubTokenEnv(env)

	// OperatorEnv carries only the operator-authored layers (workspace,
	// resolved provider, agent) — excluding passthrough and the generated
	// agentEnv plumbing — so a resolved config env change fingerprints as
	// Launch-tier identity instead of a no-op.
	operatorEnv = mergeEnv(expandEnvMap(workspaceEnv), expandEnvMap(providerEnv), expandEnvMap(agentCfgEnv))
	operatorEnv = convergence.ScrubTokenEnv(operatorEnv)

	// Step 10b: Upstream axis (Phase C). Inject the selected upstream's serving
	// env LAST so it is authoritative for the model-serving keys, and after
	// ScrubTokenEnv so its credential refs survive — which is exactly why the
	// controller-only re-pin below has to come after this block. The env-ref
	// values ($VAR) resolve from the controller environment via expandEnvMap;
	// the resolved credentials are NOT fingerprinted (the Config.Env
	// allow-list excludes them), only the selected NAME — carried to
	// runtime.Config.Upstream (launch-half) so switching upstream relaunches
	// the agent in the warm box.
	if in.Agent != nil {
		if upstreamName := in.Agent.Upstream; upstreamName != "" {
			spec, ok := in.Upstreams[upstreamName]
			if !ok {
				return nil, nil, fmt.Errorf("agent %q selects upstream %q which is not declared in [upstreams]", in.QualifiedName, upstreamName)
			}
			// Render the abstract serving fields onto the agent's HARNESS env-var
			// names (the resolved provider's upstream_env binding), so one upstream is
			// portable across harnesses. An abstract field with no matching binding is
			// a hard error — never a silent no-op (config-surface §4). $VAR refs in the
			// values resolve from the controller environment.
			if spec.HasAbstractServing() {
				var binding config.UpstreamEnvBinding
				if in.Resolved != nil {
					binding = in.Resolved.UpstreamEnv
				}
				// Per field, the target env-var name is the upstream's override if set
				// (for gateway harnesses), else the harness binding, else a hard error.
				for _, r := range []struct{ value, override, bound, field string }{
					{spec.BaseURL, spec.BaseURLEnv, binding.BaseURL, "base_url"},
					{spec.APIKey, spec.APIKeyEnv, binding.APIKey, "api_key"},
					{spec.AuthToken, spec.AuthTokenEnv, binding.AuthToken, "auth_token"},
				} {
					if r.value == "" {
						continue
					}
					envName := r.override
					if envName == "" {
						envName = r.bound
					}
					if envName == "" {
						return nil, nil, fmt.Errorf("agent %q upstream %q sets %s, but its harness %q declares no upstream_env.%s binding (set %s_env on the upstream, or upstream_env.%s on the harness)", in.QualifiedName, upstreamName, r.field, resolvedProviderName(in.Resolved), r.field, r.field, r.field)
					}
					env[envName] = processenv.ExpandSessionEnvValue(r.value)
				}
			}
			// Raw env is the harness-specific escape hatch, merged LAST (wins over the
			// abstract render and ambient/agent env for the keys it sets).
			for k, v := range expandEnvMap(spec.Env) {
				env[k] = v
			}
		}
	}
	// Managed agents are Gas City-owned recursive execution environments. Set
	// the GC-only opt-out after configurable layers so child gc commands cannot
	// re-enable product metrics; Beads telemetry remains independent.
	env[execenv.UsageMetricsDisableEnv] = execenv.UsageMetricsDisableValue
	// Same placement, same reason, for the controller-only keys: every layer
	// above is config-authored, so a literal [workspace.env] GC_CONTROLLER_TOKEN
	// — or an upstream whose api_key_env names it — would otherwise overwrite the
	// empty value the baseline pinned, and the upstream block writes after
	// the ScrubTokenEnv call. Re-pinning after the last writer makes the merge
	// order irrelevant. Empty, not deleted: the session inherits an environment
	// that already carries the controller's value, so only an explicit empty
	// assignment overrides it.
	for key, val := range processenv.ControllerOnlyEnvOverlay() {
		env[key] = val
	}
	return env, operatorEnv, nil
}
