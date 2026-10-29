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
// legacy city). casOnly can be slow, so it is asked with nothing written and
// outside the read-to-write window, and the patch is decided again on a fresh
// read after it: an incarnation minted while it is asked refuses the request. A store that must fence and cannot refuses. A row that already
// carries both keys is not written again. The caller's generation is not
// compared: every start mints a new token, and a rekey moves only the token.
//
// A row that records another token still records the request when the live
// runtime under the row's session name carries instanceToken and names no
// other row: the caller runs in that runtime, which is the row's current one
// (a legacy warm reuse mints in PreWake and keeps the running pane). The
// runtime is read after the row, inside the decision, and the proof is
// fenced to the write that follows it (runtimeProofForWrite): the row is
// read again and then the runtime's token, the last reads before the write.
// A relaunch whose start lands during the proof (its mint already moved the
// row's token, and the pane takes that token without moving the row again)
// refuses the request, and a restart after the deciding read loses the swap,
// whose re-decide reads its successor's token. Only the window between the
// last read and the write is left. A failed read is no proof. On a worker
// site the guard refuses runtime metadata, so only the row's token proves the
// incarnation there.
//
// instanceToken is a capability: it is never logged or printed.
func (m *Manager) RequestFreshRestartFenced(id, instanceToken string, casOnly func() bool) error {
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return ErrRestartRequestUnfenced
	}
	return withSessionMutationLock(id, func() error {
		gate := &restartWriteRefusal{m: m, casOnly: casOnly}
		for {
			var refusal error
			due, unasked := false, false
			written, err := m.PersistedStore().UpdateMetadataFenced(id, restartRequestAttempts, func(info Info, _ PersistedResponse) MetadataPatch {
				patch, decideErr := decideRestartRequest(info, instanceToken, m.runtimeProofForWrite)
				unasked = false
				if decideErr == nil && len(patch) > 0 {
					var answered bool
					answered, decideErr = gate.peek()
					unasked = !answered
				}
				if decideErr != nil || unasked {
					patch = nil
				}
				refusal, due = decideErr, len(patch) > 0
				return patch
			})
			if err == nil && unasked {
				// casOnly can take seconds (the CLI asks the controller), so it
				// is never asked between the deciding read and the write: ask
				// it here, with nothing written, then re-read and re-decide.
				_ = gate.ask() // the re-decide returns the refusal, after the row's own
				continue
			}
			switch {
			case err != nil:
				return err
			case refusal != nil:
				return refusal
			case due && !written:
				return ErrRestartRequestContended
			}
			return nil
		}
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
	return (&restartWriteRefusal{m: m, casOnly: casOnly}).ask()
}

// restartWriteRefusal is the refusal of a due restart-request write that the
// decision on the row allowed: the require refusal of a store that must fence
// and cannot, or ErrRestartRequestNeedsConditionalWrite when the store
// resolves no conditional writer and casOnly reports that the city records
// only by compare-and-swap. It asks casOnly at most once, so a re-decide
// after a lost swap asks nothing new.
type restartWriteRefusal struct {
	m        *Manager
	casOnly  func() bool
	answered bool
	refusal  error
}

// peek returns the refusal when it is known without asking casOnly; answered
// is false when casOnly must still be asked (the store resolves no
// conditional writer and casOnly is set).
func (r *restartWriteRefusal) peek() (answered bool, refusal error) {
	if r.answered {
		return true, r.refusal
	}
	writer, _, err := beads.ResolveConditionalWriter(beads.SessionStore{Store: r.m.store})
	switch {
	case err != nil:
		r.answered, r.refusal = true, err
	case writer != nil || r.casOnly == nil:
		r.answered = true
	}
	return r.answered, r.refusal
}

// ask returns the refusal, asking casOnly when peek cannot answer. A caller
// that writes asks it outside its read-to-write window.
func (r *restartWriteRefusal) ask() error {
	if answered, refusal := r.peek(); answered {
		return refusal
	}
	r.answered = true
	if r.casOnly() {
		r.refusal = ErrRestartRequestNeedsConditionalWrite
	}
	return r.refusal
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
	if !m.runtimeTokenIs(row.SessionName, instanceToken) {
		return false
	}
	runtimeID, err := runtime.MetaValue(m.sp.GetMeta(row.SessionName, "GC_SESSION_ID"))
	if err != nil {
		return false
	}
	runtimeID = strings.TrimSpace(runtimeID)
	return runtimeID == "" || runtimeID == strings.TrimSpace(row.ID)
}

// runtimeProofForWrite is runtimeCarriesToken fenced to the write that
// follows the decision. Once the runtime proves the caller, the row is read
// again and then the runtime's token, the last reads before the write. The
// proof holds only while the row records the caller's token, or still the
// one the proof was made against, and the runtime still carries
// instanceToken. A relaunch moves the row's token at its mint and the
// runtime's at its start, which does not move the row again, so neither the
// compare-and-swap nor, without a conditional writer, anything else would see
// a pane that took its successor's token during the proof.
func (m *Manager) runtimeProofForWrite(row Info, instanceToken string) bool {
	if !m.runtimeCarriesToken(row, instanceToken) {
		return false
	}
	again, err := m.PersistedStore().Get(row.ID)
	if err != nil || again.Closed {
		return false
	}
	switch strings.TrimSpace(again.InstanceToken) {
	case instanceToken:
		return true
	case strings.TrimSpace(row.InstanceToken):
		return m.runtimeTokenIs(row.SessionName, instanceToken)
	}
	return false
}

// runtimeTokenIs reports whether the live runtime named sessionName carries
// instanceToken as its GC_INSTANCE_TOKEN. A read error, a missing runtime or
// a manager without a provider proves nothing.
func (m *Manager) runtimeTokenIs(sessionName, instanceToken string) bool {
	if m.sp == nil || strings.TrimSpace(sessionName) == "" {
		return false
	}
	tok, err := runtime.MetaValue(m.sp.GetMeta(sessionName, runtime.InstanceTokenEnv))
	return err == nil && strings.TrimSpace(tok) == instanceToken
}
