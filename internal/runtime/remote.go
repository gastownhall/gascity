package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ErrRemoteSession is the common sentinel for a provider-native remote
// session operation failure. Use errors.As with [RemoteSessionError] to read
// the stable classification without parsing provider prose.
var ErrRemoteSession = errors.New("remote session operation failed")

// ErrRemoteCapabilityUnsupported reports that the selected runtime did not
// declare the requested provider-native remote-session operation. Callers must
// fail closed rather than silently substituting a different provider or local
// runtime.
var ErrRemoteCapabilityUnsupported = errors.New("remote session capability is unsupported")

// MaxRemoteTranscriptEvents is the largest event page a caller may request.
// Remote transcripts are read-through provider data; they are never persisted
// as session-bead metadata.
const MaxRemoteTranscriptEvents = 1000

// RemoteSessionOperation names one independently discoverable operation on a
// provider-native coding session. Values are also the RPP v0 handshake tokens.
type RemoteSessionOperation string

// Provider-native remote-session operations.
const (
	RemoteSessionCreate     RemoteSessionOperation = "remote.create"
	RemoteSessionAdopt      RemoteSessionOperation = "remote.adopt"
	RemoteSessionStatus     RemoteSessionOperation = "remote.status"
	RemoteSessionFollowUp   RemoteSessionOperation = "remote.followup"
	RemoteSessionTranscript RemoteSessionOperation = "remote.transcript"
	RemoteSessionCancel     RemoteSessionOperation = "remote.cancel"
	RemoteSessionClose      RemoteSessionOperation = "remote.close"
)

// RemoteSessionCapabilities is the provider-declared remote lifecycle surface.
// Unknown operations remain round-trippable for forward compatibility, while
// callers act only on exact operations they understand.
type RemoteSessionCapabilities struct {
	Operations []RemoteSessionOperation `json:"operations"`
}

// Supports reports whether op was declared exactly.
func (c RemoteSessionCapabilities) Supports(op RemoteSessionOperation) bool {
	for _, candidate := range c.Operations {
		if candidate == op {
			return true
		}
	}
	return false
}

// CanonicalOperations returns a sorted, duplicate-free copy of Operations.
func (c RemoteSessionCapabilities) CanonicalOperations() []RemoteSessionOperation {
	seen := make(map[RemoteSessionOperation]struct{}, len(c.Operations))
	for _, op := range c.Operations {
		if op != "" {
			seen[op] = struct{}{}
		}
	}
	result := make([]RemoteSessionOperation, 0, len(seen))
	for op := range seen {
		result = append(result, op)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// RemoteSessionRef carries provider-authored opaque identifiers. SessionID is
// the durable conversation/agent/task identity; RunID is the current provider
// turn when the provider distinguishes a conversation from its runs. Neither
// value may be parsed or used as a credential.
type RemoteSessionRef struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
}

// Validate checks only structural presence. Provider identifiers are opaque.
func (r RemoteSessionRef) Validate() error {
	if strings.TrimSpace(r.SessionID) == "" {
		return fmt.Errorf("remote session ref: session_id is required")
	}
	return nil
}

// RemoteOwnershipFence is an opaque controller-authored session/work
// generation. Every provider mutation carries it; an adapter must reject a
// token that differs from the token bound during create/adopt. The token is not
// a credential and grants no provider access by itself.
type RemoteOwnershipFence struct {
	Token string `json:"token"`
}

// Validate rejects an unfenced mutation.
func (f RemoteOwnershipFence) Validate() error {
	if strings.TrimSpace(f.Token) == "" {
		return fmt.Errorf("remote ownership fence: token is required")
	}
	return nil
}

// RemoteSource identifies the Git source a native cloud session should open.
// Provider environment IDs and credentials remain adapter configuration.
type RemoteSource struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
}

// Validate rejects transport control characters and URI userinfo. Remote
// source identity may be an HTTPS URL, an SSH scp-style location, or a
// provider-native repository identifier, but credentials never cross RPP.
func (s RemoteSource) Validate() error {
	if err := validateRemoteLocation(s.Repository); err != nil {
		return fmt.Errorf("remote source: repository: %w", err)
	}
	if strings.ContainsAny(s.Ref, "\r\n\x00") {
		return fmt.Errorf("remote source: ref contains a control character")
	}
	return nil
}

func validateRemoteLocation(value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("contains a control character")
	}
	if !strings.Contains(value, "://") {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("invalid URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("URL userinfo is forbidden")
	}
	return nil
}

