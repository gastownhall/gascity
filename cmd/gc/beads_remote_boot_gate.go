package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// The boot capability gate (DESIGN C5 G10), at the composition root.
//
// gc start and the supervisor run it once per city, after the config loads
// and before the city starts anything that opens a store: every scope (the
// city and each active rig) whose metadata selects a registered remote
// backend is checked with wire_compat (beads.CheckRemoteScopeBootGate), and a
// scope the server cannot serve natively refuses the city's start with a
// *beads.RemoteCapabilityGateError naming the scope, the check, the missing
// tokens and the native_transport = "off" escape hatch. gc doctor reports the
// same verdicts under the check name wire_compat.
//
// Under native_transport = "off" (or the deprecated GC_BEADS_FORCE_FALLBACK)
// no scope opens natively, so the gate has nothing to protect and does not
// dial.

// checkRemoteScopeBootGate is the per-scope gate, behind a seam for tests.
var checkRemoteScopeBootGate = beads.CheckRemoteScopeBootGate

// remoteBootGateScope is one scope the gate evaluates.
type remoteBootGateScope struct {
	Label string // "city" or `rig "name"`
	Root  string
}

// remoteBootGateScopes lists the city and every active (not suspended) rig
// with a path, rig paths made absolute against the city.
func remoteBootGateScopes(cityPath string, cfg *config.City) []remoteBootGateScope {
	scopes := []remoteBootGateScope{{Label: "city", Root: cityPath}}
	if cfg == nil {
		return scopes
	}
	suspended := loadSuspensionStateBestEffort(cityPath)
	for _, rig := range cfg.Rigs {
		path := strings.TrimSpace(rig.Path)
		if path == "" || suspensionstate.EffectiveRigSuspended(suspended, rig.Name, rig.EffectiveSuspendedOnStart()) {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cityPath, path)
		}
		scopes = append(scopes, remoteBootGateScope{Label: fmt.Sprintf("rig %q", rig.Name), Root: path})
	}
	return scopes
}

// remoteBootGateApplies reports whether this city opens remote scopes
// natively at all.
func remoteBootGateApplies(cfg *config.City) bool {
	return resolvedNativeTransportMode(cfg) != beads.NativeTransportOff && !beads.ForceNativeFallbackActive()
}

// remoteBeadsBootGate refuses a city one of whose remote scopes fails the
// gate. A city with no remote scope passes without dialing anything.
func remoteBeadsBootGate(ctx context.Context, cityPath string, cfg *config.City) error {
	if !remoteBootGateApplies(cfg) {
		return nil
	}
	var refusals []error
	for _, scope := range remoteBootGateScopes(cityPath, cfg) {
		if _, remote, err := checkRemoteScopeBootGate(ctx, cityPath, scope.Root); remote && err != nil {
			refusals = append(refusals, err)
		}
	}
	return errors.Join(refusals...)
}

// remoteWireCompatDoctorCheck is gc doctor's view of the boot gate.
type remoteWireCompatDoctorCheck struct {
	cityPath string
	cfg      *config.City
}

// newRemoteWireCompatDoctorCheck returns the check, or nil when no scope of
// the city selects a remote backend (nothing to report).
func newRemoteWireCompatDoctorCheck(cityPath string, cfg *config.City) doctor.Check {
	for _, scope := range remoteBootGateScopes(cityPath, cfg) {
		if _, remote := beads.RemoteBackendActivationRoot(scope.Root, cityPath); remote {
			return &remoteWireCompatDoctorCheck{cityPath: cityPath, cfg: cfg}
		}
	}
	return nil
}

func (c *remoteWireCompatDoctorCheck) Name() string { return string(beads.RemoteBootGateCheck) }

func (c *remoteWireCompatDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	result := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK}
	if !remoteBootGateApplies(c.cfg) {
		result.Message = `remote beads scopes run through the bd CLI (beads.native_transport = "off"); the boot capability gate does not apply`
		return result
	}
	var failed, warned, passed []string
	for _, scope := range remoteBootGateScopes(c.cityPath, c.cfg) {
		verdict, remote, err := checkRemoteScopeBootGate(context.Background(), c.cityPath, scope.Root)
		if !remote {
			continue
		}
		line := fmt.Sprintf("%s (%s): %s", scope.Label, scope.Root, verdict.Summary)
		switch {
		case err != nil:
			failed = append(failed, err.Error())
		case verdict.State == contract.PreflightCheckWarn:
			warned = append(warned, line)
		default:
			passed = append(passed, line)
		}
	}
	result.Details = append(append(append(result.Details, failed...), warned...), passed...)
	switch {
	case len(failed) > 0:
		result.Status = doctor.StatusError
		result.Message = fmt.Sprintf("boot capability gate: %d remote beads scope(s) would refuse the city's start", len(failed))
		result.FixHint = `upgrade the bd serve behind the scope, re-point it with gc storage connect, or set beads.native_transport = "off"`
	case len(warned) > 0:
		result.Status = doctor.StatusWarning
		result.Message = fmt.Sprintf("boot capability gate: %d remote beads scope(s) start with recorded fallbacks", len(warned))
	default:
		result.Message = fmt.Sprintf("boot capability gate: %d remote beads scope(s) pass", len(passed))
	}
	return result
}

func (c *remoteWireCompatDoctorCheck) CanFix() bool                     { return false }
func (c *remoteWireCompatDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *remoteWireCompatDoctorCheck) WarmupEligible() bool             { return false }
