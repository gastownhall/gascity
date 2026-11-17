package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// storageRigsCheck is the doctor's per-rig storage verdict on a split city
// (S4b): each rig imported, clean, awaiting import, retained (a clear left
// pending), stranded or unreachable — the same census `gc storage status`
// prints — plus the id-reuse check: a rig row under an id the rig's import
// moved for a different bead, which by-id reads answer with the binding's
// relic.
type storageRigsCheck struct {
	cfg      *config.City
	cityPath string
}

func newStorageRigsCheck(cfg *config.City, cityPath string) *storageRigsCheck {
	return &storageRigsCheck{cfg: cfg, cityPath: cityPath}
}

func (c *storageRigsCheck) Name() string                   { return "storage-rigs" }
func (c *storageRigsCheck) CanFix() bool                   { return false }
func (c *storageRigsCheck) WarmupEligible() bool           { return false }
func (c *storageRigsCheck) Fix(*doctor.CheckContext) error { return nil }

func (c *storageRigsCheck) Run(*doctor.CheckContext) *doctor.CheckResult {
	target, ok, err := resolveInfraBindingTarget(c.cityPath, c.cfg)
	if err != nil {
		return warnCheck(c.Name(), "could not resolve the city binding", "fix [storage] in city.toml, then rerun gc doctor", []string{err.Error()})
	}
	if !ok || c.cfg == nil || len(c.cfg.Rigs) == 0 {
		return okCheck(c.Name(), "n/a: this city serves every class from its work stores, or declares no rig")
	}
	var details, findings []string
	reused := 0
	for _, r := range censusRigs(c.cityPath, c.cfg, target, false) {
		if r.blocking() {
			details = append(details, rigCensusLine(r))
		}
		if len(r.Reused) > 0 {
			reused += len(r.Reused)
			details = append(details, fmt.Sprintf("rig %s: %d row(s) reuse an id its import moved for a different bead, so by-id reads answer them with the binding's copy: %s", r.Rig, len(r.Reused), infraStrandedIDList(r.Reused)))
		}
	}
	if len(details) == 0 {
		return okCheck(c.Name(), "every rig is imported or clean: rig stores hold only work")
	}
	findings = append(findings, fmt.Sprintf("%d rig finding(s)", len(details)))
	if reused > 0 {
		findings = append(findings, fmt.Sprintf("%d reused id(s)", reused))
	}
	sort.Strings(details)
	return warnCheck(c.Name(), strings.Join(findings, "; "), "run `"+rigImportAllInstruction()+"`; re-create a row with a reused id under a new id", details)
}
