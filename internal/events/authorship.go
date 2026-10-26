package events

import (
	"fmt"
	"sync"
)

// Authorship is the class of process whose own observation an event type
// records: the one class that may author it. It decides what a worker site (a
// box) may record: a box records Worker types only, and the controller's
// projector refuses the rest (engdocs/design/des-box-daemon-v0.md D13, D14;
// remote-worker-coherence-and-door.md §5).
//
// The rule that assigns a type, in order:
//
//  1. A decision the design records binds, whatever the rule below would
//     say: DES-BX D13 and CD §5 (remote-worker-coherence-and-door.md). Those
//     entries are pinned by TestAuthorshipKeepsTheRecordedDecisions.
//  2. Ledger for the bead mutation types the controller derives from the
//     ledger itself.
//  3. Worker when any emitter can run at a worker site: in a session, or in
//     an agent-facing command a session runs (gc hook, gc mail, gc session,
//     gc sling, gc formula, gc convoy control --serve, gc event emit).
//  4. Controller otherwise: every emitter runs in the controller or the
//     supervisor (the reconciler, order dispatch, the API server, supervisor
//     loops) or is a city-host admin command run beside them (gc start, gc
//     init, gc rig add, gc suspend and gc resume, gc storage migrate, gc
//     dolt-state, the bd provider script), never at a worker site.
//
// The emitters are found by following each constant through its references,
// helpers included, not only through events.Event literals. Entries below
// whose emitters include an admin command or contradict rule 3 under a
// recorded decision say so.
//
// Worker types carry what the writer asserts: until bd principals (BD-8) a
// box's actor, subject and session are attributed by assertion (D-TRUST).
// One Worker type is not recorded as asserted: execution.step_started is
// accepted as a subject-only intent, and the projector re-derives it from a
// live step read (D13, CD §5) instead of recording the box's copy.
//
// A worker site's recorder (D13's factRecorder) drops every Ledger and
// Controller type at the source, and its RecordAck must return a non-nil
// error for each event it does not write, a dropped type included, as
// events.AckRecorder requires: a caller that marks durable state on a nil ack
// (executionevent.EmitCurrent's step_defined marker) would otherwise lose the
// fact for good.
type Authorship string

// The authorship classes.
const (
	// AuthorWorker types may be recorded by any gc process, a box's
	// included. An unregistered (custom pack) type is Worker.
	AuthorWorker Authorship = "worker"
	// AuthorController types are recorded only by the controller or the
	// supervisor, from state only they hold. A box copy would be a forgery
	// or a duplicate.
	AuthorController Authorship = "controller"
	// AuthorLedger types are derived once, by the controller, from the bead
	// ledger's own mutations (its cache or journal feed). A writer never
	// records its own copy of them.
	AuthorLedger Authorship = "ledger"
)

// Valid reports whether a is one of the three classes.
func (a Authorship) Valid() bool {
	switch a {
	case AuthorWorker, AuthorController, AuthorLedger:
		return true
	}
	return false
}

var (
	authorshipMu       sync.RWMutex
	authorshipRegistry = map[string]Authorship{}
)

// RegisterAuthorship records eventType's authorship. It is the authorship
// sibling of RegisterPayload: every event type this package defines is
// registered by the table below, and a package that defines event types of
// its own registers them beside their declaration, from init. Like
// RegisterPayload it panics on a programming error (an empty type, an invalid
// class, or a second registration with another class); registering the same
// class again is a no-op.
func RegisterAuthorship(eventType string, a Authorship) {
	if eventType == "" {
		panic("events: RegisterAuthorship with an empty event type")
	}
	if !a.Valid() {
		panic(fmt.Sprintf("events: RegisterAuthorship(%q) with invalid authorship %q", eventType, a))
	}
	authorshipMu.Lock()
	defer authorshipMu.Unlock()
	if existing, ok := authorshipRegistry[eventType]; ok {
		if existing != a {
			panic(fmt.Sprintf("events: authorship of %q already registered as %s, cannot re-register as %s", eventType, existing, a))
		}
		return
	}
	authorshipRegistry[eventType] = a
}

// RegisteredAuthorship returns eventType's explicit registration, if any.
func RegisteredAuthorship(eventType string) (Authorship, bool) {
	authorshipMu.RLock()
	defer authorshipMu.RUnlock()
	a, ok := authorshipRegistry[eventType]
	return a, ok
}

