package execenv

import "strings"

// SessionScopedEnvKeys are the per-session identity variables the session
// lifecycle injects into an agent runtime, plus the agent provider's own
// session id and a hook's deadline. Any gc process run from an agent's shell
// inherits them. A long-lived process gc starts for the city or for a
// runtime (a managed Dolt server and its scope watchdog, an exec order, a
// runtime pack's op and any server it leaves running) belongs to no session,
// so it is started without them (WithoutSessionIdentity): a re-parented
// process carrying an agent's GC_SESSION_ID is that session's target for the
// orphan sweep, which would end it once the session closes, and a pack-side
// identity scan keys on GC_INSTANCE_TOKEN. A hook's deadline would be stale
// for anything such a process spawns after the hook that started it.
var SessionScopedEnvKeys = []string{
	"GC_HOOK_DEADLINE",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_ALIAS",
	"GC_AGENT",
	"GC_TEMPLATE",
	"GC_SESSION_ORIGIN",
	"GC_RUNTIME_EPOCH",
	"GC_CONTINUATION_EPOCH",
	"GC_INSTANCE_TOKEN",
	"BEADS_HOLDER_TOKEN",
	"BEADS_ACTOR",
	"CLAUDE_CODE_SESSION_ID",
}

// WithoutSessionIdentity returns a copy of env without any
// SessionScopedEnvKeys entry. env is not modified.
func WithoutSessionIdentity(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if isSessionScoped(key) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func isSessionScoped(key string) bool {
	for _, k := range SessionScopedEnvKeys {
		if k == key {
			return true
		}
	}
	return false
}
