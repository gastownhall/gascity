package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// unroutedReadyWorkSampleIDs bounds the bead ids one scope's finding lists.
const unroutedReadyWorkSampleIDs = 10

// unroutedReadyWorkCheck reports open work that carries no gc.routed_to and
// that adoptableUnroutedWork accepts: a bead made with plain `bd create`
// that no pool claims until something routes it. It is advisory and never
// routes anything; routing is the operator's or the controller's job.
type unroutedReadyWorkCheck struct {
	cfg      *config.City
	cityPath string
	open     func(path string) (beads.StoreReader, error)
}

// newUnroutedReadyWorkCheck builds the check over cfg. open returns the bead
// store at a scope path (the city path or a rig path).
func newUnroutedReadyWorkCheck(cfg *config.City, cityPath string, open func(path string) (beads.StoreReader, error)) *unroutedReadyWorkCheck {
	return &unroutedReadyWorkCheck{cfg: cfg, cityPath: cityPath, open: open}
}

// Name returns the check name.
func (c *unroutedReadyWorkCheck) Name() string { return "unrouted-ready-work" }

// CanFix returns false: routing is the operator's or the controller's job.
func (c *unroutedReadyWorkCheck) CanFix() bool { return false }

// Fix is a no-op.
func (c *unroutedReadyWorkCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible returns false: unrouted backlog is a steady-state report,
// not a start-up blocker.
func (c *unroutedReadyWorkCheck) WarmupEligible() bool { return false }

// unroutedReadyWorkFinding is one scope's adoptable unrouted work.
type unroutedReadyWorkFinding struct {
	scope   string
	target  bool
	count   int
	samples []string
}

func (f unroutedReadyWorkFinding) describe() string {
	why := "the scope sets no unrouted_work_target"
	if f.target {
		why = "the scope's unrouted_work_target has not adopted them yet"
	}
	sample := strings.Join(f.samples, ", ")
	if f.count > len(f.samples) {
		sample += ", …"
	}
	return fmt.Sprintf("%s: %d open bead(s) carry no gc.routed_to; %s (%s)", f.scope, f.count, why, sample)
}

// Run reports each scope holding adoptable unrouted work, and each scope it
// could not read as skipped, never as OK.
func (c *unroutedReadyWorkCheck) Run(ctx *doctor.CheckContext) *doctor.CheckResult {
	if c.cfg == nil {
		return okCheck(c.Name(), "no config available")
	}
	findings, skipped := c.collect(ctx)
	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no open work is waiting without a route")
	}
	details := make([]string, 0, len(findings)+len(skipped))
	var hints []string
	noTarget, withTarget := false, false
	for _, f := range findings {
		details = append(details, f.describe())
		noTarget = noTarget || !f.target
		withTarget = withTarget || f.target
	}
	if noTarget {
		hints = append(hints, "route each bead with `gc sling <target> <id>`, set unrouted_work_target for the scope, or add a hold label if the beads are parked on purpose")
	}
	if withTarget {
		hints = append(hints, "where the scope sets unrouted_work_target the controller has not adopted them yet; check `gc status` and `gc doctor` route-recovery-quarantine")
	}
	details = append(details, skipped...)
	msg := fmt.Sprintf("%d scope(s) hold open work no pool will claim until it is routed", len(findings))
	switch {
	case len(findings) == 0:
		msg = fmt.Sprintf("unrouted-ready-work check skipped %d scope(s)", len(skipped))
		hints = append(hints, "fix bead store access, then rerun gc doctor")
	case len(skipped) > 0:
		msg = fmt.Sprintf("%s; %d scope(s) skipped", msg, len(skipped))
	}
	result := warnCheck(c.Name(), msg, strings.Join(hints, "; "), details)
	result.Severity = doctor.SeverityAdvisory
	return result
}

// collect reads the city plus every non-suspended, path-bearing rig, as
// pool-idle-routed-work does, and groups the adoptable unrouted rows by owning
// scope.
func (c *unroutedReadyWorkCheck) collect(ctx *doctor.CheckContext) (findings []unroutedReadyWorkFinding, skipped []string) {
	type scopePath struct{ label, path, target string }
	paths := []scopePath{{label: "city", path: c.cityPath, target: c.cfg.Workspace.UnroutedWorkTarget}}
	suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
	for _, rig := range c.cfg.Rigs {
		if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
			continue
		}
		paths = append(paths, scopePath{label: "rig " + rig.Name, path: rig.Path, target: rig.UnroutedWorkTarget})
	}
	policy := unroutedAdoptionPolicyFor(c.cfg)
	byScope := map[string]*unroutedReadyWorkFinding{}
	for _, sp := range paths {
		if strings.TrimSpace(sp.path) == "" || c.open == nil {
			continue
		}
		if ctx.Canceled() {
			skipped = append(skipped, sp.label+" skipped: check abandoned at its timeout")
			continue
		}
		reader, err := c.open(sp.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sp.label, err))
			continue
		}
		// The same Live open read the route-recovery backstop scans, so both
		// see one population: Live makes Status "open" mean raw open.
		items, err := reader.List(beads.ListQuery{Status: "open", AllowScan: true, Live: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing open beads: %v", sp.label, err))
			continue
		}
		for _, b := range items {
			if !adoptableUnroutedWork(b) {
				continue
			}
			label, hasTarget := sp.label, strings.TrimSpace(sp.target) != ""
			if sc, ok := policy.scopeFor(b.ID); ok {
				label, hasTarget = sc.label, sc.route != ""
			}
			f := byScope[label]
			if f == nil {
				f = &unroutedReadyWorkFinding{scope: label, target: hasTarget}
				byScope[label] = f
			}
			f.count++
			f.samples = append(f.samples, b.ID)
		}
	}
	for _, f := range byScope {
		sort.Strings(f.samples)
		if len(f.samples) > unroutedReadyWorkSampleIDs {
			f.samples = f.samples[:unroutedReadyWorkSampleIDs]
		}
		findings = append(findings, *f)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].scope < findings[j].scope })
	return findings, skipped
}
