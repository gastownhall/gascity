package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

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
// never fails startup. A failure is a warning, and since the marker is written
// only after a successful recompute, the next start retries it.
//
// It runs in the foreground, before agents start, on purpose. Scopes are
// repaired in parallel (blockedRepairParallelism), so the wait is the slowest
// scope, not the sum. In the background, agents would write to a store whose
// column is being rewritten wholesale. bd refuses the recompute over a dirty
// working set, so a racing write turns the repair into a warning, and the
// hidden work stays hidden until the next start, which may be days away.
// Repairing first also means the controller's first dispatch tick already
// sees the corrected column.
func repairBlockedFlagsOnUpgrade(cityPath string, cfg *config.City, stderr io.Writer, cmdName string) {
	if gcDoltSkip() || cfg == nil {
		return
	}
	runBlockedRepairForScopes(cityPath, cfg, blockedRepairScopes(cityPath, cfg), stderr, cmdName)
}

// startRepairBlockedFlags is the start paths' call into the repair, a seam so
// tests can pin that `gc start` and the supervisor make it.
var startRepairBlockedFlags = repairBlockedFlagsOnUpgrade

// rigAddRepairBlockedFlags is `gc rig add`'s call into the repair, a seam so
// tests can pin that it is made for the added rig only.
var rigAddRepairBlockedFlags = repairBlockedFlagsForAddedRig

// repairBlockedFlagsForAddedRig is the same one-shot repair, with the same
// marker, for a rig `gc rig add` just attached to a city. An existing store
// adopted as a rig may have crossed migration 0059 under an older bd; without
// this it would wait for the next `gc start`. A rig whose store init was
// deferred to the controller has no live store yet and is left to that start.
func repairBlockedFlagsForAddedRig(cityPath string, cfg *config.City, rigName string, stderr io.Writer) {
	if gcDoltSkip() || cfg == nil {
		return
	}
	runBlockedRepairForScopes(cityPath, cfg, blockedRepairScopesForRig(cityPath, cfg, rigName), stderr, "gc rig add")
}

// blockedRepairScopesForRig is blockedRepairScopes narrowed to one rig.
func blockedRepairScopesForRig(cityPath string, cfg *config.City, rigName string) []blockedRepairScope {
	// city.toml may record the rig relative to the city; scopes need roots.
	resolveRigPaths(cityPath, cfg.Rigs)
	var scopes []blockedRepairScope
	for _, s := range blockedRepairScopes(cityPath, cfg) {
		if s.id == "rig/"+rigName {
			scopes = append(scopes, s)
		}
	}
	return scopes
}

