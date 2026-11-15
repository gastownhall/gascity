package workertest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// checkProgramStatusSurfaces is WC-PSTAT-001: the declared profile's
// recorded emission, run as the pane's program on an armed session under
// the shadow switch, surfaces through the runtime program-status seam at
// its first report, its turn's start and its turn's end, carrying the
// declared app; and the program probes exactly when the declaration puts it
// under the relay.
func checkProgramStatusSurfaces(t *testing.T, profile Profile) Result {
	t.Helper()
	code := RequirementProgramStatusSurfaces
	exp := profile.ProgramStatus
	evidence := map[string]string{"provider": profile.Provider, "verified": exp.Verified}
	if exp.Verified == "" || len(exp.Emission.FirstReport) == 0 || len(exp.Emission.TurnStart) == 0 || len(exp.Emission.TurnEnd) == 0 {
		return Fail(profile.ID, code, "declared profile has no recorded emission (verified build and first-report, turn-start and turn-end steps)").WithEvidence(evidence)
	}
	s := runtimetest.FakeProgramStatusFixtures().Armed(t, runtime.ProgramStatusShadow, exp.Emission.FirstReport...)
	steps := []struct {
		label     string
		emit      []string
		state     func(runtime.ProgramStatus) bool
		wantState string
		starts    uint32
		ends      uint32
	}{
		{"first report", nil, runtime.ProgramStatus.InputBoundary, "an input boundary", 0, 0},
		{"turn start", exp.Emission.TurnStart, runtime.ProgramStatus.Running, "working", 1, 0},
		{"turn end", exp.Emission.TurnEnd, runtime.ProgramStatus.InputBoundary, "an input boundary", 1, 1},
	}
	var seen []string
	for _, step := range steps {
		s.Emit(t, step.emit...)
		r, err := readProgramStatus(s)
		if err != nil {
			return Fail(profile.ID, code, fmt.Sprintf("%s: %v", step.label, err)).WithEvidence(evidence)
		}
		seen = append(seen, fmt.Sprintf("%s=%s/%s", step.label, r.Validity, r.State))
		evidence["readings"] = strings.Join(seen, ", ")
		switch {
		case r.Mode != runtime.ProgramStatusShadow:
			return Fail(profile.ID, code, fmt.Sprintf("%s: reading mode %q, want %q", step.label, r.Mode, runtime.ProgramStatusShadow)).WithEvidence(evidence)
		case !r.Valid() || !step.state(r):
			return Fail(profile.ID, code, fmt.Sprintf("%s: reading %s/%q, want valid and %s", step.label, r.Validity, r.State, step.wantState)).WithEvidence(evidence)
		case r.App != exp.App:
			return Fail(profile.ID, code, fmt.Sprintf("%s: reading app %q, want the declared %q", step.label, r.App, exp.App)).WithEvidence(evidence)
		case r.TurnStarts != step.starts || r.TurnEnds != step.ends:
			return Fail(profile.ID, code, fmt.Sprintf("%s: turn starts/ends %d/%d, want %d/%d", step.label, r.TurnStarts, r.TurnEnds, step.starts, step.ends)).WithEvidence(evidence)
		case r.Probed != exp.Relay:
			return Fail(profile.ID, code, fmt.Sprintf("%s: program probed=%t, but the declaration's relay=%t", step.label, r.Probed, exp.Relay)).WithEvidence(evidence)
		}
	}
	return Pass(profile.ID, code, fmt.Sprintf("%s reports surface at first report, turn start and turn end", exp.Verified)).WithEvidence(evidence)
}

// readProgramStatus takes s's reading through the routed leaf's
// program-status seam.
func readProgramStatus(s runtimetest.ProgramStatusSession) (runtime.ProgramStatus, error) {
	ps, ok := runtime.ProgramStatusFor(s.Provider, s.Name)
	if !ok {
		return runtime.ProgramStatus{}, fmt.Errorf("provider %T has no program-status seam for %q", s.Provider, s.Name)
	}
	r, err := ps.ProgramStatus(s.Name)
	if err != nil {
		return r, fmt.Errorf("ProgramStatus(%q): %w", s.Name, err)
	}
	return r, nil
}

// Kills a WC-PSTAT-001 check that passes an emission which never reports,
// reports under another app, never ends its turn, or whose probing
// disagrees with the relay declaration.
func TestProgramStatusSurfacesCheckFailsBrokenEmissions(t *testing.T) {
	base := profileByID(t, ProfileCodexTmuxCLI)
	relayed := profileByID(t, ProfileClaudeTmuxCLI)
	for name, c := range map[string]struct {
		profile Profile
		mutate  func(*ProgramStatusExpectation)
		want    string
	}{
		"silent program": {base, func(e *ProgramStatusExpectation) {
			e.Emission.FirstReport = []string{"probe-gated 10ms", "report state=idle:app=codex"}
		}, "first report"},
		"foreign app": {base, func(e *ProgramStatusExpectation) {
			e.Emission.TurnStart = []string{"report state=working:app=other"}
		}, "app"},
		"turn never ends": {base, func(e *ProgramStatusExpectation) {
			e.Emission.TurnEnd = []string{"report state=working:app=codex"}
		}, "turn end"},
		"tap-only program probes": {base, func(e *ProgramStatusExpectation) {
			e.Emission.FirstReport = append([]string{"raw " + osc7501ProbeBELHex}, e.Emission.FirstReport...)
		}, "relay=false"},
		"relayed program never probes": {relayed, func(e *ProgramStatusExpectation) {
			e.Emission.FirstReport = []string{"term bel", "report state=idle:app=claude-code"}
		}, "relay=true"},
		"no recorded emission": {base, func(e *ProgramStatusExpectation) {
			e.Emission = ProgramStatusEmission{}
		}, "no recorded emission"},
	} {
		t.Run(name, func(t *testing.T) {
			p := c.profile
			c.mutate(&p.ProgramStatus)
			if r := checkProgramStatusSurfaces(t, p); r.Status != ResultFail || !strings.Contains(r.Detail, c.want) {
				t.Fatalf("result %s %q, want a failure naming %q", r.Status, r.Detail, c.want)
			}
		})
	}
}

// Kills a declared profile whose recorded emission does not pass
// WC-PSTAT-001, independently of profile selection.
func TestProgramStatusSurfacesDeclaredProfilesPass(t *testing.T) {
	var declared int
	for _, p := range ProgramStatusProfiles() {
		if !p.ProgramStatus.Declared {
			continue
		}
		declared++
		if r := checkProgramStatusSurfaces(t, p); !r.Passed() {
			t.Errorf("%s: %s %q (%v)", p.ID, r.Status, r.Detail, r.Evidence)
		}
	}
	if declared == 0 {
		t.Fatal("no profile declares program status")
	}
}

// profileByID returns the phase-1 profile id.
func profileByID(t *testing.T, id ProfileID) Profile {
	t.Helper()
	for _, p := range Phase1Profiles() {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("no phase-1 profile %s", id)
	return Profile{}
}
