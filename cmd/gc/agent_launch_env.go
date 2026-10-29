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

// agentLaunchEnvInput is what the generated layer of an agent's launch
// environment is built from. resolveTemplate builds it when a session is
// created, and the worker's launch-env resolver rebuilds it when a session
// whose runtime is gone starts again, so both launches get the same layer.
type agentLaunchEnvInput struct {
	CityPath      string
	Rigs          []config.Rig
	Workspace     *config.Workspace
	Upstreams     map[string]config.UpstreamSpec
	Agent         *config.Agent
	Resolved      *config.ResolvedProvider
	QualifiedName string
	WorkDir       string
	Rig           string
	RigRoot       string
}

// agentScopeLaunchEnv returns the generated, scope-derived keys of an agent's
// launch environment: the work dir, the city runtime env, the rig or city bead
// scope, GC_BIN and the session backend projection. It carries no session
// identity keys; the caller adds those.
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
	// trace file (closes #1650). The generated layer merges last, so it
	// wins over both the passthrough env and the uniform city default
	// seeded by cityRuntimeEnvMapForCity above.
	if in.Agent.Name == config.ControlDispatcherAgentName {
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

// composeAgentLaunchEnv layers an agent's launch environment: baseline, then
// the workspace, provider and agent env, then the generated layer, then the
// selected upstream, and finally the pins no config layer may override. It
// also returns the operator-authored layers alone, which fingerprint the
// launch.
func composeAgentLaunchEnv(baseline, generated map[string]string, in agentLaunchEnvInput) (env, operatorEnv map[string]string, err error) {
	// Workspace.Env sits between the baseline and the provider so a
	// per-provider/agent/patch entry can still override a workspace-wide
	// default.
	var workspaceEnv map[string]string
	if in.Workspace != nil {
		workspaceEnv = in.Workspace.Env
	}
	var resolvedEnv map[string]string
	if in.Resolved != nil {
		resolvedEnv = in.Resolved.Env
	}
	env = mergeEnv(baseline, expandEnvMap(workspaceEnv), expandEnvMap(resolvedEnv), expandEnvMap(in.Agent.Env), generated)
	processenv.PrependGCBinDirToPATH(env, env["GC_BIN"])
	env = convergence.ScrubTokenEnv(env)

	// OperatorEnv carries only the operator-authored layers (workspace,
	// resolved provider, agent) — excluding the baseline and the generated
	// plumbing — so a resolved config env change fingerprints as
	// Launch-tier identity instead of a no-op.
	operatorEnv = mergeEnv(expandEnvMap(workspaceEnv), expandEnvMap(resolvedEnv), expandEnvMap(in.Agent.Env))
	operatorEnv = convergence.ScrubTokenEnv(operatorEnv)

	// Upstream axis (Phase C). Inject the selected upstream's serving env LAST
	// so it is authoritative for the model-serving keys, and after
	// ScrubTokenEnv so its credential refs survive — which is exactly why the
	// controller-only re-pin below has to come after this block. The env-ref
	// values ($VAR) resolve from the controller environment via expandEnvMap;
	// the resolved credentials are NOT fingerprinted (the Config.Env
	// allow-list excludes them), only the selected NAME — carried to
	// runtime.Config.Upstream (launch-half) so switching upstream relaunches
	// the agent in the warm box.
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
	// Managed agents are Gas City-owned recursive execution environments. Set
	// the GC-only opt-out after configurable layers so child gc commands cannot
	// re-enable product metrics; Beads telemetry remains independent.
	env[execenv.UsageMetricsDisableEnv] = execenv.UsageMetricsDisableValue

	// Same placement, same reason, for the controller-only keys: every layer
	// above is config-authored, so a literal [workspace.env] GC_CONTROLLER_TOKEN
	// — or an upstream whose api_key_env names it — would otherwise overwrite the
	// empty value the baseline pinned, and the upstream block writes after the
	// ScrubTokenEnv call. Re-pinning after the last writer makes the merge order
	// irrelevant. Empty, not deleted: the session inherits an environment that
	// already carries the controller's value, so only an explicit empty
	// assignment overrides it.
	for key, val := range processenv.ControllerOnlyEnvOverlay() {
		env[key] = val
	}
	return env, operatorEnv, nil
}
