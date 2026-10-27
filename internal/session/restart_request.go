package session

import (
	"errors"
	"strings"
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
// re-reading and re-deciding on a lost swap. Without one (a legacy city) the
// patch is written right after the deciding read, as UpdateMetadataFenced
// does; a store that must fence and cannot refuses. A row that already
// carries both keys is not written again. The caller's generation is not
// compared: every start mints a new token, and a rekey moves only the token.
//
// instanceToken is a capability: it is never logged or printed.
func (m *Manager) RequestFreshRestartFenced(id, instanceToken string) error {
	instanceToken = strings.TrimSpace(instanceToken)
	if instanceToken == "" {
		return ErrRestartRequestUnfenced
	}
	return withSessionMutationLock(id, func() error {
		var refusal error
		due := false
		written, err := m.PersistedStore().UpdateMetadataFenced(id, restartRequestAttempts, func(info Info, _ PersistedResponse) MetadataPatch {
			patch, decideErr := decideRestartRequest(info, instanceToken)
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

// decideRestartRequest is the fence's decision on one fresh read of the row:
// a closed row refuses with ErrSessionClosed, a row whose instance_token is
// not instanceToken refuses with ErrStaleRestartRequest, a row that already
// carries both keys needs no patch, and any other row gets both keys.
func decideRestartRequest(info Info, instanceToken string) (MetadataPatch, error) {
	if info.Closed {
		return nil, ErrSessionClosed
	}
	if strings.TrimSpace(info.InstanceToken) != instanceToken {
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
