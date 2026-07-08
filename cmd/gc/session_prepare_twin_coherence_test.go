package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestPrepareStartCandidateTwinNeverConsumedStale is the WI-6 W5 red-team drift
// guard. prepareStartCandidateForCity re-Gets the session bead and preWakeCommit /
// buildPreparedStart mutate it, so candidate.info must be kept coherent with the
// re-Got + mutated bead. The rejected fix (front-door coherence Gets that swallowed
// their error) drifted: on a bead the front-door read rejects (lost BOTH type and
// gc:session label — IsSessionBeadOrRepairable == false) the raw store.Get at the
// re-Get boundary still succeeds, so candidate.session went fresh while the swallowed
// twin stayed stale, launching stale template_overrides.
//
// The fold-based fix keeps the twin coherent: the EARLY twin is re-projected from the
// SAME bead the re-Get returned (no separate, swallowable Get), and buildPreparedStart
// folds its own mutations. This test proves the twin tracks the re-Got bead's
// template_overrides — NOT the stale append value — and would FAIL against the
// swallow-error code (verified by temporarily restoring the front-door-Get-then-
// swallow form: the assertions below trip because info stays stale on the rejected bead).
func TestPrepareStartCandidateTwinNeverConsumedStale(t *testing.T) {
	store := beads.NewMemStore()
	const freshOverrides = `{"model":"opus"}`
	// A mid-start bead that lost BOTH its session type and its gc:session label — the
	// exact shape the old EARLY front-door Get rejected while the raw re-Get succeeded.
	session, err := store.Create(beads.Bead{
		Title: "worker",
		Metadata: map[string]string{
			"session_name":       "worker",
			"template":           "worker",
			"template_overrides": freshOverrides,
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	// The append-captured twin carries a STALE override — the divergence the re-Get
	// boundary must correct (out-of-band template_overrides change since append).
	staleInfo := sessionpkg.InfoFromPersistedBead(beads.Bead{
		ID: session.ID,
		Metadata: map[string]string{
			"session_name":       "worker",
			"template_overrides": `{"model":"sonnet"}`,
		},
	})
	candidate := startCandidate{
		session: &session,
		info:    staleInfo,
		tp: TemplateParams{
			TemplateName:     "worker",
			SessionName:      "worker",
			Command:          "claude",
			ResolvedProvider: optionSchemaProvider(),
		},
	}

	prepared, err := prepareStartCandidate(
		candidate,
		&config.City{Agents: []config.Agent{{Name: "worker"}}},
		store,
		&clock.Fake{Time: time.Now()},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}

	// Twin re-projected at the re-Get boundary — the fresh store value, not the stale
	// append value.
	if prepared.candidate.info.TemplateOverrides != freshOverrides {
		t.Fatalf("info.TemplateOverrides = %q, want fresh %q — twin left stale (swallow-error drift)",
			prepared.candidate.info.TemplateOverrides, freshOverrides)
	}
	// Coherent with the re-Got raw bead the write helpers still see.
	if got := prepared.candidate.session.Metadata["template_overrides"]; prepared.candidate.info.TemplateOverrides != got {
		t.Fatalf("twin/raw drift: info=%q raw=%q", prepared.candidate.info.TemplateOverrides, got)
	}
	// buildPreparedStart consumed the fresh override off the twin (opus), not the stale sonnet.
	if !strings.Contains(prepared.cfg.Command, "claude-opus-4-8") {
		t.Fatalf("command %q should apply the fresh opus override off the re-projected twin", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-sonnet-4-6") {
		t.Fatalf("command %q applied the STALE sonnet override — twin consumed stale", prepared.cfg.Command)
	}
}
