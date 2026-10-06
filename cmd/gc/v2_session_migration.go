package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// v2SessionMigration counts the open enterprise-era session rows v2 refuses
// to boot over (CONTRACT C4.5 item 1b, C11), which legacy runs over: by state
// main does not know (other than drain-ack stop-pending), by pool-slot session
// name two or more rows share (P3 spec F8), and by configured named identity
// two or more rows claim.
type v2SessionMigration struct {
	UnknownStates, SharedSlotNames, DuplicateNamed map[string]int
}

// readV2SessionMigration counts one live census of every session leg, folded
// first-leg-wins by bead ID as legacy folds it. A failed or partial read is an
// error: a row it missed may be one v2 refuses.
func readV2SessionMigration(cityPath, cityName string, cfg *config.City, sessions beads.Store, rigs map[string]beads.Store) (v2SessionMigration, error) {
	rows, err := collectOpenSessionInfos(cityPath, cfg, sessions, rigs, buildSuspendedRigPathsForCity(cfg, cityPath), true, true, nil)
	return tallyV2SessionMigration(cfg, cityName, rows), err
}

// tallyV2SessionMigration counts open rows, already folded, by class.
func tallyV2SessionMigration(cfg *config.City, cityName string, rows []session.Info) v2SessionMigration {
	m := v2SessionMigration{UnknownStates: map[string]int{}, SharedSlotNames: map[string]int{}, DuplicateNamed: map[string]int{}}
	for _, info := range rows {
		state, unknown, slot, named := v2SessionMigrationKeys(cfg, cityName, info)
		if unknown {
			m.UnknownStates[state]++
		}
		m.SharedSlotNames[slot]++
		m.DuplicateNamed[named]++
	}
	for _, shared := range []map[string]int{m.SharedSlotNames, m.DuplicateNamed} {
		maps.DeleteFunc(shared, func(key string, n int) bool { return key == "" || n < 2 })
	}
	return m
}

// v2SessionMigrationKeys is what one open row counts under: an unknown state,
// its pool-slot session name, its configured named identity ("" for none).
func v2SessionMigrationKeys(cfg *config.City, cityName string, info session.Info) (state string, unknown bool, slot, named string) {
	if !isKnownStateInfo(info) && !isDrainAckStopPendingInfo(info) {
		state, unknown = info.MetadataState, true
	}
	if name := strings.TrimSpace(info.SessionName); name != "" && strings.TrimSpace(info.PoolSlot) != "" {
		slot = name
	}
	if identity := namedSessionIdentityInfo(info); identity != "" && isNamedSessionInfo(info) && session.NamedSessionInfoContinuityEligible(info) {
		if _, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
			named = identity
		}
	}
	return state, unknown, slot, named
}

// refusal is v2's boot error while m holds any row, or nil.
func (m v2SessionMigration) refusal() error {
	if len(m.UnknownStates)+len(m.SharedSlotNames)+len(m.DuplicateNamed) == 0 {
		return nil
	}
	states, total := "", 0
	for _, state := range slices.Sorted(maps.Keys(m.UnknownStates)) {
		states += fmt.Sprintf(" %q=%d", state, m.UnknownStates[state])
		total += m.UnknownStates[state]
	}
	if states != "" {
		states = " (" + states[1:] + ")"
	}
	return fmt.Errorf("enterprise-era session rows: %d open row(s) in a state main does not know%s, %d pool-slot session name(s) shared by open rows, "+
		"%d configured named session(s) claimed by more than one open row; v2 refuses to boot over them: run gc doctor --check v2-session-migration to list them, or remove session_reconciler to run legacy",
		total, states, len(m.SharedSlotNames), len(m.DuplicateNamed))
}
