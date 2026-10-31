package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// doltDriftCheck detects bd-vs-gc Dolt drift in managed-city topology: rigs
// configured inherited_city but running their own Dolt (Case A), port-file
// mismatches between rig mirrors and the canonical managed city port
// (Case B), stale .dolt/sql-server.info files left over from abandoned
// rig-local servers (Case C), and inherited rigs whose .beads/dolt-server.port
// mirror is missing or blank where the port-file sync would pin it (Case D —
// raw bd in such a rig resolves no managed port at all). Embedded rigs serve
// their own store and are skipped, as the port-file sync skips them.
type doltDriftCheck struct {
	cityPath string
	cfg      *config.City

	// repinnablePortDrift is set by Run when at least one rig's
	// .beads/dolt-server.port needs the port-file sync's repair: a non-empty
	// mirror that disagrees with the managed city port (Case B), or a missing
	// or blank mirror the sync would write (Case D).
	repinnablePortDrift bool
	// liveRigLocalBlocking is set by Run when a rig configured inherited_city
	// still has a live rig-local Dolt (Case A). Re-pinning a port file while a
	// rig-local server is listening would point the file at the wrong server,
	// so it suppresses the auto-fix until the operator resolves ownership.
	liveRigLocalBlocking bool
}

func newDoltDriftCheck(cityPath string, cfg *config.City) *doltDriftCheck {
	return &doltDriftCheck{cityPath: cityPath, cfg: cfg}
}

func (c *doltDriftCheck) Name() string { return "dolt-drift" }

func (c *doltDriftCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
	// Reset fix-eligibility state; Run may be invoked again (the doctor re-runs
	// it after a successful Fix to verify), so stale flags must not persist.
	c.repinnablePortDrift = false
	c.liveRigLocalBlocking = false
	if c.cfg == nil || !workspaceUsesManagedBdStoreContract(c.cityPath, c.cfg.Rigs) {
		r.Status = doctor.StatusOK
		r.Message = "not using bd-backed Dolt topology"
		return r
	}

	cityState, _, err := resolveDesiredCityEndpointState(c.cityPath, c.cfg.Dolt, config.EffectiveHQPrefix(c.cfg))
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("resolve city endpoint state: %v", err)
		return r
	}
	if cityState.EndpointOrigin != contract.EndpointOriginManagedCity {
		r.Status = doctor.StatusOK
		r.Message = "city not in managed_city mode; drift check not applicable"
		return r
	}

	managedPort := currentResolvableManagedDoltPort(c.cityPath)

	// City-level classification probes for Case D, resolved lazily on the
	// first rig whose mirror turns out missing or blank, so a city whose
	// mirrors are all healthy pays for none of them.
	cityMirrorProbes := &portMirrorCityProbes{cityPath: c.cityPath}

	var errors []string
	var warnings []string

	for i := range c.cfg.Rigs {
		rig := c.cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if !rigUsesManagedBdStoreContract(c.cityPath, rig) {
			continue
		}
		embedded, err := scopeMetadataRecordsEmbeddedDolt(fsys.OSFS{}, rig.Path)
		if err != nil {
			errors = append(errors, fmt.Sprintf("rig %q classify storage mode: %v", rig.Name, err))
			continue
		}
		if embedded {
			// An embedded rig serves its own store (ga-p9iuv): its port file is
			// not gc's to re-pin, so there is no drift to report or fix.
			continue
		}
		rigState, err := resolveDesiredRigEndpointState(c.cityPath, rig, cityState)
		if err != nil {
			errors = append(errors, fmt.Sprintf("rig %q resolve endpoint state: %v", rig.Name, err))
			continue
		}
		if rigState.EndpointOrigin != contract.EndpointOriginInheritedCity {
			// Rig is explicit (or not inherited). No drift analysis applies —
			// its Dolt is its own responsibility.
			continue
		}

		livePID, livePort, infoExists, liveRigLocal := rigLocalDoltPIDFromSQLServerInfo(rig.Path)

		// Case A: rig is inherited_city but has a live rig-local Dolt.
		if liveRigLocal {
			c.liveRigLocalBlocking = true
			errors = append(errors, fmt.Sprintf(
				"rig %q endpoint_origin=inherited_city but rig-local Dolt pid %d is listening on port %d (.dolt/sql-server.info); stop the rig-local server or acknowledge it with `gc rig set-endpoint %s --self --port %d --force`",
				rig.Name, livePID, livePort, rig.Name, livePort,
			))
		} else if infoExists {
			// Case C: stale rig-local sql-server.info (file present, but the
			// recorded PID is not serving the recorded port).
			warnings = append(warnings, fmt.Sprintf(
				"rig %q has stale .dolt/sql-server.info (pid %d is not a live Dolt listener for the recorded port); safe to delete after confirming no rig-local server is running",
				rig.Name, livePID,
			))
		}

		// Case B: a non-empty rig port file that disagrees with the managed
		// city port. Case D: the mirror is missing or blank where the
		// port-file sync would pin it, leaving raw bd in the rig with no
		// managed port to resolve.
		if managedPort != "" {
			rigPortFile := filepath.Join(rig.Path, ".beads", "dolt-server.port")
			data, readErr := os.ReadFile(rigPortFile)
			got := strings.TrimSpace(string(data))
			switch {
			case readErr == nil && got != "":
				if got != managedPort {
					c.repinnablePortDrift = true
					errors = append(errors, fmt.Sprintf(
						"rig %q .beads/dolt-server.port=%s disagrees with managed city port %s; `gc doctor --fix` re-pins it (or next `gc start` overwrites it)",
						rig.Name, got, managedPort,
					))
				}
			case readErr == nil || scopeArtifactAbsent(readErr):
				// Case D: a blank (whitespace-only) file or no file at all.
				// A live rig-local Dolt (Case A, reported above) owns the rig,
				// so nothing is added — restoring the inherited mirror now
				// would point raw bd at the wrong server. Any other read
				// error stays silent as before: `--fix` cannot replace, say,
				// a directory sitting at the mirror path.
				if !liveRigLocal {
					shape := "empty"
					if readErr != nil {
						shape = "missing"
					}
					expectsMirror, probeErr := rigExpectsManagedPortMirror(c.cityPath, cityState, rig, cityMirrorProbes)
					if probeErr != nil {
						errors = append(errors, fmt.Sprintf("rig %q classify port mirror ownership: %v", rig.Name, probeErr))
					} else if expectsMirror {
						c.repinnablePortDrift = true
						errors = append(errors, fmt.Sprintf(
							"rig %q .beads/dolt-server.port is %s, so raw bd in the rig cannot reach the managed city Dolt on port %s; `gc doctor --fix --check dolt-drift` restores it (or next `gc start` rewrites it)",
							rig.Name, shape, managedPort,
						))
					}
				}
			}
		}
	}

	if len(errors) == 0 && len(warnings) == 0 {
		r.Status = doctor.StatusOK
		r.Message = "no bd-vs-gc Dolt drift detected"
		return r
	}
	if len(errors) > 0 {
		r.Status = doctor.StatusError
		plural := ""
		if len(errors) != 1 {
			plural = "s"
		}
		r.Message = fmt.Sprintf("%d drift issue%s between rig-local Dolt state and managed city topology", len(errors), plural)
		r.Details = append(append([]string{}, errors...), warnings...)
		r.FixHint = "`gc doctor --fix` re-pins or restores rig port-file mirrors; for a live rig-local Dolt, stop it or acknowledge explicit ownership with `gc rig set-endpoint <rig> --self --port <port> --force` (use --external for non-local servers); remove stale .dolt/sql-server.info files"
		return r
	}
	plural := ""
	if len(warnings) != 1 {
		plural = "s"
	}
	r.Status = doctor.StatusWarning
	r.Message = fmt.Sprintf("%d stale rig-local Dolt state file%s", len(warnings), plural)
	r.Details = warnings
	r.FixHint = "remove stale .dolt/sql-server.info files whose PID is not serving the recorded port"
	return r
}

