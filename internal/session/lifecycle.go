package session

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// DefaultGeneration is the first runtime epoch for a newly created session.
	DefaultGeneration = 1

	// DefaultContinuationEpoch is the first conversation identity epoch.
	DefaultContinuationEpoch = 1
)

// NewInstanceToken returns a cryptographically random token for fencing
// drain/stop and async delivery against stale session incarnations.
func NewInstanceToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// RuntimeEnv returns the per-incarnation environment variables a live session
// runtime should receive from the controller/session manager.
func RuntimeEnv(sessionID, sessionName string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := map[string]string{
		"GC_SESSION_ID":         sessionID,
		"GC_SESSION_NAME":       sessionName,
		"GC_RUNTIME_EPOCH":      strconv.Itoa(generation),
		"GC_CONTINUATION_EPOCH": strconv.Itoa(continuationEpoch),
		"GC_INSTANCE_TOKEN":     instanceToken,
	}
	return env
}

// RuntimeEnvWithAlias extends RuntimeEnv with the public session alias.
// Alias-aware commands use GC_ALIAS as their canonical mailbox/target
// identity; an explicit empty value clears stale template defaults.
func RuntimeEnvWithAlias(sessionID, sessionName, alias string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := RuntimeEnv(sessionID, sessionName, generation, continuationEpoch, instanceToken)
	env["GC_ALIAS"] = alias
	return env
}

// RuntimeEnvWithSessionContext extends RuntimeEnvWithAlias with the
// session-model context shared by controller, CLI, and API starts.
//
// GC_AGENT and BEADS_ACTOR share the same alias-first ownership identity so
// `bd update --claim` stamps work.Assignee with a form the session's work
// query already polls via $GC_ALIAS / $GC_SESSION_NAME. A sanitized-only
// BEADS_ACTOR (e.g. bd__dog-<beadID>) was the #5048 stranding: claims under a
// form later pool respawns no longer export.
func RuntimeEnvWithSessionContext(sessionID, sessionName, alias, template, origin string, generation, continuationEpoch int, instanceToken string) map[string]string {
	env := RuntimeEnvWithAlias(sessionID, sessionName, alias, generation, continuationEpoch, instanceToken)
	if template != "" {
		env["GC_TEMPLATE"] = template
	}
	if origin != "" {
		env["GC_SESSION_ORIGIN"] = origin
	}
	identity := strings.TrimSpace(alias)
	if identity == "" {
		identity = strings.TrimSpace(sessionName)
	}
	if identity != "" {
		env["GC_AGENT"] = identity
		env["BEADS_ACTOR"] = identity
	}
	return env
}

// SyncRuntimeAlias updates the live runtime session metadata to reflect the
// current public alias. Clearing the alias removes GC_ALIAS from the runtime.
func SyncRuntimeAlias(sp runtime.Provider, sessionName, alias string) error {
	if sp == nil || sessionName == "" {
		return nil
	}
	if alias == "" {
		return sp.RemoveMeta(sessionName, "GC_ALIAS")
	}
	return sp.SetMeta(sessionName, "GC_ALIAS", alias)
}
