package session

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrStaleRestartRequest reports that the session row records another
// incarnation than the requester's; nothing was recorded.
var ErrStaleRestartRequest = errors.New("the session row records another incarnation than the requester's")

// ErrRestartRequestUnfenced reports that an in-session restart request named
// no incarnation (no instance token); nothing was read or recorded.
var ErrRestartRequestUnfenced = errors.New("the restart request names no incarnation")

// ErrRestartRequestContended reports that the session row changed under every
// attempt to record the restart; nothing was recorded.
var ErrRestartRequestContended = errors.New("the session row changed on every attempt to record the restart")

// ErrRestartRequestNeedsConditionalWrite reports that the city records a
// restart request only by compare-and-swap (a v2 city) and its session store
// resolves no conditional writer; nothing was recorded.
var ErrRestartRequestNeedsConditionalWrite = errors.New("this city records a restart request only by compare-and-swap, and its session store resolves no conditional writer")

// restartRequestAttempts bounds the re-read and re-decide loop of a fenced
// restart request (the hook's started write uses the same bound).
const restartRequestAttempts = 5

// RequestFreshRestartFenced is RequestFreshRestart for a request made from
// inside the session (SG-3; the reconciler-v2 owner's ruling of 2026-10-27,
// condition 1). It records restart_requested and continuation_reset_pending
// only while the row still records the caller's incarnation, instanceToken
// being the caller's GC_INSTANCE_TOKEN, so a late request from a dead
// incarnation cannot restart its successor.
//
// It decides on a fresh read and writes by compare-and-swap at that read's
// revision wherever the store resolves a conditional writer (every v2 city),
// re-reading and re-deciding on a lost swap. Without one the patch is written
// right after the deciding read, as UpdateMetadataFenced does, only when
// casOnly allows it: casOnly reports whether the city records only by
// compare-and-swap (a v2 city), and is asked only when a patch is due and the
// store resolves no conditional writer, so a v2 city never takes the checked
// write (ErrRestartRequestNeedsConditionalWrite); a nil casOnly allows it (a
// legacy city). A store that must fence and cannot refuses. A row that already
// carries both keys is not written again. The caller's generation is not
// compared: every start mints a new token, and a rekey moves only the token.
//
// A row that records another token still records the request when the live
// runtime under the row's session name carries instanceToken and names no
// other row: the caller runs in that runtime, which is the row's current one
// (a legacy warm reuse mints in PreWake and keeps the running pane). The
// runtime is read after the row, inside the decision, so a restart after the
// deciding read loses the swap and the re-decide reads its successor's
// token. A failed read is no proof. On a worker site the guard refuses
// runtime metadata, so only the row's token proves the incarnation there.
//
// instanceToken is a capability: it is never logged or printed.
func (m *Manager) RequestFreshRestartFenced(id, instanceToken string, casOnly func() bool) error {
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return ErrRestartRequestUnfenced
	}
	return withSessionMutationLock(id, func() error {
		var refusal error
		due := false
		writeRefusal := m.restartWriteRefusal(casOnly)
		written, err := m.PersistedStore().UpdateMetadataFenced(id, restartRequestAttempts, func(info Info, _ PersistedResponse) MetadataPatch {
			patch, decideErr := decideRestartRequest(info, instanceToken, m.runtimeCarriesToken)
			if decideErr == nil && len(patch) > 0 {
				decideErr = writeRefusal()
			}
			if decideErr != nil {
				patch = nil
			}
			refusal, due = decideErr, len(patch) > 0
			return patch
		})
		switch {
		case err != nil:
			return err
		case refusal != nil:
			return refusal
		case due && !written:
			return ErrRestartRequestContended
		}
		return nil
	})
}

// CheckFreshRestartFenced is RequestFreshRestartFenced's decision on a fresh
// read of the row, with nothing written: nil when the request would be
// recorded or is already pending, else the refusal the request would return,
// the checked write's refusal under casOnly among them.
// A caller that acts on the session before it records the request (a self
// handoff's mail) decides with it first, so a refused incarnation acts on
// nothing. The fenced write stays the authority: the row can move between
// this read and that write.
//
// instanceToken is a capability: it is never logged or printed.
func (m *Manager) CheckFreshRestartFenced(id, instanceToken string, casOnly func() bool) error {
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return ErrRestartRequestUnfenced
	}
	info, err := m.PersistedStore().Get(id)
	if err != nil {
		return err
	}
	patch, err := decideRestartRequest(info, instanceToken, m.runtimeCarriesToken)
	if err != nil || len(patch) == 0 {
		return err
	}
	return m.restartWriteRefusal(casOnly)()
}

// restartWriteRefusal returns the refusal of a due restart-request write that
// the decision on the row allowed: the require refusal of a store that must
// fence and cannot, or ErrRestartRequestNeedsConditionalWrite when the store
// resolves no conditional writer and casOnly reports that the city records
// only by compare-and-swap. It resolves the writer and asks casOnly at most
// once, on its first call, so a re-decide after a lost swap asks nothing new.
func (m *Manager) restartWriteRefusal(casOnly func() bool) func() error {
	var (
		asked   bool
		refusal error
	)
	return func() error {
		if asked {
			return refusal
		}
		asked = true
		writer, _, err := beads.ResolveConditionalWriter(beads.SessionStore{Store: m.store})
		switch {
		case err != nil:
			refusal = err
		case writer == nil && casOnly != nil && casOnly():
			refusal = ErrRestartRequestNeedsConditionalWrite
		}
		return refusal
	}
}

// decideRestartRequest is the fence's decision on one fresh read of the row:
// a closed row refuses with ErrSessionClosed; a row whose instance_token is
// not instanceToken, and whose live runtime (runtimeCarries) does not carry
// it either, refuses with ErrStaleRestartRequest; a row that already carries
// both keys needs no patch; and any other row gets both keys.
func decideRestartRequest(info Info, instanceToken string, runtimeCarries func(row Info, instanceToken string) bool) (MetadataPatch, error) {
	if info.Closed {
		return nil, ErrSessionClosed
	}
	if strings.TrimSpace(info.InstanceToken) != instanceToken && !runtimeCarries(info, instanceToken) {
		return nil, ErrStaleRestartRequest
	}
	if info.RestartRequested == "true" && info.ContinuationResetPending == "true" {
		return nil, nil
	}
	return MetadataPatch{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}, nil
}

// runtimeCarriesToken reports whether the live runtime under row's session
// name carries instanceToken as its GC_INSTANCE_TOKEN and names no other row.
// A runtime whose GC_SESSION_ID is another row's is Foreign to row (cmd/gc's
// compareIdentity), so its token proves nothing here; one with no
// GC_SESSION_ID (legacy adoption never stamped it) is judged by its token
// alone. Any read error, a missing runtime, or a manager without a provider
// proves nothing.
func (m *Manager) runtimeCarriesToken(row Info, instanceToken string) bool {
	if m.sp == nil || strings.TrimSpace(row.SessionName) == "" {
		return false
	}
	tok, err := runtime.MetaValue(m.sp.GetMeta(row.SessionName, runtime.InstanceTokenEnv))
	if err != nil || strings.TrimSpace(tok) != instanceToken {
		return false
	}
	runtimeID, err := runtime.MetaValue(m.sp.GetMeta(row.SessionName, "GC_SESSION_ID"))
	if err != nil {
		return false
	}
	runtimeID = strings.TrimSpace(runtimeID)
	return runtimeID == "" || runtimeID == strings.TrimSpace(row.ID)
}