// portMirrorCityProbes lazily resolves the city-level classification probes
// Case D needs, so a Run whose rig mirrors are all healthy (the common case)
// pays for none of them: the probes run only when some rig's mirror turns out
// missing or blank.
type portMirrorCityProbes struct {
	cityPath string

	providerOwnedResolved bool
	providerOwnedResult   bool

	completeBindingResolved bool
	completeBindingResult   bool
}

func (p *portMirrorCityProbes) providerOwned() (bool, error) {
	if !p.providerOwnedResolved {
		owned, err := scopeProviderOwned(p.cityPath, p.cityPath)
		if err != nil {
			return false, err
		}
		p.providerOwnedResult, p.providerOwnedResolved = owned, true
	}
	return p.providerOwnedResult, nil
}

func (p *portMirrorCityProbes) completeStorageBinding() (bool, error) {
	if !p.completeBindingResolved {
		complete, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(p.cityPath))
		if err != nil {
			return false, err
		}
		p.completeBindingResult, p.completeBindingResolved = complete, true
	}
	return p.completeBindingResult, nil
}

// rigExpectsManagedPortMirror reports whether the port-file sync
// (syncConfiguredDoltPortFiles — this check's Fix path, and what `gc start`
// runs) would write the managed city port into an inherited rig's
// .beads/dolt-server.port mirror. Run has already established everything the
// two paths share (managed_city origin, a resolvable managed port, a
// non-embedded rig on the bd contract whose desired endpoint is
// inherited_city); this helper answers the skips Run cannot apply itself,
// each one the sync's own classification so detection and repair cannot drift
// apart silently:
//
//   - a proxied-server city removes rig mirrors instead of pinning them;
//   - a provider-owned city or rig is not gc's mirror to write;
//   - a complete storage binding on city or rig resolves through the opaque
//     backend, not a loopback port mirror.
//
// Classification errors are returned, not swallowed: guessing "no" keeps
// doctor green on the severed mirror it exists to catch, and guessing "yes"
// offers a re-pin the sync then refuses to perform.
func rigExpectsManagedPortMirror(cityPath string, cityState contract.ConfigState, rig config.Rig, cityProbes *portMirrorCityProbes) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(cityState.DoltMode), "proxied-server") {
		return false, nil
	}
	cityProviderOwned, err := cityProbes.providerOwned()
	if err != nil {
		return false, fmt.Errorf("classify city provider ownership: %w", err)
	}
	if cityProviderOwned {
		return false, nil
	}
	cityCompleteBinding, err := cityProbes.completeStorageBinding()
	if err != nil {
		return false, fmt.Errorf("classify city storage binding: %w", err)
	}
	if cityCompleteBinding {
		return false, nil
	}
	rigProviderOwned, err := scopeProviderOwned(cityPath, rig.Path)
	if err != nil {
		return false, fmt.Errorf("classify rig %q provider ownership: %w", rig.Name, err)
	}
	if rigProviderOwned {
		return false, nil
	}
	rigCompleteBinding, err := scopeHasCompleteStorageBinding(scopeMetadataJSONPath(rig.Path))
	if err != nil {
		return false, fmt.Errorf("classify rig %q storage binding: %w", rig.Name, err)
	}
	return !rigCompleteBinding, nil
}