// RemoteCreateRequest creates a provider-native session. RequestID is an
// idempotency key. Existing asks the adapter to adopt the same remote identity
// after controller restart instead of creating a second provider task.
type RemoteCreateRequest struct {
	RequestID string               `json:"request_id"`
	Fence     RemoteOwnershipFence `json:"fence"`
	Prompt    []ContentBlock       `json:"prompt"`
	Source    RemoteSource         `json:"source,omitempty"`
	Existing  *RemoteSessionRef    `json:"existing,omitempty"`
}

// Validate checks the create/adopt safety invariants.
func (r RemoteCreateRequest) Validate() error {
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("remote create request: request_id is required")
	}
	if err := r.Fence.Validate(); err != nil {
		return fmt.Errorf("remote create request: %w", err)
	}
	if len(r.Prompt) == 0 {
		return fmt.Errorf("remote create request: prompt is required")
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("remote create request: %w", err)
	}
	if r.Existing != nil {
		if err := r.Existing.Validate(); err != nil {
			return fmt.Errorf("remote create request: existing: %w", err)
		}
	}
	return nil
}

// RemoteAdoptRequest rebinds a previously persisted opaque remote identity to
// the current ownership generation without creating provider work.
type RemoteAdoptRequest struct {
	Ref   RemoteSessionRef     `json:"ref"`
	Fence RemoteOwnershipFence `json:"fence"`
}

// Validate checks the adopt request.
func (r RemoteAdoptRequest) Validate() error {
	if err := r.Ref.Validate(); err != nil {
		return fmt.Errorf("remote adopt request: %w", err)
	}
	if err := r.Fence.Validate(); err != nil {
		return fmt.Errorf("remote adopt request: %w", err)
	}
	return nil
}

// RemoteFollowUpRequest appends one idempotent provider turn to a remote
// session under an ownership fence.
type RemoteFollowUpRequest struct {
	RequestID string               `json:"request_id"`
	Ref       RemoteSessionRef     `json:"ref"`
	Fence     RemoteOwnershipFence `json:"fence"`
	Content   []ContentBlock       `json:"content"`
}

// Validate checks the follow-up request.
func (r RemoteFollowUpRequest) Validate() error {
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("remote follow-up request: request_id is required")
	}
	if err := r.Ref.Validate(); err != nil {
		return fmt.Errorf("remote follow-up request: %w", err)
	}
	if err := r.Fence.Validate(); err != nil {
		return fmt.Errorf("remote follow-up request: %w", err)
	}
	if len(r.Content) == 0 {
		return fmt.Errorf("remote follow-up request: content is required")
	}
	return nil
}

// RemoteMutationRequest is shared by idempotent cancel and close operations.
type RemoteMutationRequest struct {
	RequestID string               `json:"request_id"`
	Ref       RemoteSessionRef     `json:"ref"`
	Fence     RemoteOwnershipFence `json:"fence"`
}

// Validate checks the mutating request.
func (r RemoteMutationRequest) Validate() error {
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("remote mutation request: request_id is required")
	}
	if err := r.Ref.Validate(); err != nil {
		return fmt.Errorf("remote mutation request: %w", err)
	}
	if err := r.Fence.Validate(); err != nil {
		return fmt.Errorf("remote mutation request: %w", err)
	}
	return nil
}

// RemoteTranscriptQuery requests a bounded provider transcript page after an
// opaque cursor. Cursor values are provider-authored and never parsed.
type RemoteTranscriptQuery struct {
	Ref         RemoteSessionRef `json:"ref"`
	AfterCursor string           `json:"after_cursor,omitempty"`
	Limit       int              `json:"limit"`
}

// Validate enforces the bounded read contract.
func (q RemoteTranscriptQuery) Validate() error {
	if err := q.Ref.Validate(); err != nil {
		return fmt.Errorf("remote transcript query: %w", err)
	}
	if q.Limit <= 0 || q.Limit > MaxRemoteTranscriptEvents {
		return fmt.Errorf("remote transcript query: limit must be between 1 and %d", MaxRemoteTranscriptEvents)
	}
	return nil
}

// RemoteSessionPhase is the normalized provider lifecycle state.
type RemoteSessionPhase string

