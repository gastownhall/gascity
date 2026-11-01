package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/session"
)

// poolTriggerBindWorkDir is the work dir bindPoolSessionTriggerBead binds the
// pool seat info to for request: the request's verified work dir
// (verifiedPoolTriggerWorkDir), except when a running seat is reused for work
// it does not own, which keeps the directory its row records.
//
// Legacy realization binds a seat to work it does not own in two ways: it
// reuses an idle seat for anonymous new demand (a request with no
// SessionBeadID), and allocateScaleDemandToConcrete rebinds a seat's own
// new-tier request (a seat in its post-create protection window, or one in
// flight) to other demand once its trigger is no longer demanded. When the
// seat's runtime is running, nothing restarts it: the work dir is not a
// restart input (runtime.ConfigFingerprint), and the warm-bind nudge asks the
// running process to claim the work where it is (deliverWarmBindClaimNudge).
// Moving the row's work dir would make the row name a directory the process
// never ran in, and `gc hook --claim` reads the row's directory as the seat's
// own (hookClaimPoolSeatWorkDir): a seat inside one bead's verified worktree
// would then claim work bound to another worktree, or work bound to none.
// With the recorded directory kept, the claim leaves that work, the seat
// drains, and the work gets a session where the controller places it. The v2
// allocator binds no live row at all (selectPoolRow).
//
// A resume of work assigned to the seat (poolRequestResumesAssignedWorkInfo)
// keeps its existing rule in computePoolTriggerBindingPatch: a fresh-mode seat
// reassigned while alive is cycled into the new work's directory, so its row
// must move. The request's evidence is still verified first, so unusable
// evidence skips the item as before. A row that records no directory, a
// request for the directory the row already records, and a seat whose runtime
// is not running bind as before.
func poolTriggerBindWorkDir(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, info session.Info, request SessionRequest) (string, error) {
	workDir, err := verifiedPoolTriggerWorkDir(bp, cfgAgent, qualifiedName, request)
	if err != nil || workDir == "" || bp == nil || poolRequestResumesAssignedWorkInfo(request, bp.assignedWorkBeads, info) {
		return workDir, err
	}
	recorded := preparedStartSessionWorkDir(info)
	if recorded == "" || recorded == workDir {
		return workDir, nil
	}
	if pathutil.SamePath(resolveWorkDirAgainstCity(bp.cityPath, recorded), workDir) || !poolSeatRuntimeRunning(bp, info) {
		return workDir, nil
	}
	return recorded, nil
}

// poolSeatRuntimeRunning reports whether the runtime of the pool seat info is
// running, asked of the build's provider as legacy's create path asks it
// (poolCreateViewOf). No provider, or a row with no runtime name, is not
// running.
func poolSeatRuntimeRunning(bp *agentBuildParams, info session.Info) bool {
	if bp == nil || bp.sp == nil {
		return false
	}
	name := strings.TrimSpace(info.SessionNameMetadata)
	if name == "" {
		return false
	}
	return bp.sp.IsRunning(name)
}
