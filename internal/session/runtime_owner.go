//go:build !windows

package session

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// runtimeOwnerKey is the session metadata key holding the runtime-owner
// record: the uid that launched the session's runtime, the boot it ran in,
// and the identity of each of its process-table roots.
const runtimeOwnerKey = "runtime_owner"

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
// the gate refuses only on identity it verified. A failed row read is
// returned, wrapped.
func (m *Manager) checkRuntimeOwner(sessionID string) error {
	b, err := m.store.Get(sessionID)
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
