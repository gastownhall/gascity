package exec

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.RemoteSessionProvider = (*Provider)(nil)

var remoteOperations = []runtime.RemoteSessionOperation{
	runtime.RemoteSessionCreate,
	runtime.RemoteSessionAdopt,
	runtime.RemoteSessionStatus,
	runtime.RemoteSessionFollowUp,
	runtime.RemoteSessionTranscript,
	runtime.RemoteSessionCancel,
	runtime.RemoteSessionClose,
}

// RemoteCapabilities reports the exact remote.* RPP operations declared by
// the adapter handshake. A broken handshake returns the empty, fail-closed
// capability set; Protocol exposes the underlying error to diagnostics.
func (p *Provider) RemoteCapabilities() runtime.RemoteSessionCapabilities {
	info, err := p.Protocol()
	if err != nil {
		return runtime.RemoteSessionCapabilities{}
	}
	operations := make([]runtime.RemoteSessionOperation, 0, len(remoteOperations))
	for _, op := range remoteOperations {
		if info.Has(string(op)) {
			operations = append(operations, op)
		}
	}
	return runtime.RemoteSessionCapabilities{Operations: operations}
}

type remoteEnvelope[T any] struct {
	OK     bool                        `json:"ok"`
	Result *T                          `json:"result,omitempty"`
	Error  *runtime.RemoteSessionError `json:"error,omitempty"`
}

type remoteStatusRequest struct {
	Ref runtime.RemoteSessionRef `json:"ref"`
}

func runRemoteOperation[T any](
	ctx context.Context,
	p *Provider,
	name string,
	operation runtime.RemoteSessionOperation,
	timeout time.Duration,
	request any,
) (T, error) {
	var zero T
	info, err := p.Protocol()
	if err != nil {
		return zero, fmt.Errorf("remote %s protocol handshake: %w", operation, err)
	}
	if !info.Has(string(operation)) {
		return zero, fmt.Errorf("%w: %s", runtime.ErrRemoteCapabilityUnsupported, operation)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return zero, fmt.Errorf("remote %s request: %w", operation, err)
	}
	op := remoteOperationCommand(operation)
	out, err := p.runWithContext(ctx, timeout, payload, op, name)
	if err != nil {
		return zero, fmt.Errorf("remote %s transport: %w", operation, err)
	}
	var envelope remoteEnvelope[T]
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		return zero, fmt.Errorf("remote %s response: invalid JSON envelope: %w", operation, err)
	}
	if envelope.OK {
		if envelope.Error != nil || envelope.Result == nil {
			return zero, fmt.Errorf("remote %s response: successful envelope must contain result only", operation)
		}
		return *envelope.Result, nil
	}
	if envelope.Error == nil || envelope.Result != nil {
		return zero, fmt.Errorf("remote %s response: failed envelope must contain error only", operation)
	}
	envelope.Error.Message = runtime.RedactSecrets(envelope.Error.Message, runtime.SetupCommandSecrets(nil))
	if err := envelope.Error.Validate(); err != nil {
		return zero, fmt.Errorf("remote %s response: %w", operation, err)
	}
	return zero, envelope.Error
}

func remoteOperationCommand(operation runtime.RemoteSessionOperation) string {
	switch operation {
	case runtime.RemoteSessionCreate:
		return "remote-create"
	case runtime.RemoteSessionAdopt:
		return "remote-adopt"
	case runtime.RemoteSessionStatus:
		return "remote-status"
	case runtime.RemoteSessionFollowUp:
		return "remote-follow-up"
	case runtime.RemoteSessionTranscript:
		return "remote-transcript"
	case runtime.RemoteSessionCancel:
		return "remote-cancel"
	case runtime.RemoteSessionClose:
		return "remote-close"
	default:
		return ""
	}
}

// RemoteCreate creates or idempotently adopts one provider-native session.
func (p *Provider) RemoteCreate(ctx context.Context, name string, request runtime.RemoteCreateRequest) (runtime.RemoteSessionSnapshot, error) {
	if err := request.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteSessionSnapshot](ctx, p, name, runtime.RemoteSessionCreate, p.startTimeout, request)
	if err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result.Message = runtime.RedactSecrets(result.Message, runtime.SetupCommandSecrets(nil))
	if err := result.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	if request.Existing != nil && result.Ref.SessionID != request.Existing.SessionID {
		return runtime.RemoteSessionSnapshot{}, fmt.Errorf("remote create response: adopted session_id %q does not match persisted identity", result.Ref.SessionID)
	}
	return result, nil
}

