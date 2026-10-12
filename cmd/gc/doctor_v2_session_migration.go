package main

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// v2SessionMigrationCheck lists, per leg with sample IDs, the open session rows
// v2 refuses to boot over. It opens each leg read-only itself: doctor's store
// factory can write (beads v1.3.1's native Dolt open migrates the schema). An
// unchecked leg may hide a refused row: an error under v2, a warning otherwise.
type v2SessionMigrationCheck struct {
	cfg      *config.City
	cityName string
	openLegs func() []v2SessionLeg
}

// v2SessionLeg is one census leg; with no read-only open, notChecked says why.
type v2SessionLeg struct {
	ref        string
	store      beads.Store
	notChecked string
}

func newV2SessionMigrationCheck(cfg *config.City, cityPath string) *v2SessionMigrationCheck {
	return &v2SessionMigrationCheck{
		cfg:      cfg,
		cityName: loadedCityName(cfg, cityPath),
		openLegs: func() []v2SessionLeg { return openV2SessionLegs(cityPath, cfg) },
	}
}

// Name implements doctor.Check.
func (c *v2SessionMigrationCheck) Name() string { return "v2-session-migration" }

// CanFix implements doctor.Check.
func (c *v2SessionMigrationCheck) CanFix() bool { return false }

// WarmupEligible implements doctor.Check: a census costs a list per leg.
func (c *v2SessionMigrationCheck) WarmupEligible() bool { return false }

// Fix implements doctor.Check.
func (c *v2SessionMigrationCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run implements doctor.Check.
func (c *v2SessionMigrationCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	var rows []session.Info
	var details []string
	seen, notChecked := map[string]bool{}, 0
	for _, leg := range c.openLegs() {
		if leg.store == nil {
			notChecked++
			details = append(details, fmt.Sprintf("leg %s: not checked (no read-only open): %s", leg.ref, leg.notChecked))
			continue
		}
		infos, err := sessionFrontDoor(leg.store).ListAll(session.ListAllOptions{Live: true})
		_ = closeBeadStoreHandle(leg.store)
		open := 0
		for _, info := range infos {
			// Folded first-leg-wins by bead ID, as the boot census folds.
			id := strings.TrimSpace(info.ID)
			if info.Closed || seen[id] {
				continue
			}
			seen[id] = id != ""
			rows = append(rows, info)
			open++
		}
		if err != nil {
			notChecked++
			details = append(details, fmt.Sprintf("leg %s: not checked (read failed after %d row(s)): %v", leg.ref, open, err))
			continue
		}
		details = append(details, fmt.Sprintf("leg %s: %d open session row(s)", leg.ref, open))
	}
	m := tallyV2SessionMigration(c.cfg, c.cityName, rows)
	details = append(details, v2SessionMigrationSamples(c.cfg, c.cityName, m, rows)...)
	flagged := 0
	for _, n := range m.UnknownStates {
		flagged += n
	}
	msg := fmt.Sprintf("%d open session row(s): %d in a state main does not know, %d shared pool-slot session name(s), %d named session(s) claimed by more than one row",
		len(rows), flagged, len(m.SharedSlotNames), len(m.DuplicateNamed))
	mode, _, _ := c.cfg.Daemon.SessionReconcilerMode()
	switch {
	case notChecked > 0 && mode == config.SessionReconcilerV2:
		return errorCheck(c.Name(), msg+fmt.Sprintf("; %d leg(s) not checked, counts are a lower bound", notChecked),
			"read the unchecked legs from a store copy", details)
	case notChecked > 0:
		return warnCheck(c.Name(), msg+fmt.Sprintf("; %d leg(s) not checked, counts are a lower bound", notChecked),
			"read the unchecked legs from a store copy; a legacy city warns here, so a census must look for this warning's \"not checked\" details, not the exit code", details)
	case m.refusal() != nil && mode == config.SessionReconcilerV2:
		return warnCheck(c.Name(), msg+"; v2 refuses to boot over them", "the repair (--fix) is not available yet", details)
	case m.refusal() != nil:
		msg += " (legacy runs over these; v2 would refuse to boot)"
	}
	r := okCheck(c.Name(), msg)
	r.Details = details
	return r
}

// v2SessionMigrationSamples lists up to ten bead IDs per class m counted.
func v2SessionMigrationSamples(cfg *config.City, cityName string, m v2SessionMigration, rows []session.Info) []string {
	samples := map[string][]string{}
	add := func(class, id string) {
		if len(samples[class]) < 10 {
			samples[class] = append(samples[class], id)
		}
	}
	for _, info := range rows {
		state, unknown, slot, named := v2SessionMigrationKeys(cfg, cityName, info)
		if unknown {
			add(fmt.Sprintf("state %q", state), info.ID)
		}
		if m.SharedSlotNames[slot] > 0 {
			add(fmt.Sprintf("shared slot name %q", slot), info.ID)
		}
		if m.DuplicateNamed[named] > 0 {
			add(fmt.Sprintf("named session %q", named), info.ID)
		}
	}
	var out []string
	for _, class := range slices.Sorted(maps.Keys(samples)) {
		out = append(out, fmt.Sprintf("%s: %s", class, strings.Join(samples[class], " ")))
	}
	return out
}

// openV2SessionLegs opens the census legs read-only in the session plan's
// order: the sessions binding (SQLite mode=ro), the city, the serving rigs.
func openV2SessionLegs(cityPath string, cfg *config.City) []v2SessionLeg {
	var legs []v2SessionLeg
	if target, ok, err := resolveInfraBindingTarget(cityPath, cfg); err != nil {
		legs = append(legs, v2SessionLeg{ref: "binding", notChecked: err.Error()})
	} else if ok {
		present, err := infraPathExists(target.Database) // no database yet: no rows
		leg := v2SessionLeg{ref: "binding:" + target.Binding}
		if err == nil && present {
			leg.store, err = openInfraBindingReadOnly(target)
		}
		if err != nil {
			leg.notChecked = err.Error()
		}
		if err != nil || present {
			legs = append(legs, leg)
		}
	}
	legs = append(legs, openV2SessionScopeLeg(cityPath, cfg, "city", cityPath, bdCommandRunnerForCity(cityPath)))
	suspended := buildSuspendedRigPathsForCity(cfg, cityPath)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" || suspended[filepath.Clean(rig.Path)] {
			continue
		}
		legs = append(legs, openV2SessionScopeLeg(cityPath, cfg, "rig:"+rig.Name, rig.Path, bdCommandRunnerForRig(cityPath, cfg, rig.Path)))
	}
	return legs
}

