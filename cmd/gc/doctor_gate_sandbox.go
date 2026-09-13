package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/doctor"
)

// doctorGateSandboxReadTimeout bounds the single bd fork this check makes.
// Matches the store preflight's bound so a saturated data plane costs doctor
// the same as it already does upstream of this check.
const doctorGateSandboxReadTimeout = 5 * time.Second

// gateSandboxEnv reproduces the environment convergence hands a gate command:
// HOME sandboxed to the city and an explicit whitelist, nothing inherited.
//
// One deliberate addition: BEADS_DOLT_AUTO_START=0. A gate inherits whatever
// the controller has set, but a diagnostic must never start a Dolt server as a
// side effect. The store preflight this check is gated behind already probes
// with auto-start off, so the two probes stay comparable.
func gateSandboxEnv(cityPath string) []string {
	gateEnv := convergence.ConditionEnv{CityPath: cityPath, StorePath: cityPath}.Environ()
	env := make([]string, 0, len(gateEnv)+1)
	for _, entry := range gateEnv {
		if name, _, ok := strings.Cut(entry, "="); ok && name == "BEADS_DOLT_AUTO_START" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "BEADS_DOLT_AUTO_START=0")
}

// gateSandboxReadCheck verifies that a bead read succeeds inside the gate
// sandbox, not just inside the controller's own environment.
//
// Convergence gates run with HOME=<CityPath> so a gate command cannot see the
// controller's home directory (.ssh, .gnupg). Anything a gc/bd read resolves
// from HOME therefore has to be threaded into the whitelist explicitly; when
// something is missed, every read inside every gate fails while the same read
// from the controller succeeds. That asymmetry is invisible without a probe —
// it surfaced only as workflows burning their retry attempts on gates that
// could not see the store (ga-pqlgh).
//
// The probe is the differential: it runs the same read the store preflight
// already ran, changing only the environment. The preflight is the control, so
// this check is registered only when the preflight passed; a failure here means
// the sandbox is blind, not that the store is down.
type gateSandboxReadCheck struct {
	cityPath string
	// probe performs the read. Injectable so tests never fork bd.
	probe func(cityPath string, env []string) error
}

func newGateSandboxReadCheck(cityPath string) *gateSandboxReadCheck {
	return &gateSandboxReadCheck{cityPath: cityPath, probe: defaultGateSandboxRead}
}

// defaultGateSandboxRead runs the store preflight's read under the gate
// environment. Read-only and bounded.
func defaultGateSandboxRead(cityPath string, env []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), doctorGateSandboxReadTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bd", "list", "--json", "--limit", "1")
	cmd.Dir = cityPath
	cmd.Env = env
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(string(out)); detail != "" {
		return fmt.Errorf("%w: %s", err, doctorClipGateOutput(detail))
	}
	return err
}

// doctorClipGateOutput keeps a failing probe's output to one readable line.
func doctorClipGateOutput(out string) string {
	const maxLen = 300
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > maxLen {
		return out[:maxLen] + "…"
	}
	return out
}

// Name returns the doctor check identifier.
func (c *gateSandboxReadCheck) Name() string { return "gate-sandbox-reads" }

// Run performs a bead read under the gate environment and reports the result.
func (c *gateSandboxReadCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}
	env := gateSandboxEnv(c.cityPath)
	if err := c.probe(c.cityPath, env); err != nil {
		res.Status = doctor.StatusError
		res.Message = fmt.Sprintf("bead read failed inside the gate sandbox (the same read succeeded for the controller): %v", err)
		res.Details = gateSandboxDetails(env)
		res.FixHint = "gate commands get HOME=<city> plus an explicit env whitelist, so anything gc/bd resolves from HOME must be threaded in: " +
			"check that BEADS_CREDENTIALS_FILE and GC_HOME in the details above point at real, readable paths " +
			"(gc threads the resolved beads credentials file into the gate env; set BEADS_CREDENTIALS_FILE in the controller's environment to override)"
		return res
	}
	res.Status = doctor.StatusOK
	res.Message = "gate sandbox can read the bead store"
	res.Details = gateSandboxDetails(env)
	return res
}

// gateSandboxDetails reports the whitelisted values that decide whether a gate
// read can resolve its credentials and caches.
func gateSandboxDetails(env []string) []string {
	wanted := []string{"HOME", "GC_HOME", "BEADS_DIR", "BEADS_CREDENTIALS_FILE"}
	values := make(map[string]string, len(wanted))
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok {
			values[name] = value
		}
	}
	details := make([]string, 0, len(wanted))
	for _, name := range wanted {
		value, ok := values[name]
		if !ok {
			value = "(not set)"
		}
		details = append(details, name+"="+value)
	}
	return details
}

// CanFix reports that this check has no automatic remediation.
func (c *gateSandboxReadCheck) CanFix() bool { return false }

// Fix is a no-op; the remedy is controller-side env threading.
func (c *gateSandboxReadCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible keeps the probe out of `gc start`; it forks bd.
func (c *gateSandboxReadCheck) WarmupEligible() bool { return false }
