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
