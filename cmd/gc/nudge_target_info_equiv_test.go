package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestNudgeTargetInfoEquivalence is the byte-identical oracle for migrating the
// nudge dispatcher off raw session beads. resolveNudgeTargetFromSessionInfo must
// produce exactly the nudgeTarget that resolveNudgeTargetFromSessionBead does for
// the same bead once projected through InfoFromPersistedBead. The transport cases
// specifically guard the fidelity trap: the resolver reads the RAW transport
// metadata (via i.TransportMetadata), not the normalized i.Transport, so the
// empty/whitespace-transport beads must still take the found.Session fallback.
func TestNudgeTargetInfoEquivalence(t *testing.T) {
	cityPath := "/tmp/test-city"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude", Session: "tmux"},
		},
	}

	beadsIn := []beads.Bead{
		{
			ID:     "ga-full",
			Type:   session.BeadType,
			Title:  "full",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":           "frontend/worker",
				"agent_name":         "frontend/worker-1",
				"common_name":        "the-worker",
				"alias":              "worker-alias",
				"provider":           "claude",
				"transport":          "acp",
				"session_name":       "worker-session",
				"continuation_epoch": "3",
				"alias_history":      "old-alias,older-alias",
			},
		},
		{
			// Empty transport → resolver must fall back to the agent's Session.
			ID:     "ga-empty-transport",
			Type:   session.BeadType,
			Title:  "empty-transport",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"provider":     "claude",
				"session_name": "empty-transport-session",
			},
		},
		{
			// Whitespace transport → TrimSpace on both raw and Info must agree.
			ID:     "ga-ws-transport",
			Type:   session.BeadType,
			Title:  "ws-transport",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "worker",
				"transport":    "   ",
				"provider":     "claude",
				"session_name": "ws-transport-session",
			},
		},
		{
			// No session_name → sessionNameFromBeadID fallback on both sides.
			ID:     "ga-no-sn",
			Type:   session.BeadType,
			Title:  "no-sn",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "scribe",
			},
		},
		{
			// Bare metadata, only an ID.
			ID:       "ga-bare",
			Type:     session.BeadType,
			Title:    "bare",
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{},
		},
	}

	for _, b := range beadsIn {
		want := resolveNudgeTargetFromSessionBead(cityPath, cfg, b)
		got := resolveNudgeTargetFromSessionInfo(cityPath, cfg, session.InfoFromPersistedBead(b))
		if !reflect.DeepEqual(want, got) {
			t.Errorf("bead %q: resolveNudgeTargetFromSessionInfo = %#v, want (bead form) %#v", b.ID, got, want)
		}
	}
}
