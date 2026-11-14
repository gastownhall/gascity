package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// sessionRuntimeTarget captures the public identity and runtime session name
// needed by session-facing CLI commands.
type sessionRuntimeTarget struct {
	cityPath    string
	display     string
	sessionName string
	sessionID   string // resolved session bead ID; empty for the env-derived current session
}

func defaultSessionDisplayIdentity() string {
	for _, key := range []string{"GC_ALIAS", "GC_SESSION_ID", "GC_AGENT"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return ""
}

func currentSessionRuntimeTarget() (sessionRuntimeTarget, error) {
	display := defaultSessionDisplayIdentity()
	if display == "" {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (GC_ALIAS/GC_SESSION_ID not set)")
	}
	sessionName := strings.TrimSpace(os.Getenv("GC_TMUX_SESSION"))
	if sessionName == "" {
		sessionName = strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	}
	if sessionName == "" {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (GC_SESSION_NAME not set)")
	}
	cityPath, ok := resolveExplicitCityPathEnv()
	if !ok {
		if cityPath, ok = resolveCityPathFromGCDir(); !ok {
			cityPath, ok = resolveCityPathFromCwd()
		}
	}
	if !ok {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (city context not set)")
	}
	if err := gateControlPlaneAt(cityPath); err != nil {
		return sessionRuntimeTarget{}, err
	}
	return sessionRuntimeTarget{
		cityPath:    cityPath,
		display:     display,
		sessionName: sessionName,
	}, nil
}

func resolveSessionRuntimeTarget(identifier string, warningWriter ...io.Writer) (sessionRuntimeTarget, error) {
	target, err := resolveNudgeTarget(identifier, warningWriter...)
	if err != nil {
		return sessionRuntimeTarget{}, err
	}
	return sessionRuntimeTargetFromNudge(target), nil
}

// resolveSessionRuntimeTargetInCity resolves identifier in the city at
// cityPath with cfg. Unlike resolveSessionRuntimeTarget, a store that will not
// open is returned as that failure, never as session.ErrSessionNotFound, so a
// not-found error here means a store that opened has no such session. stderr
// receives the report of a configured named session the resolution
// materialized for the reconciler to start; nil discards it.
func resolveSessionRuntimeTargetInCity(cityPath string, cfg *config.City, identifier string, stderr io.Writer) (sessionRuntimeTarget, error) {
	store, opened, err := openNudgeBeadStoreOwned(cityPath)
	if err != nil {
		return sessionRuntimeTarget{}, err
	}
	defer closeBeadStoreHandle(opened) //nolint:errcheck // best-effort
	if store.Store == nil {
		return sessionRuntimeTarget{}, fmt.Errorf("opening the city store at %q: no store", cityPath)
	}
	target, err := resolveNudgeTargetInStore(cityPath, cfg, store, identifier, false, stderr)
	if err != nil {
		return sessionRuntimeTarget{}, err
	}
	return sessionRuntimeTargetFromNudge(target), nil
}

func sessionRuntimeTargetFromNudge(target nudgeTarget) sessionRuntimeTarget {
	display := target.agentKey()
	if display == "" {
		display = target.sessionName
	}
	return sessionRuntimeTarget{
		cityPath:    target.cityPath,
		display:     display,
		sessionName: target.sessionName,
		sessionID:   target.sessionID,
	}
}
