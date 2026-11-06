package main

// gc hook current's verified read context: the stores the command's decision
// reads — the session row (stamp, work_started cell, identities) through the
// routed session front door, and every work leg a claim id is resolved against
// live. The stamp this command prints is a close target for a formula step
// (mol-scoped-work closes what stdout names), so nothing is printed on the
// stamp's word alone: each candidate id is read through WorkLegs.readLive —
// a relocated class binding before the work ledger, so a frozen pre-migration
// copy never answers — and held to the strict predicate beside
// claimedWorkStillHeldBy. The command stays read-only throughout: repairing
// the stamp is the claim path's job, not a read's side effect.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// hookCurrentContext is gc hook current's read context: the routed session
// front door, the city config the session's release scope is derived from, the
// work legs claim ids are read live through, and the bound rig stores that
// failed to open — while any is short, a claim the legs cannot see is not
// provably absent, and absence is a verdict this command must not guess at.
type hookCurrentContext struct {
	sessFront *session.Store
	cfg       *config.City
	legs      WorkLegs
	short     []rigStoreOpenFailure
}

// hookCurrentReadContext is the seam cmdHookCurrent builds its verified read
// context through, overridable in tests.
var hookCurrentReadContext = hookCurrentVerifiedReadContext

// hookCurrentVerifiedReadContext opens the city's stores once and returns the
// context `gc hook current` verifies the claim stamp against.
//
// The session front door is routed (cliSessionFrontDoor), so a relocated
// sessions class is reached the same way the claim path's stamp write reached
// it; the config is loaded without pack refresh, matching the other hook-path
// roots. The city gate follows the drain-ack one (openDrainAckCity): a
// directory with no city.toml is not read at all, because opening a store
// there would PROVISION one where none belongs — hook current is reachable
// from shells with no city around.
//
// Rig stores come from the underlying openStandaloneRigStores walk rather
// than buildStandaloneRigStoresWithConfig, which logs open failures away and
// would make the legs' coverage look complete when it is not. Failures are
// retained on the context instead: a rig that cannot answer leaves absence
// unprovable, which the read step refuses on rather than reading as "gone".
func hookCurrentVerifiedReadContext() (hookCurrentContext, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return hookCurrentContext{}, err
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); err != nil {
		return hookCurrentContext{}, fmt.Errorf("%s has no city.toml: only a real city's ledger can verify a current claim, and opening a store there would provision one", cityPath)
	}
	cfg, _ := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	store, err := openCityStoreAtWithConfig(cityPath, cfg)
	if err != nil {
		return hookCurrentContext{}, err
	}
	rigs, short := openStandaloneRigStores(cityRigs(cfg), cityPath, oneShotRigStoreOpener(cfg), sequentialRigStoreOpens)
	return hookCurrentContext{
		sessFront: cliSessionFrontDoor(store, cfg, cityPath),
		cfg:       cfg,
		// The work handle comes from the residency layer (cliStores), not a
		// counted literal at this site: the router mints it over the same
		// opened city store the drain-ack seat binds its leg over.
		legs:  workLegsFromCensus(cityPath, cfg, cityWorkLegOf(cliStores(cityPath, store).Work()), rigs),
		short: short,
	}, nil
}

// hookCurrentEntryIDs is the work_started cell's recorded delivery ids:
// trimmed, nonempty, deduplicated, the cell's newest-first order preserved.
// The cell's store spellings are deliberately not consulted — residency, not
// the store a past hook ran in, decides which leg holds each id.
func hookCurrentEntryIDs(cell session.WorkStarted) []string {
	seen := make(map[string]struct{}, len(cell.Claims))
	var ids []string
	for _, c := range cell.Claims {
		id := strings.TrimSpace(c.Bead)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// hookCurrentQualifyingIDs verifies every recorded delivery id and returns the
// qualifying ones, the cell's newest-first order preserved. A read error
// aborts the scan whole: a candidate found before the error does not print,
// because the ids that were never reached could change the count.
func hookCurrentQualifyingIDs(read hookCurrentReader, cell session.WorkStarted) ([]string, error) {
	var live []string
	for _, id := range hookCurrentEntryIDs(cell) {
		v := read.verify(id)
		if v.err != nil {
			return nil, v.err
		}
		if v.held {
			live = append(live, id)
		}
	}
	return live, nil
}

// hookCurrentNewerQualifying returns the first qualifying recorded delivery
// newer than the stamp, or "" when none is. The cell lists deliveries newest
// first, so the entries before the stamp's own position were recorded after
// the stamp was written; a stamp the cell does not list at all is treated as
// older than every entry, because its age relative to them is unknown. A
// newer delivery whose work is still live contradicts the stamp: the session
// took that work on later, so the stamp is not its current claim, whatever
// its own row says. A read error aborts the scan, not skips the entry.
func hookCurrentNewerQualifying(read hookCurrentReader, cell session.WorkStarted, stamp string) (string, error) {
	ids := hookCurrentEntryIDs(cell)
	k := slices.Index(ids, stamp)
	if k < 0 {
		k = len(ids)
	}
	for _, id := range ids[:k] {
		v := read.verify(id)
		if v.err != nil {
			return "", v.err
		}
		if v.held {
			return id, nil
		}
	}
	return "", nil
}

// hookCurrentRow is one work id's verdict: why the read failed (err), or why
// the row does not qualify (reason) when it does not.
type hookCurrentRow struct {
	err    error
	held   bool
	reason hookCurrentStaleReason
}

// hookCurrentReader verifies work ids live through the context's legs, one
// exact-id read per invocation: the stamp and the cell entries name the same
// rows again and again, and each id's answer cannot change within a run.
type hookCurrentReader struct {
	legs      WorkLegs
	short     []rigStoreOpenFailure
	sessionID string
	scope     ReleaseScope
	cache     map[string]hookCurrentRow
}

// newHookCurrentReader reads claims for sessionID, whose release scope is
// scope, through ctx.
func newHookCurrentReader(ctx hookCurrentContext, sessionID string, scope ReleaseScope) hookCurrentReader {
	return hookCurrentReader{
		legs:      ctx.legs,
		short:     ctx.short,
		sessionID: sessionID,
		scope:     scope,
		cache:     map[string]hookCurrentRow{},
	}
}

// verify reads id live and holds it to the strict predicate. A clean miss is
// the stale reason "gone"; a miss while a bound rig store failed to open is an
// error, because absence is only a verdict under complete coverage. readLive
// itself guarantees a found row carries the id asked for.
func (r hookCurrentReader) verify(id string) hookCurrentRow {
	if v, ok := r.cache[id]; ok {
		return v
	}
	var v hookCurrentRow
	b, found, err := r.legs.readLive(id)
	switch {
	case err != nil:
		v.err = fmt.Errorf("reading claim %s: %w", id, err)
	case !found:
		if len(r.short) > 0 {
			v.err = fmt.Errorf("reading claim %s: not found, and residency coverage is incomplete (%s), so its absence cannot be proven", id, hookCurrentShortWhy(r.short))
		} else {
			v.reason = hookCurrentStaleGone
		}
	default:
		v.reason, v.held = hookCurrentClaimHeld(b, r.sessionID, r.scope)
	}
	r.cache[id] = v
	return v
}

// hookCurrentShortWhy names the rig stores whose failed opens leave coverage
// incomplete.
func hookCurrentShortWhy(short []rigStoreOpenFailure) string {
	parts := make([]string, 0, len(short))
	for _, f := range short {
		parts = append(parts, fmt.Sprintf("rig %s: %v", f.rig, f.err))
	}
	return strings.Join(parts, "; ")
}
