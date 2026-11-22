package main

import (
	"fmt"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Build-info readers of the bd-beads-version check, injectable for tests.
// The bd reader scans the file and never executes it.
var (
	bdBeadsVersionReadBdBuildInfo = readGoBuildInfo
	bdBeadsVersionReadGcBuildInfo = debug.ReadBuildInfo
)

// bdBeadsVersionCheck compares the beads module embedded in each bd-backed
// scope's bd with the one gc links. gc migrates a store to the schema of the
// beads it links, and a bd built from another beads version then fails every
// read of that store with "schema version mismatch". bd-schema compares a
// database with gc; this compares bd with gc.
//
// A skew is a warning, never an error: it is advisory, and it never blocks
// gc start, whose warm-up runs the check too. Nothing is compared, and the
// check is OK with the reason, when either version is unknown.
type bdBeadsVersionCheck struct {
	cityPath string
	cfg      *config.City
	// resolveBd is resolveBdBinaryForScope, injectable for tests.
	resolveBd func(cityPath, scopeRoot string) (string, error)
}

func newBdBeadsVersionCheck(cityPath string, cfg *config.City) *bdBeadsVersionCheck {
	return &bdBeadsVersionCheck{cityPath: cityPath, cfg: cfg, resolveBd: resolveBdBinaryForScope}
}

func (c *bdBeadsVersionCheck) Name() string                     { return "bd-beads-version" }
func (c *bdBeadsVersionCheck) CanFix() bool                     { return false }
func (c *bdBeadsVersionCheck) Fix(_ *doctor.CheckContext) error { return nil }
func (c *bdBeadsVersionCheck) WarmupEligible() bool             { return true }

// bdBeadsVersionScope is one scope whose bd the check compares.
type bdBeadsVersionScope struct {
	label string
	root  string
}

// scopes returns the city scope and every bound rig scope.
func (c *bdBeadsVersionCheck) scopes() []bdBeadsVersionScope {
	out := []bdBeadsVersionScope{{label: "city", root: bdCityScopeTarget(c.cityPath, c.cfg).ScopeRoot}}
	for _, rig := range c.cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			out = append(out, bdBeadsVersionScope{label: fmt.Sprintf("rig %q", rig.Name), root: bdRigScopeTarget(c.cityPath, rig).ScopeRoot})
		}
	}
	return out
}

// bdBeadsVersionRead is what one bd binary's build info says: the beads
// version it embeds, or why that is unknown.
type bdBeadsVersionRead struct {
	version string
	unknown string
}

func readBdBeadsVersion(bdPath string) bdBeadsVersionRead {
	info, err := bdBeadsVersionReadBdBuildInfo(bdPath)
	if err != nil {
		return bdBeadsVersionRead{unknown: fmt.Sprintf("no Go build info (a shim or non-Go bd?): %v", err)}
	}
	version, ok := contract.BeadsModuleVersion(info)
	if !ok {
		return bdBeadsVersionRead{unknown: "its build info names no beads version to compare (a (devel) source build, a replaced module, or no beads module)"}
	}
	return bdBeadsVersionRead{version: version}
}

// linkedBeadsVersion returns the beads version gc links, or why it is unknown.
func linkedBeadsVersion() (version, unknown string) {
	info, ok := bdBeadsVersionReadGcBuildInfo()
	if !ok {
		return "", "gc was built without module build info"
	}
	version, known := contract.BeadsModuleVersion(info)
	if !known {
		return "", "gc's build info names no beads version to compare (a (devel) source build or a replaced module)"
	}
	return version, ""
}

func (c *bdBeadsVersionCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Severity: doctor.SeverityAdvisory}
	if c.cfg == nil {
		r.Message = "not compared: no city config"
		return r
	}
	var scopes []bdBeadsVersionScope
	for _, scope := range c.scopes() {
		if providerUsesBdStoreContract(rawBeadsProviderForScope(scope.root, c.cityPath)) {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 0 {
		r.Message = "not compared: no scope uses a bd-backed store"
		return r
	}
	linked, unknown := linkedBeadsVersion()
	if unknown != "" {
		r.Message = "not compared: " + unknown
		return r
	}

	var skewed, uncompared []string
	matched := 0
	// One read per resolved binary: scopes sharing a bd read it once.
	reads := map[string]bdBeadsVersionRead{}
	for _, scope := range scopes {
		bdPath, err := c.resolveBd(c.cityPath, scope.root)
		if err != nil {
			uncompared = append(uncompared, fmt.Sprintf("%s: bd not resolved: %v", scope.label, err))
			continue
		}
		key := bdPath
		if resolved, err := filepath.EvalSymlinks(bdPath); err == nil {
			key = resolved
		}
		read, seen := reads[key]
		if !seen {
			read = readBdBeadsVersion(bdPath)
			reads[key] = read
		}
		switch {
		case read.unknown != "":
			uncompared = append(uncompared, fmt.Sprintf("%s: %s: %s", scope.label, bdPath, read.unknown))
		case read.version != linked:
			skewed = append(skewed, fmt.Sprintf("%s: %s embeds beads %s, but gc links beads %s", scope.label, bdPath, read.version, linked))
		default:
			matched++
		}
	}

	switch {
	case len(skewed) > 0:
		r.Status = doctor.StatusWarning
		if len(skewed) == 1 {
			r.Message = skewed[0] + "; that bd fails every read of a store gc migrated"
		} else {
			r.Message = fmt.Sprintf("%d scope(s) run a bd built from another beads version than gc links (%s); such a bd fails every read of a store gc migrated", len(skewed), linked)
		}
		r.Details = make([]string, 0, len(skewed)+len(uncompared))
		r.Details = append(r.Details, skewed...)
		r.Details = append(r.Details, uncompared...)
		r.FixHint = "run `make install-bd` in the gascity checkout gc was built from, then make sure that bd is the one the scope resolves (PATH order, or BD_BIN in [workspace.env])"
	case len(uncompared) > 0 && matched == 0:
		r.Message = "not compared: " + strings.Join(uncompared, "; ")
		r.Details = uncompared
	case len(uncompared) > 0:
		r.Message = fmt.Sprintf("every compared bd embeds beads %s, the version gc links; not compared: %s", linked, strings.Join(uncompared, "; "))
		r.Details = uncompared
	default:
		r.Message = fmt.Sprintf("every bd-backed scope's bd embeds beads %s, the version gc links", linked)
	}
	return r
}
