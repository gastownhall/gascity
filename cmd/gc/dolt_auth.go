package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/doltauth"
)

// applyResolvedDoltAuthEnv resolves the scope's Dolt auth into env. Its
// ambient BEADS_* fallbacks read through the native-open env guard: a
// concurrent scope's native open projects its own credentials file into the
// process env for the length of the open.
func applyResolvedDoltAuthEnv(env map[string]string, authScopeRoot, fallbackUser string) {
	if env == nil {
		return
	}
	auth := doltauth.ResolveScopedFromEnv(authScopeRoot, fallbackUser, env, ambientNativeDoltOpenEnv)
	applyResolvedAuthValue(env, "GC_DOLT_USER", auth.User)
	applyResolvedAuthValue(env, "GC_DOLT_PASSWORD", auth.Password)
	applyResolvedAuthValue(env, "BEADS_CREDENTIALS_FILE", auth.CredentialsFileOverride)
}

func applyResolvedAuthValue(env map[string]string, key, value string) {
	if env == nil {
		return
	}
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		env[key] = trimmed
		return
	}
	delete(env, key)
}
