package main

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

// censusOwnerLivenessCheck detects resource-census ledger rows
// (test/test-resources.toml) whose owner_bead no longer resolves in the
// scope's bead store. Detection only: it never repairs the ledger.
type censusOwnerLivenessCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

// newCensusOwnerLivenessCheck constructs a censusOwnerLivenessCheck.
func newCensusOwnerLivenessCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *censusOwnerLivenessCheck {
	return &censusOwnerLivenessCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

// Name returns the check's identifier.
func (c *censusOwnerLivenessCheck) Name() string { return "census-owner-liveness" }

// CanFix reports that this check is detection-only.
func (c *censusOwnerLivenessCheck) CanFix() bool { return false }

// Fix is a no-op; this check never auto-repairs findings.
func (c *censusOwnerLivenessCheck) Fix(_ *doctor.CheckContext) error { return nil }

// Run scans the city and each non-suspended, path-bearing rig's
// resource-census ledger for owner_bead references that no longer resolve
// in that scope's bead store.
func (c *censusOwnerLivenessCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	var findings []string
	var skipped []string

	c.scanScope(&findings, &skipped, "city", c.cityPath)
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			c.scanScope(&findings, &skipped, "rig "+rig.Name, rig.Path)
		}
	}

	if len(findings) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no dangling owner_bead references found in resource-census ledgers")
	}

	details := append([]string{}, findings...)
	details = append(details, skipped...)
	sort.Strings(details)

	if len(findings) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("census-owner-liveness check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}

	message := fmt.Sprintf("found %d dangling owner_bead reference(s) in resource-census ledgers", len(findings))
	if len(skipped) > 0 {
		message = fmt.Sprintf("%s (and skipped %d scope(s))", message, len(skipped))
	}
	fixHint := "re-point the ledger row's owner_bead through council review (see TESTING.md), or fix bead store access and rerun gc doctor"
	return warnCheck(c.Name(), message, fixHint, details)
}

// scanScope loads the resource-census ledger at path, if any, and checks
// each unique owner_bead it references against the scope's bead store.
// A missing ledger file is expected for almost every scope and is skipped
// silently; any other load error, store-open error, or non-not-found Get
// error is recorded as a skip with a reason rather than treated as a
// dangling finding.
func (c *censusOwnerLivenessCheck) scanScope(findings, skipped *[]string, label, path string) {
	if c.newStore == nil || strings.TrimSpace(path) == "" {
		return
	}

	ledgerPath := filepath.Join(path, "test", "test-resources.toml")
	ledger, err := resourcecensus.LoadLedger(ledgerPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return
		}
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: loading resource-census ledger: %v", label, err))
		return
	}

	rows := collectCensusOwnerBeadRows(ledger)
	if len(rows) == 0 {
		return
	}

	store, err := c.newStore(path)
	if err != nil {
		*skipped = append(*skipped, fmt.Sprintf("%s skipped: opening bead store: %v", label, err))
		return
	}

	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		_, err := store.Get(id)
		switch {
		case err == nil:
			continue
		case errors.Is(err, beads.ErrNotFound):
			*findings = append(*findings, fmt.Sprintf("%s: dangling owner_bead=%s rows=[%s]", label, id, strings.Join(rows[id], "; ")))
		default:
			*skipped = append(*skipped, fmt.Sprintf("%s skipped: checking owner_bead %s: %v", label, id, err))
		}
	}
}

// collectCensusOwnerBeadRows collects, per unique owner_bead, a
// human-readable descriptor of every ledger row that references it across
// all four row categories.
func collectCensusOwnerBeadRows(ledger resourcecensus.Ledger) map[string][]string {
	rows := map[string][]string{}

	addBaseline := func(category string, list []resourcecensus.Baseline) {
		for _, row := range list {
			id := strings.TrimSpace(row.OwnerBead)
			if id == "" {
				continue
			}
			desc := fmt.Sprintf("%s: scope=%s resource=%s", category, row.Scope, row.Resource)
			rows[id] = append(rows[id], desc)
		}
	}
	addBaseline("audit_baseline", ledger.AuditBaseline)
	addBaseline("debt", ledger.Debt)
	addBaseline("small_debt", ledger.SmallDebt)

	for _, row := range ledger.Medium {
		id := strings.TrimSpace(row.OwnerBead)
		if id == "" {
			continue
		}
		desc := fmt.Sprintf("medium: package_dir=%s package_name=%s owner=%s", row.PackageDir, row.PackageName, row.Owner)
		rows[id] = append(rows[id], desc)
	}

	return rows
}
