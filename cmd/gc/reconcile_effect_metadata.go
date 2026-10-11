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
// row and written in one CAS with its title and label deltas. The lock
// spans the read and the write only: the verb reads no runtime and takes
// no lease. An alias it writes owes the runtime its sync, A7's own
// runtime-alias effect (under the lease) a later pass.

// causeAliasLock: the city alias lock could not be taken; nothing is
// written unlocked.
const causeAliasLock = "alias-lock"

// causeLabelsUnguarded: the store's conditional write cannot guard the
// label deltas (LL1), so nothing is written.
const causeLabelsUnguarded = "labels-unguarded"

// withCityAliasLock is session.WithCitySessionAliasLock; a test injects
// its own.
var withCityAliasLock = session.WithCitySessionAliasLock

// rowMetadataSections are the verb's one section; runtimeAliasSections the
// runtime-alias sync's, a fresh read of the runtime and its Call.
var (
	rowMetadataSections  = []section{probed(aliasAvailability, rowMetadataStep)}
	runtimeAliasSections = []section{called(0, section{Decide: runtimeAliasStep}, syncRuntimeAlias)}
)

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

// rowMetadataStep decides the identity again on the fresh row: a lock the
// pass took for other alias work is redecided; a converged row is a no-op.
// A written alias owes the runtime its sync (AliasOwed).
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
	step := txStep{Write: patch, Title: id.Title, AddLabels: id.Add, RemoveLabels: id.Remove}
	if _, aliased := patch["alias"]; aliased {
		step.Facts.AliasOwed = &v.It.Key
	}
	return step
}

// causeConverged: the row's identity needs nothing.
const causeConverged = "converged"

// runtimeAliasStep passes the fresh row to the sync when its runtime reads
// alive and Current; otherwise the debt is paid (a stopped runtime starts
// with the row's alias), a no-op.
func runtimeAliasStep(v txView) txStep {
	if v.RT == nil || !v.RT.Alive() || compareIdentity(v.Row, v.RT.Identity) != identityCurrent {
		return txStep{Done: true, Cause: causeNotCurrent}
	}
	return txStep{Pass: v.Row}
}

// causeNotCurrent: the row's runtime is not alive and Current.
const causeNotCurrent = "not-current"

// syncRuntimeAlias is SyncRuntimeAlias on the row's Current runtime
// (SESS-716); legacy logs its error, and so does this.
func syncRuntimeAlias(_ context.Context, c txCaps, row session.Info) (struct{}, error) {
	if strings.TrimSpace(row.SessionName) != "" {
		if err := session.SyncRuntimeAlias(c.start.sp, row); err != nil && c.start.stderr != nil {
			fmt.Fprintf(c.start.stderr, "v2 runtime alias %s: syncing %q: %v\n", row.ID, row.Alias, err) //nolint:errcheck // best-effort stderr
		}
	}
	return struct{}{}, nil
}
