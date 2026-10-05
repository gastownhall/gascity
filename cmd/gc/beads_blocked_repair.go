package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/deps"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
)

// blockedRepairMarkerKey is the bd config key that records, inside each
// scope's own database, the bd version whose `bd recompute-blocked` last ran
// over that scope. It is durable store state, written through bd's front door
// under the custom.* namespace bd reserves for integrations, so it travels with
// the database (a restored or cloned store carries its own answer) and no file
// outside the store records it.
const blockedRepairMarkerKey = "custom.gascity.blocked_repair_bd_version"

// blockedRepairScope is one bd-backed scope the upgrade repair visits.
type blockedRepairScope struct {
	// id names the scope in logs and events: "city" or "rig/<name>".
	id   string
	root string
}

// blockedRepairDeps are the side-effecting collaborators of runBlockedRepair.
type blockedRepairDeps struct {
	openStore func(scope blockedRepairScope) *beads.BdStore
	// openRecorder is called at most once, on the first completed repair, so a
	// start with nothing to repair never opens the event log.
	openRecorder func() (events.Recorder, func())
}

// repairBlockedFlagsOnUpgrade is gc start's one-shot is_blocked repair.
//
// beads migration 0059 over-sets the denormalized is_blocked column on stores
// upgraded from bd <= 1.3.0 under Dolt < 2.4.0 and the embedded engine
// (gastownhall/beads#7037): blockedness leaks across relates-to,
// discovered-from and every other non-blocking edge. `bd ready` and gc's ready
// projection both trust the column, so the affected work silently stops being
// dispatched. `bd recompute-blocked` rebuilds the column from the dependency
// graph and is idempotent.
//
// It runs once per gc-owned, Dolt-backed bd scope per bd version: on the first
// start after the scope's bd changes, or when the scope carries no marker. It
// never blocks startup. A failure is a warning, and since the marker is written
// only after a successful recompute, the next start retries it.
func repairBlockedFlagsOnUpgrade(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) {
	if gcDoltSkip() || cfg == nil {
		return
	}
	scopes := blockedRepairScopes(cityPath, cfg)
	if len(scopes) == 0 {
		return
	}
	runBlockedRepair(scopes, blockedRepairDeps{
		openStore: func(scope blockedRepairScope) *beads.BdStore {
			if scope.id == "city" {
				return bdStoreForCityWithConfig(scope.root, cityPath, cfg)
			}
			return bdStoreForRig(scope.root, cityPath, cfg)
		},
		openRecorder: func() (events.Recorder, func()) {
			rec, err := openCityEventsLog(cityPath, io.Discard)
			if err != nil {
				fmt.Fprintf(stderr, "%s: warning: is_blocked repair: events recorder unavailable: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
				return events.Discard, func() {}
			}
			return rec, func() { _ = rec.Close() }
		},
	}, stderr, cmdName)
}

// blockedRepairScopes lists the city and rig scopes the repair applies to.
func blockedRepairScopes(cityPath string, cfg *config.City) []blockedRepairScope {
	var scopes []blockedRepairScope
	if blockedRepairScopeEligible(cityPath, cfg, cityPath) {
		scopes = append(scopes, blockedRepairScope{id: "city", root: cityPath})
	}
	for _, rig := range cfg.Rigs {
		root := strings.TrimSpace(rig.Path)
		if root == "" || samePath(root, cityPath) {
			continue
		}
		if blockedRepairScopeEligible(cityPath, cfg, root) {
			scopes = append(scopes, blockedRepairScope{id: "rig/" + rig.Name, root: root})
		}
	}
	return scopes
}

// blockedRepairScopeEligible reports whether scopeRoot is a bd scope whose Dolt
// store gc owns. Non-bd providers have no is_blocked column to repair; a
// non-Dolt backend never ran the affected migration; and a store gc does not
// serve (an opaque storage binding, a bd-owned direct upstream, or an external
// Dolt endpoint) is its operator's to repair, not gc's to write to on start.
// Any read failure answers false: the repair is best-effort and must not act on
// a scope it could not classify.
func blockedRepairScopeEligible(cityPath string, cfg *config.City, scopeRoot string) bool {
	if !scopeUsesManagedBdStoreContract(cityPath, scopeRoot) {
		return false
	}
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil || !ok || state.Backend != "dolt" {
		return false
	}
	if bound, err := scopeStoreIsExternallyBound(cityPath, scopeRoot); err != nil || bound {
		return false
	}
	if bdOwned, err := scopeIsBdOwnedDirectExternal(cityPath, scopeRoot); err != nil || bdOwned {
		return false
	}
	return !initScopeUsesExternalDolt(cityPath, scopeRoot, cfg)
}

// runBlockedRepair visits each scope independently: one scope's failure never
// stops the others.
func runBlockedRepair(scopes []blockedRepairScope, d blockedRepairDeps, stderr io.Writer, cmdName string) {
	var rec events.Recorder
	var closeRec func()
	defer func() {
		if closeRec != nil {
			closeRec()
		}
	}()
	for _, scope := range scopes {
		outcome, err := repairBlockedFlagsForScope(d.openStore(scope))
		if err != nil {
			fmt.Fprintf(stderr, "%s: warning: is_blocked repair for %s did not run, will retry on next start: %v (to repair by hand: bd recompute-blocked in %s)\n", cmdName, scope.id, err, scope.root) //nolint:errcheck // best-effort stderr
			continue
		}
		if !outcome.ran {
			continue
		}
		fmt.Fprintf(stderr, "%s: recomputed is_blocked for %s under bd %s: %d rows corrected\n", cmdName, scope.id, outcome.bdVersion, outcome.rowsCorrected) //nolint:errcheck // best-effort stderr
		if outcome.markerErr != nil {
			fmt.Fprintf(stderr, "%s: warning: is_blocked repair for %s: recording marker %s failed, will rerun on next start: %v\n", cmdName, scope.id, blockedRepairMarkerKey, outcome.markerErr) //nolint:errcheck // best-effort stderr
		}
		if rec == nil {
			rec, closeRec = d.openRecorder()
		}
		recordBlockedRecomputed(rec, scope, outcome)
	}
}

func recordBlockedRecomputed(rec events.Recorder, scope blockedRepairScope, outcome blockedRepairOutcome) {
	payload, err := json.Marshal(events.BlockedRecomputedPayload{
		Scope:         scope.id,
		RowsCorrected: outcome.rowsCorrected,
		BDVersion:     outcome.bdVersion,
	})
	if err != nil {
		return
	}
	rec.Record(events.Event{
		Type:    events.BeadsBlockedRecomputed,
		Actor:   "gc",
		Subject: scope.id,
		Payload: payload,
	})
}

// blockedRepairOutcome is what one scope's repair did.
type blockedRepairOutcome struct {
	ran           bool
	bdVersion     string
	rowsCorrected int
	// markerErr is a failure to record the marker after a successful
	// recompute. The repair itself happened; the next start repeats it.
	markerErr error
}

// repairBlockedFlagsForScope runs the recompute when the scope's marker does not
// name the running bd version, then records that version as the marker. A bd
// that predates `bd recompute-blocked` is skipped without a marker, so the
// repair runs once the scope's bd is upgraded.
func repairBlockedFlagsForScope(store *beads.BdStore) (blockedRepairOutcome, error) {
	version, err := store.BDVersion()
	if err != nil {
		return blockedRepairOutcome{}, err
	}
	if deps.CompareVersions(version, beads.RecomputeBlockedMinBDVersion) < 0 {
		return blockedRepairOutcome{}, nil
	}
	marker, err := store.ConfigGet(blockedRepairMarkerKey)
	if err != nil {
		return blockedRepairOutcome{}, err
	}
	if strings.TrimSpace(marker) == version {
		return blockedRepairOutcome{}, nil
	}
	rows, err := store.RecomputeBlocked()
	if err != nil {
		return blockedRepairOutcome{}, err
	}
	return blockedRepairOutcome{
		ran:           true,
		bdVersion:     version,
		rowsCorrected: rows,
		markerErr:     store.ConfigSet(blockedRepairMarkerKey, version),
	}, nil
}
