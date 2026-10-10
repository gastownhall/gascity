package session

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// Facts are a row's values for the keys a premise compares, in registry
// order: every key the registry puts in the premise (Field.InPremise), then
// the assignment identity keys outside it, which only an effect that reads
// the row's work (L5) compares. Two rows agree on the compared keys exactly
// when their Facts, projected alike, are ==. An absent key reads as "".
type Facts struct{ v [numFactKeys]string }

// numFactKeys is the number of keys Facts holds; TestFactsKeys pins it to
// the registry.
const numFactKeys = 72

// factKeys are Facts' keys, and identityFrom the index of the first
// assignment identity key outside the premise.
var factKeys, identityFrom = func() ([]string, int) {
	var premise, identity []string
	for _, f := range fields {
		switch {
		case f.InPremise():
			premise = append(premise, f.Key)
		case f.Class == ClassAssignmentIdentity:
			identity = append(identity, f.Key)
		}
	}
	return append(premise, identity...), len(premise)
}()

// FactsOf is meta's Facts.
func FactsOf(meta map[string]string) Facts {
	var f Facts
	for i, k := range factKeys[:min(len(factKeys), numFactKeys)] {
		f.v[i] = meta[k]
	}
	return f
}

// Premise is f without the assignment identity keys: what a premise compares
// unless its effect reads the row's work.
func (f Facts) Premise() Facts {
	for i := identityFrom; i < numFactKeys; i++ {
		f.v[i] = ""
	}
	return f
}

// FactsSite names a premise. A site the registry lists (factsSites)
// compares Facts by its entry; FactsDefault compares all of them.
type FactsSite string

// The premise sites.
const (
	// FactsDefault compares every key Facts holds.
	FactsDefault FactsSite = ""
	// FactsAfterStart is a v2 transaction's premise after a Call that starts
	// a runtime.
	FactsAfterStart FactsSite = "v2.tx.after-start"
	// FactsCommit is a v2 transaction's commit after its own write (v5 S2).
	FactsCommit FactsSite = "v2.tx.commit"
	// FactsLegacyKill is a legacy controller kill decided on a tick's row.
	FactsLegacyKill FactsSite = "legacy.kill"
	// FactsLegacyStopPending is a legacy drain's basis, read at the drain's
	// begin: its stop-pending mark and its timeout kill.
	FactsLegacyStopPending FactsSite = "legacy.stop-pending"
	// FactsLegacyDrainStop is the kill of a row marked stop-pending.
	FactsLegacyDrainStop FactsSite = "legacy.drain-stop"
	// FactsLegacyResetEvict is the eviction of a runtime a stalled
	// continuation reset is waiting on.
	FactsLegacyResetEvict FactsSite = "legacy.reset-evict"
)

// factsSite is how a site compares Facts: without Drop's keys, or only
// Only's, with the fresh row's state among States when set, and, with
// NoWake, not live when the decided row was not (a Decided site's rule); and
// why.
type factsSite struct {
	Drop, Only, States []string
	NoWake             bool
	Reason             string
}

// The keys of a legacy drain's basis: the incarnation, the operator's intent
// and the operator's requests. Not the drain's own lifecycle writes, nor the
// counters and markers the controller keeps while it drains. Not held_until:
// an agent's heartbeat writes it alone, and must not void a drain (#3994);
// an operator's hold is told by sleep_intent=user-hold, which a suspend
// writes beside it.
var drainBasisKeys = []string{
	"generation", "instance_token",
	"sleep_intent", "wait_hold", "quarantined_until", "suspended_at", "sleep_reason",
	"wake_request", "restart_requested",
}

// factsSites are the premise sites that do not compare every key.
var factsSites = map[FactsSite]factsSite{
	FactsAfterStart: {
		Drop:   []string{"session_key", beadmeta.CurrentClaimBeadIDMetadataKey},
		Reason: "a runtime the Call started writes its own resumable session key and the claim it took while the start is in flight; those do not make the row another row. Nothing writes the current bead or detached_at then, so they stay compared",
	},
	FactsCommit: {
		Only:   []string{"instance_token"},
		States: []string{string(StateCreating), string(StateActive), string(StateAwake)},
		Reason: "S2's commit: the row still carries the token the effect wrote and is creating, active or awake; holds do not veto it, so an agent's heartbeat held_until during startup cannot orphan the runtime it started, while a suspend or a kill moves state and refuses",
	},
	// TODO(R8 ratchet, mc-z9o37): widen to the whole disposition class
	// (drainBasisKeys' intent keys), caller by caller, with a test each.
	FactsLegacyKill: {
		Only:   []string{"generation", "instance_token", "sleep_intent", "held_until"},
		NoWake: true,
		Reason: "what an operator moves under the lease: the incarnation, the hold an attach consumes or a suspend sets, and dormancy (a row decided dormant that an attach woke is live now); the controller's own tick heals the rest",
	},
	FactsLegacyStopPending: {
		Only:   drainBasisKeys,
		Reason: "a drain stops the incarnation it began on, for the operator intent and requests it began under: a resume that consumed the hold, a suspend, a wake or restart request, or a new incarnation voids it (SR3-1, SR2-1); the drain's own state, drain_at, an agent's heartbeat and the controller's counters and markers move while it runs and do not",
	},
	FactsLegacyDrainStop: {
		Only:   []string{"state", "state_reason", "generation", "instance_token"},
		Reason: "the async stop and the escalation kill the incarnation that is still stop-pending; the mark already judged the operator's intent, and a resume cannot take a stop-pending row out (ErrSessionStopping)",
	},
	FactsLegacyResetEvict: {
		Only:   []string{"continuation_reset_pending", "reset_committed_at"},
		Reason: "the eviction rests on the same continuation reset still pending; a row that moved on is not stalled",
	},
}

// Match reports whether fresh is expect as site compares them.
func (s FactsSite) Match(fresh, expect Facts) bool { return len(s.Moved(fresh, expect)) == 0 }

// Moved names what keeps fresh from being expect as site compares them: the
// keys whose values differ, and "state" when the site wants another state or
// (NoWake) fresh is live where expect was not.
func (s FactsSite) Moved(fresh, expect Facts) []string {
	e := factsSites[s]
	var moved []string
	if len(e.States) > 0 && !slices.Contains(e.States, strings.TrimSpace(fresh.get("state"))) ||
		e.NoWake && liveMetadataState(fresh.get("state")) && !liveMetadataState(expect.get("state")) {
		moved = append(moved, "state")
	}
	f, x := fresh.under(e), expect.under(e)
	for i, k := range factKeys[:min(len(factKeys), numFactKeys)] {
		if f.v[i] != x.v[i] && !slices.Contains(moved, k) {
			moved = append(moved, k)
		}
	}
	return moved
}

// under is f as e compares it.
func (f Facts) under(e factsSite) Facts {
	for i, k := range factKeys[:min(len(factKeys), numFactKeys)] {
		if slices.Contains(e.Drop, k) || len(e.Only) > 0 && !slices.Contains(e.Only, k) {
			f.v[i] = ""
		}
	}
	return f
}

// get is key's value in f.
func (f Facts) get(key string) string {
	if i := slices.Index(factKeys, key); i >= 0 && i < numFactKeys {
		return f.v[i]
	}
	return ""
}
