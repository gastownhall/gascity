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
// start it makes through a process-table-scanner provider, so a later start
// under another uid, whose scan cannot see this uid's processes, can still
// tell whether the runtime it would replace is alive.
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
	if boot := runtimeLeaseBootID(); owner.Boot != "" && boot != "" && owner.Boot != boot {
		return nil
	}
	var live []string
	for _, root := range owner.Roots {
		if pidutil.AliveWithStartTime(root.PID, root.Start) {
			live = append(live, strconv.Itoa(root.PID))
		}
	}
	if len(live) == 0 {
		return nil
	}
	pids := strings.Join(live, ",")
	log.Printf("session: refusing to start %s: runtime recorded under uid %d is still alive (pids %s)", sessionID, owner.UID, pids)
	return fmt.Errorf("session %s: a runtime started under uid %d is still alive (pids %s), and processes of another uid are not visible to this one; "+
		"stop it as uid %d (gc stop or gc supervisor stop as that user) or run the supervisor as uid %d, then clear any leftover record with "+
		"bd update %s --unset-metadata %s: %w",
		sessionID, owner.UID, pids, owner.UID, owner.UID, sessionID, runtimeOwnerKey, runtime.ErrRuntimeUnavailable)
}

// recordRuntimeOwner writes sessionID's runtime-owner record after a start
// whose provider Start succeeded: this process's euid, the boot id, and the
// pid and start time of each root the scan reports for the session in this
// city, tracked or not, that is not city infrastructure (which outlives
// sessions and would hold the fence shut). A provider that cannot scan the
// process table records nothing. Recording is best effort: the runtime is
// already up, so a scan error, a root whose start time or classification is
// unreadable, an empty root set or a failed write is logged with the
// session and never fails the start. A failed scan still records the roots
// it did report, and an empty root set still writes the record, which
// retires one an earlier owner left.
func (m *Manager) recordRuntimeOwner(sessionID string) {
	scanner, ok := runtime.AsProcessTableScanner(m.sp)
	if !ok || sessionID == "" {
		return
	}
	found, scanErr := scanner.FindRuntimesBySessionID(sessionID)
	if scanErr != nil {
		log.Printf("session: recording runtime owner of %s: scanning for its runtime roots: %s", sessionID, proctable.SummarizeScanError(scanErr))
	}
	cityPath := pathutil.NormalizePathForCompare(strings.TrimSpace(m.cityPath))
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
		return
	}
	if err := m.store.SetMetadata(sessionID, runtimeOwnerKey, string(raw)); err != nil {
		log.Printf("session: recording runtime owner of %s: writing %s: %v", sessionID, runtimeOwnerKey, err)
	}
}
