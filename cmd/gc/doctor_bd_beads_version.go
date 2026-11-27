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

// bdBeadsVersionCheck compares the beads module each bd-backed scope's bd was
// built from with the beads library gc links, by the version policy the
// native-store preflight applies (contract.RelateBDVersion). It warns about a
// bd older than that library: once a store is migrated past the schema such a
// bd knows (by gc's library where beads.allow_schema_behind_migrate allows
// it, or by a newer client), the bd refuses to read it. A newer release of
// the same major version reads it. It also warns about a newer major
// version, which semver does not promise is compatible. bd-schema compares a
// database with gc; this compares bd with gc.
//
// A warning is advisory, and it never blocks gc start, whose warm-up runs the
// check too. Nothing is compared, and the check is OK with the reason, when
// either version names no beads release (contract.ReadBeadsBuild).
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

// bdBackedScopes returns the city scope and every bound rig scope whose
// store is bd-backed.
func (c *bdBeadsVersionCheck) bdBackedScopes() []bdBeadsVersionScope {
	all := []bdBeadsVersionScope{{label: "city", root: bdCityScopeTarget(c.cityPath, c.cfg).ScopeRoot}}
	for _, rig := range c.cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			all = append(all, bdBeadsVersionScope{label: fmt.Sprintf("rig %q", rig.Name), root: bdRigScopeTarget(c.cityPath, rig).ScopeRoot})
		}
	}
	var backed []bdBeadsVersionScope
	for _, scope := range all {
		if providerUsesBdStoreContract(rawBeadsProviderForScope(scope.root, c.cityPath)) {
			backed = append(backed, scope)
		}
	}
	return backed
}

// readBdBeadsBuild reads which beads a bd binary was built from.
func readBdBeadsBuild(bdPath string) contract.BeadsBuild {
	info, err := bdBeadsVersionReadBdBuildInfo(bdPath)
	if err != nil {
		return contract.BeadsBuild{Unconfirmed: fmt.Sprintf("no Go build info, as in a shim or a non-Go bd: %v", err)}
	}
	return contract.ReadBeadsBuild(info)
}

// linkedBeadsVersion returns the beads version gc links, or why it cannot be
// compared.
func linkedBeadsVersion() (version, unconfirmed string) {
	info, ok := bdBeadsVersionReadGcBuildInfo()
	if !ok {
		return "", "gc was built without module build info"
	}
	build := contract.ReadBeadsBuild(info)
	if build.Unconfirmed != "" {
		return "", "the beads gc links names no release (" + build.Unconfirmed + ")"
	}
	return build.Version, ""
}

// bdBeadsVersionComparison sorts each scope's bd by the version policy.
type bdBeadsVersionComparison struct {
	linked       string
	incompatible []string
	uncompared   []string
	compatible   int
}

// compare resolves and reads each scope's bd, reading each binary once.
func (c *bdBeadsVersionCheck) compare(scopes []bdBeadsVersionScope, linked string) bdBeadsVersionComparison {
	cmp := bdBeadsVersionComparison{linked: linked}
	reads := map[string]contract.BeadsBuild{}
	for _, scope := range scopes {
		bdPath, err := c.resolveBd(c.cityPath, scope.root)
		if err != nil {
			cmp.uncompared = append(cmp.uncompared, fmt.Sprintf("%s: bd not resolved: %v", scope.label, err))
			continue
		}
		key := bdPath
		if resolved, err := filepath.EvalSymlinks(bdPath); err == nil {
			key = resolved
		}
		build, seen := reads[key]
		if !seen {
			build = readBdBeadsBuild(bdPath)
			reads[key] = build
		}
		if build.Unconfirmed != "" {
			cmp.uncompared = append(cmp.uncompared, fmt.Sprintf("%s: %s (beads version unknown: %s)", scope.label, bdPath, build.Unconfirmed))
			continue
		}
		switch relation := contract.RelateBDVersion(build.Version, linked); relation {
		case contract.BDVersionOlder:
			cmp.incompatible = append(cmp.incompatible, fmt.Sprintf("%s: %s embeds beads %s, older than the beads %s gc links; once a store is migrated past that bd's schema, the bd refuses to read it", scope.label, bdPath, build.Version, linked))
		case contract.BDVersionNewerMajor:
			cmp.incompatible = append(cmp.incompatible, fmt.Sprintf("%s: %s embeds beads %s, a newer major version than the beads %s gc links, which semver does not promise is compatible", scope.label, bdPath, build.Version, linked))
		default:
			if relation.Compatible() {
				cmp.compatible++
			} else {
				cmp.uncompared = append(cmp.uncompared, fmt.Sprintf("%s: %s (beads version unknown: %s is not semver)", scope.label, bdPath, build.Version))
			}
		}
	}
	return cmp
}

// result renders the comparison as the check's result.
func (cmp bdBeadsVersionComparison) result(r *doctor.CheckResult) *doctor.CheckResult {
	switch {
	case len(cmp.incompatible) > 0:
		r.Status = doctor.StatusWarning
		if len(cmp.incompatible) == 1 {
			r.Message = cmp.incompatible[0]
		} else {
			r.Message = fmt.Sprintf("%d scope(s) resolve a bd older than, or a major version ahead of, the beads %s gc links", len(cmp.incompatible), cmp.linked)
		}
		r.Details = append(append(make([]string, 0, len(cmp.incompatible)+len(cmp.uncompared)), cmp.incompatible...), cmp.uncompared...)
		r.FixHint = fmt.Sprintf("install bd %s, or a newer release of the same major version, and make sure it is the bd each listed scope resolves (PATH order, or BD_BIN in [workspace.env])", cmp.linked)
	case len(cmp.uncompared) > 0 && cmp.compatible == 0:
		r.Message = "not compared: " + strings.Join(cmp.uncompared, "; ")
		r.Details = cmp.uncompared
	case len(cmp.uncompared) > 0:
		r.Message = fmt.Sprintf("every compared bd embeds a beads version compatible with the %s gc links; not compared: %s", cmp.linked, strings.Join(cmp.uncompared, "; "))
		r.Details = cmp.uncompared
	default:
		r.Message = fmt.Sprintf("every bd-backed scope's bd embeds a beads version compatible with the %s gc links", cmp.linked)
	}
	return r
}

func (c *bdBeadsVersionCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Severity: doctor.SeverityAdvisory}
	if c.cfg == nil {
		r.Message = "not compared: no city config"
		return r
	}
	scopes := c.bdBackedScopes()
	if len(scopes) == 0 {
		r.Message = "not compared: no scope uses a bd-backed store"
		return r
	}
	linked, unconfirmed := linkedBeadsVersion()
	if unconfirmed != "" {
		r.Message = "not compared: " + unconfirmed
		return r
	}
	return c.compare(scopes, linked).result(r)
}
