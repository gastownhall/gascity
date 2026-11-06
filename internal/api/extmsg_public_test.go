package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// This state deliberately offers no lifecycle: tests drive durable relay
// explicitly and start no production worker or native provider.
type publicAdmissionTestState struct {
	*lanePokeState
	messaging beads.MailStore
}

func (s *publicAdmissionTestState) ExtMsgPublicStore() beads.MailStore { return s.messaging }

func newPublicAdmissionEnv(t *testing.T) (*opsEnv, *publicAdmissionTestState, extmsg.ExternalInboundMessage, []extmsg.PublicRecipient) {
	t.Helper()
	e, lane := newLaneOpsEnv(t, "message")
	messages := newOpsTestStore()
	services := extmsg.NewServices(beadstest.Mail(messages), lane.SessionsBeadStore())
	e.fs.adapterReg = extmsg.NewAdapterRegistry()
	e.fs.extmsgSvc = &services
	state := &publicAdmissionTestState{lanePokeState: lane, messaging: beads.MailStore{Store: messages}}
	e.resolver.state = state
	info := e.session(t, "public recipient")
	ref := extmsg.ConversationRef{ScopeID: "fixture", Provider: "zoo", AccountID: "local", ConversationID: "public-room", Kind: extmsg.ConversationRoom}
	_, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: ref, SessionID: info.ID,
		Owner: extmsg.MembershipOwnerManual, BackfillPolicy: extmsg.MembershipBackfillSinceJoin, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	message := extmsg.ExternalInboundMessage{
		Conversation: ref, Actor: extmsg.ExternalActor{ID: "github:42", DisplayName: "Person"},
		ProviderMessageID: "public-1", Text: "Inspect once", ReceivedAt: time.Now().UTC(),
	}
	deliveries := []extmsg.PublicRecipient{{SessionID: info.ID, Policy: extmsg.PublicMessagePolicy{When: opjournal.WhenBoundary}}}
	return e, state, message, deliveries
}

func publicAdmissionBody(t *testing.T, message extmsg.ExternalInboundMessage, key string, recipients []extmsg.PublicRecipient) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"message": message, "request_key": key, "deliveries": recipients})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPublicInboundDurableAdmissionOneAppendOneLaneAndProcessReplay(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "room-key", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "public admission", first, http.StatusOK)
	got := decodeInto[extmsg.InboundResult](t, first)
	if got.Admission == nil || got.TranscriptEntry == nil || len(got.Deliveries) != 1 {
		t.Fatalf("receipt=%s", first.Body.String())
	}
	if rows := e.opRows(t); len(rows) != 0 {
		t.Fatalf("HTTP acceptance also dispatched legacy notice: %v", rows)
	}
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	ops := e.opRows(t)
	if len(ops) != 1 {
		t.Fatalf("lane op count=%d", len(ops))
	}
	op, err := e.opStore(t).Get(ops[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if op.Policy.Guarantee != opjournal.GuaranteeAtMostOnce || op.Causation != got.TranscriptEntry.ID ||
		op.Correlation != got.Admission.ID || op.Conversation != targets[0].SessionID {
		t.Fatalf("native op=%+v", op)
	}
	// A new API process has no in-memory idempotency or delivery state.
	e.sm = NewSupervisorMux(e.resolver, nil, false, "test", "", time.Now())
	e.h = wrapTestSupervisorMiddleware(e.sm)
	retry := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "replay", retry, http.StatusOK)
	again := decodeInto[extmsg.InboundResult](t, retry)
	if again.Admission == nil || again.Admission.ID != got.Admission.ID ||
		len(again.Deliveries) != 1 || again.Deliveries[0].OperationID != op.ID {
		t.Fatalf("replay=%s", retry.Body.String())
	}
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.opRows(t)) != 1 {
		t.Fatal("replay dispatched twice")
	}
	entries, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation,
	})
	if err != nil || len(entries) != 1 {
		t.Fatalf("public entries=%d error=%v", len(entries), err)
	}
}

func TestPublicInboundChangedFrozenRequestConflict(t *testing.T) {
	for _, what := range []string{"text", "target", "policy"} {
		t.Run(what, func(t *testing.T) {
			e, _, message, targets := newPublicAdmissionEnv(t)
			first := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "one-key", targets))
			wantStatus(t, "first", first, http.StatusOK)
			switch what {
			case "text":
				message.Text = "changed"
			case "target":
				targets[0].SessionID = "other-city-session"
			case "policy":
				targets[0].Policy.When = opjournal.WhenSteer
			}
			changed := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "one-key", targets))
			wantStatus(t, "changed replay", changed, http.StatusConflict)
			if problemCode(t, changed) != "conflict-concurrent-modify" {
				t.Fatalf("body=%s", changed.Body.String())
			}
		})
	}
}

