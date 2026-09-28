package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	workertest "github.com/gastownhall/gascity/internal/worker/workertest"
)

func TestPhase2InitialInputDelivery(t *testing.T) {
	reporter := newPhase2Reporter(t, "phase2-input-delivery")

	// The resume cases below model a legitimate resume (the keyed transcript is
	// present on disk). Stub the stale-resume probe to report "present" so the
	// pre-flight guard does not reclassify these resumes as fresh starts; the
	// guard's missing-transcript behavior is covered by TestStaleResumeKeyProbe
	// and the transcript layer's TestHasKeyedTranscript.
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

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
					"initial_message":        "Ship it.",
					phase2OverrideOption(tc): tc.overrideValue,
				})

				reporter.Require(t, inputOverrideDefaultsResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputInProgressResumeRestart), func(t *testing.T) {
				prepared := preparePhase2ResumeRestartStart(t, tc, map[string]string{
					"initial_message": "Do the first task.",
				}, true)

				reporter.Require(t, inProgressResumeRestartResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputPreClaimResumeRestart), func(t *testing.T) {
				prepared := preparePhase2ResumeRestartStart(t, tc, map[string]string{
					"initial_message": "Do the first task.",
				}, false)

				reporter.Require(t, preClaimResumeRestartResult(tc, prepared))
			})
		})
	}
}

// TestPhase2HookInstalledResumeInputDelivery re-runs the resume input
// requirements with the profile's provider hooks installed and adds
// WC-INPUT-006. Installing hooks is what flips a hook-primed family
// (opencode, mimocode) onto the configured-nudge-only resume path; every
// other family must keep the rendered prompt in its restart turn even with
// hooks installed, because their hooks prime once at SessionStart.
func TestPhase2HookInstalledResumeInputDelivery(t *testing.T) {
	reporter := newPhase2Reporter(t, "phase2-input-delivery-hook-installed")

	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	for _, tc := range selectedPhase2ProviderCases(t) {
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			t.Run(string(workertest.RequirementInputHookPrimedResumeRoleOmitted), func(t *testing.T) {
				prepared := preparePhase2StartWithHooks(t, tc, "already-started", map[string]string{
					"initial_message": "Do the first task.",
				}, true)

				reporter.Require(t, hookPrimedResumeRoleResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputInitialMessageResume), func(t *testing.T) {
				prepared := preparePhase2StartWithHooks(t, tc, "already-started", map[string]string{
					"initial_message": "Do the first task.",
				}, true)

				reporter.Require(t, initialMessageResumeResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputInProgressResumeRestart), func(t *testing.T) {
				prepared := preparePhase2ResumeRestartStartWithHooks(t, tc, map[string]string{
					"initial_message": "Do the first task.",
				}, true, true)

				reporter.Require(t, inProgressResumeRestartResult(tc, prepared))
			})

			t.Run(string(workertest.RequirementInputPreClaimResumeRestart), func(t *testing.T) {
				prepared := preparePhase2ResumeRestartStartWithHooks(t, tc, map[string]string{
					"initial_message": "Do the first task.",
				}, false, true)

				reporter.Require(t, preClaimResumeRestartResult(tc, prepared))
			})

			// A fresh start with hooks installed still delivers the rendered
			// prompt on the launch path for every family: the hook-primed
			// resume shortcut must never leak into a first incarnation
			// (gastownhall/gascity#5238).
			t.Run(string(workertest.RequirementInputInitialMessageFirstStart), func(t *testing.T) {
				prepared := preparePhase2StartWithHooks(t, tc, "", map[string]string{
					"initial_message": "Do the first task.",
				}, true)

				reporter.Require(t, initialMessageFirstStartResult(tc, prepared))
			})
		})
	}
}

