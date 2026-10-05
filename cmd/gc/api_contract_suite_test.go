package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/api/apicontract"
)

// TestAPIContractSuite is the in-process API contract suite (P1-4/P1-5 of the
// supervisor API test-hardening plan). One real city (see
// api_contract_harness_test.go) serves every family below through the
// generated client; every response is validated against the live OpenAPI
// document; and at the end an operationId coverage guard fails if the spec
// grew an operation that is neither exercised here nor explicitly waived in
// contractWaivers.
//
// How to extend: add calls to the family that owns the resource (or add a
// family to contractFamilies), using h.client and expectStatus. If an
// operation genuinely cannot run in-process, add it to contractWaivers with
// the reason and the owner that covers it instead. Delete a waiver as soon as
// the suite exercises the operation — the guard fails on redundant waivers.
func TestAPIContractSuite(t *testing.T) {
	h := newContractHarness(t)

	ran, failed := 0, 0
	for _, fam := range contractFamilies {
		executed := false
		ok := t.Run(fam.name, func(t *testing.T) {
			executed = true
			before := len(h.transport.Failures())
			fam.run(t, h)
			for _, err := range h.transport.Failures()[before:] {
				var v *apicontract.ViolationError
				if errors.As(err, &v) {
					if reason, known := contractKnownSpecViolations[v.OperationID]; known {
						t.Logf("KNOWN BUG still present (%s): %v", reason, err)
						continue
					}
				}
				t.Errorf("spec violation: %v", err)
			}
		})
		if executed {
			ran++
		}
		if !ok {
			failed++
		}
	}

	if ran != len(contractFamilies) || failed > 0 {
		t.Logf("operationId coverage guard skipped: %d/%d families ran, %d failed", ran, len(contractFamilies), failed)
		return
	}
	cov := apicontract.CheckCoverage(h.spec.Operations(), h.transport.ExercisedIDs(), contractWaivers)
	t.Logf("API contract coverage: %d/%d operations exercised (%.1f%%), %d waived",
		cov.Exercised, cov.Total, cov.Percent(), cov.Waived)
	if err := cov.Err(); err != nil {
		t.Error(err)
	}
	if cov.Percent() < contractCoverageFloor {
		t.Errorf("API contract coverage %.1f%% fell below the ratchet floor %.1f%%", cov.Percent(), contractCoverageFloor)
	}
}

// contractFamily is one resource family of the suite. Families share the
// harness and run in order; later families may rely on state earlier ones
// created only where documented in the family.
type contractFamily struct {
	name string
	run  func(t *testing.T, h *contractHarness)
}

var contractFamilies = []contractFamily{
	{"supervisor", contractSupervisorFamily},
	{"beads", contractBeadsFamily},
	{"sling", contractSlingFamily},
	{"sessions", contractSessionsFamily},
	{"agents", contractAgentsFamily},
	{"rigs", contractRigsFamily},
	{"events", contractEventsFamily},
	{"formulas", contractFormulasFamily},
	{"convoys", contractConvoysFamily},
	{"orders", contractOrdersFamily},
	{"mail", contractMailFamily},
	{"extmsg", contractExtmsgFamily},
	{"async-202", contractAsyncFamily},
	{"config", contractConfigFamily},
}

// contractCoverageFloor is a ratchet: raise it when the suite covers more
// operations; never lower it to make a change pass.
const contractCoverageFloor = 95.0
