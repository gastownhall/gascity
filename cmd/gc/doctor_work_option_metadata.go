package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type workOptionMetadataMigrationCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newWorkOptionMetadataMigrationCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *workOptionMetadataMigrationCheck {
	return &workOptionMetadataMigrationCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *workOptionMetadataMigrationCheck) Name() string {
	return "work-option-metadata-migration"
}

func (c *workOptionMetadataMigrationCheck) CanFix() bool { return true }

func (c *workOptionMetadataMigrationCheck) WarmupEligible() bool { return false }

type workOptionLegacyKey struct {
	legacy    string
	canonical string
}

var workOptionLegacyKeys = []workOptionLegacyKey{
	{legacy: "gc.model", canonical: dispatchOptionMetadataKey("model")},
	{legacy: "gc.reasoning", canonical: dispatchOptionMetadataKey("effort")},
}

const (
	legacyPerDispatchModelSourceKey = "gc.per_dispatch_model"
	templateOverridesMetadataKey    = "template_overrides"
)

type workOptionMetadataMigration struct {
	legacy      string
	canonical   string
	value       string
	canonicalOK bool
}

type workOptionSessionCleanup struct {
	source                     string
	removeAutoStampedModel     bool
	updatedTemplateOverrides   string
	updateTemplateOverridesKey bool
}

type workOptionMigrationTarget struct {
	label          string
	store          beads.Store
	beadID         string
	migrations     []workOptionMetadataMigration
	sessionCleanup *workOptionSessionCleanup
}

func (c *workOptionMetadataMigrationCheck) collect() (targets []workOptionMigrationTarget, skipped []string) {
	scopes := []struct{ label, path string }{{"city", c.cityPath}}
	if c.cfg != nil {
		suspState, _ := loadSuspensionState(fsys.OSFS{}, c.cityPath)
		for _, rig := range c.cfg.Rigs {
			if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) || strings.TrimSpace(rig.Path) == "" {
				continue
			}
			scopes = append(scopes, struct{ label, path string }{"rig " + rig.Name, rig.Path})
		}
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		items, err := store.List(beads.ListQuery{Type: "task", Sort: beads.SortCreatedAsc})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing task beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			migrations := workOptionMetadataMigrations(b)
			if len(migrations) == 0 {
				continue
			}
			targets = append(targets, workOptionMigrationTarget{
				label:      sc.label,
				store:      store,
				beadID:     b.ID,
				migrations: migrations,
			})
		}
		sessions, err := loadSessionBeads(store)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing session beads: %v", sc.label, err))
			continue
		}
		for _, b := range sessions {
			cleanup := workOptionSessionCleanupMigration(b)
			if cleanup == nil {
				continue
			}
			targets = append(targets, workOptionMigrationTarget{
				label:          sc.label,
				store:          store,
				beadID:         b.ID,
				sessionCleanup: cleanup,
			})
		}
	}
	return targets, skipped
}

func workOptionMetadataMigrations(b beads.Bead) []workOptionMetadataMigration {
	if b.Metadata == nil {
		return nil
	}
	var migrations []workOptionMetadataMigration
	for _, key := range workOptionLegacyKeys {
		value := strings.TrimSpace(b.Metadata[key.legacy])
		if value == "" {
			continue
		}
		migrations = append(migrations, workOptionMetadataMigration{
			legacy:      key.legacy,
			canonical:   key.canonical,
			value:       value,
			canonicalOK: strings.TrimSpace(b.Metadata[key.canonical]) != "",
		})
	}
	return migrations
}

func workOptionSessionCleanupMigration(b beads.Bead) *workOptionSessionCleanup {
	if b.Metadata == nil {
		return nil
	}
	source := strings.TrimSpace(b.Metadata[legacyPerDispatchModelSourceKey])
	if source == "" {
		return nil
	}
	cleanup := &workOptionSessionCleanup{source: source}
	rawOverrides := strings.TrimSpace(b.Metadata[templateOverridesMetadataKey])
	if rawOverrides == "" {
		return cleanup
	}
	var overrides map[string]string
	if err := json.Unmarshal([]byte(rawOverrides), &overrides); err != nil || len(overrides) == 0 {
		return cleanup
	}
	if strings.TrimSpace(overrides["model"]) != source {
		return cleanup
	}
	delete(overrides, "model")
	cleanup.removeAutoStampedModel = true
	cleanup.updateTemplateOverridesKey = true
	if len(overrides) == 0 {
		return cleanup
	}
	raw, err := json.Marshal(overrides)
	if err != nil {
		cleanup.updateTemplateOverridesKey = false
		return cleanup
	}
	cleanup.updatedTemplateOverrides = string(raw)
	return cleanup
}

func (c *workOptionMetadataMigrationCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		return okCheck(c.Name(), "no live beads use legacy work option metadata")
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, fmt.Sprintf("%s bead %s has %s", tgt.label, tgt.beadID, describeWorkOptionMigrationTarget(tgt)))
	}
	details = append(details, skipped...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("work option metadata migration skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d live bead(s) use legacy work option metadata", len(targets)),
		"run gc doctor --fix to migrate gc.model/gc.reasoning and clear stale session per-dispatch model overrides",
		details)
}

func describeWorkOptionMigrationTarget(tgt workOptionMigrationTarget) string {
	if tgt.sessionCleanup != nil {
		return describeWorkOptionSessionCleanup(*tgt.sessionCleanup)
	}
	return describeWorkOptionMigrations(tgt.migrations)
}

func describeWorkOptionMigrations(migrations []workOptionMetadataMigration) string {
	parts := make([]string, 0, len(migrations))
	for _, migration := range migrations {
		parts = append(parts, fmt.Sprintf("%s -> %s", migration.legacy, migration.canonical))
	}
	return strings.Join(parts, ", ")
}

func describeWorkOptionSessionCleanup(cleanup workOptionSessionCleanup) string {
	if cleanup.removeAutoStampedModel {
		return legacyPerDispatchModelSourceKey + " and auto-stamped template_overrides.model"
	}
	return legacyPerDispatchModelSourceKey
}

func (c *workOptionMetadataMigrationCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped := c.collect()
	for _, tgt := range targets {
		kvs := make(map[string]string, len(tgt.migrations)*2)
		for _, migration := range tgt.migrations {
			if !migration.canonicalOK {
				kvs[migration.canonical] = migration.value
			}
			kvs[migration.legacy] = ""
		}
		if cleanup := tgt.sessionCleanup; cleanup != nil {
			kvs[legacyPerDispatchModelSourceKey] = ""
			if cleanup.updateTemplateOverridesKey {
				kvs[templateOverridesMetadataKey] = cleanup.updatedTemplateOverrides
			}
		}
		if err := tgt.store.SetMetadataBatch(tgt.beadID, kvs); err != nil {
			return fmt.Errorf("%s bead %s: migrate work option metadata: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("work-option-metadata-migration skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
