package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
)

// The producer API (DES-O1 §11, OJ-20): an external producer proposes,
// reads and cancels a session's conversation-lane ops. It writes only lane
// ops, only of the kinds the city's running lane host executes, and only
// with a delivery mode that lane lowers, so a proposal is never silently
// shadowed by a legacy path nor parked unexecuted. The ops.* gates do not
// apply: they govern the legacy request surfaces, which record shadow ops.

// t3PrincipalPrefix starts the asserted principal of a T3 Code environment,
// t3:<environment id> (COJ T3 critic §3). The wire pattern
// (ConversationProducerBody) admits only it and operator.
const t3PrincipalPrefix = "t3:"

// previewConversation stands in for the session while a proposal is
// validated before its session is resolved: resolving a configured named
// session materializes its row, which a refused request must not do.
const previewConversation = "preview"

// ConversationLaneHost is the optional State extension through which the
// producer API reaches the controller's conversation lane host. A State
// without it runs no lane host: Propose refuses every kind and session reads
// carry no observed turn.
type ConversationLaneHost interface {
	// ConversationLaneKinds is the op kinds the city's lane host executes:
	// those [ops] authoritative listed when the host started, while it runs,
	// and nil when no host runs. A config reload does not change them.
	ConversationLaneKinds() []string
	// PokeConversationLane rings the lane host for conversation after the
	// API wrote one of its lane ops, so the op does not wait for the host's
	// anti-entropy scan.
	PokeConversationLane(conversation string)
	// AwaitConversationOp waits until op id of conversation closes, or ctx
	// ends, and returns the op as last read: closed, or still open when ctx
	// ended. It wakes at the end of each pass over the conversation and
	// re-reads the op at least every second. A read that fails is an
	// error.
	AwaitConversationOp(ctx context.Context, conversation, id string) (opjournal.Op, error)
}

// conversationLaneKinds is the kinds the city's running lane host executes,
// nil when the State offers no lane host.
func (s *Server) conversationLaneKinds() []string {
	if h, ok := s.state.(ConversationLaneHost); ok {
		return h.ConversationLaneKinds()
	}
	return nil
}

// pokeConversationLane rings the conversation lane for conv, when the State
// offers it.
func (s *Server) pokeConversationLane(conv string) {
	if h, ok := s.state.(ConversationLaneHost); ok {
		h.PokeConversationLane(conv)
	}
}

// conversationOpStore opens the op store the producer API reads and writes.
func (s *Server) conversationOpStore() (*opjournal.Store, error) {
	if s.ops == nil {
		return nil, apierr.OpJournalUnavailable.Msg("op_journal_unavailable: this server has no operation journal")
	}
	st, err := s.openOpStore()
	if err != nil {
		return nil, apierr.OpJournalUnavailable.Msg("op_journal_unavailable: opening the operation journal: " + err.Error())
	}
	return st, nil
}

// producerOf is the producer a request asserts: operator unless it names a
// t3:<environment id>, whose actor class it must name.
func producerOf(b ConversationProducerBody) (opjournal.Producer, error) {
	p := opjournal.Producer{Principal: b.Principal, Actor: b.Actor}
	if p.Principal == "" {
		p.Principal = legacyop.PrincipalOperator
	}
	if p.Actor == "" {
		if strings.HasPrefix(p.Principal, t3PrincipalPrefix) {
			return opjournal.Producer{}, apierr.ValidationFailed.Msg("actor_required: a " + t3PrincipalPrefix + "<environment id> principal names its actor class")
		}
		p.Actor = opjournal.ActorOperator
	}
	return p, nil
}

// proposeClientKey is the client key a proposal is recorded under: the body's
// or the header's, which must agree when both are set, or a fresh one.
func proposeClientKey(body, header string) (key string, minted bool, err error) {
	switch {
	case body != "" && header != "" && body != header:
		return "", false, apierr.ValidationFailed.Msg("client_key_mismatch: the body's client_key and the Idempotency-Key header differ")
	case body != "":
		return body, false, nil
	case header != "":
		return header, false, nil
	}
	key, err = legacyop.MintKey()
	if err != nil {
		return "", false, apierr.Internal.Msg(err.Error())
	}
	return key, true, nil
}

