// Package events provides tier-0 observability for Gas City.
//
// Events are infrastructure records of what happened (agent lifecycle,
// bead operations, controller state). The recorder writes JSON lines to
// .gc/events.jsonl; the reader scans them back. Recording is best-effort:
// errors are logged to stderr but never returned to callers.
//
// Agent observation data (messages, tool calls, thinking) is read directly
// from provider session logs via the sessionlog package, not the event bus.
package events

import (
	"context"
	"encoding/json"
	"time"
)

// Event type constants. Only types we actually emit today.
const (
	SessionWoke             = "session.woke"
	SessionStopped          = "session.stopped"
	SessionCrashed          = "session.crashed"
	BeadCreated             = "bead.created"
	BeadClosed              = "bead.closed"
	BeadDeleted             = "bead.deleted"
	BeadUpdated             = "bead.updated"
	BeadWorktreeReaped      = "bead.worktree.reaped"
	BeadWorktreeReapSkipped = "bead.worktree.reap_skipped"
	// BeadClaimRejected fires when a worker attempts to claim a work bead that
	// is already live-claimed by a different worker — the claim is rejected as
	// an idempotent no-op rather than fanning out a second concurrent claim.
	// Turns the otherwise-silent lost-claim race (RCA gc-typpc: one bead, four
	// concurrent polecat claims) into an observable signal. ADR-0009.
	BeadClaimRejected  = "bead.claim_rejected"
	MailSent           = "mail.sent"
	MailRead           = "mail.read"
	MailArchived       = "mail.archived"
	MailMarkedRead     = "mail.marked_read"
	MailMarkedUnread   = "mail.marked_unread"
	MailReplied        = "mail.replied"
	MailDeleted        = "mail.deleted"
	SessionDraining    = "session.draining"
	SessionUndrained   = "session.undrained"
	SessionQuarantined = "session.quarantined"
	SessionIdleKilled  = "session.idle_killed"
	// SessionMaxAgeKilled fires when the controller preemptively restarts a
	// long-running session because its wall-clock age exceeded the agent's
	// max_session_age threshold. Motivating case: provider SDKs that cache
	// credentials at session start and wedge when the cached token expires.
	SessionMaxAgeKilled = "session.max_age_killed"
	SessionSuspended    = "session.suspended"
	SessionUpdated      = "session.updated"
	// SessionDrainAckedWithAssignedWork fires when a session acknowledges
	// drain (via `gc runtime drain-ack`) while still holding the assignee
	// on an open or in-progress work bead. Distinguishes a worker that
	// exited mid-task (e.g., per-turn cap, crash) from a worker that
	// performed a clean phase handoff (the latter null the bead's
	// assignee before drain-acking). The reconciler emits this as a
	// mechanism-only signal; pack-level subscribers own the recovery
	// policy (commit-and-push, clear-assignee-and-respawn, or escalate).
	// See gastownhall/gascity#2293.
	SessionDrainAckedWithAssignedWork = "session.drain_acked_with_assigned_work"
	// SessionStranded fires when a pool slot retains an in-progress work
	// bead after its runtime has exited — i.e., the worker process is
	// gone but the bead's assignee/state still references it. Surfaces
	// the reconciler-detected leak so pack-level subscribers can decide
	// whether to clear-assignee-and-respawn or escalate.
	SessionStranded = "session.stranded"
	// SessionResetStalled fires when a session reset was committed but
	// the follow-up wake remains pending past the configured startup
	// timeout. Operators use the typed payload to correlate the stuck
	// session, template, reset timestamp, and elapsed wait.
	SessionResetStalled = "session.reset_stalled"
	// SessionWorkQueryFailed fires when the current managed session's
	// work-discovery query subprocess is killed by an external signal or
	// aborted by the runner-imposed timeout before producing output.
	// Emission requires the current session ID so the lifecycle payload
	// remains correlated; the companion reconciler handler is tracked in
	// #1497.
	SessionWorkQueryFailed = "session.work_query_failed"
	// SessionColdStartTimeout fires when a pool session's first runtime spawn
	// (a pending create) exceeds the start deadline and is rolled back. It is
	// per-session: it fires whenever a fresh spawn times out, including a warm
	// pool scale-up adding capacity — not only when the whole pool was at zero.
	// Emitted by the session reconciler's start-result commit path; the
	// envelope's Subject carries the session name.
	SessionColdStartTimeout = "session.cold_start_timeout"
	ConvoyCreated           = "convoy.created"
	ConvoyClosed            = "convoy.closed"
	ControllerStarted       = "controller.started"
	ControllerStopped       = "controller.stopped"
	// SupervisorStarted fires once per supervisor startup, after the
	// instance lock is acquired. Its payload classifies how the previous
	// supervisor instance exited (clean, crash, or unknown), derived from
	// the clean-shutdown handoff token the STOPPING path leaves behind,
	// so flap alerts can distinguish a crash loop from deploy restarts.
	SupervisorStarted = "supervisor.started"
	// SupervisorShutdownRequested fires when the supervisor's main loop
	// observes a shutdown trigger (signal or socket stop) and is about to
	// cancel the supervisor context. Carries attribution so operators can
	// answer "why did the supervisor exit" without scraping macOS/launchd
	// logs.
	SupervisorShutdownRequested = "supervisor.shutdown_requested"
	// SupervisorRequest records one bounded audit entry for a request handled
	// by the machine-wide supervisor API. Payloads omit request bodies and
	// query strings.
	SupervisorRequest = "supervisor.request"
	CitySuspended     = "city.suspended"
	CityResumed       = "city.resumed"
	// Typed async request result events. 5 success types (one per
	// operation, fully typed payload) + 1 shared failure type.
	RequestResultCityCreate     = "request.result.city.create"
	RequestResultCityUnregister = "request.result.city.unregister"
	RequestResultSessionCreate  = "request.result.session.create"
	RequestResultSessionMessage = "request.result.session.message"
	RequestResultSessionSubmit  = "request.result.session.submit"
	RequestFailed               = "request.failed"

	// Non-terminal city lifecycle events recorded in the per-city
	// event log during init/unregister for diagnostics.
	CityCreated                     = "city.created"
	CityUnregisterRequested         = "city.unregister_requested"
	OrderFired                      = "order.fired"
	OrderCompleted                  = "order.completed"
	OrderFailed                     = "order.failed"
	ProviderSwapped                 = "provider.swapped"
	WorkerOperation                 = "worker.operation"
	ProjectIdentityStamped          = "project.identity.stamped"
	SupervisorFSPressureSkippedTick = "supervisor.fs_pressure.skipped_tick"

	// MoleculeResolved fires once at the molecule-autoclose Go close site
	// when a molecule root transitions to closed. It carries the
	// state-transition record (issue, from/to status, close reason) joined
	// to the resolving session, resolved from the root's stamped metadata
	// (gc.session_name / gc.session_id / gc.work_dir). It is additive: the
	// existing bead.closed emission is unchanged. A manual non-molecule
	// `bd close` produces bead.closed but NOT molecule.resolved, so this
	// event attributes molecule-resolution closes only — a root hand-closed
	// directly has no resolving session and degrades to empty session fields.
	MoleculeResolved = "molecule.resolved"

	// External messaging events.
	ExtMsgBound          = "extmsg.bound"
	ExtMsgUnbound        = "extmsg.unbound"
	ExtMsgGroupCreated   = "extmsg.group_created"
	ExtMsgAdapterAdded   = "extmsg.adapter_added"
	ExtMsgAdapterRemoved = "extmsg.adapter_removed"
	ExtMsgInbound        = "extmsg.inbound"
	ExtMsgOutbound       = "extmsg.outbound"

	// ExtMsgOutboundChannelMismatch fires when a session attempts to publish
	// to a conversation that is bound to a different session. The publish is
	// rejected; this event turns that otherwise-silent cross-wire into an
	// observable signal (RCA gc-5aie6: per-PL Slack channel cross-wiring).
	ExtMsgOutboundChannelMismatch = "extmsg.outbound_channel_mismatch"

	// EventsRotated is the forensic anchor written as the first event in
	// a freshly-rotated active log. Its payload carries the prior
	// archive's filename and seq range so log readers can stitch back
	// across rotations.
	EventsRotated = "events.rotated"

	// Postgres credential resolution. Emitted by the bd-env projection
	// path on every successful pgauth resolve. The payload identifies
	// the scope and the resolution tier that supplied the value; it
	// MUST NOT carry the password value (asserted by
	// TestPostgresEventOmitsPassword).
	PostgresCredentialResolved = "pg.credential_resolved"

	// ProviderHealthGateAlert fires once per red episode when the provider-health
	// gate parks respawns for a provider. Carries episode ID, onset time, and
	// session count. One alert per episode; AlertSent is cleared on green so
	// the next episode fires independently. (ADR-0013 A1 M3a)
	ProviderHealthGateAlert = "provider.health_gate_alert"

	// Emergency events are dolt-independent escalation records written to
	// .gc/emergency and mirrored into the city event log.
	EmergencySignaled = "emergency.signaled"
	EmergencyAcked    = "emergency.acked"
)

