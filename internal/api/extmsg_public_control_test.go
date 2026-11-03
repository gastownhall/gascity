package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/opjournal"
)

func publicCancelBody(t *testing.T, body string) string {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	v["cancel_deliveries"] = true
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

type publicControlProjection struct {
	Admission struct {
		ID              string `json:"id"`
		CancelRequested bool   `json:"cancel_requested"`
	} `json:"admission"`
	Deliveries []struct {
		ID               string `json:"id"`
		State            string `json:"state"`
		StateVersion     uint64 `json:"state_version"`
		OperationID      string `json:"operation_id"`
		OperationOutcome string `json:"operation_outcome"`
		ErrorCode        string `json:"error_code"`
		Retryable        bool   `json:"retryable"`
	} `json:"deliveries"`
}

func TestPublicCancelBeforePreparationNeverAdmitsAndCannotResurrect(t *testing.T) {
	e, _, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "cancel-before", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit", first, http.StatusOK)
	canceled := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, body))
	wantStatus(t, "cancel", canceled, http.StatusOK)
	got := decodeInto[publicControlProjection](t, canceled)
	if !got.Admission.CancelRequested || len(got.Deliveries) != 1 || got.Deliveries[0].State != "canceled" || got.Deliveries[0].OperationID != "" {
		t.Fatalf("canceled=%s", canceled.Body.String())
	}
	for range 2 {
		if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
			t.Fatal(err)
		}
		retry := e.post(t, "/extmsg/inbound", "", body)
		wantStatus(t, "original admission retry", retry, http.StatusOK)
		replay := decodeInto[publicControlProjection](t, retry)
		if replay.Deliveries[0].State != "canceled" {
			t.Fatalf("retry resurrected: %s", retry.Body.String())
		}
	}
	if len(e.opRows(t)) != 0 {
		t.Fatal("canceled before preparation admitted a native operation")
	}
}

func TestPublicCancelMissingAdmissionCreatesNothing(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "never-admitted", targets)
	rec := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, body))
	wantStatus(t, "missing cancellation", rec, http.StatusNotFound)
	rows, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation})
	if err != nil || len(rows) != 0 || len(e.opRows(t)) != 0 {
		t.Fatalf("cancel created state rows=%v err=%v", rows, err)
	}
}

func TestPublicDeliveryClosedBeforePreparationIsTerminalAndObservable(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "closed-before", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit", first, http.StatusOK)
	if err := e.store.Close(targets[0].SessionID); err != nil {
		t.Fatal(err)
	}
	if err := e.server(t).relayExtMsgPublic(context.Background()); err == nil {
		t.Fatal("closed target was not reported")
	}
	replay := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "replay receipt despite closed target", replay, http.StatusOK)
	got := decodeInto[publicControlProjection](t, replay)
	if len(got.Deliveries) != 1 || got.Deliveries[0].State != "failed" || got.Deliveries[0].Retryable || got.Deliveries[0].ErrorCode != "target_closed" {
		t.Fatalf("closed=%s", replay.Body.String())
	}
	changed, err := state.EventProvider().List(events.Filter{Type: "extmsg.public_delivery_changed"})
	if err != nil || len(changed) == 0 {
		t.Fatalf("no state invalidation: %v %v", changed, err)
	}
	if len(e.opRows(t)) != 0 {
		t.Fatal("permanently closed target admitted native operation")
	}
}

func TestPublicDeliveryTransientReadRemainsRetryableAndRecoversSameIntent(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "read-outage", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit", first, http.StatusOK)
	state.sessionsBeadStore = &publicSessionsReadFailure{opsTestStore: e.store, target: targets[0].SessionID}
	if err := e.server(t).relayExtMsgPublic(context.Background()); err == nil {
		t.Fatal("outage not reported")
	}
	replay := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "read outage receipt", replay, http.StatusOK)
	got := decodeInto[publicControlProjection](t, replay)
	if len(got.Deliveries) != 1 || got.Deliveries[0].State != "queued" || !got.Deliveries[0].Retryable {
		t.Fatalf("outage=%s", replay.Body.String())
	}
	state.sessionsBeadStore = nil
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := e.post(t, "/extmsg/inbound", "", body)
	done := decodeInto[publicControlProjection](t, recovered)
	if done.Deliveries[0].ID != got.Deliveries[0].ID || done.Deliveries[0].State != "admitted" || len(e.opRows(t)) != 1 {
		t.Fatalf("recovered=%s", recovered.Body.String())
	}
}

type publicFinalHandoffFailure struct {
	*opsTestStore
	mu   sync.Mutex
	fail bool
}

func (s *publicFinalHandoffFailure) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return s, true
}