// proposeRequest is the lane op request body describes for conv.
func proposeRequest(conv, key string, minted bool, producer opjournal.Producer, body ConversationOpProposeBody) opjournal.Request {
	req := opjournal.Request{
		Conversation: conv,
		Kind:         opjournal.Kind(body.Kind),
		Executor:     opjournal.ExecutorLane,
		MintedKey:    minted,
		ClientKey:    key,
		Producer:     producer,
		Payload:      body.Payload,
		After:        body.After,
		ClientSeq:    body.ClientSeq,
		NotBefore:    timeOrZero(body.NotBefore),
		StartBy:      timeOrZero(body.StartBy),
		FinishBy:     timeOrZero(body.FinishBy),
		Causation:    body.Causation,
		Correlation:  body.Correlation,
	}
	if p := body.Policy; p != nil {
		req.Policy.Guarantee = opjournal.Guarantee(p.Guarantee)
		if m := p.Message; m != nil {
			req.Policy.Message = opjournal.MessagePolicy{
				When:          opjournal.When(m.When),
				Wake:          opjournal.Wake(m.Wake),
				Role:          opjournal.Role(m.Role),
				OnUnsupported: opjournal.Fallback(m.OnUnsupported),
				NowGeneration: m.NowGeneration,
				NowTurn:       m.NowTurn,
			}
		}
		if i := p.Interrupt; i != nil {
			req.Policy.Interrupt = opjournal.InterruptPolicy{OnFailure: opjournal.OnFailure(i.OnFailure)}
		}
	}
	if sc := body.Scope; sc != nil {
		req.Scope = opjournal.Scope{
			Kind:        opjournal.ScopeKind(sc.Kind),
			Epoch:       sc.Epoch,
			Generation:  sc.Generation,
			Turn:        sc.Turn,
			Interaction: sc.Interaction,
		}
	}
	return req
}

