package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

func publicRoutedParityFixture(t *testing.T) (*opsEnv, *publicAdmissionTestState, extmsg.ExternalInboundMessage, []extmsg.PublicRecipient, string, string) {
	t.Helper()
	e, state, message, targets := newPublicAdmissionEnv(t)
	peer := e.session(t, "public peer")
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	_, err := state.ExtMsgServices().Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller: caller, Conversation: message.Conversation, SessionID: peer.ID,
		Owner: extmsg.MembershipOwnerManual, BackfillPolicy: extmsg.MembershipBackfillSinceJoin, Now: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	targets = append(targets, extmsg.PublicRecipient{SessionID: peer.ID, Policy: targets[0].Policy})
	services, err := extmsg.NewServicesWithSessionDirectory(state.messaging, session.NewStore(state.SessionsBeadStore()))
	if err != nil {
		t.Fatal(err)
	}
	e.fs.extmsgSvc = &services
	ids := []string{targets[0].SessionID, peer.ID}
	sort.Strings(ids)
	for n, id := range ids {
		alias := "myrig/native-first"
		if n == 1 {
			alias = "myrig/native-second"
		}
		if err := e.store.SetMetadataBatch(id, map[string]string{"alias": alias}); err != nil {
			t.Fatal(err)
		}
	}
	// Route to the LAST sorted explicit recipient. A first-recipient event
	// projection is therefore observably different from the actual route.
	routed := ids[1]
	group, err := services.Groups.EnsureGroup(context.Background(), caller, extmsg.EnsureGroupInput{
		RootConversation: message.Conversation, Mode: extmsg.GroupModeLauncher, DefaultHandle: "public-lead",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = services.Groups.UpsertParticipant(context.Background(), caller, extmsg.UpsertParticipantInput{
		GroupID: group.ID, Handle: "public-lead", SessionID: routed, Public: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	message.ExplicitTarget = "public-lead"
	message.Text = "Inspect these files.\n\nFiles attached by the sender (read these files to inspect their contents):\nAttached file \"image.png\": /fixture/owned/image.png"
	message.ReplyToMessageID = "public-parent"
	message.Attachments = []extmsg.ExternalAttachment{{ProviderID: "file-one", URL: "file:///fixture/owned/image.png", MIMEType: "image/png"}}
	return e, state, message, targets, group.ID, routed
}

func TestPublicNativeNoticeRetainsFrozenExplicitRouteAndInputReferences(t *testing.T) {
	e, state, message, targets, groupID, routed := publicRoutedParityFixture(t)
	body := publicAdmissionBody(t, message, "route-parity", targets)
	first := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "admission", first, http.StatusOK)
	got := decodeInto[extmsg.InboundResult](t, first)
	if got.TranscriptEntry == nil || got.TranscriptEntry.ReplyToMessageID != message.ReplyToMessageID ||
		len(got.TranscriptEntry.Attachments) != 1 || got.TranscriptEntry.Attachments[0] != message.Attachments[0] {
		t.Fatalf("reference metadata lost: %s", first.Body.String())
	}

	// A room alias can move after admission but before the outbox runs. It
	// must not change the target discriminator in the already admitted input.
	replacement := targets[0].SessionID
	if replacement == routed {
		replacement = targets[1].SessionID
	}
	_, err := state.ExtMsgServices().Groups.UpsertParticipant(context.Background(),
		extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.UpsertParticipantInput{
			GroupID: groupID, Handle: "public-lead", SessionID: replacement, Public: true,
		})
	if err != nil {
		t.Fatal(err)
	}
	srv := e.server(t)
	if err := srv.relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := e.opRows(t)
	if len(rows) != 2 {
		t.Fatalf("operations=%d", len(rows))
	}
	for _, row := range rows {
		op, err := e.opStore(t).Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := srv.extmsgNotice(extmsgNotifyReminder{
			Provider: message.Conversation.Provider, ConversationID: message.Conversation.ConversationID,
			ActorDisplay: message.Actor.DisplayName, ActorKind: "human", Text: message.Text,
			RecipientSelector: op.Conversation, RecipientSessionID: op.Conversation,
			Handle:         srv.extmsgSessionHandleForResolvedID(op.Conversation, op.Conversation),
			ExplicitTarget: message.ExplicitTarget, ExplicitTargetSessionID: routed,
		})
		if op.Payload != want {
			t.Fatalf("native reminder changed route or reference context for %s:\n%s\nwant:\n%s", op.Conversation, op.Payload, want)
		}
		if (op.Conversation == routed) == strings.Contains(op.Payload, "Addressed to:") {
			t.Fatalf("wrong target discriminator for %s: %s", op.Conversation, op.Payload)
		}
	}
	// A retry must recover the original route, even if the current resolver is
	// unavailable. It also must not append or admit a third operation.
	state.ExtMsgServices().Groups = publicRouteReadFailure{GroupService: state.ExtMsgServices().Groups}
	retry := e.post(t, "/extmsg/inbound", "", body)
	wantStatus(t, "frozen route replay", retry, http.StatusOK)
	if err := srv.relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.opRows(t)) != 2 {
		t.Fatal("frozen replay redelivered")
	}
}

func TestPublicInboundEventRetainsActualRoutingIdentity(t *testing.T) {
	e, state, message, targets, _, routed := publicRoutedParityFixture(t)
	first := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "route-event", targets))
	wantStatus(t, "admission", first, http.StatusOK)
	admitted := decodeInto[extmsg.InboundResult](t, first)
	if err := e.server(t).relayExtMsgPublic(context.Background()); err != nil {
		t.Fatal(err)
	}
	observed, err := state.EventProvider().List(events.Filter{Type: events.ExtMsgInbound})
	if err != nil || len(observed) != 1 {
		t.Fatalf("event=%v err=%v", observed, err)
	}
	var payload extmsg.InboundEventPayload
	if err := json.Unmarshal(observed[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.TargetSession != routed || payload.TargetAgent != "" || observed[0].Subject != routed ||
		payload.Provider != message.Conversation.Provider || payload.ConversationID != message.Conversation.ConversationID ||
		payload.Actor != message.Actor.DisplayName || payload.AdmissionID != admitted.Admission.ID ||
		payload.TranscriptID != admitted.TranscriptEntry.ID {
		t.Fatalf("event routing/causation=%+v subject=%s", payload, observed[0].Subject)
	}
}

type publicRouteReadFailure struct{ extmsg.GroupService }

func (publicRouteReadFailure) ResolveInbound(context.Context, extmsg.ExternalInboundMessage) (*extmsg.GroupRouteDecision, error) {
	return nil, errors.New("group routing storage unavailable")
}

func TestPublicInboundRoutingReadFailureDoesNotAppend(t *testing.T) {
	e, state, message, targets, _, _ := publicRoutedParityFixture(t)
	state.ExtMsgServices().Groups = publicRouteReadFailure{GroupService: state.ExtMsgServices().Groups}
	first := e.post(t, "/extmsg/inbound", "", publicAdmissionBody(t, message, "route-outage", targets))
	wantStatus(t, "routing unavailable", first, http.StatusServiceUnavailable)
	records, err := state.ExtMsgServices().Transcript.List(context.Background(), extmsg.ListTranscriptInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, Conversation: message.Conversation,
	})
	if err != nil || len(records) != 0 || len(e.opRows(t)) != 0 {
		t.Fatalf("outage append=%v operations=%d err=%v", records, len(e.opRows(t)), err)
	}
}