// KnownEventTypes lists every event-type constant this package defines.
// The SSE projection uses this set (via a test) to verify that every
// event type has a registered payload — a missing registration is a
// programming error that fails CI, not a runtime condition.
var KnownEventTypes = []string{
	SessionWoke, SessionStopped, SessionCrashed,
	SessionDraining, SessionUndrained, SessionQuarantined,
	SessionIdleKilled, SessionMaxAgeKilled, SessionSuspended, SessionUpdated,
	SessionDrainAckedWithAssignedWork,
	SessionStranded,
	SessionResetStalled,
	SessionWorkQueryFailed,
	SessionColdStartTimeout,
	BeadCreated, BeadClosed, BeadDeleted, BeadUpdated,
	BeadWorktreeReaped, BeadWorktreeReapSkipped,
	BeadClaimRejected,
	MailSent, MailRead, MailArchived, MailMarkedRead, MailMarkedUnread,
	MailReplied, MailDeleted,
	ConvoyCreated, ConvoyClosed,
	ControllerStarted, ControllerStopped,
	CitySuspended, CityResumed,
	RequestResultCityCreate, RequestResultCityUnregister,
	RequestResultSessionCreate, RequestResultSessionMessage,
	RequestResultSessionSubmit, RequestFailed,
	CityCreated, CityUnregisterRequested,
	OrderFired, OrderCompleted, OrderFailed,
	ProviderSwapped, WorkerOperation, ProjectIdentityStamped, SupervisorFSPressureSkippedTick,
	MoleculeResolved,
	SupervisorStarted, SupervisorShutdownRequested, SupervisorRequest,
	ExtMsgBound, ExtMsgUnbound, ExtMsgGroupCreated,
	ExtMsgAdapterAdded, ExtMsgAdapterRemoved,
	ExtMsgInbound, ExtMsgOutbound,
	ExtMsgOutboundChannelMismatch,
	EventsRotated,
	PostgresCredentialResolved,
	EmergencySignaled, EmergencyAcked,
	// ProviderHealthGateAlert is intentionally omitted from KnownEventTypes.
	// The event is emitted by the reconciler but its typed SSE payload is not
	// yet registered in internal/api (the payload registration lives in a
	// follow-up that adds the full SSE projection). Until then, subscribers
	// receive it via the custom-event envelope.
}