func timeOrZero(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// proposeError maps a refused proposal onto the wire.
func proposeError(err error) error {
	var reuse *opjournal.KeyReuseError
	switch {
	case errors.As(err, &reuse):
		return apierr.OpKeyReuse.With("key_reuse: the client key already names another request from this principal on this session; the first request wins",
			&huma.ErrorDetail{Location: "op_id", Message: "the operation the key names", Value: reuse.Existing.ID})
	case errors.Is(err, opjournal.ErrPayloadTooLarge):
		return apierr.PayloadTooLarge.Msg(fmt.Sprintf("payload_too_large: the payload exceeds the operation journal's %d-byte limit", opjournal.MaxInlinePayload))
	case errors.Is(err, opjournal.ErrInvalidRequest), errors.Is(err, opjournal.ErrUnknownKind):
		return apierr.ValidationFailed.Msg("invalid_request: " + err.Error())
	default:
		return apierr.OpJournalUnavailable.Msg("op_journal_unavailable: recording the operation: " + err.Error())
	}
}

// humaHandleConversationOpPropose is the Huma-typed handler for POST
// /v0/city/{cityName}/session/{id}/ops (Propose). Every refusal, and the read
// of the event cursor the answer carries, comes before the session is
// resolved, so a refused request, or one whose cursor cannot be read,
// materializes no named session and writes nothing.
func (s *Server) humaHandleConversationOpPropose(ctx context.Context, input *ConversationOpProposeInput) (*ConversationOpProposeOutput, error) {
	body := input.Body
	if !slices.Contains(s.conversationLaneKinds(), body.Kind) {
		return nil, apierr.KindNotEnabled.Msgf("kind_not_enabled: no running conversation lane host executes %q (it runs the kinds [ops] authoritative listed at the controller's start), so this request wrote nothing", body.Kind)
	}
	if body.ClientSeq > math.MaxInt64 {
		// The spec's maximum is a float, which admits 2^63 itself.
		return nil, apierr.ValidationFailed.Msgf("client_seq_out_of_range: client_seq %d is over %d", body.ClientSeq, int64(math.MaxInt64))
	}
	producer, err := producerOf(body.ConversationProducerBody)
	if err != nil {
		return nil, err
	}
	key, minted, err := proposeClientKey(body.ClientKey, input.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if err := opjournal.ValidateClientKey(key); err != nil {
		return nil, apierr.InvalidIdempotencyKey.Msg("invalid_idempotency_key: " + err.Error())
	}
	sessions := s.state.SessionsBeadStore()
	if sessions.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	st, err := s.conversationOpStore()
	if err != nil {
		return nil, err
	}
	if _, err := st.Preview(proposeRequest(previewConversation, key, minted, producer, body)); err != nil {
		return nil, proposeError(err)
	}
	cursor, err := s.currentCityEventCursor()
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg(err.Error())
	}
	conv, err := s.resolveSessionIDMaterializingNamedWithContext(ctx, sessions.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	op, created, err := st.Propose(proposeRequest(conv, key, minted, producer, body))
	if err != nil {
		return nil, proposeError(err)
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
		s.pokeConversationLane(conv)
	}
	return &ConversationOpProposeOutput{Status: status, Body: ConversationOpProposeResult{
		OpID:      op.ID,
		Status:    string(op.Status),
		Created:   created,
		ClientKey: op.ClientKey,
		Cursor:    cursor,
		Op:        conversationOpRecord(op),
	}}, nil
}

// humaHandleConversationOpList is the Huma-typed handler for GET
// /v0/city/{cityName}/session/{id}/ops: the session's open ops, in the lane's
// order. A row it cannot serve (a newer build's, or a damaged one) is left
// out and named in partial_errors.
func (s *Server) humaHandleConversationOpList(_ context.Context, input *SessionIDInput) (*ListOutput[ConversationOp], error) {
	index := s.latestIndex()
	conv, st, err := s.conversationOpTarget(input.ID)
	if err != nil {
		return nil, err
	}
	ops, quarantined, err := st.ListOpenTolerant(conv)
	if err != nil {
		return nil, apierr.OpJournalUnavailable.Msg("op_journal_unavailable: " + err.Error())
	}
	items := make([]ConversationOp, 0, len(ops))
	for _, op := range ops {
		items = append(items, conversationOpRecord(op))
	}
	body := ListBody[ConversationOp]{Items: items, Total: len(items)}
	for _, q := range quarantined {
		body.Partial = true
		body.PartialErrors = append(body.PartialErrors, fmt.Sprintf("op %s: %v", q.ID, q.Err))
	}
	return &ListOutput[ConversationOp]{Index: index, Body: body}, nil
}

// humaHandleConversationOpGet is the Huma-typed handler for GET
// /v0/city/{cityName}/session/{id}/ops/{op}.
func (s *Server) humaHandleConversationOpGet(_ context.Context, input *ConversationOpGetInput) (*IndexOutput[ConversationOp], error) {
	index := s.latestIndex()
	conv, st, err := s.conversationOpTarget(input.ID)
	if err != nil {
		return nil, err
	}
	op, err := sessionOp(st, conv, input.Op)
	if err != nil {
		return nil, err
	}
	return &IndexOutput[ConversationOp]{Index: index, Body: conversationOpRecord(op)}, nil
}

// humaHandleConversationOpCancel is the Huma-typed handler for POST
// /v0/city/{cityName}/session/{id}/ops/{op}/cancel.
func (s *Server) humaHandleConversationOpCancel(_ context.Context, input *ConversationOpCancelInput) (*ConversationOpCancelOutput, error) {
	conv, st, err := s.conversationOpTarget(input.ID)
	if err != nil {
		return nil, err
	}
	if _, err := sessionOp(st, conv, input.Op); err != nil {
		return nil, err
	}
	by, reason := legacyop.PrincipalOperator, ""
	if b := input.Body; b != nil {
		if b.Principal != "" {
			by = b.Principal
		}
		reason = b.Reason
	}
	result, op, err := st.Cancel(input.Op, by, reason)
	switch {
	case err == nil:
	case errors.Is(err, opjournal.ErrIllegalTransition):
		return nil, apierr.ConflictWrongState.Msg("not_cancelable: " + err.Error())
	case errors.Is(err, opjournal.ErrInvalidRequest):
		return nil, apierr.ValidationFailed.Msg("invalid_request: " + err.Error())
	case errors.Is(err, beads.ErrNotFound):
		return nil, apierr.OpNotFound.Msgf("op %s not found", input.Op)
	default:
		return nil, apierr.OpJournalUnavailable.Msg("op_journal_unavailable: canceling the operation: " + err.Error())
	}
	if result == opjournal.CancelClosed || result == opjournal.CancelRequested {
		s.pokeConversationLane(conv)
	}
	return &ConversationOpCancelOutput{Body: ConversationOpCancelResult{Result: string(result), Op: conversationOpRecord(op)}}, nil
}

// conversationOpTarget resolves a read or cancel's session, closed sessions
// included, and opens the op store.
func (s *Server) conversationOpTarget(target string) (string, *opjournal.Store, error) {
	sessions := s.state.SessionsBeadStore()
	if sessions.Store == nil {
		return "", nil, apierr.ServiceUnavailable.Msg("no bead store configured")
	}
	conv, err := s.resolveSessionIDAllowClosedWithConfig(sessions.Store, target)
	if err != nil {
		return "", nil, humaResolveError(err)
	}
	st, err := s.conversationOpStore()
	if err != nil {
		return "", nil, err
	}
	return conv, st, nil
}

// sessionOp reads op id, which must belong to conv: an op of another session
// is not found here.
func sessionOp(st *opjournal.Store, conv, id string) (opjournal.Op, error) {
	op, err := st.Get(id)
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return opjournal.Op{}, apierr.OpNotFound.Msgf("op %s not found", id)
	case errors.Is(err, opjournal.ErrCorruptOp), errors.Is(err, opjournal.ErrUnsupportedKind),
		errors.Is(err, opjournal.ErrUnsupportedSchema), errors.Is(err, opjournal.ErrUnsupportedValue):
		return opjournal.Op{}, apierr.Internal.Msgf("reading op %s: %v", id, err)
	case err != nil:
		return opjournal.Op{}, apierr.OpJournalUnavailable.Msgf("op_journal_unavailable: reading op %s: %v", id, err)
	case op.Conversation != conv:
		return opjournal.Op{}, apierr.OpNotFound.Msgf("op %s not found on session %s", id, conv)
	}
	return op, nil
}

// observedSessionTurn is the turn conv's conversation-lane observer last
// recorded on its runtime row (X11 §5.3), read for a session whose current
// generation is generation. Cells of another generation read unknown, with
// no turn and no capabilities. It is nil when the row does not exist yet; a
// row that cannot be read is logged and left out, like the read's other
// enrichments.
func (s *Server) observedSessionTurn(conv, generation string) *SessionTurn {
	if s.ops == nil {
		return nil
	}
	st, err := s.openOpStore()
	if err != nil {
		log.Printf("api: session %s: reading its observed turn: %v", conv, err)
		return nil
	}
	row, _, found, err := st.Runtime(conv)
	if err != nil {
		log.Printf("api: session %s: reading its observed turn: %v", conv, err)
		return nil
	}
	if !found {
		return nil
	}
	cells := row.Turn
	if cells.Generation == 0 && cells.State == "" && len(cells.Caps) == 0 {
		return nil
	}
	out := &SessionTurn{Generation: cells.Generation, State: string(opjournal.TurnStateUnknown)}
	gen, err := strconv.ParseInt(strings.TrimSpace(generation), 10, 64)
	if err != nil || gen != cells.Generation {
		return out
	}
	if cells.State != "" {
		out.State = string(cells.State)
	}
	out.ID = cells.ID
	out.At = optionalTime(cells.At)
	out.LastID = cells.LastID
	out.LastEnd = string(cells.LastEnd)
	out.LastAt = optionalTime(cells.LastAt)
	out.ObservedAt = optionalTime(cells.ObservedAt)
	for _, c := range cells.Caps {
		out.Caps = append(out.Caps, SessionTurnCapPair{Key: c.Key, Value: c.Value})
	}
	return out
}
