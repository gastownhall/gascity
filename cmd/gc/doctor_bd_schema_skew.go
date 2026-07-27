package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// bdSchemaSkewSignatures identifies a bd schema-skew / unreachable-database
// hard failure. Mirrors internal/config's bdFatalSkewSignatures (ga-qyw3wn)
// so the work-query failure surface and this operator-visible check agree
// on what counts as "skewed" — kept as an independent literal rather than an
// internal/config import since cmd/gc doctor checks do not depend on the
// shell-script layer.
var bdSchemaSkewSignatures = []string{
	"schema version mismatch",
	"Unable to open database",
}

// bdSchemaSkewProbeTimeout bounds the `bd doctor` probe so a hung or
// unreachable store cannot stall a `gc doctor` sweep.
const bdSchemaSkewProbeTimeout = 10 * time.Second

// bdSchemaSkewCheck reports when the `bd` binary resolved from the current
// PATH cannot open the city's bead store because its embedded schema is
// behind the live database. A skewed bd silently makes every default
// work-query probe return an empty result instead of erroring
// (bdFatalGuardFunctionScript in internal/config/config.go closes that hole
// for the claiming path); this check gives the operator visibility into the
// underlying binary/schema mismatch itself, naming the exact resolved bd
// path so they know which binary to rebuild or reorder off PATH.
//
// Advisory only (SeverityAdvisory): it is read-only and never gates `gc
// start` or dispatch.
type bdSchemaSkewCheck struct {
	cityPath string
	lookPath config.LookPathFunc
	probe    func(bdPath, cityPath string) string
}

func newBdSchemaSkewCheck(cityPath string) *bdSchemaSkewCheck {
	return &bdSchemaSkewCheck{
		cityPath: cityPath,
		lookPath: exec.LookPath,
		probe:    runBdSchemaSkewProbe,
	}
}

// runBdSchemaSkewProbe runs a lightweight bd command that opens the city's
// database (bd doctor) and returns its combined stdout+stderr. Errors are
// intentionally not returned: a non-zero exit is expected and diagnostic
// here (bd doctor exits non-zero when it finds issues, including schema
// skew) — the signature match on the output is the actual signal.
func runBdSchemaSkewProbe(bdPath, cityPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), bdSchemaSkewProbeTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, bdPath, "-C", cityPath, "doctor").CombinedOutput()
	return string(out)
}

func (c *bdSchemaSkewCheck) Name() string { return "bd-schema-skew" }

func (c *bdSchemaSkewCheck) CanFix() bool { return false }

func (c *bdSchemaSkewCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *bdSchemaSkewCheck) WarmupEligible() bool { return false }

func (c *bdSchemaSkewCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	res := &doctor.CheckResult{Name: c.Name(), Severity: doctor.SeverityAdvisory}

	lookPath := c.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	bdPath, err := lookPath("bd")
	if err != nil {
		res.Status = doctor.StatusOK
		res.Message = "bd not found in PATH; skipping schema-skew check"
		return res
	}

	probe := c.probe
	if probe == nil {
		probe = runBdSchemaSkewProbe
	}
	output := probe(bdPath, c.cityPath)

	for _, sig := range bdSchemaSkewSignatures {
		if !strings.Contains(output, sig) {
			continue
		}
		res.Status = doctor.StatusWarning
		res.Details = []string{strings.TrimSpace(output)}
		if sig == "Unable to open database" {
			// beads emits "Unable to open database" both for a schema-skewed
			// binary and for a simply-unreachable store (a down or still-
			// starting shared Dolt sql-server, wrong BEADS_DIR/port). Naming
			// only the "rebuild your bd" remedy misdirects the operator when
			// the real cause is a down server, so cover both.
			res.Message = fmt.Sprintf("bd at %s cannot open the live database (matched %q): the store may be schema-skewed or simply unreachable", bdPath, sig)
			res.FixHint = "confirm the bead store is reachable (dolt sql-server up, correct BEADS_DIR/port); if it is, rebuild or PATH-reorder the resolved bd binary ahead of any stale copy so it knows the live schema version"
			return res
		}
		res.Message = fmt.Sprintf("bd at %s is schema-skewed against the live database (matched %q)", bdPath, sig)
		res.FixHint = "rebuild or PATH-reorder the resolved bd binary ahead of any stale copy so it knows the live schema version"
		return res
	}

	res.Status = doctor.StatusOK
	res.Message = fmt.Sprintf("bd at %s: schema OK", bdPath)
	return res
}