func (s *publicFinalHandoffFailure) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail && b.Reason == "record public recipient lane handoff" && b.Items[0].Update != nil && b.Items[0].Update.Opts.Status != nil && *b.Items[0].Update.Opts.Status == "closed" {
		s.fail = false
		return beads.GuardedResult{}, errors.New("process lost after Sessions admission before Messaging receipt")
	}
	return s.MemStore.ApplyGuarded(ctx, b)
}

func TestPublicCancelUnknownHandoffUsesExistingOperationAndActualOutcome(t *testing.T) {
	for _, finished := range []bool{false, true} {
		t.Run(map[bool]string{false: "queued-native-op", true: "already-succeeded"}[finished], func(t *testing.T) {
			e, state, message, targets := newPublicAdmissionEnv(t)
			body := publicAdmissionBody(t, message, "unknown-handoff", targets)
			first := e.post(t, "/extmsg/inbound", "", body)
			wantStatus(t, "admit", first, http.StatusOK)
			state.messaging = beads.MailStore{Store: &publicFinalHandoffFailure{opsTestStore: state.messaging.Store.(*opsTestStore), fail: true}}
			if err := e.server(t).relayExtMsgPublic(context.Background()); err == nil {
				t.Fatal("expected lost handoff receipt")
			}
			rows := e.opRows(t)
			if len(rows) != 1 {
				t.Fatalf("native operations=%d", len(rows))
			}
			if finished {
				if _, err := e.opStore(t).Close(rows[0].ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "executor"); err != nil {
					t.Fatal(err)
				}
			}
			canceled := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, body))
			wantStatus(t, "cancel unknown handoff", canceled, http.StatusOK)
			got := decodeInto[publicControlProjection](t, canceled)
			if len(got.Deliveries) != 1 || got.Deliveries[0].State != "cancel_requested" {
				t.Fatalf("claimed false cancellation: %s", canceled.Body.String())
			}
			if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
				t.Fatal(err)
			}
			replay := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, body))
			result := decodeInto[publicControlProjection](t, replay)
			wantState := "canceled"
			wantOutcome := "canceled"
			if finished {
				wantState = "admitted"
				wantOutcome = "succeeded"
			}
			if result.Deliveries[0].State != wantState || result.Deliveries[0].OperationID != rows[0].ID || result.Deliveries[0].OperationOutcome != wantOutcome || len(e.opRows(t)) != 1 {
				t.Fatalf("cancel outcome=%s", replay.Body.String())
			}
		})
	}
}

func TestPublicCancelChangedOriginalPayloadConflicts(t *testing.T) {
	e, _, message, targets := newPublicAdmissionEnv(t)
	body := publicAdmissionBody(t, message, "cancel-binding", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit", first, http.StatusOK)
	message.Text = "different request"
	changed := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, publicAdmissionBody(t, message, "cancel-binding", targets)))
	wantStatus(t, "changed cancellation payload", changed, http.StatusConflict)
	original := e.post(t, "/extmsg/inbound", "", body)
	got := decodeInto[publicControlProjection](t, original)
	if got.Admission.CancelRequested {
		t.Fatal("conflicting cancel changed admission")
	}
}

func TestPublicCancelWholeAdmissionSerializesPreparedAndUnpreparedRecipients(t *testing.T) {
	e, state, message, targets := newPublicAdmissionEnv(t)
	second := e.session(t, "second room member")
	_, err := state.ExtMsgServices().Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation,
		SessionID: second.ID, Owner: extmsg.MembershipOwnerManual, BackfillPolicy: extmsg.MembershipBackfillSinceJoin, Now: message.ReceivedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	targets = append(targets, extmsg.PublicRecipient{SessionID: second.ID})
	body := publicAdmissionBody(t, message, "whole-admission", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admit two recipients", first, http.StatusOK)
	admitted := decodeInto[extmsg.InboundResult](t, first)
	state.messaging = beads.MailStore{Store: &publicFinalHandoffFailure{opsTestStore: state.messaging.Store.(*opsTestStore), fail: true}}
	fabric, err := e.server(t).publicAdmissionStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.Relay(context.Background(), admitted.Deliveries[0].ID); err == nil {
		t.Fatal("expected unknown first handoff")
	}
	canceled := e.post(t, "/extmsg/inbound", "", publicCancelBody(t, body))
	wantStatus(t, "cancel entire admission", canceled, http.StatusOK)
	got := decodeInto[publicControlProjection](t, canceled)
	if !got.Admission.CancelRequested || len(got.Deliveries) != 2 || got.Deliveries[0].State != "cancel_requested" || got.Deliveries[1].State != "canceled" {
		t.Fatalf("partial cancellation=%s", canceled.Body.String())
	}
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	retry := e.post(t, "/extmsg/inbound", "", body)
	result := decodeInto[publicControlProjection](t, retry)
	if result.Deliveries[0].State != "canceled" || result.Deliveries[1].State != "canceled" || len(e.opRows(t)) != 1 {
		t.Fatalf("whole cancellation=%s", retry.Body.String())
	}
}
