package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/rollout"
)

// TestRolloutGateChecksAreAdvisoryPerGate proves the doctor section produces one
// report-only (SeverityAdvisory, never Error) check per registered gate, with a
// value+origin message — so it renders every gate and never gates the exit code.
func TestRolloutGateChecksAreAdvisoryPerGate(t *testing.T) {
	flags := rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require), rollout.WithFormulaV2(false))
	checks := rolloutGateChecks(flags, nil)
	if len(checks) != len(rollout.Specs()) {
		t.Fatalf("got %d checks, want one per Spec (%d)", len(checks), len(rollout.Specs()))
	}
	ctx := &doctor.CheckContext{}
	names := map[string]string{}
	for _, c := range checks {
		res := c.Run(ctx)
		if res.Severity != doctor.SeverityAdvisory {
			t.Errorf("%s: severity = %v, want SeverityAdvisory (must never block)", c.Name(), res.Severity)
		}
		if res.Status == doctor.StatusError {
			t.Errorf("%s: status = error; PR-1c doctor is render-only", c.Name())
		}
		if c.CanFix() || c.WarmupEligible() {
			t.Errorf("%s: report-only check must not CanFix/WarmupEligible", c.Name())
		}
		names[c.Name()] = res.Message
	}
	msg, ok := names["rollout:beads.conditional_writes"]
	if !ok {
		t.Fatalf("missing beads gate check; got %v", names)
	}
	// Assert the exact value+origin, not just that "origin=" appears — origin
	// exists to reveal an env override, so a hardcoded literal must not satisfy it.
	if !strings.Contains(msg, "= require (origin=config)") {
		t.Errorf("message = %q, want %q", msg, "beads.conditional_writes = require (origin=config)")
	}
}

// TestRolloutGateCheckNoticeWarns proves a gate carrying a notice renders as a
// warning with the notice's actual message in Details, and — critically — that
// the FlagKey filter is per-gate: a notice belonging to the beads gate must NOT
// flip an unrelated gate's line to a warning.
func TestRolloutGateCheckNoticeWarns(t *testing.T) {
	f, err := rollout.Resolve(
		&config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}},
		rollout.ResolveOptions{LookupEnv: func(k string) (string, bool) {
			if k == "GC_BEADS_CONDITIONAL_WRITES" {
				return "auto", true // env overrides config → a beads-keyed notice
			}
			return "", false
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var beadsSpec, fv2Spec rollout.Spec
	for _, s := range rollout.Specs() {
		switch s.Key {
		case "beads.conditional_writes":
			beadsSpec = s
		case "daemon.formula_v2":
			fv2Spec = s
		}
	}

	res := rolloutGateCheck{spec: beadsSpec, flags: f}.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory {
		t.Errorf("gate with a notice: status=%v severity=%v, want warning+advisory", res.Status, res.Severity)
	}
	// The notice's real message must reach Details, not a blank/placeholder.
	hasNotice := false
	for _, d := range res.Details {
		if strings.Contains(d, "GC_BEADS_CONDITIONAL_WRITES") && strings.Contains(d, "overrides config") {
			hasNotice = true
		}
	}
	if !hasNotice {
		t.Errorf("beads notice message missing from Details; got %v", res.Details)
	}

	// Cross-gate: the same Flags carries only the beads notice, so the unrelated
	// daemon.formula_v2 line must stay OK (kills a deleted-FlagKey-filter mutant).
	if fv2Spec.Key == "" {
		t.Fatal("daemon.formula_v2 spec not found")
	}
	other := rolloutGateCheck{spec: fv2Spec, flags: f}.Run(&doctor.CheckContext{})
	if other.Status != doctor.StatusOK {
		t.Errorf("unrelated gate flipped to %v by a beads-keyed notice; want StatusOK. Details=%v", other.Status, other.Details)
	}
}

// TestRolloutGateChecksResolveError proves a resolve failure registers a single
// advisory warning rather than crashing or blocking.
func TestRolloutGateChecksResolveError(t *testing.T) {
	checks := rolloutGateChecks(rollout.Flags{}, errors.New("beads.conditional_writes: invalid mode"))
	if len(checks) != 1 {
		t.Fatalf("resolve error: want 1 check, got %d", len(checks))
	}
	res := checks[0].Run(&doctor.CheckContext{})
	if checks[0].Name() != "rollout:resolve" || res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory {
		t.Errorf("resolve-error check = %s/%v/%v, want rollout:resolve/warning/advisory", checks[0].Name(), res.Status, res.Severity)
	}
}

// hasRolloutCheck reports whether any registered check is a rollout gate line.
func hasRolloutCheck(checks []doctor.Check, name string) bool {
	for _, c := range checks {
		if c.Name() == name {
			return true
		}
	}
	return false
}

// TestBuildDoctorChecksRegistersRolloutGates proves the composition seam wires the
// rollout section into the doctor check set when the config loads cleanly.
func TestBuildDoctorChecksRegistersRolloutGates(t *testing.T) {
	cfg := &config.City{Beads: config.BeadsConfig{ConditionalWrites: "require"}}
	flags := rollout.ForTest(rollout.WithBeadsConditionalWrites(rollout.Require))
	checks := buildDoctorChecks(t.TempDir(), cfg, nil, buildDoctorChecksOpts{RolloutFlags: flags})
	if !hasRolloutCheck(checks, "rollout:beads.conditional_writes") {
		t.Error("buildDoctorChecks did not register the beads rollout gate")
	}
}

// TestBuildDoctorChecksRegistersRolloutResolveError proves a boot resolve error
// surfaces as its single advisory check through the composition seam.
func TestBuildDoctorChecksRegistersRolloutResolveError(t *testing.T) {
	checks := buildDoctorChecks(t.TempDir(), &config.City{}, nil, buildDoctorChecksOpts{RolloutResolveErr: errors.New("boom")})
	if !hasRolloutCheck(checks, "rollout:resolve") {
		t.Error("buildDoctorChecks did not register the rollout resolve-error check")
	}
	if hasRolloutCheck(checks, "rollout:beads.conditional_writes") {
		t.Error("resolve error should suppress the per-gate lines")
	}
}

// TestBuildDoctorChecksSkipsRolloutGatesWhenConfigFailed proves a config-load
// failure omits the rollout section entirely, so the parse error is not masked
// by a confusing gate line.
func TestBuildDoctorChecksSkipsRolloutGatesWhenConfigFailed(t *testing.T) {
	checks := buildDoctorChecks(t.TempDir(), nil, errors.New("parse error"), buildDoctorChecksOpts{RolloutFlags: rollout.ForTest()})
	for _, c := range checks {
		if strings.HasPrefix(c.Name(), "rollout:") {
			t.Errorf("rollout gate %q registered despite config load failure", c.Name())
		}
	}
}