func runBlockedRepairForScopes(cityPath string, cfg *config.City, scopes []blockedRepairScope, stderr io.Writer, cmdName string) {
	if len(scopes) == 0 {
		return
	}
	runBlockedRepair(scopes, blockedRepairDeps{
		openStore: func(scope blockedRepairScope) *beads.BdStore {
			if scope.id == "city" {
				return bdStoreForCity(scope.root, cityPath)
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

// blockedRepairParallelism bounds how many scopes probe or recompute at once.
// Each recompute is one bd subprocess holding one connection to its scope's
// Dolt; four keeps a many-rig city's first start short without stampeding a
// shared server.
const blockedRepairParallelism = 4

// runBlockedRepair probes every scope, then recomputes the ones whose marker
// does not name the running bd, both phases bounded by
// blockedRepairParallelism. Scopes are independent: one scope's failure never
// stops the others. Output is printed in scope order once each phase is done.
func runBlockedRepair(scopes []blockedRepairScope, d blockedRepairDeps, stderr io.Writer, cmdName string) {
	stores := make([]*beads.BdStore, len(scopes))
	for i, scope := range scopes {
		stores[i] = d.openStore(scope)
	}
	plans := make([]blockedRepairPlan, len(scopes))
	forEachBounded(len(scopes), blockedRepairParallelism, func(i int) {
		plans[i] = planBlockedRepair(stores[i])
	})
	var due []int
	for i, plan := range plans {
		switch {
		case plan.err != nil:
			warnBlockedRepairFailed(stderr, cmdName, scopes[i], plan.err)
		case plan.due:
			due = append(due, i)
		}
	}
	if len(due) == 0 {
		return
	}

	started := time.Now()
	fmt.Fprintf(stderr, "%s: repairing blocked flags in %d scope(s) after a bd version change (beads#7037)...\n", cmdName, len(due)) //nolint:errcheck // best-effort stderr
	outcomes := make([]blockedRepairOutcome, len(due))
	errs := make([]error, len(due))
	forEachBounded(len(due), blockedRepairParallelism, func(k int) {
		i := due[k]
		outcomes[k], errs[k] = applyBlockedRepair(stores[i], plans[i].bdVersion)
	})

	var rec events.Recorder
	var closeRec func()
	defer func() {
		if closeRec != nil {
			closeRec()
		}
	}()
	repaired := 0
	for k, i := range due {
		scope := scopes[i]
		if errs[k] != nil {
			warnBlockedRepairFailed(stderr, cmdName, scope, errs[k])
			continue
		}
		repaired++
		outcome := outcomes[k]
		fmt.Fprintf(stderr, "%s: recomputed is_blocked for %s under bd %s: %d rows corrected\n", cmdName, scope.id, outcome.bdVersion, outcome.rowsCorrected) //nolint:errcheck // best-effort stderr
		if outcome.markerErr != nil {
			fmt.Fprintf(stderr, "%s: warning: is_blocked repair for %s: recording marker %s failed, will rerun on next start: %v\n", cmdName, scope.id, blockedRepairMarkerKey, outcome.markerErr) //nolint:errcheck // best-effort stderr
		}
		if rec == nil {
			rec, closeRec = d.openRecorder()
		}
		recordBlockedRecomputed(rec, scope, outcome)
	}
	fmt.Fprintf(stderr, "%s: blocked-flag repair finished for %d of %d scope(s) in %s\n", cmdName, repaired, len(due), time.Since(started).Round(time.Millisecond)) //nolint:errcheck // best-effort stderr
}

func warnBlockedRepairFailed(stderr io.Writer, cmdName string, scope blockedRepairScope, err error) {
	fmt.Fprintf(stderr, "%s: warning: is_blocked repair for %s did not run, will retry on next start: %v (to repair by hand: bd recompute-blocked in %s)\n", cmdName, scope.id, err, scope.root) //nolint:errcheck // best-effort stderr
}

// forEachBounded calls fn(0..n-1) with at most limit calls in flight and
// returns when all have returned.
func forEachBounded(n, limit int, fn func(int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
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
	bdVersion     string
	rowsCorrected int
	// markerErr is a failure to record the marker after a successful
	// recompute. The repair itself happened; the next start repeats it.
	markerErr error
}

// blockedRepairPlan is one scope's probe result.
type blockedRepairPlan struct {
	due       bool
	bdVersion string
	err       error
}

// planBlockedRepair reports whether the scope's marker does not name the
// running bd version. A bd that predates `bd recompute-blocked` is never due
// (and gets no marker), so the repair runs once the scope's bd is upgraded.
func planBlockedRepair(store *beads.BdStore) blockedRepairPlan {
	version, err := store.BDVersion()
	if err != nil {
		return blockedRepairPlan{err: err}
	}
	if deps.CompareVersions(version, beads.RecomputeBlockedMinBDVersion) < 0 {
		return blockedRepairPlan{}
	}
	marker, err := store.ConfigGet(blockedRepairMarkerKey)
	if err != nil {
		return blockedRepairPlan{err: err}
	}
	return blockedRepairPlan{due: strings.TrimSpace(marker) != version, bdVersion: version}
}

// applyBlockedRepair runs the recompute, then records version as the scope's
// marker. The marker is written only after a successful recompute.
func applyBlockedRepair(store *beads.BdStore, version string) (blockedRepairOutcome, error) {
	rows, err := store.RecomputeBlocked()
	if err != nil {
		return blockedRepairOutcome{}, err
	}
	return blockedRepairOutcome{
		bdVersion:     version,
		rowsCorrected: rows,
		markerErr:     store.ConfigSet(blockedRepairMarkerKey, version),
	}, nil
}
