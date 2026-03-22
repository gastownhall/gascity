package main

import (
	"fmt"
	"os"
	"strings"
)

// sessionRuntimeTarget captures the public identity and runtime session name
// needed by session-facing CLI commands.
type sessionRuntimeTarget struct {
	cityPath    string
	display     string
	sessionName string
}

func currentSessionRuntimeTarget() (sessionRuntimeTarget, error) {
	cityPath := strings.TrimSpace(os.Getenv("GC_CITY"))
	if cityPath == "" {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (GC_CITY not set)")
	}
	display := defaultMailIdentity()
	if display == "human" {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (GC_ALIAS/GC_SESSION_ID not set)")
	}
	sessionName := strings.TrimSpace(os.Getenv("GC_TMUX_SESSION"))
	if sessionName == "" {
		sessionName = strings.TrimSpace(os.Getenv("GC_SESSION_NAME"))
	}
	if sessionName == "" {
		return sessionRuntimeTarget{}, fmt.Errorf("not in session context (GC_SESSION_NAME not set)")
	}
	return sessionRuntimeTarget{
		cityPath:    cityPath,
		display:     display,
		sessionName: sessionName,
	}, nil
}

func resolveSessionRuntimeTarget(identifier string) (sessionRuntimeTarget, error) {
	target, err := resolveNudgeTarget(identifier)
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
		sessionName: target.sessionName,
	}, nil
}
