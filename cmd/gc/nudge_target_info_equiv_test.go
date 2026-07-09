package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestNudgeTargetFromSessionInfoGolden pins the behavior of
// resolveNudgeTargetFromSessionInfo directly. It began life as an equivalence
// oracle against the now-deleted raw-bead sibling
// (resolveNudgeTargetFromSessionBead); that oracle completed its migration
// purpose once the dispatcher and resolveNudgeTarget both moved onto the Info
// path, so this test now hard-codes the goldens the oracle proved.
//
// The transport cases still guard the fidelity trap: the resolver reads the RAW
// transport metadata (via i.TransportMetadata), not the normalized i.Transport,
// so the empty- and whitespace-transport beads must still take the found.Session
// fallback ("tmux"). ga-no-sn / ga-bare guard the sessionNameFromBeadID fallback.
func TestNudgeTargetFromSessionInfoGolden(t *testing.T) {
	cityPath := "/tmp/test-city"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude", Session: "tmux"},
		},
	}

	cases := []struct {
		name                  string
		bead                  beads.Bead
		wantSessionID         string
		wantSessionName       string
		wantIdentity          string
		wantAlias             string
		wantAliasHistory      []string
		wantTransport         string
		wantProvider          string
		wantContinuationEpoch string
	}{
		{
			name: "full",
			bead: beads.Bead{
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
			wantSessionID:         "ga-full",
			wantSessionName:       "worker-session",
			wantIdentity:          "frontend/worker-1",
			wantAlias:             "worker-alias",
			wantAliasHistory:      []string{"old-alias", "older-alias"},
			wantTransport:         "acp",
			wantProvider:          "claude",
			wantContinuationEpoch: "3",
		},
		{
			// Empty transport → resolver must fall back to the agent's Session.
			name: "empty-transport",
			bead: beads.Bead{
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
			wantSessionID:   "ga-empty-transport",
			wantSessionName: "empty-transport-session",
			wantIdentity:    "worker",
			wantTransport:   "tmux",
			wantProvider:    "claude",
		},
		{
			// Whitespace transport → TrimSpace on the raw value yields "", so the
			// found.Session fallback fires identically to the empty case.
			name: "ws-transport",
			bead: beads.Bead{
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
			wantSessionID:   "ga-ws-transport",
			wantSessionName: "ws-transport-session",
			wantIdentity:    "worker",
			wantTransport:   "tmux",
			wantProvider:    "claude",
		},
		{
			// No session_name → sessionNameFromBeadID fallback; unknown template
			// resolves no agent, so transport/provider stay their raw (empty) values.
			name: "no-session-name",
			bead: beads.Bead{
				ID:     "ga-no-sn",
				Type:   session.BeadType,
				Title:  "no-sn",
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"template": "scribe",
				},
			},
			wantSessionID:   "ga-no-sn",
			wantSessionName: "s-ga-no-sn",
			wantIdentity:    "scribe",
		},
		{
			// Bare metadata, only an ID → identity falls through to the session name.
			name: "bare",
			bead: beads.Bead{
				ID:       "ga-bare",
				Type:     session.BeadType,
				Title:    "bare",
				Labels:   []string{session.LabelSession},
				Metadata: map[string]string{},
			},
			wantSessionID:   "ga-bare",
			wantSessionName: "s-ga-bare",
			wantIdentity:    "s-ga-bare",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveNudgeTargetFromSessionInfo(cityPath, cfg, session.InfoFromPersistedBead(tc.bead))

			if got.sessionID != tc.wantSessionID {
				t.Errorf("sessionID = %q, want %q", got.sessionID, tc.wantSessionID)
			}
			if got.sessionName != tc.wantSessionName {
				t.Errorf("sessionName = %q, want %q", got.sessionName, tc.wantSessionName)
			}
			if got.identity != tc.wantIdentity {
				t.Errorf("identity = %q, want %q", got.identity, tc.wantIdentity)
			}
			if got.alias != tc.wantAlias {
				t.Errorf("alias = %q, want %q", got.alias, tc.wantAlias)
			}
			if !reflect.DeepEqual(got.aliasHistory, tc.wantAliasHistory) {
				t.Errorf("aliasHistory = %#v, want %#v", got.aliasHistory, tc.wantAliasHistory)
			}
			if got.transport != tc.wantTransport {
				t.Errorf("transport = %q, want %q", got.transport, tc.wantTransport)
			}
			provider := ""
			if got.resolved != nil {
				provider = got.resolved.Name
			}
			if provider != tc.wantProvider {
				t.Errorf("resolved provider = %q, want %q", provider, tc.wantProvider)
			}
			if got.continuationEpoch != tc.wantContinuationEpoch {
				t.Errorf("continuationEpoch = %q, want %q", got.continuationEpoch, tc.wantContinuationEpoch)
			}
		})
	}
}

// TestNudgeTargetFromSessionInfoFullGolden pins the ENTIRE nudgeTarget for the
// richest bead via reflect.DeepEqual, so a regression in any field buildNudgeTarget
// derives — including the ones the field-level cases above do not assert
// (cityPath, cityName, cfg wiring, and the parsed agent value) — is still caught.
func TestNudgeTargetFromSessionInfoFullGolden(t *testing.T) {
	cityPath := "/tmp/test-city"
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "claude"},
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude", Session: "tmux"},
		},
	}
	b := beads.Bead{
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
	}

	got := resolveNudgeTargetFromSessionInfo(cityPath, cfg, session.InfoFromPersistedBead(b))
	want := nudgeTarget{
		cityPath:          "/tmp/test-city",
		cityName:          "test-city",
		cfg:               cfg,
		alias:             "worker-alias",
		aliasHistory:      []string{"old-alias", "older-alias"},
		identity:          "frontend/worker-1",
		transport:         "acp",
		agent:             config.Agent{Name: "worker-1", Dir: "frontend"},
		resolved:          &config.ResolvedProvider{Name: "claude"},
		sessionID:         "ga-full",
		continuationEpoch: "3",
		sessionName:       "worker-session",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("full nudgeTarget mismatch:\n got=%#v\nwant=%#v", got, want)
	}
}
