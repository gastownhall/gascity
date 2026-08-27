package main

import (
	"sort"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

// sessionClassStoreID is the §12.5 row that answers a different question from
// every other row. The rest report what the city's rollout mode resolves to on
// a store; this one reports whether the session class HAS the fencing it
// requires, which no mode can turn off.
const sessionClassStoreID = "sessions (required)"

// ConditionalWritesStatus builds the §12.5 status-wire block from the
// controller's own latched state: the boot-resolved mode and origin, the
// side-effect-free per-store inspection (probe/latch memos, never a fresh
// probe — a status poll must not shell out to bd), the session class's
// required-capability verdict, and the retained rollout notices including live
// drift. It implements the api layer's conditionalWritesStatusProvider.
func (cs *controllerState) ConditionalWritesStatus() *api.StatusConditionalWrites {
	flags := cs.RolloutFlags()
	mode := flags.BeadsConditionalWrites()
	out := &api.StatusConditionalWrites{
		Mode:   string(mode),
		Origin: string(flags.OriginOf(rollout.KeyBeadsConditionalWrites)),
	}
	if mode == rollout.ModeUnset {
		// A zero Flags value (boot resolve error) or an unthreaded caller:
		// the write path treats unset as legacy, so the wire says off.
		out.Mode = string(rollout.Off)
		out.Origin = string(rollout.OriginBuiltin)
	}

	notices := append([]rollout.Notice(nil), flags.Notices()...)
	drift := cs.RolloutDriftNotices()
	notices = append(notices, drift...)
	for _, n := range notices {
		out.Notices = append(out.Notices, api.StatusRolloutNotice{
			Kind:        string(n.Kind),
			FlagKey:     n.FlagKey,
			EnvVar:      n.EnvVar,
			ConfigValue: n.ConfigValue,
			EnvValue:    n.EnvValue,
			Message:     n.Message,
		})
	}

	// The session-class row is emitted in EVERY mode, off included. The
	// requirement belongs to the reconciler, not to the rollout gate, so an
	// operator running with the gate off still needs to see whether the class
	// the keyed reconciler writes through can fence — and a split city used to
	// show nothing at all here (ga-f7v2ft.162).
	sessionRow, sessionMissing := cs.sessionClassRequirementVerdict()
	if sessionRow != nil {
		out.Stores = append(out.Stores, *sessionRow)
	}

	if out.Mode == string(rollout.Off) {
		// Mode-gated verdicts are moot when the gate is off: the write path
		// never consults it, so per-store rows would be noise on every poll.
		// A missing REQUIREMENT is not moot in any mode.
		out.Effective = "off"
		if sessionMissing {
			out.Effective = "fail_closed"
		}
		return out
	}

	cs.mu.RLock()
	stores := map[string]beads.Store{"city": cs.cityBeadStore}
	for name, store := range cs.beadStores {
		stores["rig/"+name] = store
	}
	// The relocated coordination classes are served from a binding this
	// process opened outside the beads factory. Enumerating work stores alone
	// is what let a split city report effective=active while nothing at all was
	// reported about the store its session rows live in (ga-f7v2ft.162).
	for _, engine := range cs.storageRoutes.openedEngines() {
		stores[engine.conditionalWritesStoreID()] = engine.store
	}
	cs.mu.RUnlock()

	incapable := false
	ids := make([]string, 0, len(stores))
	for id := range stores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		store := stores[id]
		if store == nil {
			continue
		}
		insp := beads.InspectConditionalWrites(store)
		out.Stores = append(out.Stores, api.StatusConditionalWriteStoreVerdict{
			StoreID: id,
			Kind:    conditionalWritesEventStoreKind(insp.StoreKind),
			Probe:   insp.Probe,
			Latch:   insp.Latch,
			Capable: insp.Capable,
			Reason:  insp.Reason,
		})
		if !insp.Capable {
			incapable = true
		}
	}

	// Severity order: a missing REQUIREMENT outranks everything — no mode
	// makes it acceptable — then a store refusing or silently degrading
	// writes, then a pending config edit (which still shows in Notices).
	switch {
	case sessionMissing, incapable && mode == rollout.Require:
		out.Effective = "fail_closed"
	case incapable:
		out.Effective = "degraded"
	case len(drift) > 0:
		out.Effective = "pending_restart"
	default:
		out.Effective = "active"
	}
	return out
}

// sessionClassRequirementVerdict reports whether the store serving the session
// class can fence its own writes, and whether that requirement is unmet.
//
// The question is capability, not policy: no mode participates, because this
// row exists to say "the reconciler needs this and it is/is not there", not
// "the rollout gate resolved to X here".
//
// It is answered from the side-effect-free inspection rather than from
// beads.RequiredConditionalWriter, which the boot preflight uses. The
// difference matters: a capability PROBE may shell out (BdStore runs four bd
// subprocesses on its first), and a status poll must never do that. The
// inspection's Implements flag is a type assertion, and its probe/latch fields
// are memos — so a store that has definitively reported incapable is caught
// here, and one that simply has not been exercised yet reads as meeting the
// requirement until something actually tries. The eager, expensive answer
// belongs to boot, where it is paid once.
//
// A nil row means the controller holds no session-class store yet — an absent
// store, not an absent capability.
func (cs *controllerState) sessionClassRequirementVerdict() (*api.StatusConditionalWriteStoreVerdict, bool) {
	if cs == nil {
		return nil, false
	}
	store := cs.SessionsBeadStore().Store
	if store == nil {
		return nil, false
	}
	insp := beads.InspectConditionalWrites(store)
	verdict := &api.StatusConditionalWriteStoreVerdict{
		StoreID: sessionClassStoreID,
		Kind:    conditionalWritesEventStoreKind(insp.StoreKind),
		Probe:   insp.Probe,
		Latch:   insp.Latch,
		Capable: true,
	}
	switch {
	case !insp.Implements:
		verdict.Reason = "the store does not implement conditional writes"
	case !insp.Capable:
		verdict.Reason = insp.Reason
		if verdict.Reason == "" {
			verdict.Reason = "the store reported it cannot fence a write"
		}
	default:
		return verdict, false
	}
	verdict.Capable = false
	verdict.Probe = beads.ConditionalWriteProbeIncapable
	verdict.Reason = "FATAL: the session reconciler requires conditional writes on this class and this store cannot provide them: " + verdict.Reason
	return verdict, true
}
