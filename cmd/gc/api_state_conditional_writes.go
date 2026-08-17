package main

import (
	"sort"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

// ConditionalWritesStatus builds the §12.5 status-wire block from the
// controller's own latched state: the boot-resolved mode and origin, the
// side-effect-free per-store inspection (probe/latch memos, never a fresh
// probe — a status poll must not shell out to bd), and the retained rollout
// notices including live drift. It implements the api layer's
// conditionalWritesStatusProvider.
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

	if out.Mode == string(rollout.Off) {
		// Verdicts are moot when the gate is off: the write path never
		// fences, so per-store rows would be noise on every status poll.
		out.Effective = "off"
		return out
	}

	cs.mu.RLock()
	stores := map[string]beads.Store{"city": cs.cityBeadStore}
	for name, store := range cs.beadStores {
		stores["rig/"+name] = store
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

	// Severity order: a store refusing or silently degrading writes matters
	// more than a pending config edit — the drift still shows in Notices.
	switch {
	case incapable && mode == rollout.Require:
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
