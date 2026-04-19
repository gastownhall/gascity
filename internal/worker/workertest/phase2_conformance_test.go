package workertest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	worker "github.com/gastownhall/gascity/internal/worker"
)

func TestPhase2Catalog(t *testing.T) {
	expected := []RequirementCode{
		RequirementStartupOutcomeBound,
		RequirementStartupCommandMaterialization,
		RequirementStartupRuntimeConfigMaterialization,
		RequirementInputInitialMessageFirstStart,
		RequirementInputInitialMessageResume,
		RequirementInputOverrideDefaults,
		RequirementTranscriptDiagnostics,
		RequirementInteractionSignal,
		RequirementInteractionPending,
		RequirementInteractionRespond,
		RequirementInteractionReject,
		RequirementInteractionInstanceLocalDedup,
		RequirementInteractionDurableHistory,
		RequirementInteractionLifecycleHistory,
		RequirementToolEventNormalization,
		RequirementToolEventOpenTail,
		RequirementRealTransportProof,
	}

	catalog := Phase2Catalog()
	if len(catalog) != len(expected) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(expected))
	}

	seen := make(map[RequirementCode]Requirement, len(catalog))
	for _, requirement := range catalog {
		if requirement.Group == "" {
			t.Fatalf("requirement %s has empty group", requirement.Code)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = requirement
	}
	for _, code := range expected {
		if _, ok := seen[code]; !ok {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}
}

func TestPhase2HistoryDiagnostics(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-history", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			path := writeMalformedHistoryTranscript(t, profile)
			history, err := worker.SessionLogAdapter{}.LoadHistory(worker.LoadRequest{
				Provider:       profile.Provider,
				TranscriptPath: path,
			})
			reporter.Require(t, historyDiagnosticsResult(profile.ID, path, history, err))
		})
	}
}

func TestPhase2StartupOutcomeBounds(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-startup", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		outcome string
		delay   time.Duration
	}{
		{name: "Ready", outcome: "ready", delay: 10 * time.Millisecond},
		{name: "Blocked", outcome: "blocked", delay: 15 * time.Millisecond},
		{name: "Failed", outcome: "failed", delay: 20 * time.Millisecond},
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					run := runFakeStartup(t, profile.ID, tt.outcome, tt.delay)
					reporter.Require(t, startupOutcomeResult(profile.ID, tt.outcome, tt.delay, run))
				})
			}
		})
	}
}

func TestPhase2RequiredInteractions(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			t.Run(string(RequirementInteractionSignal), func(t *testing.T) {
				run := runFakeInteraction(t, profile.ID)
				reporter.Require(t, interactionSignalResult(profile.ID, run))
			})

			sp := runtime.NewFake()
			sessionName := "worker-int-" + strings.ReplaceAll(string(profile.ID), "/", "-")
			if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
				t.Fatalf("Start: %v", err)
			}

			pending := &runtime.PendingInteraction{
				RequestID: "req-1",
				Kind:      "approval",
				Prompt:    "Allow Read?",
				Options:   []string{"approve", "deny"},
				Metadata: map[string]string{
					"profile":   string(profile.ID),
					"tool_name": "Read",
				},
			}
			sp.SetPendingInteraction(sessionName, pending)

			t.Run(string(RequirementInteractionPending), func(t *testing.T) {
				got, err := sp.Pending(sessionName)
				reporter.Require(t, pendingInteractionResult(profile.ID, got, pending, err))
			})

			t.Run(string(RequirementInteractionReject), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: "wrong-req",
					Action:    "approve",
				})
				stillPending, pErr := sp.Pending(sessionName)
				reporter.Require(t, rejectInteractionResult(profile.ID, err, stillPending, pErr, len(sp.Responses[sessionName])))
			})

			t.Run(string(RequirementInteractionRespond), func(t *testing.T) {
				err := sp.Respond(sessionName, runtime.InteractionResponse{
					RequestID: pending.RequestID,
					Action:    "approve",
					Text:      "continue",
				})
				got, pErr := sp.Pending(sessionName)
				reporter.Require(t, respondInteractionResult(profile.ID, err, got, pErr, sp.Responses[sessionName]))
			})
		})
	}
}

func TestPhase2DurableInteractionHistory(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction-history", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			path := writeInteractionHistoryTranscript(t, profile)
			history := loadHistory(t, profile.Provider, path)
			reporter.Require(t, interactionDurableHistoryResult(profile.ID, path, history))
		})
	}
}

func TestPhase2InteractionLifecycleHistory(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-interaction-lifecycle", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name        string
		finalState  worker.InteractionState
		wantPending bool
	}{
		{name: "dismissed", finalState: worker.InteractionStateDismissed, wantPending: false},
		{name: "resumed_after_restart", finalState: worker.InteractionStateResumedAfterRestart, wantPending: true},
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			for _, tt := range tests {
				tt := tt
				t.Run(tt.name, func(t *testing.T) {
					path := writeInteractionLifecycleTranscript(t, profile, tt.finalState)
					history := loadHistory(t, profile.Provider, path)
					reporter.Require(t, interactionLifecycleHistoryResult(profile.ID, path, history, tt.finalState, tt.wantPending))
				})
			}
		})
	}
}

func TestPhase2ToolEventSubstrate(t *testing.T) {
	reporter := NewSuiteReporter(t, "phase2-tool", map[string]string{
		"tier":  "worker-core",
		"phase": "phase2",
	})

	profiles, err := selectedProfiles()
	if err != nil {
		t.Fatal(err)
	}

	for _, profile := range profiles {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			t.Run(string(RequirementToolEventNormalization), func(t *testing.T) {
				path := writeToolTranscript(t, profile, false)
				history := loadHistory(t, profile.Provider, path)
				reporter.Require(t, toolNormalizationResult(profile.ID, path, history))
			})

			t.Run(string(RequirementToolEventOpenTail), func(t *testing.T) {
				path := writeToolTranscript(t, profile, true)
				history := loadHistory(t, profile.Provider, path)
				reporter.Require(t, toolOpenTailResult(profile.ID, path, history))
			})
		})
	}
}
