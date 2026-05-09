package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

func TestPhase2InitialInputDelivery(t *testing.T) {
	reporter := newPhase2Reporter(t, "phase2-input-delivery")

	for _, tc := range selectedPhase2ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			t.Run(string(workertest.RequirementInputInitialMessageFirstStart), func(t *testing.T) {
				prepared := preparePhase2Start(t, tc, "", map[string]string{
					"initial_message": "Do the first task.",
				})

				reporter.Require(t, initialMessageFirstStartResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputInitialMessageResume), func(t *testing.T) {
				prepared := preparePhase2Start(t, tc, "already-started", map[string]string{
					"initial_message": "Do the first task.",
				})

				reporter.Require(t, initialMessageResumeResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputOverrideDefaults), func(t *testing.T) {
				prepared := preparePhase2Start(t, tc, "", map[string]string{
					"initial_message": "Ship it.",
					"model":           tc.wantModelOverride,
				})

				reporter.Require(t, inputOverrideDefaultsResult(tc, prepared))
			})
		})
	}
}

func TestPhase2HookEnabledClaudeFirstTurnStartupPayload(t *testing.T) {
	tc := phase2ProviderCaseForFamily(t, "claude")
	prepared := preparePhase2Start(t, tc, "", map[string]string{
		"initial_message": "Do the first task.",
	})

	if !prepared.candidate.tp.HookEnabled {
		t.Fatal("HookEnabled = false, want true for Claude phase2 profile")
	}
	if prepared.candidate.tp.ResolvedProvider == nil {
		t.Fatal("ResolvedProvider = nil, want Claude provider metadata")
	}
	if !prepared.candidate.tp.ResolvedProvider.SupportsHooks {
		t.Fatal("SupportsHooks = false, want true for Claude phase2 profile")
	}
	if prepared.cfg.PromptSuffix == "" {
		t.Fatal("PromptSuffix = empty, want first-turn startup payload to stay on launch for hook-enabled Claude")
	}
	if got := prepared.cfg.Nudge; got != "nudge-claude" {
		t.Fatalf("Nudge = %q, want existing worker nudge preserved separately", got)
	}

	payload, evidence, err := phase2PromptPayload(tc, prepared)
	if err != nil {
		t.Fatalf("phase2PromptPayload: %v (evidence=%v)", err, evidence)
	}
	if !strings.Contains(payload, "Base worker prompt") {
		t.Fatalf("payload = %q, want base startup prompt", payload)
	}
	if !strings.Contains(payload, "User message:\nDo the first task.") {
		t.Fatalf("payload = %q, want initial_message on first start", payload)
	}
	if strings.Count(payload, "Do the first task.") != 1 {
		t.Fatalf("payload = %q, want initial_message exactly once", payload)
	}
}

func TestPhase2InputResultFailureClassification(t *testing.T) {
	tc := selectedPhase2ProviderCases(t)[0]

	t.Run("prompt suffix parse failure stays requirement-scoped", func(t *testing.T) {
		prepared := preparePhase2Start(t, tc, "", map[string]string{
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
		prepared := preparePhase2Start(t, tc, "", map[string]string{
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

func preparePhase2Start(t *testing.T, tc phase2ProviderCase, startedConfigHash string, overrides map[string]string) *preparedStart {
	t.Helper()

	rawOverrides, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("json.Marshal(overrides): %v", err)
	}

	store := beads.NewMemStore()
	metadata := map[string]string{
		"session_name":        "phase2-" + tc.family,
		"template":            "worker",
		"template_overrides":  string(rawOverrides),
		"started_config_hash": startedConfigHash,
	}
	if startedConfigHash != "" {
		metadata["session_key"] = "phase2-resume-key"
	}
	session, err := store.Create(beads.Bead{
		Title:    "phase2-" + tc.family,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	prepared, err := prepareStartCandidate(startCandidate{
		session: &session,
		tp:      phase2TemplateParams(t, tc, "Base worker prompt"),
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate(%s): %v", tc.profileID, err)
	}
	return prepared
}