// Event is a single recorded occurrence in the system.
//
// RunID/SessionID are opaque correlation ids stamped at the record site (run
// root via the bead metadata run-chain; session bead id), used by downstream
// consumers to join an event to its run/session. They are additive and
// omitempty: old records lack them and unmarshal as "". They are NOT derived
// from Payload — the redacted export forwards them as typed primitives, never by
// decoding the free-form payload.
type Event struct {
	Seq       uint64          `json:"seq"`
	Type      string          `json:"type"`
	Ts        time.Time       `json:"ts"`
	Actor     string          `json:"actor"`
	Subject   string          `json:"subject,omitempty"`
	Message   string          `json:"message,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	StepID    string          `json:"step_id,omitempty"`
}

// Recorder records events. Safe for concurrent use. Best-effort.
// This sub-interface is used by callers that only need to write events.
type Recorder interface {
	Record(e Event)
}

// Provider is the full interface for event backends. It embeds Recorder
// for writing and adds reading, querying, and watching. Implementations
// include FileRecorder (built-in JSONL file) and exec (user-supplied
// script via fork/exec).
type Provider interface {
	Recorder

	// List returns events matching the filter.
	List(filter Filter) ([]Event, error)

	// LatestSeq returns the highest sequence number, or 0 if empty.
	LatestSeq() (uint64, error)

	// Watch returns a Watcher that yields events with Seq > afterSeq.
	// The watcher blocks on Next() until an event arrives or ctx is
	// canceled. Callers must call Close() when done.
	Watch(ctx context.Context, afterSeq uint64) (Watcher, error)

	// Close releases any resources held by the provider.
	Close() error
}

// TailProvider is an optional extension for providers that can return the
// trailing matching events without scanning or materializing the whole history.
type TailProvider interface {
	ListTail(filter Filter, limit int) ([]Event, error)
}

// Watcher yields events one at a time. Created by [Provider.Watch].
// Callers must call Close() when done watching.
type Watcher interface {
	// Next blocks until the next event is available, the context is
	// canceled, or the watcher is closed. Returns the event or an error.
	// Implementations must unblock any in-flight Next call when Close
	// is called or the parent context is canceled.
	Next() (Event, error)

	// Close stops the watcher, unblocks any pending Next call, and
	// releases resources. Safe to call concurrently with Next.
	Close() error
}

// Discard silently drops all events.
var Discard Recorder = discardRecorder{}

type discardRecorder struct{}

func (discardRecorder) Record(Event) {}
