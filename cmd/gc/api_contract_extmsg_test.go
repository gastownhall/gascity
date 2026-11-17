package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// contractExtmsgFamily covers external messaging: adapter registry, a
// conversation binding to a session, groups and participants, normalized
// inbound into the transcript, outbound, and teardown — each mutation
// checked on its read path.
func contractExtmsgFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	sess := h.createAgentSession(t, "contract extmsg")
	conv := genclient.ConversationRef{Provider: "contract", AccountId: "acct-1", ScopeId: "scope-1", ConversationId: "conv-1", Kind: genclient.Room}

	reg, err := c.RegisterExtmsgAdapterWithResponse(ctx, city, &genclient.RegisterExtmsgAdapterParams{XGCRequest: contractCSRF},
		genclient.RegisterExtmsgAdapterJSONRequestBody{Provider: conv.Provider, AccountId: conv.AccountId, Name: ptr("contract-adapter")})
	expectStatus(t, "register adapter", reg, err, http.StatusCreated)
	adapters, err := c.GetV0CityByCityNameExtmsgAdaptersWithResponse(ctx, city)
	expectStatus(t, "list adapters", adapters, err, http.StatusOK)
	if !strings.Contains(string(adapters.Body), "contract-adapter") {
		t.Fatalf("adapter list missing contract-adapter: %s", adapters.Body)
	}

	bound, err := c.PostV0CityByCityNameExtmsgBindWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgBindParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgBindJSONRequestBody{SessionId: ptr(sess.Id), Conversation: &conv})
	expectStatus(t, "bind conversation", bound, err, http.StatusOK)
	bindings, err := c.GetV0CityByCityNameExtmsgBindingsWithResponse(ctx, city, &genclient.GetV0CityByCityNameExtmsgBindingsParams{SessionId: ptr(sess.Id)})
	expectStatus(t, "list bindings", bindings, err, http.StatusOK)
	if !strings.Contains(string(bindings.Body), conv.ConversationId) {
		t.Fatalf("bindings for %s missing %s: %s", sess.Id, conv.ConversationId, bindings.Body)
	}

	group, err := c.EnsureExtmsgGroupWithResponse(ctx, city, &genclient.EnsureExtmsgGroupParams{XGCRequest: contractCSRF},
		genclient.EnsureExtmsgGroupJSONRequestBody{RootConversation: &conv, Mode: ptr("launcher"), DefaultHandle: ptr("contract")})
	expectStatus(t, "ensure group", group, err, http.StatusCreated)
	groupID := mustJSON(t, "ensure group", group.JSON201, group).ID
	groups, err := c.GetV0CityByCityNameExtmsgGroupsWithResponse(ctx, city, &genclient.GetV0CityByCityNameExtmsgGroupsParams{
		Provider: ptr(conv.Provider), AccountId: ptr(conv.AccountId), ScopeId: ptr(conv.ScopeId), ConversationId: ptr(conv.ConversationId), Kind: ptr(string(conv.Kind)),
	})
	expectStatus(t, "list groups", groups, err, http.StatusOK)
	if !strings.Contains(string(groups.Body), groupID) {
		t.Fatalf("groups missing %s: %s", groupID, groups.Body)
	}
	joined, err := c.PostV0CityByCityNameExtmsgParticipantsWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgParticipantsParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgParticipantsJSONRequestBody{GroupId: groupID, Handle: "contract", SessionId: sess.Id})
	expectStatus(t, "upsert participant", joined, err, http.StatusOK)

	inbound, err := c.PostV0CityByCityNameExtmsgInboundWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgInboundParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgInboundJSONRequestBody{Message: &genclient.ExternalInboundMessage{
			Actor:             genclient.ExternalActor{Id: "user-1", DisplayName: "Contract User"},
			Conversation:      conv,
			ProviderMessageId: "msg-1",
			ReceivedAt:        time.Now().UTC(),
			Text:              "hello from the contract suite",
		}})
	expectStatus(t, "inbound message", inbound, err, http.StatusOK)
	transcript, err := c.GetV0CityByCityNameExtmsgTranscriptWithResponse(ctx, city, &genclient.GetV0CityByCityNameExtmsgTranscriptParams{
		Provider: ptr(conv.Provider), AccountId: ptr(conv.AccountId), ScopeId: ptr(conv.ScopeId), ConversationId: ptr(conv.ConversationId), Kind: ptr(string(conv.Kind)),
	})
	expectStatus(t, "transcript", transcript, err, http.StatusOK)
	if !strings.Contains(string(transcript.Body), "hello from the contract suite") {
		t.Fatalf("transcript missing the inbound message: %s", transcript.Body)
	}
	acked, err := c.PostV0CityByCityNameExtmsgTranscriptAckWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgTranscriptAckParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgTranscriptAckJSONRequestBody{SessionId: sess.Id, Conversation: &conv, Sequence: ptr(int64(1))})
	expectStatus(t, "transcript ack", acked, err, http.StatusOK)

	// The registered adapter has no callback, so delivery fails; outbound
	// must answer the documented typed refusal, not a 500.
	outbound, err := c.PostV0CityByCityNameExtmsgOutboundWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgOutboundParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgOutboundJSONRequestBody{SessionId: sess.Id, Conversation: &conv, Text: ptr("reply")})
	expectStatus(t, "outbound message", outbound, err, http.StatusOK, http.StatusUnprocessableEntity)

	left, err := c.DeleteV0CityByCityNameExtmsgParticipantsWithResponse(ctx, city, &genclient.DeleteV0CityByCityNameExtmsgParticipantsParams{XGCRequest: contractCSRF},
		genclient.DeleteV0CityByCityNameExtmsgParticipantsJSONRequestBody{GroupId: groupID, Handle: "contract"})
	expectStatus(t, "remove participant", left, err, http.StatusOK)
	unbound, err := c.PostV0CityByCityNameExtmsgUnbindWithResponse(ctx, city, &genclient.PostV0CityByCityNameExtmsgUnbindParams{XGCRequest: contractCSRF},
		genclient.PostV0CityByCityNameExtmsgUnbindJSONRequestBody{SessionId: ptr(sess.Id), Conversation: &conv})
	expectStatus(t, "unbind conversation", unbound, err, http.StatusOK)
	after, err := c.GetV0CityByCityNameExtmsgBindingsWithResponse(ctx, city, &genclient.GetV0CityByCityNameExtmsgBindingsParams{SessionId: ptr(sess.Id)})
	expectStatus(t, "list bindings after unbind", after, err, http.StatusOK)
	if strings.Contains(string(after.Body), conv.ConversationId) {
		t.Fatalf("binding still listed after unbind: %s", after.Body)
	}
	unreg, err := c.DeleteV0CityByCityNameExtmsgAdaptersWithResponse(ctx, city, &genclient.DeleteV0CityByCityNameExtmsgAdaptersParams{XGCRequest: contractCSRF},
		genclient.DeleteV0CityByCityNameExtmsgAdaptersJSONRequestBody{Provider: conv.Provider, AccountId: conv.AccountId})
	expectStatus(t, "unregister adapter", unreg, err, http.StatusOK)
}
