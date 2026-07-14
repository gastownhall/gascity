package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
)

// TestPrepareStartCandidateTwinNeverConsumedStale is the WI-6 W5 red-team drift
// guard. prepareStartCandidateForCity re-Gets the session bead and preWakeCommit /
// buildPreparedStart mutate it, so candidate.info must be kept coherent with the
// re-Got + mutated bead. The append-captured twin can go stale when the persisted
// template_overrides changes out of band between append and start; a fix that
// swallowed a coherence Get would leave the twin on that stale value and launch it.
//
// The fold-based fix keeps the twin coherent: the EARLY twin is re-projected from the
// SAME bead the single front-door re-Get returned (no separate, swallowable Get), and
// buildPreparedStart folds its own mutations. This test proves the twin tracks the
// re-Got bead's fresh template_overrides — NOT the stale append value — so it FAILS
// against any swallow-error form that leaves info stale.
func TestPrepareStartCandidateTwinNeverConsumedStale(t *testing.T) {
	store := beads.NewMemStore()
	const freshOverrides = `{"model":"opus"}`
	// A mid-start session bead whose persisted template_overrides was changed out of
	// band (opus) since the append-captured twin was taken (sonnet). The single
	// front-door re-Get must reload this fresh value onto the twin.
	session, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
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
	staleInfo := seedSessionInfo(beads.Bead{
		ID: session.ID,
		Metadata: map[string]string{
			"session_name":       "worker",
			"template_overrides": `{"model":"sonnet"}`,
		},
	})
	candidate := startCandidate{
		info: staleInfo,
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
	// Coherent with the persisted bead the write helpers still see through the
	// store front door (candidate.info is now the sole read surface; the raw
	// pointer is gone).
	stored, err := store.Get(prepared.candidate.info.ID)
	if err != nil {
		t.Fatalf("Get persisted bead: %v", err)
	}
	if got := stored.Metadata["template_overrides"]; prepared.candidate.info.TemplateOverrides != got {
		t.Fatalf("twin/store drift: info=%q store=%q", prepared.candidate.info.TemplateOverrides, got)
	}
	// buildPreparedStart consumed the fresh override off the twin (opus), not the stale sonnet.
	if !strings.Contains(prepared.cfg.Command, "claude-opus-4-8") {
		t.Fatalf("command %q should apply the fresh opus override off the re-projected twin", prepared.cfg.Command)
	}
	if strings.Contains(prepared.cfg.Command, "claude-sonnet-4-6") {
		t.Fatalf("command %q applied the STALE sonnet override — twin consumed stale", prepared.cfg.Command)
	}
}
