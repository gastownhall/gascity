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

// The bd-beads-version doctor check is gc's warning half of keeping bd and
// gc in lockstep (the make install-bd half lives in scripts/install-bd-pinned.sh):
// gc migrates a city store forward to the schema of the beads module it links,
// so a scope whose bd was built from another beads version fails every read
// with "schema version mismatch". For every bd-backed scope the check resolves
// that scope's bd the way its commands do, reads the beads module embedded in
// the binary's build info (never executing it), and warns when it differs from
// gc's own. It is the complement of bd-schema, which compares each database
// with gc: this one compares bd with gc.
//
// The warning is never an error and never gates anything: a bd distribution can
// legitimately carry a different beads release train while still speaking a
// schema the linked library opens. Either side whose version is unknown (no
// bd resolved, a shim or non-Go bd, a (devel) or replaced build, a gc without
// module info) keeps the check silent with the reason in its message, because
// a warning that cannot name both versions cannot be acted on.
type bdBeadsVersionCheck struct {
	cityPath string
	cfg      *config.City
	// readBinaryBuildInfo reads a bd binary's embedded build info without
	// running it; injectable for tests.
	readBinaryBuildInfo func(path string) (*debug.BuildInfo, error)
	// readOwnBuildInfo reads gc's own build info; injectable for tests.
	readOwnBuildInfo func() (*debug.BuildInfo, bool)
}

// bdBeadsVersionReadBinaryBuildInfo and bdBeadsVersionReadOwnBuildInfo are the
// live readers the check is built with; package vars so tests that construct
// the check through buildDoctorChecks (the warm-up path) can inject both.
var (
	bdBeadsVersionReadBinaryBuildInfo = readGoBuildInfo
	bdBeadsVersionReadOwnBuildInfo    = debug.ReadBuildInfo
)

// newBdBeadsVersionCheck builds the check over the city and its bound rigs.
func newBdBeadsVersionCheck(cityPath string, cfg *config.City) *bdBeadsVersionCheck {
	return &bdBeadsVersionCheck{
		cityPath:            cityPath,
		cfg:                 cfg,
		readBinaryBuildInfo: bdBeadsVersionReadBinaryBuildInfo,
		readOwnBuildInfo:    bdBeadsVersionReadOwnBuildInfo,
	}
}

// Name implements doctor.Check.
func (*bdBeadsVersionCheck) Name() string { return "bd-beads-version" }

// CanFix implements doctor.Check: installing the matching bd is the
// operator's make install-bd.
func (*bdBeadsVersionCheck) CanFix() bool { return false }

// Fix implements doctor.Check.
func (*bdBeadsVersionCheck) Fix(_ *doctor.CheckContext) error { return nil }

// WarmupEligible implements doctor.Check: the check reads files only, so gc
// start's warm-up can warn about a skewed bd before the first session does.
func (*bdBeadsVersionCheck) WarmupEligible() bool { return true }

// scopes returns the city scope and every bound rig scope.
func (c *bdBeadsVersionCheck) scopes() []execStoreTarget {
	out := []execStoreTarget{bdCityScopeTarget(c.cityPath, c.cfg)}
	for _, rig := range c.cfg.Rigs {
		if strings.TrimSpace(rig.Path) != "" {
			out = append(out, bdRigScopeTarget(c.cityPath, rig))
		}
	}
	return out
}

// scopeDisplayName names a scope in a warning detail or a not-compared note:
// the operator-facing label plus the root, so a city with several same-named
// rigs across paths can tell them apart.
func scopeDisplayName(target execStoreTarget) string {
	return fmt.Sprintf("%s (%s)", scopeLabel(target), target.ScopeRoot)
}

// Run implements doctor.Check.
func (c *bdBeadsVersionCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name(), Status: doctor.StatusOK, Severity: doctor.SeverityAdvisory}
	if c.cfg == nil {
		r.Message = "no city config; no bd to compare with gc"
		return r
	}
	own, ok := c.readOwnBuildInfo()
	if !ok {
		r.Message = "gc carries no module build info; no beads version to compare against"
		return r
	}
	ownVersion, ok := contract.BeadsModuleVersion(own)
	if !ok {
		r.Message = "gc's own beads module version is unknown (a (devel) or replaced build); nothing to compare"
		return r
	}

	var skew, skewVersions, unknown []string
	compared := 0
	// One read per distinct binary: a city whose rigs share one bd (a symlink
	// or a shared pin) reads it once, and every scope resolving that binary
	// gets the same verdict — including scopes sharing a shim, whose not-
	// compared reason must stay the shim's, not a misread (devel).
	type binaryRead struct {
		version    string
		unreadable bool
	}
	binaries := map[string]*binaryRead{}
	for _, target := range c.scopes() {
		if !providerUsesBdStoreContract(rawBeadsProviderForScope(target.ScopeRoot, c.cityPath)) {
			continue
		}
		label := scopeDisplayName(target)
		bdPath, err := resolveBdBinaryForScope(c.cityPath, target.ScopeRoot)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("%s: no bd resolved: %v", label, err))
			continue
		}
		key := bdPath
		if abs, evalErr := filepath.EvalSymlinks(bdPath); evalErr == nil {
			key = abs
		}
		read, cached := binaries[key]
		if !cached {
			read = &binaryRead{}
			if info, err := c.readBinaryBuildInfo(bdPath); err != nil {
				read.unreadable = true
			} else {
				read.version, _ = contract.BeadsModuleVersion(info)
			}
			binaries[key] = read
		}
		switch {
		case read.unreadable:
			unknown = append(unknown, fmt.Sprintf("%s: %s carries no readable Go build info (a shim or non-Go binary)", label, bdPath))
		case read.version == "":
			unknown = append(unknown, fmt.Sprintf("%s: %s embeds no comparable beads version (a (devel) or replaced build)", label, bdPath))
		case read.version != ownVersion:
			skew = append(skew, fmt.Sprintf("%s: %s embeds beads %s, gc links %s", label, bdPath, read.version, ownVersion))
			skewVersions = append(skewVersions, read.version)
		default:
			compared++
		}
	}

	if len(skew) > 0 {
		r.Status = doctor.StatusWarning
		// The message carries both versions itself: `gc start`'s warm-up mail
		// to the mayor quotes the message and the fix hint, not the details,
		// and a warning that cannot name the versions cannot be acted on.
		r.Message = fmt.Sprintf("%d scope(s) resolve a bd built from another beads module than gc links (e.g. bd %s vs gc %s); its reads can fail with schema version mismatch", len(skew), skewVersions[0], ownVersion)
		details := make([]string, 0, len(skew)+len(unknown))
		details = append(details, skew...)
		details = append(details, unknown...)
		r.Details = details
		r.FixHint = "run `make install-bd`, and make sure that bd is the one the scope resolves (PATH order or BD_BIN)"
		return r
	}
	switch {
	case compared > 0 && len(unknown) > 0:
		r.Message = fmt.Sprintf("every compared scope's bd embeds the beads module gc links (%s); %d scope(s) could not be compared: %s", ownVersion, len(unknown), strings.Join(unknown, "; "))
	case compared > 0:
		r.Message = fmt.Sprintf("every bd-backed scope's bd embeds the beads module gc links (%s)", ownVersion)
	case len(unknown) > 0:
		r.Message = fmt.Sprintf("no scope's bd could be compared with gc's beads %s: %s", ownVersion, strings.Join(unknown, "; "))
	default:
		r.Message = "no scope uses the bd store contract; no bd to compare with gc"
	}
	return r
}
