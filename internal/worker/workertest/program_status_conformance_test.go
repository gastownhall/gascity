package workertest

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// programStatusChecks are the checks of the landed WC-PSTAT rows, by code. A
// row joins this map in the change that removes it from programStatusPending.
var programStatusChecks = map[RequirementCode]func(t *testing.T, profile Profile) Result{
	RequirementProgramStatusSurfaces: checkProgramStatusSurfaces,
	RequirementProgramStatusPhase:    checkProgramStatusPhase,
}

// providerDataProgramStatus returns the program-status declaration gc's
// builtin provider data arms for profile's family under the shadow switch on
// a terminal transport: the arm config.ProgramStatusBinding.Arm derives,
// which is what a session of that family starts with. The family comes from
// the profile's provider, not a canonical worker identity: omp and grok
// declare program status without one.
func providerDataProgramStatus(t *testing.T, profile Profile) ProgramStatusDeclaration {
	t.Helper()
	spec, ok := config.BuiltinProviders()[ProfileFamily(profile)]
	if !ok {
		t.Fatalf("profile %s: family %q is not a builtin provider", profile.ID, ProfileFamily(profile))
	}
	arm, _ := spec.ProgramStatus.Arm(runtime.ProgramStatusShadow, false)
	return ProgramStatusDeclaration{Declared: arm.Capture, App: arm.App, Relay: arm.Relay}
}

// TestProgramStatusConformance runs the WC-PSTAT rows for every profile: a
// declaration that drifts fails, an undeclared profile and a pending row
// are recorded and skipped, and a landed row runs its check (recorded and
// skipped when the check reports the profile unsupported).
func TestProgramStatusConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "program_status", map[string]string{"tier": "worker-core"})
	profiles, err := filterProfiles(ProgramStatusProfiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range profiles {
		t.Run(string(profile.ID), func(t *testing.T) {
			for _, req := range ProgramStatusCatalog() {
				t.Run(string(req.Code), func(t *testing.T) {
					result, run := ProgramStatusGate(profile, req.Code, providerDataProgramStatus(t, profile))
					switch {
					case run:
						check, ok := programStatusChecks[req.Code]
						if !ok {
							t.Fatalf("%s left pending with no check registered", req.Code)
						}
						checked := check(t, profile)
						if checked.Status == ResultUnsupported {
							// A landed row its check cannot run for this
							// profile (WC-PSTAT-002 needs a worker identity):
							// record the outcome but do not fail the suite.
							reporter.Record(checked)
							t.Skip(checked.Detail)
						}
						reporter.Require(t, checked)
					case result.Status == ResultUnsupported:
						reporter.Record(result)
						t.Skip(result.Detail)
					default:
						reporter.Require(t, result)
					}
				})
			}
		})
	}
}

// Kills a catalog that drops, renumbers or regroups a WC-PSTAT row, words
// one for a transport, or leaves a pending row without its slice.
func TestProgramStatusCatalogRegistersWCPSTATRows(t *testing.T) {
	catalog := ProgramStatusCatalog()
	want := []RequirementCode{
		"WC-PSTAT-001", "WC-PSTAT-002", "WC-PSTAT-003", "WC-PSTAT-004",
		"WC-PSTAT-005", "WC-PSTAT-006", "WC-PSTAT-007", "WC-PSTAT-008",
	}
	var got []RequirementCode
	for _, req := range catalog {
		got = append(got, req.Code)
		if req.Group != "program_status" || req.Description == "" {
			t.Errorf("%s: group %q, description %q", req.Code, req.Group, req.Description)
		}
		if strings.Contains(strings.ToLower(req.Description), "tmux") {
			t.Errorf("%s names a transport: %q", req.Code, req.Description)
		}
		if _, checked := programStatusChecks[req.Code]; checked {
			continue
		}
		if slice, pending := ProgramStatusPendingSlice(req.Code); !pending || !strings.Contains(slice, "ga-f4ovjm.") {
			t.Errorf("%s has no check and no pending slice (%q)", req.Code, slice)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("catalog codes %q, want %q", got, want)
	}
	for code := range programStatusPending {
		if !slices.Contains(want, code) {
			t.Errorf("pending row %s is not in the catalog", code)
		}
		if _, checked := programStatusChecks[code]; checked {
			t.Errorf("%s is both pending and checked", code)
		}
	}
}

// Kills a gate that lets a drifting declaration (declared, app or relay)
// through, holds an undeclared profile to a row, runs a pending row, or
// skips a landed row of a declared profile.
func TestProgramStatusGate(t *testing.T) {
	undeclared := Phase1Profiles()[0]
	undeclared.ProgramStatus = ProgramStatusExpectation{}
	declaration := ProgramStatusDeclaration{Declared: true, App: "agent", Relay: true}
	declared := undeclared
	declared.ProgramStatus.ProgramStatusDeclaration = declaration
	code := RequirementProgramStatusPendingKindOnly

	for name, c := range map[string]struct {
		profile        Profile
		implementation ProgramStatusDeclaration
	}{
		"provider data declares, profile does not": {undeclared, declaration},
		"profile declares, provider data does not": {declared, ProgramStatusDeclaration{}},
		"app drifts":   {declared, ProgramStatusDeclaration{Declared: true, App: "other", Relay: true}},
		"relay drifts": {declared, ProgramStatusDeclaration{Declared: true, App: "agent"}},
	} {
		if r, run := ProgramStatusGate(c.profile, code, c.implementation); run || r.Status != ResultFail || !strings.Contains(r.Detail, "drift") {
			t.Errorf("%s: %+v run=%t, want a drift failure", name, r, run)
		}
	}
	if r, run := ProgramStatusGate(undeclared, code, ProgramStatusDeclaration{}); run || r.Status != ResultUnsupported || !strings.Contains(r.Detail, "does not declare") {
		t.Fatalf("undeclared: %+v run=%t, want unsupported", r, run)
	}
	r, run := ProgramStatusGate(declared, code, declaration)
	if run || r.Status != ResultUnsupported || r.Evidence["pending_slice"] != "ga-f4ovjm.22" {
		t.Fatalf("declared, row pending: %+v run=%t, want unsupported naming ga-f4ovjm.22", r, run)
	}
	if r, run := ProgramStatusGate(declared, RequirementProgramStatusPhase, declaration); !run || r.Status != "" {
		t.Fatalf("declared, WC-PSTAT-002 landed: %+v run=%t, want the row run", r, run)
	}

	slice := programStatusPending[code]
	delete(programStatusPending, code)
	t.Cleanup(func() { programStatusPending[code] = slice })
	if r, run := ProgramStatusGate(declared, code, declaration); !run || r.Status != "" {
		t.Fatalf("declared, row landed: %+v run=%t, want the row run", r, run)
	}
}

// Kills a builtin family that declares program status without a WC-PSTAT
// profile (its declaration would never be held to the rows), and a family
// listed twice.
func TestEveryDeclaredBuiltinHasProgramStatusProfile(t *testing.T) {
	byFamily := map[string]Profile{}
	for _, profile := range ProgramStatusProfiles() {
		family := ProfileFamily(profile)
		if _, dup := byFamily[family]; dup {
			t.Errorf("family %q has two program-status profiles", family)
		}
		byFamily[family] = profile
	}
	for family, spec := range config.BuiltinProviders() {
		if !spec.ProgramStatus.Declared() {
			continue
		}
		if profile, ok := byFamily[family]; !ok || !profile.ProgramStatus.Declared {
			t.Errorf("builtin %q declares program status but no WC-PSTAT profile expects it", family)
		}
	}
}