// Normalized provider lifecycle phases.
const (
	RemoteSessionQueued    RemoteSessionPhase = "queued"
	RemoteSessionRunning   RemoteSessionPhase = "running"
	RemoteSessionWaiting   RemoteSessionPhase = "waiting"
	RemoteSessionSucceeded RemoteSessionPhase = "succeeded"
	RemoteSessionFailed    RemoteSessionPhase = "failed"
	RemoteSessionCanceled  RemoteSessionPhase = "canceled"
	RemoteSessionExpired   RemoteSessionPhase = "expired"
)

// Terminal reports whether the provider run will perform no more work.
func (p RemoteSessionPhase) Terminal() bool {
	switch p {
	case RemoteSessionSucceeded, RemoteSessionFailed, RemoteSessionCanceled, RemoteSessionExpired:
		return true
	default:
		return false
	}
}

func validRemoteSessionPhase(p RemoteSessionPhase) bool {
	switch p {
	case RemoteSessionQueued, RemoteSessionRunning, RemoteSessionWaiting,
		RemoteSessionSucceeded, RemoteSessionFailed, RemoteSessionCanceled, RemoteSessionExpired:
		return true
	default:
		return false
	}
}

// RemoteFailureKind is a stable, provider-neutral failure class. It contains
// no account, plan, price, or quota amount.
type RemoteFailureKind string

// Stable provider-neutral remote failure classes.
const (
	RemoteFailureNone      RemoteFailureKind = ""
	RemoteFailureAuth      RemoteFailureKind = "auth"
	RemoteFailureQuota     RemoteFailureKind = "quota"
	RemoteFailureNetwork   RemoteFailureKind = "network"
	RemoteFailurePolicy    RemoteFailureKind = "policy"
	RemoteFailureOwnership RemoteFailureKind = "ownership"
	RemoteFailureNotFound  RemoteFailureKind = "not_found"
	RemoteFailureCursor    RemoteFailureKind = "cursor"
	RemoteFailureProvider  RemoteFailureKind = "provider"
	RemoteFailureUnknown   RemoteFailureKind = "unknown"
)

func validRemoteFailureKind(kind RemoteFailureKind) bool {
	switch kind {
	case RemoteFailureNone, RemoteFailureAuth, RemoteFailureQuota,
		RemoteFailureNetwork, RemoteFailurePolicy, RemoteFailureOwnership,
		RemoteFailureNotFound, RemoteFailureCursor, RemoteFailureProvider,
		RemoteFailureUnknown:
		return true
	default:
		return false
	}
}

// RemoteGitHandoff is provider-reported branch/PR output. It is metadata only;
// Git and Beads remain the source of truth for work and delivery state.
type RemoteGitHandoff struct {
	Repository  string `json:"repository"`
	Branch      string `json:"branch,omitempty"`
	PullRequest string `json:"pull_request,omitempty"`
}

// Validate keeps credentials and transport control characters out of durable
// Git handoff metadata.
func (h RemoteGitHandoff) Validate() error {
	if strings.TrimSpace(h.Repository) == "" {
		return fmt.Errorf("repository is required")
	}
	if err := validateRemoteLocation(h.Repository); err != nil {
		return fmt.Errorf("repository: %w", err)
	}
	if strings.ContainsAny(h.Branch, "\r\n\x00") {
		return fmt.Errorf("branch contains a control character")
	}
	if err := validateRemoteLocation(h.PullRequest); err != nil {
		return fmt.Errorf("pull_request: %w", err)
	}
	return nil
}

