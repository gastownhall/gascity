package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

func TestPhase3InitialInputDelivery(t *testing.T) {
	const basePrompt = "Base worker prompt"
	reporter := newPhase3Reporter(t, "phase3-input")

	for _, tc := range selectedPhase3ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			t.Run(string(workertest.RequirementInputInitialMessageFirstStart), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "", map[string]string{
					"initial_message": "Do the first task.",
				})

				reporter.Require(t, initialMessageFirstStartResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputInitialMessageResume), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "already-started", map[string]string{
					"initial_message": "Do the first task.",
				})

				reporter.Require(t, initialMessageResumeResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputOverrideDefaults), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "", map[string]string{
					"initial_message": "Ship it.",
					"model":           tc.wantModelOverride,
				})

				reporter.Require(t, inputOverrideDefaultsResult(tc, prepared))
			})
		})
	}
}

func TestPhase3InputResultFailureClassification(t *testing.T) {
	tc := selectedPhase3ProviderCases(t)[0]

	t.Run("prompt suffix parse failure stays requirement-scoped", func(t *testing.T) {
		prepared := preparePhase3Start(t, tc, "Base worker prompt", "", map[string]string{
			"initial_message": "Do the first task.",
		})
		prepared.cfg.PromptSuffix = "'one' 'two'"

		result := initialMessageFirstStartResult(tc, prepared)
		if result.Status != workertest.ResultFail {
			t.Fatalf("result.Status = %q, want fail", result.Status)
		}
		if result.Requirement != workertest.RequirementInputInitialMessageFirstStart {
			t.Fatalf("result.Requirement = %q, want %q", result.Requirement, workertest.RequirementInputInitialMessageFirstStart)
		}
		if got := result.Evidence["prompt_suffix_parse_error"]; got == "" {
			t.Fatal("prompt_suffix_parse_error = empty, want parse failure evidence")
		}
	})

	t.Run("missing resolved provider fails without panic", func(t *testing.T) {
		prepared := preparePhase3Start(t, tc, "Base worker prompt", "", map[string]string{
			"initial_message": "Ship it.",
			"model":           tc.wantModelOverride,
		})
		prepared.candidate.tp.ResolvedProvider = nil

		result := inputOverrideDefaultsResult(tc, prepared)
		if result.Status != workertest.ResultFail {
			t.Fatalf("result.Status = %q, want fail", result.Status)
		}
		if result.Requirement != workertest.RequirementInputOverrideDefaults {
			t.Fatalf("result.Requirement = %q, want %q", result.Requirement, workertest.RequirementInputOverrideDefaults)
		}
		if got := result.Evidence["resolved_provider"]; got != "" {
			t.Fatalf("resolved_provider = %q, want empty when provider is missing", got)
		}
	})
}

func preparePhase3Start(t *testing.T, tc phase3ProviderCase, prompt, startedConfigHash string, overrides map[string]string) *preparedStart {
	t.Helper()

	rawOverrides, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("json.Marshal(overrides): %v", err)
	}

	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "phase3-" + tc.family,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "phase3-" + tc.family,
			"template":            "worker",
			"template_overrides":  string(rawOverrides),
			"started_config_hash": startedConfigHash,
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		session: &session,
		tp:      phase3TemplateParams(t, tc, prompt),
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate(%s): %v", tc.profileID, err)
	}
	return prepared
}
