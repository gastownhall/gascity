package main

import (
	"context"
	"errors"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A14's drift effects (CONTRACT v5 §4; plan C7b2-1): a live drift's
// session_live run again on the row's own runtime (SESS-565), under its
// lease; the asleep named repair is the fresh heal's (healSections), with
// its rotated key minted here, in the CAS that writes it.

// causeRunLive: the provider's RunLive failed; nothing is written, and the
// row backs off.
const causeRunLive = "run-live"

// liveApplySections are the live drift's Call and its record: decided
// again on the fresh row and runtime read, session_live run on the routed
// leaf, then the live hashes and session.updated.
var liveApplySections = []section{
	called(0, section{Decide: liveApplyStep}, runLive),
	{Decide: liveAppliedStep},
}

// liveRun is what runLive runs: the row's hash config on its leaf.
type liveRun struct {
	leaf runtime.Provider
	name string
	cfg  runtime.Config
	live string
}

// liveApplyStep hands the Call the run when A14 still decides it on the
// fresh row and runtime read.
func liveApplyStep(v txView) txStep {
	w := v.World.withRow(v.It.Key, v.Row, v.Meta).withRuntime(v.It.Key, v.RT)
	if fresh, _ := decideRow(&w, v.Alloc, v.It.Key); fresh.Kind != intentLiveApply {
		return txStep{Refuse: causeRedecided}
	}
	cfg, _, _ := v.World.coreHash(v.Row)
	return txStep{Pass: liveRun{leaf: v.RT.leaf, name: strings.TrimSpace(v.Row.SessionName), cfg: cfg, live: runtime.LiveFingerprint(cfg)}}
}

// runLive is legacy's sp.RunLive on the row's runtime.
func runLive(_ context.Context, _ txCaps, in liveRun) (string, error) {
	if in.leaf == nil {
		return "", errors.New("v2 live apply: no runtime leaf")
	}
	return in.live, in.leaf.RunLive(in.name, in.cfg)
}

// liveAppliedStep records the live hashes RunLive applied, with legacy's
// session.updated; a failed RunLive writes nothing and fails.
func liveAppliedStep(v txView) txStep {
	live, err := callResult[string](v)
	if err != nil {
		return txStep{Fail: causeRunLive, Err: err}
	}
	res, _ := v.World.Templates.lookup(v.Row)
	step := txStep{Write: liveHashPatch(live), Done: true}
	step.Facts.Events = []events.Event{{Type: events.SessionUpdated, Actor: "gc", Subject: res.TP.DisplayName(), Message: "session_live re-applied"}}
	return step
}

// mintRepairKey adds to a drift repair's write the key its provider takes
// (freshRestartSessionKeyInfo's mint), on the fresh row it writes; a mint
// error leaves the key unwritten, as legacy's. Only the repair's own kind
// mints: its fresh decision is the repair (redecideRow holds the kind), so
// a runtime that came up since the pass (an operator's attach), which
// decides A6's awake heal, refuses instead.
func mintRepairKey(v txView, step txStep) txStep {
	if v.It.Kind != intentDriftRepair || len(step.Write) == 0 {
		return step
	}
	step.Write = withRepairKey(step.Write, v.World, v.Row)
	return step
}

// withRepairKey is patch with the key a drift repair mints for row, where
// its provider takes one.
func withRepairKey(patch session.MetadataPatch, w *World, row session.Info) session.MetadataPatch {
	res, _ := w.Templates.lookup(row)
	if restartKeyMints(res.TP, row) {
		if key, err := session.GenerateSessionKey(); err == nil {
			patch = maps.Clone(patch)
			patch["session_key"] = key
		}
	}
	return patch
}
