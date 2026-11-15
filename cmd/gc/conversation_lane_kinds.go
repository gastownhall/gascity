package main

// The conversation lane's effective kinds (OJ-21 21a, DES-OJ21 §6). [ops]
// authoritative left unset runs message, interrupt and respond on the lane
// where the city's sessions store is SQLite and applies guarded batches (a
// split city), the configuration the maintainer-city soak proves; any other
// store keeps their legacy delivery until the city splits its storage
// (D-NOLANE: lanes need a split store). An explicit list, [] included, always
// wins. The controller latches the kinds when its lane host first finds its
// sessions store; the CLI reads them at once over the store it opened.

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// laneDefaultKinds are the kinds an unset [ops] authoritative runs on the
// lane over a store laneDefaultApplies to.
var laneDefaultKinds = []string{
	string(opjournal.KindMessage),
	string(opjournal.KindInterrupt),
	string(opjournal.KindRespond),
}

// laneStoreFit is how a sessions store can host the conversation lane.
type laneStoreFit int

const (
	// laneStoreFitNone: no store, or one that cannot apply guarded batches
	// (bd-served, exec, DoltLite); it hosts no lane, so the city keeps
	// legacy delivery until it splits its storage.
	laneStoreFitNone laneStoreFit = iota
	// laneStoreFitListed: guarded batches whose conditional writes do not
	// land in SQLite (native Dolt); it hosts the kinds [ops] authoritative
	// lists, and an unset list runs none.
	laneStoreFitListed
	// laneStoreFitDefault: SQLite with guarded batches (a split city); an
	// unset [ops] authoritative runs laneDefaultKinds on it.
	laneStoreFitDefault
)

// String names f for test failures and diagnostics.
func (f laneStoreFit) String() string {
	switch f {
	case laneStoreFitListed:
		return "listed"
	case laneStoreFitDefault:
		return "default"
	}
	return "none"
}

// laneStoreFitFor classifies sessions by the capabilities the lane host
// needs: guarded batches to host a lane at all, and conditional writes that
// land in a SQLite store (opjournal.CheckRecordsByDefault, the ops gates' own
// allowlist) for the unset default. A zero handle (no store) fits none. It
// does no I/O.
func laneStoreFitFor(sessions beads.SessionStore) laneStoreFit {
	if sessions == (beads.SessionStore{}) {
		return laneStoreFitNone
	}
	if _, ok := beads.GuardedBatcherFor(sessions); !ok {
		return laneStoreFitNone
	}
	if opjournal.CheckRecordsByDefault(sessions) != nil {
		return laneStoreFitListed
	}
	return laneStoreFitDefault
}

// laneDefaultApplies reports whether an unset [ops] authoritative runs
// laneDefaultKinds over sessions (laneStoreFitDefault).
func laneDefaultApplies(sessions beads.SessionStore) bool {
	return laneStoreFitFor(sessions) == laneStoreFitDefault
}

// effectiveLaneKinds is the op kinds the conversation lane executes for cfg
// over the sessions-class store sessions: [ops] authoritative when it is set,
// an explicit [] included; unset, laneDefaultKinds where laneDefaultApplies,
// else none. defaulted reports that the unset default supplied the kinds. A
// nil cfg runs none.
func effectiveLaneKinds(cfg *config.City, sessions beads.SessionStore) (kinds []string, defaulted bool) {
	if cfg == nil {
		return nil, false
	}
	if cfg.Ops.AuthoritativeSet() {
		return slices.Clone(cfg.Ops.AuthoritativeKinds()), false
	}
	if laneDefaultApplies(sessions) {
		return slices.Clone(laneDefaultKinds), true
	}
	return nil, false
}

// laneLegacyDeliveryWarnings are the deprecation warnings for cfg where the
// conversation lane runs kinds over a store laneDefaultApplies to or not
// (defaultApplies): each legacy-delivery key the city still sets, and an
// explicit [ops] authoritative = [] that holds back the default. They are
// empty where no lane runs and the default does not apply, since legacy
// delivery is still that city's only delivery. A key warning never asks for
// removal: each key still governs the legacy deliverers until legacy
// delivery is removed in a future release (one after gc init splits storage
// by default), and removing [daemon] nudge_dispatcher would restart the
// per-session pollers.
func laneLegacyDeliveryWarnings(cfg *config.City, kinds []string, defaultApplies bool) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	if cfg.Ops.AuthoritativeSet() && len(cfg.Ops.AuthoritativeKinds()) == 0 && defaultApplies {
		out = append(out, "[ops] authoritative = [] keeps message, interrupt and respond on legacy delivery; "+
			"this rollback works only until legacy delivery is removed in a future release")
	}
	if len(kinds) == 0 {
		return out
	}
	listed := strings.Join(kinds, ", ")
	message := slices.Contains(kinds, string(opjournal.KindMessage))
	control := slices.Contains(kinds, string(opjournal.KindInterrupt)) || slices.Contains(kinds, string(opjournal.KindRespond))
	for _, k := range []struct {
		key, value string
		// superseded: the lane serves the requests the key governs.
		superseded bool
	}{
		{"[daemon] nudge_dispatcher", cfg.Daemon.NudgeDispatcher, message},
		{"[session] nudge_poll_interval", cfg.Session.NudgePollInterval, message},
		{"[ops] content", cfg.Ops.ContentMode, message},
		{"[ops] control", cfg.Ops.ControlMode, control},
	} {
		if !k.superseded || strings.TrimSpace(k.value) == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s is deprecated: the conversation lane delivers %s here, and the key is retired when legacy delivery is removed in a future release; leave it set until then, since it still governs legacy delivery here (queue items written before the upgrade, sessions without a row)", k.key, listed))
	}
	return out
}
