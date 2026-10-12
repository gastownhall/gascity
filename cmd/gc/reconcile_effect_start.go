package main

import (
	"context"
	"errors"
	"strings"
	"time"

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
// row whose runtime is alive and Current), for the commit's hashes, with the
// row's transcript probed as legacy's adoption reads it (SESS-542: a launch
// that already created its conversation is no first start); nothing
// otherwise.
func prepareAdoption(_ context.Context, r effectReads, v txView) (*preparedStart, error) {
	if v.RT == nil || !v.RT.Alive() || !creating(v.Row) || compareIdentity(v.Row, v.RT.Identity) != identityCurrent {
		return nil, nil
	}
	res, ok := v.World.Templates.lookup(v.Row)
	if !ok || res.Err != nil {
		return nil, errTemplateUnresolved
	}
	repaired := foldInfo(v.Row, concretePoolWorkDirRepairPatch(startCandidate{info: v.Row, tp: res.TP}, v.World.CityPath, v.World.Env.Cfg)) // the dir prepare launches in
	return prepareRow(r, v.World, v.Row, res.TP, transcriptOf(r, v.World, repaired, res.TP))
}

// transcriptOf is the state of row's session key's transcript in the work
// dir its launch runs in (S-1), read-only: unknown with no key, no work dir
// or no probe.
func transcriptOf(r effectReads, w *World, row session.Info, tp TemplateParams) sessTranscriptState {
	sk := strings.TrimSpace(row.SessionKey)
	if sk == "" {
		return sessTranscriptUnknown
	}
	dir := launchWorkDir(r, w, row, tp)
	if dir == "" {
		return sessTranscriptUnknown
	}
	switch present, probeable := staleResumeKeyProbe(sessionTranscriptProvider(tp.ResolvedProvider, row), dir, sk); {
	case !probeable:
		return sessTranscriptUnknown
	case present:
		return sessTranscriptPresent
	}
	return sessTranscriptAbsent
}

// launchWorkDir is the work dir prepare launches row in: the task's, the
// row's, the template's.
func launchWorkDir(r effectReads, w *World, row session.Info, tp TemplateParams) string {
	if dir := resolvePreparedTaskWorkDir(startCandidate{info: row, tp: tp}, w.CityPath, w.Env.Cfg, r.city, taskWorkDirs(w)); dir != "" {
		return dir
	}
	if dir := preparedStartSessionWorkDir(row); dir != "" {
		return resolveWorkDirAgainstCity(w.CityPath, dir)
	}
	return tp.WorkDir
}

// errTemplateUnresolved: the pass's memo holds no template for the row.
var errTemplateUnresolved = errors.New("v2 start: the row's template is unresolved")

// prepareRow is legacy's prepare of row as template tp with the effect's
// transcript state (S-1) through the read-only stores, with legacy's task
// work dir resolver over the pass's assigned work: it writes nothing.
func prepareRow(r effectReads, w *World, row session.Info, tp TemplateParams, transcript sessTranscriptState) (*preparedStart, error) {
	// The claimed-work probe reads the seat's work through the city work
	// legs (NEW2-1), never the sessions store.
	sources := dispatchOptionSources{claimedWork: reachableClaimedWorkProbe(seatWorkFor(r.legs, releaseScope(row, w.Env.Cfg)))}
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
// present, the step that goes on to the Launch with the row it acts on. An
// adopt never launches. A runtime alive and Current is noted (v5 O4).
func verbStep(adopt bool) func(txView, *preparedStart, error) txStep {
	return func(v txView, prepared *preparedStart, prepErr error) txStep {
		res, ok := v.World.Templates.lookup(v.Row)
		switch cause := runtimeCause(v.Row, v.RT); {
		case !ok || res.Err != nil:
			return healInstead(v, txStep{Refuse: causeTemplate, Err: res.Err})
		case cause == causeNotPresent && !adopt:
			return txStep{Pass: launchRow{w: v.World, row: v.Row, tp: res.TP, agent: res.Agent}}
		case cause != "":
			return txStep{Refuse: cause}
		}
		noted := effectFacts{Noted: &notedRuntime{Name: strings.TrimSpace(v.Row.SessionName), At: v.Now}}
		switch {
		case !creating(v.Row):
			return txStep{Done: true, Cause: causeAlreadyRunning, Facts: noted}
		case prepared == nil:
			return txStep{Fail: causePrepare, Err: prepErr}
		}
		return txStep{Write: startCommit(prepared, v.Row, v.Now), Done: true, Facts: noted}
	}
}

// healInstead is refuse, a start's refusal before its PreWake, or the A6
// heal the start carries (intent.Heal) written alone while its fresh read
// still finds the runtime absent, on which every such heal rests.
func healInstead(v txView, refuse txStep) txStep {
	heal := v.It.Heal
	if heal == nil || v.RT == nil || v.RT.Class != rtAbsent {
		return refuse
	}
	patch := heal.Patch
	if heal.Kind == intentDriftRepair { // the composed repair's cleared key, rotated as its own effect does
		patch = withRepairKey(patch, v.World, v.Row)
	}
	step := txStep{Write: patch, Class: heal.Class, Done: true, Cause: refuse.Refuse, Facts: refuse.Facts}
	if heal.Event != nil {
		step.Facts.Events = append(step.Facts.Events, *heal.Event)
	}
	return step
}

// runtimeCause is S1's table's refusal for row's fresh runtime read rt, ""
// when the runtime is alive and Current; causeNotPresent when it is absent.
func runtimeCause(row session.Info, rt *txRuntime) string {
	switch rt.Class {
	case rtUnsupported:
		return causeLivenessUnsupported
	case rtAbsent:
		return causeNotPresent
	case rtCorpse, rtZombie:
		return causeDead
	}
	if !rt.Alive() { // unknown, or the session object changed around the identity read
		return causeLivenessUnknown
	}
	switch compareIdentity(row, rt.Identity) {
	case identityCurrent:
		return ""
	case identityStaleSelf: // arm A3 re-keys it (X1); a pending create's S7 exit is C5a3's
		return causeTokenDrift
	case identityNewerSelf:
		return causeNewerSelf
	case identityForeign: // a pending create's rollback is C5a2's
		return causeOccupied
	}
	return causeAttribution
}

// startCommit is startCommitPatch writing the state legacy's next tick
// heals a started row to: awake where it writes active, its state_reason
// kept (every canonical reader reads both as active).
func startCommit(prepared *preparedStart, row session.Info, now time.Time) session.MetadataPatch {
	patch := startCommitPatch(prepared, row, now)
	if patch["state"] == string(session.StateActive) {
		patch["state"] = string(session.StateAwake)
	}
	return patch
}

// creating reports a row in the creating state: S1's uncommitted intent.
func creating(row session.Info) bool {
	return session.State(strings.TrimSpace(row.MetadataState)) == session.StateCreating
}
