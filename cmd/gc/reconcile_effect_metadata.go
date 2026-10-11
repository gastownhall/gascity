package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// A7's row-metadata verb (CONTRACT v5 M1; C7d): under the city alias lock
// when its alias work needs it (aliasLockAround), the alias's availability
// read live (aliasAvailability), the identity decided again on the fresh
// row and written in one CAS with its title and label deltas, then the
// runtime alias synced when the runtime reads Current.

// causeAliasLock: the city alias lock could not be taken; nothing is
// written unlocked.
const causeAliasLock = "alias-lock"

// withCityAliasLock is session.WithCitySessionAliasLock; a test injects
// its own.
var withCityAliasLock = session.WithCitySessionAliasLock

// rowMetadataSections are the verb's one section and its runtime alias
// Call.
var rowMetadataSections = []section{called(0, probed(aliasAvailability, rowMetadataStep), syncRuntimeAlias)}

// aliasLockAround takes the intent's alias lock around the effect, outside
// every row lock; a lock error refuses, with a row backoff.
func aliasLockAround(_ context.Context, a aroundCaps, run func() settlement) settlement {
	if a.it.AliasLock == "" {
		return run()
	}
	var s settlement
	if err := withCityAliasLock(a.reads.cityPath, a.it.AliasLock, func() error { s = run(); return nil }); err != nil {
		return settlement{Outcome: settledRefused, Cause: causeAliasLock, Err: err}
	}
	return s
}

// aliasCheck is the live availability read of the row's managed alias:
// err is its conflict, nil when free.
type aliasCheck struct{ err error }

// aliasAvailability is legacy's EnsureAliasAvailableWithConfig[ForOwner],
// under the lock the around took, for alias work that is not blocked.
func aliasAvailability(_ context.Context, r effectReads, v txView) (aliasCheck, error) {
	id, _, lock, ok := v.World.rowIdentityOf(v.It.Key, v.Row)
	if !ok || lock == "" || id.Alias == "" {
		return aliasCheck{}, nil
	}
	if id.Owner != "" {
		return aliasCheck{err: session.EnsureAliasAvailableWithConfigForOwner(r.city, v.World.Env.Cfg, id.Alias, v.Row.ID, id.Owner)}, nil
	}
	return aliasCheck{err: session.EnsureAliasAvailableWithConfig(r.city, v.World.Env.Cfg, id.Alias, v.Row.ID)}, nil
}

// aliasSync is the runtime alias the Call syncs: the row as written.
type aliasSync struct {
	sync bool
	row  session.Info
}

// rowMetadataStep decides the identity again on the fresh row: a lock the
// pass took for other alias work is redecided; a converged row is a no-op.
// An alias written on a row whose runtime reads Current is synced to it.
func rowMetadataStep(v txView, chk aliasCheck, _ error) txStep {
	id, blocked, lock, ok := v.World.rowIdentityOf(v.It.Key, v.Row)
	if !ok || lock != v.It.AliasLock {
		return txStep{Refuse: causeRedecided}
	}
	rebaseline := func() session.MetadataPatch {
		res, _ := v.World.Templates.lookup(v.Row)
		return baselinePatch(sessionCoreConfigForHashInfo(res.TP, v.Row), baselineAliasRerender)
	}
	patch := identityPatch(id, v.Row, blocked, chk.err, rebaseline, v.Now.UTC().Format(time.RFC3339))
	if len(patch) == 0 {
		return txStep{Done: true, Cause: causeConverged}
	}
	_, aliased := patch["alias"]
	current := aliased && v.RT != nil && v.RT.Alive() && compareIdentity(v.Row, v.RT.Identity) == identityCurrent
	return txStep{Write: patch, Title: id.Title, AddLabels: id.Add, RemoveLabels: id.Remove, Pass: aliasSync{sync: current, row: foldInfo(v.Row, patch)}}
}

// causeConverged: the row's identity needs nothing.
const causeConverged = "converged"

// syncRuntimeAlias is SyncRuntimeAlias on a Current runtime after the
// alias landed (SESS-716); legacy logs its error, and so does this.
func syncRuntimeAlias(_ context.Context, c txCaps, in aliasSync) (struct{}, error) {
	if in.sync && strings.TrimSpace(in.row.SessionName) != "" {
		if err := session.SyncRuntimeAlias(c.start.sp, in.row); err != nil && c.start.stderr != nil {
			fmt.Fprintf(c.start.stderr, "v2 row metadata %s: syncing runtime alias %q: %v\n", in.row.ID, in.row.Alias, err) //nolint:errcheck // best-effort stderr
		}
	}
	return struct{}{}, nil
}