// openV2SessionScopeLeg is a scope's bd store running only `bd --readonly list`
// (no export-file reap), or a not-checked leg when bd has no read-only open.
func openV2SessionScopeLeg(cityPath string, cfg *config.City, ref, dir string, run beads.CommandRunner) v2SessionLeg {
	backend, ok, err := contract.ReadMetadataBackend(fsys.OSFS{}, scopeMetadataJSONPath(dir))
	if !ok {
		backend = beadsBackend(cityPath)
	}
	leg := v2SessionLeg{ref: ref, notChecked: v2SessionBdRefusal(rawBeadsProviderForScope(dir, cityPath), scopeUsesProxiedDoltMode(cityPath, dir), backend)}
	if err != nil {
		leg.notChecked = err.Error()
	}
	if leg.notChecked == "" {
		leg.store = beads.NewBdStoreWithPrefix(dir, readOnlyBdRunner(run), issuePrefixForScope(dir, cityPath, cfg), bdStoreOptionsForConfig(cfg)...)
	}
	return leg
}

// v2SessionBdRefusal says why a scope's bd has no read-only open, or "".
// Verified on bd d3ab32773462 and dd95460966: --readonly skips version
// tracking, version-bump auto-migrate, auto-start and post-run maintenance,
// and opens embedded Dolt write-refusing, server Dolt without schema init, and
// PostgreSQL non-provisioning (`list` is on its read-only command list). bd
// refuses --readonly on proxied-server Dolt; no other backend is verified.
func v2SessionBdRefusal(provider string, proxied bool, backend string) string {
	switch {
	case !contract.ProviderUsesBDContract(provider):
		return fmt.Sprintf("provider %q is not read through bd", provider)
	case proxied:
		return "proxied-server Dolt, where bd refuses --readonly"
	case !contract.IsDoltBackend(backend) && !strings.EqualFold(strings.TrimSpace(backend), "postgres"):
		return fmt.Sprintf("backend %q has no verified bd --readonly open", backend)
	}
	return ""
}

var errV2SessionNotReadOnly = errors.New("v2-session-migration runs only `bd --readonly list`")

// readOnlyBdRunner runs `bd list` as `bd --readonly list` and refuses all else:
// bd may open any other subcommand writable (on PostgreSQL even --readonly).
func readOnlyBdRunner(run beads.CommandRunner) beads.CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 || args[0] != "list" {
			return nil, fmt.Errorf("%w: %s %s", errV2SessionNotReadOnly, name, strings.Join(args, " "))
		}
		return run(dir, name, append([]string{"--readonly"}, args...)...)
	}
}
