package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestPreparedStartOversizedPromptHardFailsBeforeLaunch covers the
// integration path for gastownhall/gascity ga-q8wgom.1.1: an oversized
// startup prompt bound for a runtime with no usable post-start delivery
// (subprocess execs the rendered command as a single "sh -c" argv element,
// see internal/runtime/subprocess/subprocess.go) must hard-fail inside
// buildPreparedStart, BEFORE any runtime.Config carrying the giant prompt
// is ever constructed for Provider.Start/exec.Command to consume.
func TestPreparedStartOversizedPromptHardFailsBeforeLaunch(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "subprocess",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err == nil {
		t.Fatalf("buildPreparedStart() error = nil, want a hard-fail error for an oversized prompt on an unsupported runtime")
	}
	if !errors.Is(err, errOversizedPromptUnsupportedRuntime) {
		t.Errorf("buildPreparedStart() error = %v, want errors.Is(err, errOversizedPromptUnsupportedRuntime)", err)
	}
	if strings.Contains(err.Error(), oversizedPrompt) {
		t.Errorf("buildPreparedStart() error leaks prompt content")
	}
	if prepared != nil {
		if len(prepared.cfg.PromptSuffix) > 0 {
			t.Errorf("buildPreparedStart() must not construct a runtime.Config carrying the oversized prompt in argv, got PromptSuffix len=%d", len(prepared.cfg.PromptSuffix))
		}
		if strings.Contains(prepared.cfg.Command, oversizedPrompt) {
			t.Errorf("buildPreparedStart() must not fold the oversized prompt into cfg.Command")
		}
	}
}

// TestPreparedStartOversizedPromptFallsBackOnNudgeCapableRuntime covers the
// companion path: an oversized prompt bound for a runtime with a working
// post-start Nudge (tmux) must still succeed, routing the prompt through the
// nudge instead of argv, rather than hard-failing.
func TestPreparedStartOversizedPromptFallsBackOnNudgeCapableRuntime(t *testing.T) {
	oversizedPrompt := repeatToBytes("a", maxPromptSuffixRawBytes)

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "claude",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "tmux",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart() unexpected error for a nudge-capable runtime: %v", err)
	}
	if len(prepared.cfg.PromptSuffix) > 0 {
		t.Errorf("buildPreparedStart() must not place an oversized prompt in argv even on a nudge-capable runtime, got PromptSuffix len=%d", len(prepared.cfg.PromptSuffix))
	}
	if !strings.Contains(prepared.cfg.Nudge, oversizedPrompt) {
		t.Errorf("buildPreparedStart() must route the oversized prompt through cfg.Nudge on a nudge-capable runtime")
	}
	if !prepared.promptDelivered {
		t.Errorf("buildPreparedStart() promptDelivered = false, want true (delivery falls back to nudge, it does not become non-delivery)")
	}
}

// TestPreparedStartOversizedPromptByteExactWithEmbeddedQuotesAndNewlines
// covers the realistic Claude/Codex-family regression called for by
// ga-q8wgom.1.1: a 100-150KB prompt containing embedded single quotes, double
// quotes, and newlines (representative of real assistant output and code
// blocks) must still reach the nudge-fallback path byte-for-byte, with zero
// prompt bytes anywhere in argv/Command, regardless of the special
// characters it contains.
func TestPreparedStartOversizedPromptByteExactWithEmbeddedQuotesAndNewlines(t *testing.T) {
	unit := "it's \"quoted\" and\nmultiline\n"
	oversizedPrompt := repeatToBytes(unit, maxPromptSuffixRawBytes+25000) // ~125KB: inside the 100-150KB range

	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName:             "worker",
			SessionName:              "worker",
			Command:                  "codex",
			Prompt:                   oversizedPrompt,
			EffectiveSessionProvider: "tmux",
		},
	}

	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart() unexpected error: %v", err)
	}
	if len(prepared.cfg.PromptSuffix) > 0 || prepared.cfg.PromptFlag != "" {
		t.Errorf("buildPreparedStart() must not place any prompt bytes in argv, got PromptSuffix len=%d PromptFlag=%q", len(prepared.cfg.PromptSuffix), prepared.cfg.PromptFlag)
	}
	if strings.Contains(prepared.cfg.Command, unit) {
		t.Errorf("buildPreparedStart() must not fold the prompt into cfg.Command")
	}
	if prepared.cfg.Nudge != oversizedPrompt {
		t.Errorf("buildPreparedStart() must deliver the prompt byte-exact through the nudge (no escaping/mangling of embedded quotes or newlines); got len=%d, want len=%d", len(prepared.cfg.Nudge), len(oversizedPrompt))
	}
	if !prepared.promptDelivered {
		t.Errorf("buildPreparedStart() promptDelivered = false, want true")
	}
}
