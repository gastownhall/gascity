package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// sessionRuntimeTarget captures the public identity and runtime session name
// needed by session-facing CLI commands.
type sessionRuntimeTarget struct {
	cityPath               string
	display                string
	sessionID              string
	sessionName            string
	requesterSessionID     string
	requesterInstanceToken string
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
	return sessionRuntimeTarget{
		cityPath:               cityPath,
		display:                display,
		sessionID:              strings.TrimSpace(os.Getenv("GC_SESSION_ID")),
		sessionName:            sessionName,
		requesterSessionID:     strings.TrimSpace(os.Getenv("GC_SESSION_ID")),
		requesterInstanceToken: strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN")),
	}, nil
}

func bindExplicitDrainAckRequester(target sessionRuntimeTarget, requesterSessionID, requesterInstanceToken string) (sessionRuntimeTarget, error) {
	requesterSessionID = strings.TrimSpace(requesterSessionID)
	requesterInstanceToken = strings.TrimSpace(requesterInstanceToken)
	if requesterSessionID == "" {
		return target, nil
	}
	if target.sessionID == "" || target.sessionID != requesterSessionID {
		return sessionRuntimeTarget{}, fmt.Errorf("refusing cross-session drain acknowledgement from %q to %q", requesterSessionID, target.sessionID)
	}
	target.requesterSessionID = requesterSessionID
	target.requesterInstanceToken = requesterInstanceToken
	return target, nil
}

func resolveSessionRuntimeTarget(identifier string, warningWriter ...io.Writer) (sessionRuntimeTarget, error) {
	target, err := resolveNudgeTarget(identifier, warningWriter...)
	if err != nil {
		return sessionRuntimeTarget{}, err
	}
	display := target.agentKey()
	if display == "" {
		display = target.sessionName
	}
	return sessionRuntimeTarget{
		cityPath:    target.cityPath,
		display:     display,
		sessionID:   target.sessionID,
		sessionName: target.sessionName,
	}, nil
}
