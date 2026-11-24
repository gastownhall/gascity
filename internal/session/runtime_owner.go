//go:build !windows

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// runtimeOwner is the runtime-owner record. A Manager writes it after each
// start it makes through a process-table-scanner provider, the legacy
// reconciler after a warm relaunch, and a legacy controller's startup for a
// running session whose record names no live root, so a later start under
// another uid, whose scan cannot see this uid's processes, can still tell
// whether the runtime it would replace is alive.
type runtimeOwner struct {
	UID   int                `json:"uid"`
	Boot  string             `json:"boot"`
	Roots []runtimeOwnerRoot `json:"roots"`
}

// runtimeOwnerRoot is one recorded runtime root: its pid and the start time
// pidutil.StartTime read for it, which tells a reused pid apart.
type runtimeOwnerRoot struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
}

// currentEUID is the effective uid a start runs under.
var currentEUID = os.Geteuid

// parseRuntimeOwner decodes a runtime_owner value.
func parseRuntimeOwner(raw string) (runtimeOwner, error) {
	var owner runtimeOwner
	if err := json.Unmarshal([]byte(raw), &owner); err != nil {
		return runtimeOwner{}, fmt.Errorf("decoding %s %q: %w", runtimeOwnerKey, raw, err)
	}
	return owner, nil
}

// checkRuntimeOwner refuses a start of sessionID, with an error wrapping
// runtime.ErrRuntimeUnavailable, while a runtime root another uid recorded
// for it in this boot is still alive at its recorded start time. A scan under
// this uid skips other uids' processes, so the orphan passes cannot see that
// runtime, and a start would run a second one beside it. A record under this
// uid, from another boot, or whose roots are all gone lets the start proceed;
// an unreadable or malformed record is logged and lets it proceed too, since
// the gate refuses only on identity it verified. A session with no row has
// no record and proceeds; any other failed row read is returned, wrapped.
func (m *Manager) checkRuntimeOwner(sessionID string) error {
	b, err := m.store.Get(sessionID)
	if errors.Is(err, beads.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking runtime owner of session %q: %w", sessionID, err)
	}
	raw := strings.TrimSpace(b.Metadata[runtimeOwnerKey])
	if raw == "" {
		return nil
	}
	owner, err := parseRuntimeOwner(raw)
	if err != nil {
		log.Printf("session: ignoring runtime owner record of %s: %v", sessionID, err)
		return nil
	}
	if owner.UID == currentEUID() {
		return nil
	}
	live := owner.liveRoots()
	if len(live) == 0 {
		return nil
	}
	pids := strings.Join(live, ",")
	log.Printf("session: refusing to start %s: runtime recorded under uid %d is still alive (pids %s)", sessionID, owner.UID, pids)
	return fmt.Errorf("session %s: a runtime started under uid %d is still alive (pids %s), and processes of another uid are not visible to this one; "+
		"stop it as uid %d (gc stop or gc supervisor stop as that user) or run the supervisor as uid %d, then clear any leftover record with "+
		"gc bd update %s --unset-metadata %s: %w",
		sessionID, owner.UID, pids, owner.UID, owner.UID, sessionID, runtimeOwnerKey, runtime.ErrRuntimeUnavailable)
}

// liveRoots returns the pids of owner's roots still alive at their recorded
// start times (pidutil.AliveWithStartTime, which needs no privilege), and
// none when owner is from another boot. A boot id either side cannot read
// counts as this boot, so only a boot verified to differ rules the roots out.
func (owner runtimeOwner) liveRoots() []string {
	if boot := runtimeLeaseBootID(); owner.Boot != "" && boot != "" && owner.Boot != boot {
		return nil
	}
	var live []string
	for _, root := range owner.Roots {
		if pidutil.AliveWithStartTime(root.PID, root.Start) {
			live = append(live, strconv.Itoa(root.PID))
		}
	}
	return live
}

// recordRuntimeOwner writes sessionID's runtime-owner record after a start
// whose provider Start succeeded (writeRuntimeOwner).
func (m *Manager) recordRuntimeOwner(sessionID string) {
	writeRuntimeOwner(m.store, m.sp, m.cityPath, sessionID)
}

// RecordRuntimeOwner writes the runtime-owner record (SESSION-START-029) of
// sessionID in cityPath, whose runtime sp has just launched again outside a
// Manager start: a warm relaunch replaces the runtime's root process, so the
// record its last start wrote names only dead pids, and a start under another
// uid would read the runtime as gone. Like a start's record it is best effort
// and logged (writeRuntimeOwner).
func (s *Store) RecordRuntimeOwner(sp runtime.Provider, cityPath, sessionID string) {
	if s == nil {
		return
	}
	writeRuntimeOwner(s.store, sp, cityPath, sessionID)
}

