package convergence

import "strings"

// ProtectedPrefix is the metadata key prefix requiring a controller token.
const ProtectedPrefix = "convergence."

// agentWritableKeys is the set of convergence.* keys that agents can
// write without a controller token.
var agentWritableKeys = map[string]bool{
	FieldAgentVerdict:     true,
	FieldAgentVerdictWisp: true,
}

// RequiresToken reports whether writing to the given metadata key
// requires a controller token. Returns true for convergence.* keys
// except the agent-writable verdict keys, and for var.* keys.
func RequiresToken(key string) bool {
	if strings.HasPrefix(key, VarPrefix) {
		return true
	}
	if !strings.HasPrefix(key, ProtectedPrefix) {
		return false
	}
	return !agentWritableKeys[key]
}

// ScrubTokenEnv returns a copy of the environment map with the controller token
// variable PINNED TO THE EMPTY STRING. Used when spawning agent sessions.
//
// Deleting the key would withhold nothing. The map is an overlay on an
// environment the session already inherits, not the whole environment: a tmux
// pane starts from the tmux server's global env, and the subprocess/ACP
// runtimes exec with os.Environ() plus the overlay appended. On each of those,
// an absent key means the controller's real token shows through — the exact
// escalation this function exists to prevent. Only an explicit empty assignment
// overrides an inherited value.
//
// On tmux the empty value drives two mechanisms, because a pane is started more
// than once: an `env -u GC_CONTROLLER_TOKEN` prefix on the initial exec, and a
// `set-environment -r` marker on the session so the warm-box relaunch path
// (respawn-pane, which takes no env argument) starts the agent without it too.
//
// The key is also in processenv.ControllerOnlyEnvKeys, which pins it into the
// session-env baseline; this is the token-specific spelling of the same
// invariant for callers that finalize an already-merged map.
func ScrubTokenEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out[TokenEnvVar] = ""
	return out
}
