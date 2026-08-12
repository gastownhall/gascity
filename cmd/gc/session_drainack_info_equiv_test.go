package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// refRunningSessionMatchesPendingCreate is the raw-metadata reference
// implementation of the runningSessionMatchesPendingCreate classifier whose
// production raw form was deleted in WI-6 R4. It is inlined here so the Info twin
// is pinned against an independent bead read (self-sufficient oracle, not a
// tautological Info-vs-Info compare).
func refRunningSessionMatchesPendingCreate(b beads.Bead, sessionName string, sp runtime.Provider) bool {
	if sp == nil {
		return false
	}
	liveID := ""
	if value, err := sp.GetMeta(sessionName, "GC_SESSION_ID"); err == nil {
		liveID = strings.TrimSpace(value)
		if liveID != "" && liveID != b.ID {
			return false
		}
	}
	expectedToken := strings.TrimSpace(b.Metadata["instance_token"])
	liveToken := ""
	if value, err := sp.GetMeta(sessionName, "GC_INSTANCE_TOKEN"); err == nil {
		liveToken = strings.TrimSpace(value)
		if liveToken != "" && liveToken != expectedToken {
			liveGeneration, _ := sp.GetMeta(sessionName, "GC_RUNTIME_EPOCH")
			expectedGeneration := strings.TrimSpace(b.Metadata["generation"])
			if strings.TrimSpace(liveGeneration) != "" && expectedGeneration != "" && strings.TrimSpace(liveGeneration) != expectedGeneration {
				return false
			}
			if liveID == "" {
				return false
			}
		}
	}
	if liveID != "" {
		return liveID == b.ID
	}
	if expectedToken == "" {
		return false
	}
	return expectedToken != "" && liveToken == expectedToken
}