// RecordAdoptedRuntimeOwners writes the runtime-owner record of each open
// session in cityPath whose runtime sp lists as running and whose row holds
// no record naming a root still alive in this boot (runtimeOwner.liveRoots),
// under any uid: none, a malformed one, one from another boot, or one whose
// roots are all gone, carry another start time or were never found. Those are
// runtimes a controller adopted rather than started, above all every runtime
// that a restart deploying the fence finds running, which no start has
// recorded, and runtimes whose record a failed write, an empty scan or a
// replaced root left stale; with no live record, a later start under another
// uid, whose scan cannot see them, starts a second runtime beside each. A
// record naming a live root is left alone whoever wrote it: this euid's names
// the runtime it recorded, and another uid's is the only evidence of a
// runtime this euid's scan cannot see. A session a provider lists only
// through a dead pane (tmux's remain-on-exit) thus keeps the other uid's
// record, and the start that replaces the pane is refused (checkRuntimeOwner)
// rather than run beside that runtime. It returns how many records it wrote,
// each best effort and logged (writeRuntimeOwner), and a listing failure; a
// partial listing still records what it listed. A provider that cannot scan
// the process table records nothing.
func (s *Store) RecordAdoptedRuntimeOwners(sp runtime.Provider, cityPath string) (int, error) {
	if s == nil {
		return 0, nil
	}
	if _, ok := runtime.AsProcessTableScanner(sp); !ok {
		return 0, nil
	}
	running, runErr := sp.ListRunning("")
	if runErr != nil {
		runErr = fmt.Errorf("listing running sessions: %w", runErr)
		if !runtime.IsPartialListError(runErr) {
			return 0, runErr
		}
	}
	if len(running) == 0 {
		return 0, runErr // nothing adopted: the rows are not read
	}
	listed := make(map[string]bool, len(running))
	for _, name := range running {
		listed[name] = true
	}
	rows, rowErr := s.listAllBeads(ListAllOptions{})
	if rowErr != nil {
		rowErr = fmt.Errorf("listing session rows: %w", rowErr)
		if !beads.IsPartialResult(rowErr) {
			return 0, rowErr
		}
	}
	recorded := 0
	for _, b := range rows {
		if !listed[infoFromPersistedBead(b).SessionName] || runtimeOwnerNamesLiveRoot(b.Metadata[runtimeOwnerKey]) {
			continue
		}
		if writeRuntimeOwner(s.store, sp, cityPath, b.ID) {
			recorded++
		}
	}
	return recorded, errors.Join(runErr, rowErr)
}

// runtimeOwnerNamesLiveRoot reports whether raw is a runtime-owner record,
// under any uid, naming a root still alive in this boot
// (runtimeOwner.liveRoots).
func runtimeOwnerNamesLiveRoot(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	owner, err := parseRuntimeOwner(raw)
	return err == nil && len(owner.liveRoots()) > 0
}

// writeRuntimeOwner writes sessionID's runtime-owner record to store: this
// process's euid, the boot id, and the pid and start time of each root sp's
// scan reports for the session in cityPath, tracked or not, except two kinds
// of root that would hold the fence shut after the runtime ends: city
// infrastructure, which outlives sessions, and a root in a foreign systemd
// user service unit (proctable.DetachedUserServiceUnit), which the unit owns
// and which carries the session's identity only through its environment
// (SESSION-START-028). A root the provider claims as a live runtime it does
// not list (LiveUnlisted, its provider-scoped evidence) is recorded wherever
// it runs. Being tracked is no such claim: a provider tracks a root by its
// session id, so while the session runs every root carrying that id reads as
// tracked, the foreign service's included. A provider that cannot scan the
// process table records nothing. Recording is best effort: the runtime is
// already up, so a scan error, a root whose start time or classification is
// unreadable, an empty root set or a failed write is logged with the session
// and never fails the caller. A failed scan still records the roots it did
// report, and an empty root set still writes the record, which retires one
// an earlier owner left. It reports whether it wrote the record.
func writeRuntimeOwner(store beads.SessionStore, sp runtime.Provider, cityPath, sessionID string) bool {
	scanner, ok := runtime.AsProcessTableScanner(sp)
	if !ok || sessionID == "" {
		return false
	}
	found, scanErr := scanner.FindRuntimesBySessionID(sessionID)
	if scanErr != nil {
		log.Printf("session: recording runtime owner of %s: scanning for its runtime roots: %s", sessionID, proctable.SummarizeScanError(scanErr))
	}
	cityPath = pathutil.NormalizePathForCompare(strings.TrimSpace(cityPath))
	owner := runtimeOwner{UID: currentEUID(), Boot: runtimeLeaseBootID(), Roots: []runtimeOwnerRoot{}}
	for _, live := range found {
		if live.SessionID != sessionID {
			continue
		}
		if cityPath != "" && pathutil.NormalizePathForCompare(strings.TrimSpace(live.City)) != cityPath {
			continue
		}
		infrastructure, err := rootIsCityInfrastructure(live)
		if err != nil {
			log.Printf("session: recording runtime owner of %s: leaving root pid=%d out: %v", sessionID, live.PID, err)
			continue
		}
		if infrastructure {
			continue
		}
		if !live.LiveUnlisted {
			if unit := proctable.DetachedUserServiceUnit(live.PID); unit != "" {
				log.Printf("session: recording runtime owner of %s: leaving root pid=%d out: detached user service %s (ppid=%d name=%q)", sessionID, live.PID, unit, live.PPID, live.Name)
				continue
			}
		}
		start, err := pidutil.StartTime(live.PID)
		if err != nil {
			log.Printf("session: recording runtime owner of %s: leaving root pid=%d out: reading its start time: %v", sessionID, live.PID, err)
			continue
		}
		owner.Roots = append(owner.Roots, runtimeOwnerRoot{PID: live.PID, Start: start})
	}
	if len(owner.Roots) == 0 {
		log.Printf("session: recording runtime owner of %s: no runtime root found for it; a start under another uid will not be fenced", sessionID)
	}
	raw, err := json.Marshal(owner)
	if err != nil {
		log.Printf("session: recording runtime owner of %s: encoding the record: %v", sessionID, err)
		return false
	}
	if err := store.SetMetadata(sessionID, runtimeOwnerKey, string(raw)); err != nil {
		log.Printf("session: recording runtime owner of %s: writing %s: %v", sessionID, runtimeOwnerKey, err)
		return false
	}
	return true
}