// RemoteSessionSnapshot is a point-in-time provider observation. Message may
// be shown to an operator after redaction but must not be persisted in Beads.
type RemoteSessionSnapshot struct {
	Ref         RemoteSessionRef   `json:"ref"`
	Phase       RemoteSessionPhase `json:"phase"`
	Failure     RemoteFailureKind  `json:"failure,omitempty"`
	Message     string             `json:"message,omitempty"`
	EventCursor string             `json:"event_cursor,omitempty"`
	Handoff     []RemoteGitHandoff `json:"handoff,omitempty"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// Validate checks the normalized snapshot without interpreting opaque IDs.
func (s RemoteSessionSnapshot) Validate() error {
	if err := s.Ref.Validate(); err != nil {
		return fmt.Errorf("remote session snapshot: %w", err)
	}
	if !validRemoteSessionPhase(s.Phase) {
		return fmt.Errorf("remote session snapshot: invalid phase %q", s.Phase)
	}
	if !validRemoteFailureKind(s.Failure) {
		return fmt.Errorf("remote session snapshot: invalid failure kind %q", s.Failure)
	}
	if s.Failure != RemoteFailureNone && s.Phase != RemoteSessionFailed {
		return fmt.Errorf("remote session snapshot: failure kind requires failed phase")
	}
	if s.Phase == RemoteSessionFailed && s.Failure == RemoteFailureNone {
		return fmt.Errorf("remote session snapshot: failed phase requires failure kind")
	}
	if s.UpdatedAt.IsZero() {
		return fmt.Errorf("remote session snapshot: updated_at is required")
	}
	for i, handoff := range s.Handoff {
		if err := handoff.Validate(); err != nil {
			return fmt.Errorf("remote session snapshot: handoff[%d]: %w", i, err)
		}
	}
	return nil
}

// RemoteReceipt acknowledges an idempotent provider mutation.
type RemoteReceipt struct {
	RequestID      string    `json:"request_id"`
	ReceiptID      string    `json:"receipt_id"`
	RunID          string    `json:"run_id,omitempty"`
	EventCursor    string    `json:"event_cursor,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// Validate checks that the acknowledgement can be reconciled durably.
func (r RemoteReceipt) Validate() error {
	if strings.TrimSpace(r.RequestID) == "" || strings.TrimSpace(r.ReceiptID) == "" {
		return fmt.Errorf("remote receipt: request_id and receipt_id are required")
	}
	if r.AcknowledgedAt.IsZero() {
		return fmt.Errorf("remote receipt: acknowledged_at is required")
	}
	return nil
}

// RemoteTranscriptEvent is one bounded read-through provider event. Text is
// transient and must be redacted before display or logging.
type RemoteTranscriptEvent struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Text      string    `json:"text,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// RemoteTranscriptPage is one bounded provider transcript page.
type RemoteTranscriptPage struct {
	Events     []RemoteTranscriptEvent `json:"events"`
	NextCursor string                  `json:"next_cursor,omitempty"`
	Terminal   bool                    `json:"terminal,omitempty"`
}

// RemoteSessionError is the stable error envelope returned by a remote
// adapter. Message must be redacted at the process boundary.
type RemoteSessionError struct {
	Kind      RemoteFailureKind `json:"kind"`
	Message   string            `json:"message,omitempty"`
	Retryable bool              `json:"retryable,omitempty"`
}

// Validate rejects adapter-specific classifications. Provider prose belongs
// in Message; control flow uses only the stable provider-neutral Kind values.
func (e *RemoteSessionError) Validate() error {
	if e == nil {
		return fmt.Errorf("remote session error is required")
	}
	if e.Kind == RemoteFailureNone || !validRemoteFailureKind(e.Kind) {
		return fmt.Errorf("remote session error: invalid failure kind %q", e.Kind)
	}
	return nil
}

// Error implements error.
func (e *RemoteSessionError) Error() string {
	if e == nil {
		return ErrRemoteSession.Error()
	}
	kind := e.Kind
	if !validRemoteFailureKind(kind) || kind == RemoteFailureNone {
		kind = RemoteFailureUnknown
	}
	if strings.TrimSpace(e.Message) == "" {
		return fmt.Sprintf("remote session %s", kind)
	}
	return fmt.Sprintf("remote session %s: %s", kind, e.Message)
}

// Unwrap exposes the common sentinel.
func (e *RemoteSessionError) Unwrap() error { return ErrRemoteSession }

// RemoteSessionProvider is the optional provider-native remote lifecycle
// extension. Implementations perform transport only. Beads remains the durable
// work graph and session checkpoint authority.
type RemoteSessionProvider interface {
	RemoteCapabilities() RemoteSessionCapabilities
	RemoteCreate(ctx context.Context, name string, request RemoteCreateRequest) (RemoteSessionSnapshot, error)
	RemoteAdopt(ctx context.Context, name string, request RemoteAdoptRequest) (RemoteSessionSnapshot, error)
	RemoteStatus(ctx context.Context, name string, ref RemoteSessionRef) (RemoteSessionSnapshot, error)
	RemoteFollowUp(ctx context.Context, name string, request RemoteFollowUpRequest) (RemoteReceipt, error)
	RemoteTranscript(ctx context.Context, name string, query RemoteTranscriptQuery) (RemoteTranscriptPage, error)
	RemoteCancel(ctx context.Context, name string, request RemoteMutationRequest) (RemoteSessionSnapshot, error)
	RemoteClose(ctx context.Context, name string, request RemoteMutationRequest) (RemoteSessionSnapshot, error)
}
