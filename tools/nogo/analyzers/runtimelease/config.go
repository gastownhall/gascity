package runtimelease

const (
	gascity = "github.com/gastownhall/gascity/"
	cmdGC   = gascity + "cmd/gc"
	session = gascity + "internal/session"
	worker  = gascity + "internal/worker"
	api     = gascity + "internal/api"
	doctor  = gascity + "internal/doctor"
)

// Analyzer is the lint as nogo runs it over the repository.
var Analyzer = New(Config{
	Packages:   []string{cmdGC, session, worker, api, doctor},
	RuntimePkg: gascity + "internal/runtime",
	// The v2 effect and step files run under runTx's lockRuntimeName and have
	// their own lint (reconcile_effects_lint_test.go).
	SkipFiles: []string{"reconcile_effect", "reconcile_steps"},
	CityHelpers: map[string]int{
		"workerKillSessionTargetWithConfig":                 0,
		"controllerKillSessionRow":                          0,
		"workerKillSessionTargetCtx":                        2,
		"workerHandleForSessionWithConfig":                  0,
		"workerHandleForSessionWithStaleKeyDetectionWaiter": 0,
		"workerHandleForSessionTargetWithConfig":            0,
		"workerFactoryWithConfig":                           0,
		"verifiedStop":                                      0,
		"advanceSessionDrainsWithSessionsTraced":            0,
		"stopRuntimeBeforeSessionBeadMutation":              0,
		"stopRuntimeBeforeSessionBeadMutationInfo":          0,
		"cycleAliveSessionForFreshReassign":                 0,
		"resetConfiguredNamedSessionForConfigDriftInfo":     0,
		"queueDrainAckAsyncStop":                            0,
		"queueDrainAckForcedTermination":                    0,
		"confirmDrainAckRuntimeDead":                        2,
		"controllerStopLease":                               1,
		"cleanupDeadRuntimeSessionCorpses":                  0,
		"releaseBeadScopedPoolRuntimeLeased":                0,
		"reapStaleSessionBeads":                             0,
		"autoSuspendChatSessions":                           0,
	},
	Sweep:   session + ".ActorSweep",
	Allowed: allowed,
})

