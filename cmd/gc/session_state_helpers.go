package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

func isDrainedSessionMetadata(meta map[string]string) bool {
	state := strings.TrimSpace(meta["state"])
	if state == "drained" {
		return true
	}
	return state == "asleep" && strings.TrimSpace(meta["sleep_reason"]) == "drained"
}

func isDrainedSessionBead(session beads.Bead) bool {
	return isDrainedSessionMetadata(session.Metadata)
}