// CanFix reports whether the detected drift is the re-pinnable kind: at least
// one rig port file needs the sync's repair — a non-empty mirror disagreeing
// with the managed city port (Case B) or a missing/blank mirror the sync
// restores (Case D) — and no rig has a conflicting live rig-local Dolt
// (Case A). Re-pinning while a rig-local server is listening would point the
// file at the wrong server, so that case is left for the operator (the
// FixHint covers it). Run populates these flags and is always invoked before
// CanFix by the doctor framework.
func (c *doltDriftCheck) CanFix() bool {
	return c.repinnablePortDrift && !c.liveRigLocalBlocking
}

// Fix re-pins the configured .beads/dolt-server.port files to the managed city
// port, repairing Case-B drift and restoring the missing or blank mirrors
// Case D reports. It reuses the same syncConfiguredDoltPortFiles path that
// `gc start` runs at startup, so a stale or severed rig port file no longer
// requires a full restart to correct. The doctor framework re-runs Run
// afterward to verify (and to surface any remaining non-re-pinnable drift).
func (c *doltDriftCheck) Fix(ctx *doctor.CheckContext) error {
	if c.cfg == nil {
		return fmt.Errorf("dolt-drift: no city config available to re-pin port files")
	}
	// Thread the framework's output writer so re-pin diagnostics are captured
	// with the rest of doctor output (and discarded on the JSON-collect path);
	// fall back to stderr for direct out-of-framework calls.
	warn := io.Writer(os.Stderr)
	if ctx != nil && ctx.Output != nil {
		warn = ctx.Output
	}
	return syncConfiguredDoltPortFiles(c.cityPath, c.cfg.Dolt, config.EffectiveHQPrefix(c.cfg), c.cfg.Rigs, warn)
}

// rigLocalDoltPIDFromSQLServerInfo reads the colon-separated PID:PORT:UUID
// content of rigPath/.dolt/sql-server.info (written by dolt sql-server). It
// returns the PID and port parsed from the file, whether the file exists at
// all, and whether that PID is currently alive and listening on the recorded
// port. When the file is missing the PID and port are zero and both bools are
// false.
func rigLocalDoltPIDFromSQLServerInfo(rigPath string) (pid int, port int, infoExists bool, pidAliveNow bool) {
	path := filepath.Join(rigPath, ".dolt", "sql-server.info")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false, false
	}
	parts := strings.SplitN(strings.TrimSpace(string(data)), ":", 3)
	if len(parts) < 1 || strings.TrimSpace(parts[0]) == "" {
		return 0, 0, true, false
	}
	parsed, convErr := strconv.Atoi(strings.TrimSpace(parts[0]))
	if convErr != nil || parsed <= 0 {
		return 0, 0, true, false
	}
	if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
		return parsed, 0, true, false
	}
	parsedPort, convErr := strconv.Atoi(strings.TrimSpace(parts[1]))
	if convErr != nil || parsedPort <= 0 {
		return parsed, 0, true, false
	}
	if !pidAlive(parsed) {
		return parsed, parsedPort, true, false
	}
	return parsed, parsedPort, true, findPortHolderPID(strconv.Itoa(parsedPort), parsed) == parsed
}