// TestPhase2DefaultConfigurationHookPrimedResume pins that the hook-primed
// resume branch is reachable with the DEFAULT configuration: no
// install_agent_hooks, no hooks_installed. The runtime stages the launch
// family's overlay plugin for every opencode/mimocode session, so those
// profiles are hook-enabled out of the box and WC-INPUT-006 must hold for
// them without any hook declaration. (TestPhase2HookInstalledResumeInputDelivery
// covers the explicit-declaration shape for every profile.)
func TestPhase2DefaultConfigurationHookPrimedResume(t *testing.T) {
	reporter := newPhase2Reporter(t, "phase2-input-delivery-default-hook-primed")

	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	for _, tc := range selectedPhase2ProviderCases(t) {
		if !phase2HookSuppliesRolePerTurnFamilies[tc.family] {
			continue
		}
		tc := tc
		t.Run(string(tc.profileID), func(t *testing.T) {
			prepared := preparePhase2Start(t, tc, "already-started", map[string]string{
				"initial_message": "Do the first task.",
			})
			if len(prepared.candidate.tp.Hints.InstallAgentHooks) != 0 {
				t.Fatalf("fixture declares install_agent_hooks %v; this test models the default configuration", prepared.candidate.tp.Hints.InstallAgentHooks)
			}
			reporter.Require(t, hookPrimedResumeRoleResult(tc, prepared))
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

	// prompt_hash pins the rendered startup TEMPLATE prompt only. Even though the
	// delivered payload above carries the one-shot initial_message, the stored hash
	// must exclude it so a later Stage-4 re-derivation from the template still
	// matches (S19); hashing the delivered payload would re-prime the session
	// forever.
	if got, want := prepared.promptHash, sessionpkg.PromptHash("Base worker prompt"); got != want {
		t.Errorf("promptHash = %q, want base-template hash %q (initial_message must be excluded)", got, want)
	}
	if prepared.promptHash == sessionpkg.PromptHash(payload) {
		t.Errorf("promptHash must not hash the delivered payload %q (which includes initial_message)", payload)
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
			"initial_message":        "Ship it.",
			phase2OverrideOption(tc): tc.overrideValue,
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
	return preparePhase2StartWithHooks(t, tc, startedConfigHash, overrides, false)
}

func preparePhase2StartWithHooks(t *testing.T, tc phase2ProviderCase, startedConfigHash string, overrides map[string]string, installHooks bool) *preparedStart {
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
		info: sessiontest.SeedBead(t, session),
		tp:   phase2TemplateParamsWithHooks(t, tc, "Base worker prompt", installHooks),
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate(%s): %v", tc.profileID, err)
	}
	return prepared
}

func preparePhase2ResumeRestartStart(t *testing.T, tc phase2ProviderCase, overrides map[string]string, assignedWork bool) *preparedStart {
	t.Helper()
	return preparePhase2ResumeRestartStartWithHooks(t, tc, overrides, assignedWork, false)
}

func preparePhase2ResumeRestartStartWithHooks(t *testing.T, tc phase2ProviderCase, overrides map[string]string, assignedWork, installHooks bool) *preparedStart {
	t.Helper()

	rawOverrides, err := json.Marshal(overrides)
	if err != nil {
		t.Fatalf("json.Marshal(overrides): %v", err)
	}

	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:  "phase2-" + tc.family,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":        "phase2-" + tc.family,
			"template":            "worker",
			"template_overrides":  string(rawOverrides),
			"started_config_hash": "already-started",
			"session_key":         "phase2-resume-key",
		},
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}

	if assignedWork {
		work, err := store.Create(beads.Bead{
			Title: "phase2 in-progress work",
			Type:  "task",
		})
		if err != nil {
			t.Fatalf("Create work bead: %v", err)
		}
		status := "in_progress"
		assignee := session.ID
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("assign work bead: %v", err)
		}
	}

	tp := phase2TemplateParamsWithHooks(t, tc, "Base worker prompt", installHooks)
	tp.Hints.Nudge = ""
	prepared, err := prepareStartCandidate(startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp:   tp,
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate(%s): %v", tc.profileID, err)
	}
	return prepared
}
