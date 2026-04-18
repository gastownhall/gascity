package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/doltauth"
)

func applyResolvedDoltAuthEnv(env map[string]string, authScopeRoot, fallbackUser string) {
	if env == nil {
		return
	}
	auth := doltauth.ResolveFromEnv(authScopeRoot, fallbackUser, env)
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
