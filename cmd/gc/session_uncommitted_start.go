package main

import (
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ownLaunchCollisionOutcome classifies a start that had to launch its own
// runtime (preparedStart.ownLaunchRequired) and found the name held by a live
// runtime (#6417). Only a runtime carrying this attempt's instance token is
// this start's, and the start converges on it as any collision does. Any other
// holder never got this start's prompt: the start defers, committing nothing
// and stopping nothing, with the collision error annotated for the lifecycle
// log (unattributed). A holder stamped with this session's ID under another
// token is another incarnation of the session (a launch by a non-controller
// start); every other holder, an unreadable one included, is unattributed.
func ownLaunchCollisionOutcome(err error, info sessionpkg.Info, identity pendingCreateIdentity) (outcome TraceOutcomeCode, unattributed error) {
	switch {
	case identity.token == runtimeTokenMatch:
		return TraceOutcomeSessionExists, nil
	case identity.id == runtimeTokenMatch && identity.token == runtimeTokenMismatch:
		return TraceOutcomeDeferred, liveIncarnationStartError(err, info, identity)
	default:
		return TraceOutcomeDeferred, unattributedStartError(err, identity)
	}
}