// allowed are the functions that may call a runtime verb directly, with the
// lease each runs under. Keep each reason
// true: a function whose lease is its caller's names its callers.
var allowed = map[string]Allowance{
	// cmd/gc: the controller and the CLI.
	cmdGC + ":gracefulStopAllWithForceSignal": {Reason: "the city stop: stops every runtime (the stop-sweep allowlist)"},
	cmdGC + ":stopProviderSwapRuntimes":       {Reason: "a provider swap: stops the old backend's runtimes after waiting out launched starts (the allowlist)"},
	cmdGC + ":startPreparedStartCandidate":    {Reason: "the legacy start, under the candidate's lease", Callers: []string{"runPreparedStartCandidate"}},
	cmdGC + ":stopStaleAsyncStartRuntime": {
		Reason:  "a stale start's cleanup, identity-checked, under the start's lease",
		Callers: []string{"commitAsyncStartResultWithContext", "executePlannedStartsTraced", "settleUncommittedAsyncStart"},
	},
	cmdGC + ":pendingCreateRuntimeClearedForRollback": {Reason: "an async start's rollback, under the start's lease", Callers: []string{"settleUncommittedAsyncStart"}},
	cmdGC + ":releaseBeadScopedPoolRuntime": {
		Reason:  "a pool runtime's teardown, under the start's lease, or the tick's flock via releaseBeadScopedPoolRuntimeLeased",
		Callers: []string{"commitAsyncStartResultWithContext", "commitStartFailure", "releaseBeadScopedPoolRuntimeLeased"},
	},
	cmdGC + ":cleanDeadRuntimeCorpse":            {Reason: "a dead runtime's cleanup, under the name's flock", Callers: []string{"cleanupDeadRuntimeSessionCorpses"}},
	cmdGC + ":stopStillBoundClosedRuntimeLeased": {Reason: "the closed-row reaper: takes the lease itself, and decides again under it", Callers: []string{"reapRuntimesBoundToClosedBeads", "stopStillBoundClosedRuntime"}},
	cmdGC + ":terminateDrainAckRuntimeByProcessTable": {
		Reason:  "the drain-ack escalation's process kill, under controllerStopLease",
		Callers: []string{"queueDrainAckForcedTermination"},
	},
	cmdGC + ":sweepProcessTableOrphans":           {Reason: "M3's process sweep: kills only roots whose row is closed or absent, or whose incarnation stopped and that started before that stop (its start-time bound, processStartSlack); a later start's roots are younger and spared"},
	cmdGC + ":teardownServerForStop":              {Reason: "the city stop: tears the shared server down after every session stopped (the stop-sweep allowlist)"},
	cmdGC + ":relaunchAgentForLaunchDrift":        {Reason: "a launch-only drift's warm-box relaunch: takes the lease itself (tryRuntimeLease) and relaunches on its Watch"},
	cmdGC + ":attachmentCachingProvider.Relaunch": {Reason: "the attachment-caching wrapper forwarding to its provider; its callers hold the lease"},
	cmdGC + ":statusProvider.Relaunch":            {Reason: "the status wrapper forwarding to its base provider; its callers hold the lease"},
	cmdGC + ":stopFenced":                         {Reason: "v2's fenced stop: its effects run under runTx's lockRuntimeName"},
	cmdGC + ":statusProvider.Start":               {Reason: "the status wrapper forwarding to its base provider; its callers hold the lease"},
	cmdGC + ":statusProvider.Stop":                {Reason: "the status wrapper forwarding to its base provider; its callers hold the lease"},
	// internal/session: the Manager.
	session + ":Manager.ensureRunning": {Reason: "takes the runtime lease (leaseRuntime) before its start, or runs under its caller's"},
	session + ":Manager.retryFreshStartAfterStaleKey": {
		Reason:  "a start's stale-key retry, under the start's lease",
		Callers: []string{"Manager.ensureRunning", "Manager.ensureRunningRuntimeOnly"},
	},
	session + ":Manager.ensureRunningRuntimeOnly": {Reason: "the legacy start's runtime bring-up, under the candidate's lease", Callers: []string{"Manager.StartRuntimeOnly"}},
	session + ":Manager.killExistingOrphans": {
		Reason:  "a start's pre-start orphan kill, under the start's lease (CreateSession's: a residual)",
		Callers: []string{"Manager.createStarted", "Manager.ensureRunning", "Manager.ensureRunningRuntimeOnly", "Manager.retryFreshStartAfterStaleKey"},
	},
	session + ":Manager.createStarted":             {Reason: "CreateSession's first start of a new row (a residual, CONTRACT §12.5)", Callers: []string{"Manager.CreateSession"}},
	session + ":Manager.Suspend":                   {Reason: "a suspend takes the lease (leaseForStop); the city stop's sweep (ActorSweep) takes none"},
	session + ":Manager.tearDownRuntimeForSuspend": {Reason: "a suspend's stop, under its lease", Callers: []string{"Manager.Suspend"}},
	session + ":Manager.CloseDetailed":             {Reason: "takes the lease (leaseForStop)"},
	session + ":Manager.Kill":                      {Reason: "takes the lease (leaseForStop), or runs under its caller's (Actor.Lease)"},
	session + ":Manager.interruptAndSubmitLocked":  {Reason: "takes the lease before it interrupts (leaseForRestartLocked)"},
	// internal/worker: runtime-only handles.
	worker + ":RuntimeHandle.StartResolved":  {Reason: "takes the name's flock (session.LeaseRuntimeName)"},
	worker + ":RuntimeHandle.stopUnderLease": {Reason: "takes the name's flock (session.LeaseRuntimeName)"},
	// internal/api.
	api + ":Server.humaHandleRigRestart": {Reason: "a rig restart: stops every agent of a rig (a sweep; a residual, CONTRACT §12.5)"},
	// internal/doctor: gc doctor --fix.
	doctor + ":ZombieSessionsCheck.Fix": {Reason: "gc doctor --fix: refuses while a controller runs (IsControllerRunning), so no controller start races it; an operator's start of the same name meanwhile is not excluded (a residual, CONTRACT §12.5)"},
	doctor + ":OrphanSessionsCheck.Fix": {Reason: "gc doctor --fix: refuses while a controller runs (IsControllerRunning), and stops only names no configured agent owns; an operator's start of such a name meanwhile is not excluded (a residual, CONTRACT §12.5)"},
}
