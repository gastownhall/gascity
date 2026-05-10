package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doltDriftCheck detects bd-vs-gc Dolt drift in managed-city topology: rigs
// configured inherited_city but running their own Dolt, port-file mismatches
// between rig mirrors and the canonical managed city port, and stale
// .dolt/sql-server.info files left over from abandoned rig-local servers.
type doltDriftCheck struct {
	cityPath string
	cfg      *config.City
}

func newDoltDriftCheck(cityPath string, cfg *config.City) *doltDriftCheck {
	return &doltDriftCheck{cityPath: cityPath, cfg: cfg}
}

func (c *doltDriftCheck) Name() string { return "dolt-drift" }

func (c *doltDriftCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}
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

	managedPort := currentManagedDoltPort(c.cityPath)

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

		// Case B: rig port file disagrees with managed city port.
		if managedPort != "" {
			rigPortFile := filepath.Join(rig.Path, ".beads", "dolt-server.port")
			if data, err := os.ReadFile(rigPortFile); err == nil {
				got := strings.TrimSpace(string(data))
				if got != "" && got != managedPort {
					errors = append(errors, fmt.Sprintf(
						"rig %q .beads/dolt-server.port=%s disagrees with managed city port %s; next `gc start` will overwrite the rig port file",
						rig.Name, got, managedPort,
					))
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
		r.FixHint = "stop the rig-local Dolt server, or acknowledge explicit rig-local ownership with `gc rig set-endpoint <rig> --self --port <port> --force`; use --external for non-local servers; remove stale .dolt/sql-server.info files"
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

func (c *doltDriftCheck) CanFix() bool { return false }

func (c *doltDriftCheck) Fix(_ *doctor.CheckContext) error { return nil }

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
	return parsed, parsedPort, true, findPortHolderPID(strconv.Itoa(parsedPort)) == parsed
}
