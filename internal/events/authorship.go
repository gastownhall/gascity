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
// The rule that assigns a type is mechanical: Ledger for the bead mutation
// types the controller derives from the ledger itself; Controller when every
// emitter runs in the controller or supervisor process (the reconciler, order
// dispatch, the API server, supervisor loops); Worker when any session or CLI
// process emits it, wherever it also runs. Worker types carry what the writer
// asserts: until bd principals (BD-8) a box's actor, subject and session are
// attributed by assertion (D-TRUST).
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

	// Worker: emitted by session and CLI processes (the claim hook, mail,
	// handoff, runtime drain, the control-dispatcher session, worker
	// operations, a store's composition root).
	SessionStopped:                 AuthorWorker,
	SessionDraining:                AuthorWorker,
	SessionUndrained:               AuthorWorker,
	SessionWorkQueryFailed:         AuthorWorker,
	SessionDrainFenceUnavailable:   AuthorWorker,
	SessionDemandClaimDivergence:   AuthorWorker,
	BeadClaimRejected:              AuthorWorker,
	BeadClaimReleased:              AuthorWorker,
	HookClaimReclaimedStale:        AuthorWorker,
	ExecutionStepStarted:           AuthorWorker,
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
	SessionStrandedClaims:                 AuthorController,
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
	// (executionevent's CompletedFactIndex), so a copy from a box is a forgery.
	BeadWorktreeReaped:       AuthorController,
	BeadWorktreeReapSkipped:  AuthorController,
	BeadDeadAssigneeReopened: AuthorController,
	BeadsBlockedRecomputed:   AuthorController,
	ExecutionWorkAssociated:  AuthorController,
	ExecutionRunAnchored:     AuthorController,
	ExecutionStepDefined:     AuthorController,
	ExecutionStepCompleted:   AuthorController,
	ExecutionStepStalled:     AuthorController,

	// Controller: the controller, the city, orders, async requests and the
	// API server's integrations.
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
	RequestResultCityCreate:       AuthorController,
	RequestResultCityUnregister:   AuthorController,
	RequestResultSessionCreate:    AuthorController,
	RequestResultSessionMessage:   AuthorController,
	RequestResultSessionSubmit:    AuthorController,
	RequestResultRigCreate:        AuthorController,
	RequestFailed:                 AuthorController,
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

	// Controller: the supervisor, the event log and storage.
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
