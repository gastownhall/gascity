package runtime

// Session environment reaches several providers as process arguments — the
// local tmux adapter and the ssh provider both build
// `new-session ... -e KEY=VALUE`. On Linux /proc/<pid>/cmdline is world-readable
// (mode 0444) and a tmux server outlives the session it hosts, so every value
// passed that way is legible to any local user for weeks.
// /proc/<pid>/environ, by contrast, is owner-only, and so is a 0600 file:
// credentials belong on one of those paths, never in argv.
//
// envArgvSafe is an ALLOW list, deliberately, for the same reason
// envFingerprintAllow (fingerprint.go) is one: a deny list of known-secret
// names silently leaks the next secret someone introduces. A name that is not
// listed here is assumed to carry credential material and is routed through the
// provider's private-file path. Misclassifying an inert variable as secret
// costs nothing but a temp file — the values arrive identically either way — so
// when in doubt, leave the name off.
//
// Only names whose values are structurally incapable of authenticating
// anything belong here: locale and terminal settings, identities, paths, and
// counters.
var envArgvSafe = map[string]bool{
	// Locale and terminal.
	"COLORTERM":   true,
	"LANG":        true,
	"LANGUAGE":    true,
	"LC_ALL":      true,
	"LC_COLLATE":  true,
	"LC_CTYPE":    true,
	"LC_MESSAGES": true,
	"LC_NUMERIC":  true,
	"LC_TIME":     true,
	"TERM":        true,
	"TZ":          true,

	// City, rig, and agent identity. BEADS_ACTOR carries the same identity
	// string as GC_AGENT (session.RuntimeEnvWithSessionContext sets both from
	// AssigneeIdentifier), so listing one and not the other would route an
	// already-public value through the private-file path and blank it out of
	// diagnostics for nothing.
	"BEADS_ACTOR":      true,
	"GC_AGENT":         true,
	"GC_ALIAS":         true,
	"GC_CITY":          true,
	"GC_CITY_PATH":     true,
	"GC_PROVIDER":      true,
	"GC_RIG":           true,
	"GC_RIG_ROOT":      true,
	"GC_TEMPLATE":      true,
	"GT_CREW":          true,
	"GT_RIG":           true,
	"GT_ROLE":          true,
	"GT_PROCESS_NAMES": true,

	// Session identity and epoch counters. GC_INSTANCE_TOKEN is NOT here: it
	// fences drain/stop and async delivery against a stale incarnation, so it
	// is a capability, not an identifier.
	"GC_CONTINUATION_EPOCH":  true,
	"GC_READY_PROMPT_PREFIX": true,
	"GC_RUNTIME_EPOCH":       true,
	"GC_SESSION_ID":          true,
	"GC_SESSION_NAME":        true,
	"GC_SESSION_ORIGIN":      true,

	// Derived paths and discovery.
	"BEADS_DIR":          true,
	"GC_AGENT_SLICE":     true,
	"GC_BIN":             true,
	"GC_BLESSED_BIN_DIR": true,
	"GC_DIR":             true,
	"GC_DOLT_PORT":       true,
	"GC_HOME":            true,
	"GC_SKILLS_DIR":      true,
}

// metaCapabilityEnv names the values a provider's private meta sidecar must
// persist even though argv refuses them.
//
// GC_INSTANCE_TOKEN is the only member, and it is here for the same reason it
// is absent from envArgvSafe: it fences drain/stop and async delivery against a
// stale incarnation, so it is a capability rather than an identifier. What makes
// it different from every other capability is that the fence's ground truth
// lives in the store — [Provider.GetMeta] is how a caller learns which
// incarnation it is talking to, and every consumer compares it that way.
//
// So refusing to persist it would not break fencing loudly; it would disable it
// silently. The consumers guard with `actual != "" && actual != expected`, which
// treats an absent token as permission to proceed. A credential that reaches
// disk is a bounded, mitigable exposure; a fence that reports success while
// enforcing nothing is not.
//
// Nothing else belongs here. A key earns membership only by having a real
// GetMeta consumer that cannot be served by an argv-safe value.
var metaCapabilityEnv = map[string]bool{
	"GC_INSTANCE_TOKEN": true,
}

// ArgvSafeEnvKey reports whether the value of the named environment variable
// may appear in a process argument vector. See [envArgvSafe].
func ArgvSafeEnvKey(key string) bool { return envArgvSafe[key] }

// MetaSeedableEnvKey reports whether the named variable may be seeded into a
// provider's meta sidecar, which is a private on-disk store rather than argv.
//
// It tracks [envArgvSafe] plus [metaCapabilityEnv] rather than carrying a list
// of its own: the store's threat model is strictly weaker than argv's (0600
// files under 0700 dirs, not a world-readable /proc entry), so anything argv
// tolerates the sidecar tolerates. Deriving it keeps one maintained
// classification and one conservative default — an unrecognized name is assumed
// to carry credential material and is withheld.
func MetaSeedableEnvKey(key string) bool {
	return ArgvSafeEnvKey(key) || metaCapabilityEnv[key]
}

// SplitEnvForMetaSeed partitions env into the entries a provider may persist in
// its meta sidecar and the entries it must not. Both maps are non-nil so callers
// can range over them without a nil check.
//
// An empty value carries no secret, so it seeds like any inert entry — the same
// rule [SplitEnvByArgvSafety] applies, and for the same reason: providers spell
// "withhold this variable" as an empty value.
func SplitEnvForMetaSeed(env map[string]string) (seed, withheld map[string]string) {
	seed = make(map[string]string, len(env))
	withheld = make(map[string]string)
	for k, v := range env {
		if v != "" && !MetaSeedableEnvKey(k) {
			withheld[k] = v
			continue
		}
		seed[k] = v
	}
	return seed, withheld
}

// ArgvSecretEnvValue reports whether this key/value pair must be kept out of
// argv. An empty value carries no secret — providers spell "withhold this
// variable" that way (see sessionEnvUnsetKeys in the tmux adapter) — so it
// never forces the private-file path. This is the single predicate every caller
// shares; do not re-derive it.
func ArgvSecretEnvValue(key, value string) bool {
	return value != "" && !ArgvSafeEnvKey(key)
}

// EnvHasArgvSecrets reports whether env carries any value that must be kept out
// of argv.
func EnvHasArgvSecrets(env map[string]string) bool {
	for k, v := range env {
		if ArgvSecretEnvValue(k, v) {
			return true
		}
	}
	return false
}

// SplitEnvByArgvSafety partitions env into the entries whose values may travel
// in argv and the entries that must not. Both maps are non-nil so callers can
// range over them without a nil check.
func SplitEnvByArgvSafety(env map[string]string) (safe, secret map[string]string) {
	safe = make(map[string]string, len(env))
	secret = make(map[string]string)
	for k, v := range env {
		if ArgvSecretEnvValue(k, v) {
			secret[k] = v
			continue
		}
		safe[k] = v
	}
	return safe, secret
}
