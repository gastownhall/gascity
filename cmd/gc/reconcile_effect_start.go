package main

import (
	"context"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// The start effect (CONTRACT v5 S1, S2): bringUp, which may launch and costs
// a token, and adopt, which never launches and costs none, are transactions
// (runTx) under the row's runtime lease, and begin with one verb section.
// Each attempt reads the runtime fresh (txRuntime: presence and identity on
// the routed leaf, absence confirmed through every fall-through hop, with the
// template's process names) and then the row, and S1's table picks the verb
// from that read and the row's token: a creating row whose runtime is alive
// and Current is committed (the adoption); a runtime alive and Current under
// any other row is left as it is; a refusal writes nothing. C5a1-2 adds the
// Launch after the verb section; C5a2 the abandon table and the rollback;
// C5a3 the dead row's classification and recycle, adoption's side effects,
// the lost-commit cleanup and the pending-create StaleSelf exit.

// Start refusal causes, beside the shared ones (causeRouteUnknown,
// causeLivenessUnknown, causeNotPresent). A refusal backs the row off (P4).
const (
	causeTemplate       = "template-unresolved"
	causeAlreadyRunning = "already-running"
	causeTokenDrift     = "token-drift"
	causeNewerSelf      = "newer-self"
	causeOccupied       = "occupied"
	causeAttribution    = "attribution-unknown"
	causeDead           = "dead" // C5a3 classifies and recycles
	causePrepare        = "prepare"
)

// adoptSections are the adopt's one section: the verb, which never
// launches.
var adoptSections = []section{probed(prepareAdoption, verbStep(true))}

// prepareAdoption is the verb section's Probe: legacy's prepare of the
// expected row, read-only, when the fresh read may commit it (a creating
// row whose runtime is alive and Current), for the commit's hashes; nothing
// otherwise.
func prepareAdoption(_ context.Context, r effectReads, v txView) (*preparedStart, error) {
	if v.RT == nil || !v.RT.Alive() || !creating(v.Row) || compareIdentity(v.Row, v.RT.Identity) != identityCurrent {
		return nil, nil
	}
	res, ok := v.World.Templates.lookup(v.Row)
	if !ok || res.Err != nil {
		return nil, errTemplateUnresolved
	}
	return prepareRow(r, v.World, v.Row, res.TP, sessTranscriptUnknown)
}

// errTemplateUnresolved: the pass's memo holds no template for the row.
var errTemplateUnresolved = errors.New("v2 start: the row's template is unresolved")

// prepareRow is legacy's prepare of row as template tp with the effect's
// transcript state (S-1) through the read-only stores, with legacy's task
// work dir resolver over the pass's assigned work: it writes nothing.
func prepareRow(r effectReads, w *World, row session.Info, tp TemplateParams, transcript sessTranscriptState) (*preparedStart, error) {
	sources := dispatchOptionSources{claimedWork: reachableClaimedWorkProbe(w.CityPath, w.Env.Cfg, r.city, servingRigStores(w.Env.Cfg, r.rigs, w.SuspendedRigPaths))}
	prepared, _, err := buildPreparedStartWithTranscript(startCandidate{info: row, tp: tp}, w.CityPath, w.Env.Cfg, r.city, taskWorkDirs(w), sources, &transcript)
	return prepared, err
}

// taskWorkDirs is legacy's task work dir resolver over the pass's assigned
// work, only when that read was not partial, as the legacy tick builds it.
func taskWorkDirs(w *World) taskWorkDirResolver {
	if d := w.Demand; !d.StorePartial && len(d.AssignedWork) > 0 {
		return newAssignedTaskWorkDirResolver(w.CityPath, d.AssignedWork)
	}
	return nil
}

// verbStep is S1's table over the attempt's fresh runtime read and the
// fresh row (the premise holds it to the pass's token): the verb, as a
// refusal, the adoption's commit, a no-op, or, for a bringUp over nothing
// present, the empty step that goes on to the Launch. An adopt never
// launches. A runtime alive and Current is noted (v5 O4).
func verbStep(adopt bool) func(txView, *preparedStart, error) txStep {
	return func(v txView, prepared *preparedStart, prepErr error) txStep {
		if res, ok := v.World.Templates.lookup(v.Row); !ok || res.Err != nil {
			return txStep{Refuse: causeTemplate, Err: res.Err}
		}
		switch v.RT.Class {
		case rtUnsupported:
			return txStep{Refuse: causeLivenessUnsupported}
		case rtAbsent:
			if adopt {
				return txStep{Refuse: causeNotPresent}
			}
			return txStep{}
		case rtCorpse, rtZombie:
			return txStep{Refuse: causeDead}
		}
		if !v.RT.Alive() { // unknown, or the session object changed around the identity read
			return txStep{Refuse: causeLivenessUnknown}
		}
		switch compareIdentity(v.Row, v.RT.Identity) {
		case identityCurrent:
		case identityStaleSelf: // arm A3 re-keys it (X1); a pending create's S7 exit is C5a3's
			return txStep{Refuse: causeTokenDrift}
		case identityNewerSelf:
			return txStep{Refuse: causeNewerSelf}
		case identityForeign: // a pending create's rollback is C5a2's
			return txStep{Refuse: causeOccupied}
		default:
			return txStep{Refuse: causeAttribution}
		}
		noted := effectFacts{Noted: &notedRuntime{Name: strings.TrimSpace(v.Row.SessionName), At: v.Now}}
		switch {
		case !creating(v.Row):
			return txStep{Done: true, Cause: causeAlreadyRunning, Facts: noted}
		case prepared == nil:
			return txStep{Fail: causePrepare, Err: prepErr}
		}
		return txStep{Write: startCommitPatch(prepared, v.Row, v.Now), Done: true, Facts: noted}
	}
}

// creating reports a row in the creating state: S1's uncommitted intent.
func creating(row session.Info) bool {
	return session.State(strings.TrimSpace(row.MetadataState)) == session.StateCreating
}
