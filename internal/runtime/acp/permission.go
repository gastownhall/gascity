package acp

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ACP methods gc serves for tool-call permissions.
const (
	methodRequestPermission = "session/request_permission"
	methodCancelRequest     = "$/cancel_request"
)

// Pending-interaction vocabulary for ACP permission requests.
const (
	interactionKindApproval   = "approval"
	interactionRequestPrefix  = "acp-"
	permissionPromptFallback  = "Permission requested"
	permissionActionCancel    = "cancel"
	permissionOutcomeSelected = "selected"
	permissionOutcomeCanceled = "cancelled" //nolint:misspell // ACP wire spelling
)

// permissionActionKinds maps the approval actions shared with the tmux
// runtime onto the ACP permission option kind each one selects.
var permissionActionKinds = map[string]string{
	"approve":        "allow_once",
	"approve_always": "allow_always",
	"deny":           "reject_once",
	"deny_always":    "reject_always",
}

// permissionOption is one choice the agent offers for a tool call.
type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// permissionToolCall is the part of the tool call gc surfaces.
type permissionToolCall struct {
	ToolCallID string `json:"toolCallId"`
	Title      string `json:"title"`
	Kind       string `json:"kind"`
}

// requestPermissionParams is the params of session/request_permission.
type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  permissionToolCall `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

// pendingPermission is one unanswered session/request_permission. id is the
// request's compacted raw JSON id, echoed verbatim in the reply; reqID is the
// pending-interaction id clients answer with.
type pendingPermission struct {
	id     json.RawMessage
	reqID  string
	params requestPermissionParams
}

// permissionRequestID builds the pending-interaction id for a request:
// "acp-<connection nonce>-<raw JSON id>". The raw id text keeps the number 7
// and the string "7" distinct. The nonce is fixed per connection, so the id
// is stable across Pending calls, but agents number their requests from 1 on
// every connection, so without it a stale answer from before a restart would
// name a different incarnation's request.
func permissionRequestID(nonce string, id json.RawMessage) string {
	return interactionRequestPrefix + nonce + "-" + string(id)
}

// newPermissionNonce returns a random per-connection RequestID component.
func newPermissionNonce() string {
	var b [6]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+).
	return hex.EncodeToString(b[:])
}

func (pp pendingPermission) requestID() string {
	return pp.reqID
}

// interaction projects the request onto the runtime vocabulary.
func (pp pendingPermission) interaction() *runtime.PendingInteraction {
	prompt := pp.params.ToolCall.Title
	if prompt == "" {
		prompt = permissionPromptFallback
	}
	meta := map[string]string{
		"source":       "acp",
		"option_count": strconv.Itoa(len(pp.params.Options)),
	}
	if pp.params.ToolCall.ToolCallID != "" {
		meta["tool_call_id"] = pp.params.ToolCall.ToolCallID
	}
	if pp.params.ToolCall.Kind != "" {
		meta["tool_kind"] = pp.params.ToolCall.Kind
	}
	options := make([]string, 0, len(pp.params.Options))
	for i, opt := range pp.params.Options {
		options = append(options, opt.Name)
		prefix := "option_" + strconv.Itoa(i) + "_"
		meta[prefix+"id"] = opt.OptionID
		meta[prefix+"kind"] = opt.Kind
		meta[prefix+"name"] = opt.Name
	}
	return &runtime.PendingInteraction{
		RequestID: pp.requestID(),
		Kind:      interactionKindApproval,
		Prompt:    prompt,
		Options:   options,
		Metadata:  meta,
	}
}

// outcome resolves a response action to the reply outcome. Mapped actions
// select the first option of their kind; any other action must equal an
// offered optionId.
func (pp pendingPermission) outcome(action string) (permissionOutcome, error) {
	if action == permissionActionCancel {
		return permissionOutcome{Outcome: permissionOutcomeCanceled}, nil
	}
	if kind, ok := permissionActionKinds[action]; ok {
		for _, opt := range pp.params.Options {
			if opt.Kind == kind {
				return permissionOutcome{Outcome: permissionOutcomeSelected, OptionID: opt.OptionID}, nil
			}
		}
		return permissionOutcome{}, fmt.Errorf("%w: action %q needs an option of kind %s, which %s does not offer",
			runtime.ErrInteractionResponseInvalid, action, kind, pp.requestID())
	}
	for _, opt := range pp.params.Options {
		if action != "" && opt.OptionID == action {
			return permissionOutcome{Outcome: permissionOutcomeSelected, OptionID: opt.OptionID}, nil
		}
	}
	return permissionOutcome{}, fmt.Errorf("%w: unknown action %q for %s",
		runtime.ErrInteractionResponseInvalid, action, pp.requestID())
}

// permissionOutcome is ACP's RequestPermissionOutcome.
type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}

// permissionReply is the wire shape of the result gc sends back.
type permissionReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  struct {
		Outcome permissionOutcome `json:"outcome"`
	} `json:"result"`
}

func encodePermissionReply(id json.RawMessage, outcome permissionOutcome) ([]byte, error) {
	reply := permissionReply{JSONRPC: "2.0", ID: id}
	reply.Result.Outcome = outcome
	return encodeReply(methodRequestPermission, reply)
}

// compactID normalizes a raw JSON id so the request id and a later
// $/cancel_request requestId compare equal regardless of whitespace.
func compactID(raw json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// holdPermission records a session/request_permission without replying.
// The reply is sent when a client responds, the turn ends, the session is
// interrupted, or the agent exits. Undecodable params are answered -32602.
func (sc *sessionConn) holdPermission(req agentRequest) {
	var params requestPermissionParams
	err := json.Unmarshal(req.Params, &params)
	var id json.RawMessage
	if err == nil {
		id, err = compactID(req.ID)
	}
	if err != nil {
		data, encErr := encodeError(req, jsonRPCInvalidParams, "invalid params: "+err.Error())
		if encErr != nil {
			fmt.Fprintf(os.Stderr, "acp: %v\n", encErr)
			return
		}
		sc.writeReplyAsync(req.Method, data)
		return
	}
	sc.mu.Lock()
	if sc.permissionNonce == "" {
		sc.permissionNonce = newPermissionNonce()
	}
	sc.permissions = append(sc.permissions, pendingPermission{
		id:     id,
		reqID:  permissionRequestID(sc.permissionNonce, id),
		params: params,
	})
	if sc.permissionHeld != nil {
		close(sc.permissionHeld)
		sc.permissionHeld = nil
	}
	sc.mu.Unlock()
}

// errNudgeBusy reports that a nudge's wait for the turn to end timed out.
var errNudgeBusy = errors.New("agent busy, timed out waiting for idle")

// waitNudgeable blocks until the turn is idle, like waitIdle, but refuses
// with runtime.ErrNudgeRefusedPendingInteraction as soon as a permission
// request is outstanding: that turn cannot end until a client answers, and
// the answer must not queue behind this wait. It returns errNudgeBusy when
// the timeout expires first.
func (sc *sessionConn) waitNudgeable(timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		sc.mu.Lock()
		sc.ensureIdleChannelLocked()
		if len(sc.permissions) > 0 {
			sc.mu.Unlock()
			return runtime.ErrNudgeRefusedPendingInteraction
		}
		if sc.activePromptID == 0 {
			sc.mu.Unlock()
			return nil
		}
		idleCh := sc.idleCh
		if sc.permissionHeld == nil {
			sc.permissionHeld = make(chan struct{})
		}
		held := sc.permissionHeld
		sc.mu.Unlock()

		select {
		case <-idleCh:
			return nil
		case <-held:
			// Re-check: the permission may already have been answered.
		case <-timer.C:
			return errNudgeBusy
		}
	}
}

// oldestPermission returns the oldest unanswered permission request, or nil.
func (sc *sessionConn) oldestPermission() *runtime.PendingInteraction {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if len(sc.permissions) == 0 {
		return nil
	}
	return sc.permissions[0].interaction()
}

// resolvePermission removes the request a response names and returns the
// encoded reply. An empty RequestID names the oldest request. On error the
// request stays outstanding.
func (sc *sessionConn) resolvePermission(resp runtime.InteractionResponse) ([]byte, error) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	idx := -1
	for i, pp := range sc.permissions {
		if resp.RequestID == "" || pp.requestID() == resp.RequestID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("%w: no outstanding permission request %q", runtime.ErrInteractionResponseInvalid, resp.RequestID)
	}
	pp := sc.permissions[idx]
	outcome, err := pp.outcome(resp.Action)
	if err != nil {
		return nil, err
	}
	data, err := encodePermissionReply(pp.id, outcome)
	if err != nil {
		return nil, err
	}
	sc.permissions = append(sc.permissions[:idx], sc.permissions[idx+1:]...)
	return data, nil
}

// cancelOutstandingPermissions answers every unanswered permission request
// with the canceled outcome and forgets them. ACP requires this when the
// client cancels the turn; gc also does it when the turn ends (no answer can
// still apply to it) and when the agent exits. Replies are written
// asynchronously so callers on the read loop never wait on the agent's stdin.
func (sc *sessionConn) cancelOutstandingPermissions() {
	sc.mu.Lock()
	outstanding := sc.permissions
	sc.permissions = nil
	sc.mu.Unlock()
	if len(outstanding) == 0 {
		return
	}
	replies := make([][]byte, 0, len(outstanding))
	for _, pp := range outstanding {
		data, err := encodePermissionReply(pp.id, permissionOutcome{Outcome: permissionOutcomeCanceled})
		if err != nil {
			fmt.Fprintf(os.Stderr, "acp: %v\n", err)
			continue
		}
		replies = append(replies, data)
	}
	go func() {
		for _, data := range replies {
			if err := sc.writeMessage(data); err != nil {
				// An exited agent cannot read the reply; that is expected.
				if !isPipeWriteError(err) {
					fmt.Fprintf(os.Stderr, "acp: canceling permission request: %v\n", err)
				}
				return
			}
		}
	}()
}

// cancelRequestedPermission handles a $/cancel_request notification naming an
// outstanding permission request: gc forgets it and, as ACP cancellation
// requires of an implementation that acts on the notification, still answers
// the original request, with the canceled outcome.
func (sc *sessionConn) cancelRequestedPermission(params json.RawMessage) {
	var cancel struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &cancel); err != nil || len(cancel.RequestID) == 0 {
		fmt.Fprintf(os.Stderr, "acp: %s without a requestId\n", methodCancelRequest)
		return
	}
	id, err := compactID(cancel.RequestID)
	if err != nil {
		return
	}
	sc.mu.Lock()
	var dropped *pendingPermission
	for i, pp := range sc.permissions {
		if bytes.Equal(pp.id, id) {
			dropped = &pp
			sc.permissions = append(sc.permissions[:i], sc.permissions[i+1:]...)
			break
		}
	}
	sc.mu.Unlock()
	if dropped == nil {
		return
	}
	data, err := encodePermissionReply(dropped.id, permissionOutcome{Outcome: permissionOutcomeCanceled})
	if err != nil {
		fmt.Fprintf(os.Stderr, "acp: %v\n", err)
		return
	}
	sc.writeReplyAsync(methodRequestPermission, data)
}

// Pending reports the oldest unanswered ACP session/request_permission as an
// approval interaction. It returns ErrSessionNotFound when this provider
// instance does not own the session's connection, and nil when nothing is
// outstanding.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	sc, err := p.ownedConn(name)
	if err != nil {
		return nil, err
	}
	return sc.oldestPermission(), nil
}

// Respond answers an outstanding permission request. Actions: approve,
// approve_always, deny and deny_always select the first option of the
// matching ACP kind; cancel answers the canceled outcome; any other action
// must equal an offered optionId. An unknown action, an option kind the agent
// did not offer, or a RequestID that names no outstanding request returns an
// error wrapping runtime.ErrInteractionResponseInvalid, and the request stays
// pending. An empty RequestID answers the oldest request.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	sc, err := p.ownedConn(name)
	if err != nil {
		return err
	}
	data, err := sc.resolvePermission(response)
	if err != nil {
		return err
	}
	if err := sc.writeMessage(data); err != nil {
		return fmt.Errorf("answering permission request %s for %q: %w", response.RequestID, name, err)
	}
	return nil
}

// ownedConn returns the in-memory connection for name, or an error wrapping
// runtime.ErrSessionNotFound when this provider instance does not own it.
func (p *Provider) ownedConn(name string) (*sessionConn, error) {
	p.mu.Lock()
	sc, ok := p.conns[name]
	p.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: ACP provider does not own session %q", runtime.ErrSessionNotFound, name)
	}
	return sc, nil
}
