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

func TestPhase3InitialInputDelivery(t *testing.T) {
	const basePrompt = "Base worker prompt"

	for _, tc := range selectedPhase3ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			t.Run(string(workertest.RequirementInputInitialMessageFirstStart), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "", map[string]string{
					"initial_message": "Do the first task.",
				})

				got := singleShellArg(t, prepared.cfg.PromptSuffix)
				want := "Base worker prompt\n\n---\n\nUser message:\nDo the first task."
				if got != want {
					t.Fatalf("PromptSuffix payload = %q, want %q", got, want)
				}
				if strings.Count(got, "Do the first task.") != 1 {
					t.Fatalf("PromptSuffix payload = %q, want initial message exactly once", got)
				}
			})

			t.Run(string(workertest.RequirementInputInitialMessageResume), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "already-started", map[string]string{
					"initial_message": "Do the first task.",
				})

				got := singleShellArg(t, prepared.cfg.PromptSuffix)
				if got != basePrompt {
					t.Fatalf("PromptSuffix payload = %q, want %q", got, basePrompt)
				}
				if strings.Contains(got, "Do the first task.") {
					t.Fatalf("PromptSuffix payload = %q, want no replayed initial message", got)
				}
			})

			t.Run(string(workertest.RequirementInputOverrideDefaults), func(t *testing.T) {
				prepared := preparePhase3Start(t, tc, basePrompt, "", map[string]string{
					"initial_message": "Ship it.",
					"model":           tc.wantModelOverride,
				})

				defaultArgs := prepared.candidate.tp.ResolvedProvider.ResolveDefaultArgs()
				if !containsOrderedArgs(prepared.cfg.Command, defaultArgs) {
					t.Fatalf("Command = %q, want default args %v", prepared.cfg.Command, defaultArgs)
				}
				if !containsOrderedArgs(prepared.cfg.Command, tc.wantModelOverrideArgs) {
					t.Fatalf("Command = %q, want model override args %v", prepared.cfg.Command, tc.wantModelOverrideArgs)
				}
				got := singleShellArg(t, prepared.cfg.PromptSuffix)
				if !strings.Contains(got, "Ship it.") {
					t.Fatalf("PromptSuffix payload = %q, want initial message", got)
				}
				if strings.Count(got, "Ship it.") != 1 {
					t.Fatalf("PromptSuffix payload = %q, want initial message exactly once", got)
				}
			})
		})
	}
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