// AuthorshipOf returns eventType's authorship: its registration, or
// AuthorWorker for a type nobody registered, which is every custom pack type.
func AuthorshipOf(eventType string) Authorship {
	if a, ok := RegisteredAuthorship(eventType); ok {
		return a
	}
	return AuthorWorker
}

// builtinAuthorship is the authorship of every event type this package
// declares. TestEveryDeclaredEventTypeHasAnAuthorship fails until a new
// constant has an entry, so a new type always forces the decision.
var builtinAuthorship = map[string]Authorship{
	// Ledger: derived from the ledger's own mutations.
	BeadCreated: AuthorLedger,
	BeadUpdated: AuthorLedger,
	BeadClosed:  AuthorLedger,
	BeadDeleted: AuthorLedger,
	// The controller's cache-notification bridge derives both from journal
	// rows' post-images (DES-O1 D18), never the writer.
	OpChanged:    AuthorLedger,
	FactRecorded: AuthorLedger,

	// Worker: emitted by session and agent-facing command processes (the
	// claim hook, mail, handoff, runtime drain, the control-dispatcher
	// session, worker operations, a store's composition root). Worker by
	// rule 3, and session.draining, undrained and stopped by D13 too.
	SessionStopped:                 AuthorWorker,
	SessionDraining:                AuthorWorker,
	SessionUndrained:               AuthorWorker,
	SessionWorkQueryFailed:         AuthorWorker,
	SessionDrainFenceUnavailable:   AuthorWorker,
	WorkStartedUnrecorded:          AuthorWorker,
	SessionDemandClaimDivergence:   AuthorWorker,
	BeadClaimRejected:              AuthorWorker,
	BeadClaimReleased:              AuthorWorker,
	BeadClaimFailed:                AuthorWorker,
	HookClaimReclaimedStale:        AuthorWorker,
	ExecutionStepStarted:           AuthorWorker, // re-derived by the projector from a live step read (D13, CD §5)
	ExecutionClaimWindowExpired:    AuthorWorker,
	MailSent:                       AuthorWorker,
	MailRead:                       AuthorWorker,
	MailArchived:                   AuthorWorker,
	MailMarkedRead:                 AuthorWorker,
	MailMarkedUnread:               AuthorWorker,
	MailReplied:                    AuthorWorker,
	MailDeleted:                    AuthorWorker,
	ConvoyCreated:                  AuthorWorker,
	ConvoyClosed:                   AuthorWorker,
	ControlStalled:                 AuthorWorker,
	ControlRootSettleFailed:        AuthorWorker,
	OrderFailed:                    AuthorWorker,
	WorkerOperation:                AuthorWorker,
	MoleculeResolved:               AuthorWorker,
	BackendCredentialResolved:      AuthorWorker,
	BeadsConditionalWritesDegraded: AuthorWorker,
	// Worker by D13 (rule 1, the BX-4a decision), as rule 3 says too:
	// executionevent.EmitCurrent records the execution topology, and gc
	// sling, gc formula cook and the control-dispatcher session (gc convoy
	// control --serve) call it through the city recorder at worker sites. A
	// box that dropped them would break the runs view, and a forged copy only
	// annotates; execution.step_completed, which mints a completion, stays
	// Controller below.
	ExecutionWorkAssociated: AuthorWorker,
	ExecutionRunAnchored:    AuthorWorker,
	ExecutionStepDefined:    AuthorWorker,
	// gc session close strands a closed seat's unserved claims and records
	// it (cmd/gc work_release.go, strandedClaimAlerts.observe), as the
	// controller's detector does; a box's copy would otherwise be dropped
	// and wait for the detector's next pass.
	SessionStrandedClaims: AuthorWorker,

	// Controller: the reconciler's session lifecycle.
	SessionWoke:                           AuthorController,
	SessionCrashed:                        AuthorController,
	SessionQuarantined:                    AuthorController,
	SessionIdleKilled:                     AuthorController,
	SessionMaxAgeKilled:                   AuthorController,
	SessionSuspended:                      AuthorController,
	SessionUpdated:                        AuthorController,
	SessionDrainAckedWithAssignedWork:     AuthorController,
	SessionDrainStopEscalated:             AuthorController,
	SessionStranded:                       AuthorController,
	SessionPoolSlotRetiredAtDrainDeadline: AuthorController,
	SessionUnknownState:                   AuthorController,
	SessionWakeRefused:                    AuthorController,
	SessionResetStalled:                   AuthorController,
	SessionColdStartTimeout:               AuthorController,
	SessionConversationReset:              AuthorController,
	SessionPending:                        AuthorController,
	SessionPendingCleared:                 AuthorController,
	ReconcilerAlert:                       AuthorController,
	ProviderHealthGateAlert:               AuthorController,
	ProviderSwapped:                       AuthorController,

	// Controller: bead repairs and graph execution facts the controller
	// derives. A logged execution.step_completed is read as journal-confirmed
	// (executionevent's CompletedFactIndex.Absorb), so a copy from a box would
	// mint a completion: Controller by D13, against rule 3.
	BeadWorktreeReaped:       AuthorController,
	BeadWorktreeReapSkipped:  AuthorController,
	BeadDeadAssigneeReopened: AuthorController,
	// Also recorded by gc rig add, a city-host admin command (rule 4).
	BeadsBlockedRecomputed: AuthorController,
	// The API server and the controller's op sweeper, both in the
	// controller process; the CLI warns on stderr instead (DES-O0API D13).
	OpsRecordingDegraded:   AuthorController,
	ExecutionStepCompleted: AuthorController,
	ExecutionStepStalled:   AuthorController,
	// The work ladder's facts: the v2 lane step records them (rule 4).
	WorkNudged:            AuthorController,
	WorkNudgeFailed:       AuthorController,
	WorkRecycleRequested:  AuthorController,
	WorkReleased:          AuthorController,
	WorkReleaseUnfinished: AuthorController,
	WorkEscalated:         AuthorController,
	BeadClaimLoopHeld:     AuthorController, // the controller's claim-loop guard (cmd/gc/claim_loop_guard.go)

	// Controller: the controller, the city, orders, async requests and the
	// API server's integrations. city.* by D13; gc suspend, gc resume and gc
	// init record them too, as city-host admin commands (rule 4).
	ControllerStarted:             AuthorController,
	ControllerStopped:             AuthorController,
	ControlDispatcherScopeGap:     AuthorController,
	CitySuspended:                 AuthorController,
	CityResumed:                   AuthorController,
	CityCreated:                   AuthorController,
	CityUnregisterRequested:       AuthorController,
	OrderFired:                    AuthorController,
	OrderCompleted:                AuthorController,
	OrderSuppressed:               AuthorController,
	OrderSkipped:                  AuthorController,
	OrderInapplicable:             AuthorController,
	RequestResultCityCreate:       AuthorController,
	RequestResultCityUnregister:   AuthorController,
	RequestResultSessionCreate:    AuthorController,
	RequestResultSessionMessage:   AuthorController,
	RequestResultSessionSubmit:    AuthorController,
	RequestResultRigCreate:        AuthorController,
	RequestFailed:                 AuthorController,
	RequestAccepted:               AuthorController,
	RigProvisionProgress:          AuthorController,
	ExtMsgBound:                   AuthorController,
	ExtMsgUnbound:                 AuthorController,
	ExtMsgGroupCreated:            AuthorController,
	ExtMsgAdapterAdded:            AuthorController,
	ExtMsgAdapterRemoved:          AuthorController,
	ExtMsgInbound:                 AuthorController,
	ExtMsgOutbound:                AuthorController,
	ExtMsgOutboundChannelMismatch: AuthorController,
	WebhookReceived:               AuthorController,
	WebhookRejected:               AuthorController,
	EmergencySignaled:             AuthorController,
	EmergencyAcked:                AuthorController,

	// Controller: the supervisor, the event log and storage. storage.* by
	// D13, also recorded by gc storage migrate; project.identity.stamped
	// also by gc dolt-state ensure-project-id, which the bd provider script
	// runs on the city host (both rule 4).
	SupervisorStarted:               AuthorController,
	SupervisorShutdownRequested:     AuthorController,
	SupervisorRequest:               AuthorController,
	SupervisorFSPressureSkippedTick: AuthorController,
	EventsRotated:                   AuthorController,
	StoreMaintenanceDone:            AuthorController,
	StoreMaintenanceFailed:          AuthorController,
	StoreDiskWarn:                   AuthorController,
	StoreDiskCritical:               AuthorController,
	ProjectIdentityStamped:          AuthorController,
	StorageBindingConverged:         AuthorController,
	StorageBindingGenesis:           AuthorController,
	StorageBindingUnconverged:       AuthorController,
	StorageBindingUncheckable:       AuthorController,
	StorageBindingNotConfigured:     AuthorController,
}

func init() {
	for eventType, a := range builtinAuthorship {
		RegisterAuthorship(eventType, a)
	}
}
