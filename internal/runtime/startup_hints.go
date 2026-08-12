package runtime

// HasManagedStartupHints reports whether cfg asks the runtime to perform
// managed startup work beyond fire-and-forget session creation.
func HasManagedStartupHints(cfg Config) bool {
	return cfg.ReadyPromptPrefix != "" ||
		cfg.ReadyDelayMs > 0 ||
		len(cfg.ProcessNames) > 0 ||
		cfg.EmitsPermissionWarning ||
		cfg.AcceptStartupDialogs != nil ||
		cfg.Nudge != "" ||
		len(cfg.PreStart) > 0 ||
		len(cfg.SessionSetup) > 0 ||
		cfg.SessionSetupScript != "" ||
		len(cfg.SessionLive) > 0
}

// ShouldAcceptStartupDialogs reports whether startup dialog handling is
// explicitly enabled or inferred from managed process hints.
func ShouldAcceptStartupDialogs(cfg Config) bool {
	if cfg.AcceptStartupDialogs != nil {
		return *cfg.AcceptStartupDialogs
	}
	return len(cfg.ProcessNames) > 0 || cfg.EmitsPermissionWarning
}