// RemoteAdopt rebinds a persisted provider-native identity without creating a
// new provider task.
func (p *Provider) RemoteAdopt(ctx context.Context, name string, request runtime.RemoteAdoptRequest) (runtime.RemoteSessionSnapshot, error) {
	if err := request.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteSessionSnapshot](ctx, p, name, runtime.RemoteSessionAdopt, p.timeout, request)
	if err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result.Message = runtime.RedactSecrets(result.Message, runtime.SetupCommandSecrets(nil))
	if err := result.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	if result.Ref.SessionID != request.Ref.SessionID {
		return runtime.RemoteSessionSnapshot{}, fmt.Errorf("remote adopt response: session_id %q does not match requested identity", result.Ref.SessionID)
	}
	return result, nil
}

// RemoteStatus reads normalized provider state for a persisted identity.
func (p *Provider) RemoteStatus(ctx context.Context, name string, ref runtime.RemoteSessionRef) (runtime.RemoteSessionSnapshot, error) {
	if err := ref.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteSessionSnapshot](ctx, p, name, runtime.RemoteSessionStatus, p.timeout, remoteStatusRequest{Ref: ref})
	if err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result.Message = runtime.RedactSecrets(result.Message, runtime.SetupCommandSecrets(nil))
	if err := result.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	if result.Ref.SessionID != ref.SessionID {
		return runtime.RemoteSessionSnapshot{}, fmt.Errorf("remote status response: session_id %q does not match requested identity", result.Ref.SessionID)
	}
	return result, nil
}

// RemoteFollowUp appends one idempotent, ownership-fenced provider turn.
func (p *Provider) RemoteFollowUp(ctx context.Context, name string, request runtime.RemoteFollowUpRequest) (runtime.RemoteReceipt, error) {
	if err := request.Validate(); err != nil {
		return runtime.RemoteReceipt{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteReceipt](ctx, p, name, runtime.RemoteSessionFollowUp, p.timeout, request)
	if err != nil {
		return runtime.RemoteReceipt{}, err
	}
	if err := result.Validate(); err != nil {
		return runtime.RemoteReceipt{}, err
	}
	if result.RequestID != request.RequestID {
		return runtime.RemoteReceipt{}, fmt.Errorf("remote follow-up response: request_id %q does not match request", result.RequestID)
	}
	return result, nil
}

// RemoteTranscript reads one bounded, redacted provider event page.
func (p *Provider) RemoteTranscript(ctx context.Context, name string, query runtime.RemoteTranscriptQuery) (runtime.RemoteTranscriptPage, error) {
	if err := query.Validate(); err != nil {
		return runtime.RemoteTranscriptPage{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteTranscriptPage](ctx, p, name, runtime.RemoteSessionTranscript, p.timeout, query)
	if err != nil {
		return runtime.RemoteTranscriptPage{}, err
	}
	if len(result.Events) > query.Limit {
		return runtime.RemoteTranscriptPage{}, fmt.Errorf("remote transcript response: provider returned %d events for limit %d", len(result.Events), query.Limit)
	}
	secrets := runtime.SetupCommandSecrets(nil)
	for i := range result.Events {
		result.Events[i].Text = runtime.RedactSecrets(result.Events[i].Text, secrets)
	}
	return result, nil
}

// RemoteCancel requests an idempotent stop of active provider work.
func (p *Provider) RemoteCancel(ctx context.Context, name string, request runtime.RemoteMutationRequest) (runtime.RemoteSessionSnapshot, error) {
	return p.remoteMutation(ctx, name, runtime.RemoteSessionCancel, request)
}

// RemoteClose requests idempotent provider-side lifecycle closure/readback.
func (p *Provider) RemoteClose(ctx context.Context, name string, request runtime.RemoteMutationRequest) (runtime.RemoteSessionSnapshot, error) {
	return p.remoteMutation(ctx, name, runtime.RemoteSessionClose, request)
}

func (p *Provider) remoteMutation(ctx context.Context, name string, operation runtime.RemoteSessionOperation, request runtime.RemoteMutationRequest) (runtime.RemoteSessionSnapshot, error) {
	if err := request.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result, err := runRemoteOperation[runtime.RemoteSessionSnapshot](ctx, p, name, operation, p.timeout, request)
	if err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	result.Message = runtime.RedactSecrets(result.Message, runtime.SetupCommandSecrets(nil))
	if err := result.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, err
	}
	if result.Ref.SessionID != request.Ref.SessionID {
		return runtime.RemoteSessionSnapshot{}, fmt.Errorf("remote %s response: session_id %q does not match requested identity", operation, result.Ref.SessionID)
	}
	return result, nil
}
