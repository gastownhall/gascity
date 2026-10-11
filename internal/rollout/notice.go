package rollout

// Origin names the precedence layer that produced a resolved value.
type Origin string

const (
	// OriginBuiltin means the gate was absent everywhere; Spec.Default was used.
	OriginBuiltin Origin = "builtin"
	// OriginConfig means the value came from merged config; env unset/inapplicable.
	OriginConfig Origin = "config"
	// OriginEnv means an env override produced the value.
	OriginEnv Origin = "env"
)

// NoticeKind names a typed resolution/lifecycle fact worth surfacing.
type NoticeKind string

const (
	// NoticeEnvOverrideActive records that a valid env value was applied while
	// config was silent — informational.
	NoticeEnvOverrideActive NoticeKind = "env_override_active"
	// NoticeEnvOverridesConfig records that a valid env value CONTRADICTS an
	// explicit config value — surfaced loudly so an operator's break-glass is not
	// mistaken for the durable config.
	NoticeEnvOverridesConfig NoticeKind = "env_overrides_config"
	// NoticeInvalidEnvIgnored records that a malformed env value was ignored and
	// the config-resolved value kept (warn-and-use-config; never refuse-to-start).
	NoticeInvalidEnvIgnored NoticeKind = "invalid_env_ignored"
	// NoticePendingRestart records that the on-disk config diverged from the
	// boot-latched value. The type ships now; the reload wiring that emits it
	// lands with the composition-root wiring (PR-1c).
	NoticePendingRestart NoticeKind = "pending_restart"
)

// Notice is one typed, structured resolution fact. Notices are retained ON the
// Flags value for the process lifetime and rendered by doctor/status later —
// never a dropped stderr line.
type Notice struct {
	Kind        NoticeKind
	FlagKey     string // Spec.Key
	EnvVar      string // Spec.EnvOverride when env-related, else ""
	ConfigValue string // raw config spelling ("" = unset)
	EnvValue    string // raw env spelling as found
	Message     string // human line, always carrying the gate and the outcome
}
