package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
		// BEADS_HOLDER_TOKEN is the incarnation credential bd records on a claim
		// (ownership-fencing DESIGN §2.4). It IS the instance token — deriving it
		// here, the single authoritative wiring point, keeps the token a session
		// presents to bd identical to the one the controller persists, so a stale
		// incarnation cannot pass as the current owner. Set unconditionally (even
		// when empty, mirroring GC_INSTANCE_TOKEN) so a fresh incarnation never
		// inherits a parent's stale holder token.
		"BEADS_HOLDER_TOKEN": instanceToken,
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

// RuntimeEnvWithSessionContext projects one session Info into the runtime
// identity environment shared by controller, CLI, and API starts. Keeping the
// runtime handle and raw persisted identity on Info lets AssigneeIdentifier
// choose the exact same ownership string as API assignment normalization,
// including the bead-ID fallback for a repairable session with no stored name.
func RuntimeEnvWithSessionContext(info Info, generation, continuationEpoch int, instanceToken string) map[string]string {
	publicAlias := runtimePublicAlias(info)
	env := RuntimeEnvWithAlias(info.ID, info.SessionName, publicAlias, generation, continuationEpoch, instanceToken)
	if template := strings.TrimSpace(info.Template); template != "" {
		env["GC_TEMPLATE"] = template
	}
	if origin := strings.TrimSpace(info.SessionOrigin); origin != "" {
		env["GC_SESSION_ORIGIN"] = origin
	}
	if identity := AssigneeIdentifier(info); identity != "" {
		env["GC_AGENT"] = identity
		env["BEADS_ACTOR"] = identity
	}
	return env
}

func runtimePublicAlias(info Info) string {
	if alias := strings.TrimSpace(info.Alias); alias != "" {
		return alias
	}
	return strings.TrimSpace(info.ConfiguredNamedIdentity)
}

// SyncRuntimeAlias updates live runtime metadata from the session's current
// public alias and durable ownership identity. Clearing an alias therefore
// falls back to the same raw session name or bead ID that API assignment uses.
func SyncRuntimeAlias(sp runtime.Provider, info Info) error {
	sessionName := strings.TrimSpace(info.SessionName)
	if sp == nil || sessionName == "" {
		return nil
	}
	type metaUpdate struct {
		key    string
		value  string
		remove bool
	}
	publicAlias := runtimePublicAlias(info)
	identity := AssigneeIdentifier(info)
	updates := []metaUpdate{
		{key: "GC_ALIAS", value: publicAlias, remove: publicAlias == ""},
		{key: "GC_AGENT", value: identity, remove: identity == ""},
		{key: "BEADS_ACTOR", value: identity, remove: identity == ""},
	}
	previous := make(map[string]string, len(updates))
	for _, update := range updates {
		value, err := sp.GetMeta(sessionName, update.key)
		if err != nil {
			return fmt.Errorf("reading %s before runtime identity update: %w", update.key, err)
		}
		previous[update.key] = value
	}
	apply := func(update metaUpdate) error {
		if update.remove {
			return sp.RemoveMeta(sessionName, update.key)
		}
		return sp.SetMeta(sessionName, update.key, update.value)
	}
	applied := make([]metaUpdate, 0, len(updates))
	for _, update := range updates {
		if err := apply(update); err != nil {
			errs := []error{fmt.Errorf("updating runtime identity %s: %w", update.key, err)}
			for i := len(applied) - 1; i >= 0; i-- {
				rollback := metaUpdate{key: applied[i].key, value: previous[applied[i].key], remove: previous[applied[i].key] == ""}
				if rollbackErr := apply(rollback); rollbackErr != nil {
					errs = append(errs, fmt.Errorf("rolling back runtime identity %s: %w", rollback.key, rollbackErr))
				}
			}
			return errors.Join(errs...)
		}
		applied = append(applied, update)
	}
	return nil
}