func TestPublicInboundRejectsUnreadyAndNonmemberBeforeAppend(t *testing.T) {
	for _, what := range []string{"no-lane", "no-messaging-batch", "nonmember", "unknown-session"} {
		t.Run(what, func(t *testing.T) {
			e, state, message, targets := newPublicAdmissionEnv(t)
			status := http.StatusServiceUnavailable
			switch what {
			case "no-lane":
				state.kinds = nil
			case "no-messaging-batch":
				state.messaging = beads.MailStore{Store: publicAdmissionNoBatch{Store: state.messaging.Store}}
			case "nonmember":
				targets[0].SessionID = e.session(t, "not a member").ID
				status = http.StatusBadRequest
			case "unknown-session":
				targets[0].SessionID = "foreign-city-session"
				status = http.StatusBadRequest
			}
			rec := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "invalid-key", targets))
			wantStatus(t, "reject", rec, status)
			entries, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{
				Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation,
			})
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejection appended entries=%d err=%v", len(entries), err)
			}
			if len(e.opRows(t)) != 0 {
				t.Fatal("rejection created native op")
			}
		})
	}
}

type publicAdmissionNoBatch struct{ beads.Store }

func TestPublicInboundConcurrentUnknownAcknowledgmentRecovery(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "concurrent-key", targets)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.post(t, "/extmsg/inbound", "", body)
			if rec.Code != http.StatusOK {
				t.Errorf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.opRows(t)) != 1 {
		t.Fatalf("native ops=%d", len(e.opRows(t)))
	}
	// Changing current membership after admission must not prevent recovery of
	// the frozen receipt; it also must not add a new recipient.
	state.kinds = nil
	rec := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "receipt with lane offline", rec, http.StatusOK)
}

func makePublicNamedFixture(t *testing.T, e *opsEnv, state *publicAdmissionTestState, message extmsg.ExternalInboundMessage, id string) {
	t.Helper()
	err := e.store.SetMetadataBatch(id, map[string]string{
		"configured_named_session": "true", "configured_named_identity": "myrig/worker",
		"configured_named_mode": "on_demand", "session_name": "test-city--worker",
		"alias": "myrig/worker", "template": "myrig/worker", "continuity_eligible": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.ExtMsgServices().Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{Conversation: message.Conversation, AgentName: "myrig/worker", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPublicInboundRejectsSameRecipientThroughNamedAndSessionAliases(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	makePublicNamedFixture(t, e, state, message, targets[0].SessionID)
	targets = append(targets, extmsg.PublicRecipient{AgentIdentity: "myrig/worker"})
	rec := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "aliases", targets))
	wantStatus(t, "duplicate alias", rec, http.StatusBadRequest)
	if len(e.opRows(t)) != 0 {
		t.Fatal("duplicate alias dispatched")
	}
	entries, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation})
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestPublicInboundNamedAndSessionFormsShareRecipientOrdering(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	makePublicNamedFixture(t, e, state, message, targets[0].SessionID)
	first := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "form-one", targets))
	wantStatus(t, "first", first, http.StatusOK)
	a := decodeInto[extmsg.InboundResult](t, first)
	message.ProviderMessageID = "public-two"
	second := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "form-two", []extmsg.PublicRecipient{{AgentIdentity: "myrig/worker"}}))
	wantStatus(t, "second", second, http.StatusOK)
	b := decodeInto[extmsg.InboundResult](t, second)
	if len(a.Deliveries) != 1 || len(b.Deliveries) != 1 || b.Deliveries[0].AfterDeliveryID != a.Deliveries[0].ID {
		t.Fatalf("recipient forms reordered: first=%+v second=%+v", a.Deliveries, b.Deliveries)
	}
}

type publicSessionsReadFailure struct {
	*opsTestStore
	target string
}

func (s *publicSessionsReadFailure) Get(id string) (beads.Bead, error) {
	if id == s.target {
		return beads.Bead{}, errors.New("injected Sessions read unavailable")
	}
	return s.opsTestStore.Get(id)
}

func TestPublicInboundTransientSessionsReadIsRetryableBeforeAppend(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	state.sessionsBeadStore = &publicSessionsReadFailure{opsTestStore: e.store, target: targets[0].SessionID}
	rec := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "read-failure", targets))
	wantStatus(t, "transient Sessions read", rec, http.StatusServiceUnavailable)
	entries, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation})
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries=%v error=%v", entries, err)
	}
}

func TestPublicInboundInvalidationIsObservableAndReceiptBound(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "event-one", targets)
	rec := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit", rec, http.StatusOK)
	admitted := decodeInto[extmsg.InboundResult](t, rec)
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	found, err := state.EventProvider().List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil || len(found) != 1 {
		t.Fatalf("public invalidation=%v err=%v", found, err)
	}
	var payload struct {
		AdmissionID  string `json:"admission_id"`
		TranscriptID string `json:"transcript_id"`
	}
	if err := json.Unmarshal(found[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AdmissionID != admitted.Admission.ID || payload.TranscriptID != admitted.TranscriptEntry.ID {
		t.Fatalf("causal invalidation=%+v", payload)
	}
	retry := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "retry", retry, http.StatusOK)
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	found, err = state.EventProvider().List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil || len(found) != 1 {
		t.Fatalf("acknowledged invalidation repeated=%v err=%v", found, err)
	}
}
