package main

import (
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/session"
)

// identityVerdict is whose runtime sits under a row's name (v5 O2). The zero
// value is identityUnknown, so a verdict never computed holds.
type identityVerdict uint8

const (
	identityUnknown   identityVerdict = iota // unread, unreadable or undecided: hold
	identityCurrent                          // carries the row's non-empty token
	identityForeign                          // another row's session ID, and not the row's token
	identityStaleSelf                        // the row's ID, another token, epoch ≤ generation: rekey (v5 S4)
	identityNewerSelf                        // the row's ID, another token, epoch > generation: census lag, hold
	identityOwnerless                        // no session ID and no token
)

var identityVerdictNames = [...]string{"unknown", "current", "foreign", "stale-self", "newer-self", "ownerless"}

func (v identityVerdict) String() string {
	if int(v) < len(identityVerdictNames) {
		return identityVerdictNames[v]
	}
	return "identity(" + strconv.Itoa(int(v)) + ")"
}

// compareIdentity is v5 O2's comparator: the only way v2 decides whose
// runtime sits under row's name. It is total, and an empty token never
// matches. A token equal to the row's is Current whatever the session ID
// (legacy adoption never stamped GC_SESSION_ID), so a legacy-adopted runtime
// is never Foreign (v5.2 M2). Row and runtime fields are compared trimmed.
//
// Liveness first: callers compare only a runtime O1 reads present, since a
// readable token on a gone runtime proves nothing.
//
// Ownerless needs a backend that seeds identity before it lists the name
// (v5 X3): identityReadable leaves only (tmux, acp, and subprocess since
// LL3); any other leaf reads not Known, so Unknown. An exiting runtime may
// still read Ownerless briefly, because subprocess clears its sidecar before
// it stops listing the name and acp right after terminating. That is benign:
// the runtime is going.
func compareIdentity(row session.Info, rt runtimeIdentity) identityVerdict {
	if !rt.Known {
		return identityUnknown
	}
	id, token := strings.TrimSpace(rt.SessionID), strings.TrimSpace(rt.Token)
	switch {
	case token != "" && token == strings.TrimSpace(row.InstanceToken):
		return identityCurrent
	case id == "" && token == "":
		return identityOwnerless
	case id == "":
		return identityUnknown
	case id != strings.TrimSpace(row.ID):
		return identityForeign
	case token == "":
		return identityUnknown
	}
	epoch, generation, ok := identityEpochs(row, rt)
	switch {
	case !ok:
		return identityUnknown
	case epoch > generation:
		return identityNewerSelf
	}
	return identityStaleSelf
}

// ownRuntime is O2's own-runtime test (legacy's table at
// session_lifecycle_parallel.go:3343-3352): v is Current, or StaleSelf with
// the runtime's epoch equal to the row's generation. The rollback, the
// lost-commit cleanup and S7's StaleSelf exit read it.
func ownRuntime(v identityVerdict, row session.Info, rt runtimeIdentity) bool {
	if v == identityCurrent {
		return true
	}
	epoch, generation, ok := identityEpochs(row, rt)
	return v == identityStaleSelf && ok && epoch == generation
}

// adoptableIdentity is named AdoptLive's rule (v5 O2, LL5; the C4c1 review
// ruling): a clean read with no session ID, which is Ownerless or the
// legacy-adopted shape (a token and no GC_SESSION_ID, Unknown to every row
// but its own), whose token the adoption captures. It never adopts Foreign,
// an unread identity, or any other Unknown. The effect re-confirms presence
// after its fresh read.
func adoptableIdentity(rt runtimeIdentity) bool {
	return rt.Known && strings.TrimSpace(rt.SessionID) == ""
}

// identityEpochs parses the runtime's epoch and the row's generation.
func identityEpochs(row session.Info, rt runtimeIdentity) (int, int, bool) {
	epoch, err := strconv.Atoi(strings.TrimSpace(rt.Epoch))
	generation, genErr := strconv.Atoi(strings.TrimSpace(row.Generation))
	return epoch, generation, err == nil && genErr == nil
}

// ownsName is C8.2(a)'s bead-scoped name, legacy's predicate verbatim
// (staleAsyncStartRuntimeAttribution): a pool-managed row whose stored
// session name embeds its bead ID, and name is that stored name. The
// pending-create rollback reads it as the own-runtime attribution, with L3
// waived (v5 C3, F2). The stop verb (v5 D3) reads it as own for the identity
// leg (L2) only, never Foreign, with L3 kept.
func ownsName(row session.Info, name string) bool {
	return isPoolManagedSessionInfo(row) && infoOwnsPoolSessionName(row) &&
		strings.TrimSpace(row.SessionNameMetadata) == strings.TrimSpace(name)
}
