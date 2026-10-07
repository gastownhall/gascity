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
// is never Foreign (v5.2 M2).
//
// Liveness first: callers compare only a runtime O1 reads present, since a
// readable token on a gone runtime proves nothing.
//
// Ownerless needs a backend whose identity write is atomic with liveness
// (v5 X3). Identity is read only on identityReadable leaves (tmux, acp, and
// subprocess since LL3), which all are; any other leaf reads not Known, so
// Unknown. A leaf added to identityReadable must be atomic too.
func compareIdentity(row session.Info, rt runtimeIdentity) identityVerdict {
	if !rt.Known {
		return identityUnknown
	}
	switch {
	case rt.Token != "" && rt.Token == strings.TrimSpace(row.InstanceToken):
		return identityCurrent
	case rt.SessionID == "" && rt.Token == "":
		return identityOwnerless
	case rt.SessionID == "":
		return identityUnknown
	case rt.SessionID != strings.TrimSpace(row.ID):
		return identityForeign
	case rt.Token == "":
		return identityUnknown
	}
	epoch, err := strconv.Atoi(rt.Epoch)
	generation, genErr := strconv.Atoi(strings.TrimSpace(row.Generation))
	switch {
	case err != nil || genErr != nil:
		return identityUnknown
	case epoch > generation:
		return identityNewerSelf
	}
	return identityStaleSelf
}

// ownsName reports whether name is a bead-scoped pool name of row (C8.2(a):
// the name embeds the row's bead ID). The stop verb (v5 D3) and the rollback
// count such a name as own for the identity check (leg L2) only, never
// Foreign. It waives nothing else, leg L3 included, and it is not the
// own-runtime exception (v5.3 D3).
func ownsName(row session.Info, name string) bool {
	row.SessionNameMetadata = name
	return infoOwnsPoolSessionName(row)
}