// TestDrainAckClassifierInfoEquivalence is the byte-identical oracle for the
// drain-ack runtime-meta family (WI-5 W2 §3.2). These classifiers can't ride the
// pure func(beads.Bead) maps in TestSessionClassifierInfoEquivalence because they
// also take a runtime.Provider (and, for assignedWorkDrainCancelReason, a
// drainTracker): the only session-bead reads are generation / instance_token / id
// / session_name, all carried verbatim on Info. This test drives each raw↔Info
// pair through both the true and false provider branches so the proof is not a
// trivial both-false pass.
func TestDrainAckClassifierInfoEquivalence(t *testing.T) {
	const (
		ackSourceKey     = reconcilerDrainAckSourceKey
		ackReasonKey     = reconcilerDrainAckReasonKey
		ackGenerationKey = reconcilerDrainAckGenerationKey
	)

	// providerMeta seeds a fresh Fake with the drain-ack env for a session name.
	newProvider := func(name string, meta map[string]string) *runtime.Fake {
		sp := runtime.NewFake()
		for k, v := range meta {
			if err := sp.SetMeta(name, k, v); err != nil {
				t.Fatalf("SetMeta %s=%s: %v", k, v, err)
			}
		}
		return sp
	}

	shapes := map[string]beads.Bead{
		"gen-3": {
			ID:       "ga-gen3",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"template": "worker", "session_name": "worker-gen3", "generation": "3", "instance_token": "tok-3"},
		},
		"gen-padded": {
			// " 3 " exercises the TrimSpace read on the ack-compare path (matches
			// expectedGeneration "3") AND the untrimmed Atoi read elsewhere — Info
			// must preserve the raw bytes verbatim.
			ID:       "ga-genpad",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"template": "worker", "session_name": "worker-genpad", "generation": " 3 ", "instance_token": "tok-pad"},
		},
		"gen-empty": {
			ID:       "ga-genempty",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"template": "worker", "session_name": "worker-genempty", "instance_token": "tok-empty"},
		},
	}

	// providerCases exercises the match / mismatch / legacy / absent provider
	// states so both branches of every classifier fire.
	providerCases := []struct {
		name string
		meta func(sessName string) map[string]string
	}{
		{"reconciler-ack-gen-3", func(string) map[string]string {
			return map[string]string{ackSourceKey: reconcilerDrainAckSourceValue, ackReasonKey: "orphaned", ackGenerationKey: "3", "GC_DRAIN_ACK": "1"}
		}},
		{"reconciler-ack-gen-mismatch", func(string) map[string]string {
			return map[string]string{ackSourceKey: reconcilerDrainAckSourceValue, ackReasonKey: "orphaned", ackGenerationKey: "99", "GC_DRAIN_ACK": "1"}
		}},
		{"reconciler-ack-no-generation", func(string) map[string]string {
			return map[string]string{ackSourceKey: reconcilerDrainAckSourceValue, ackReasonKey: "config-drift"}
		}},
		{"agent-ack", func(string) map[string]string {
			return map[string]string{ackSourceKey: drainAckSourceAgentValue, "GC_DRAIN_ACK": "1"}
		}},
		{"legacy-ack-only", func(string) map[string]string {
			return map[string]string{"GC_DRAIN_ACK": "1"}
		}},
		{"no-ack", func(string) map[string]string { return nil }},
		{"running-match", func(string) map[string]string {
			return map[string]string{"GC_INSTANCE_TOKEN": "tok-3", "GC_SESSION_ID": ""}
		}},
		{"running-id-match", func(string) map[string]string {
			return map[string]string{"GC_SESSION_ID": "ga-gen3"}
		}},
	}

	for shape, b := range shapes {
		b := b
		info := sessiontest.SeedBead(t, b)
		name := b.Metadata["session_name"]
		for _, pc := range providerCases {
			t.Run(shape+"/"+pc.name, func(t *testing.T) {
				sp := newProvider(name, pc.meta(name))

				rawReason, rawOK := reconcilerDrainAckMatchesSession(b, sp, name)
				infoReason, infoOK := reconcilerDrainAckMatchesSessionInfo(info, sp, name)
				if rawReason != infoReason || rawOK != infoOK {
					t.Errorf("reconcilerDrainAckMatchesSession: info=(%q,%v) bead=(%q,%v)", infoReason, infoOK, rawReason, rawOK)
				}

				if got, want := staleReconcilerDrainAckInfo(info, sp, name), staleReconcilerDrainAck(b, sp, name); got != want {
					t.Errorf("staleReconcilerDrainAck: info=%v bead=%v", got, want)
				}

				if got, want := staleOrLegacyDrainAckBeforeStartInfo(info, sp, name), staleOrLegacyDrainAckBeforeStart(b, sp, name); got != want {
					t.Errorf("staleOrLegacyDrainAckBeforeStart: info=%v bead=%v", got, want)
				}

				// assignedWorkDrainCancelReason with a nil tracker (ack-driven path)
				// and a tracker carrying a cancelable drain (tracker-driven path).
				if got, want := assignedWorkDrainCancelReasonInfo(info, sp, nil, name), assignedWorkDrainCancelReason(b, sp, nil, name); got != want {
					t.Errorf("assignedWorkDrainCancelReason[nil-dt]: info=%q bead=%q", got, want)
				}
				dt := newDrainTracker()
				dt.set(b.ID, &drainState{reason: "orphaned", generation: 3})
				if got, want := assignedWorkDrainCancelReasonInfo(info, sp, dt, name), assignedWorkDrainCancelReason(b, sp, dt, name); got != want {
					t.Errorf("assignedWorkDrainCancelReason[dt]: info=%q bead=%q", got, want)
				}

				if got, want := runningSessionMatchesPendingCreateInfo(info, name, sp), refRunningSessionMatchesPendingCreate(b, name, sp); got != want {
					t.Errorf("runningSessionMatchesPendingCreate: info=%v bead=%v", got, want)
				}
			})
		}
	}
}
