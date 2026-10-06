package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// contractMailFamily covers the mailbox lifecycle with read state, thread,
// and archive checked on the read path.
func contractMailFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName
	// Mail addresses resolve to sessions.
	box := h.createAgentSession(t, "contract mailbox").SessionName
	h.mailbox = box
	sent, err := c.SendMailWithResponse(ctx, city, &genclient.SendMailParams{XGCRequest: contractCSRF},
		genclient.SendMailJSONRequestBody{To: box, From: ptr("api-contract"), Subject: "contract hello", Body: ptr("first")})
	expectStatus(t, "send mail", sent, err, http.StatusCreated)
	msg := mustJSON(t, "send mail", sent.JSON201, sent)
	if msg.Read || msg.Subject != "contract hello" {
		t.Fatalf("sent message = %s", contractBody(sent))
	}

	inbox, err := c.GetV0CityByCityNameMailWithResponse(ctx, city, &genclient.GetV0CityByCityNameMailParams{Agent: ptr(box)})
	expectStatus(t, "list mail", inbox, err, http.StatusOK)
	if !strings.Contains(string(inbox.Body), msg.Id) {
		t.Fatalf("inbox missing %s: %s", msg.Id, inbox.Body)
	}
	h.expectUnread(t, 1)

	got, err := c.GetV0CityByCityNameMailByIdWithResponse(ctx, city, msg.Id, nil)
	expectStatus(t, "get mail", got, err, http.StatusOK)
	read, err := c.PostV0CityByCityNameMailByIdReadWithResponse(ctx, city, msg.Id,
		&genclient.PostV0CityByCityNameMailByIdReadParams{XGCRequest: contractCSRF})
	expectStatus(t, "mark read", read, err, http.StatusOK)
	h.expectUnread(t, 0)
	unread, err := c.PostV0CityByCityNameMailByIdMarkUnreadWithResponse(ctx, city, msg.Id,
		&genclient.PostV0CityByCityNameMailByIdMarkUnreadParams{XGCRequest: contractCSRF})
	expectStatus(t, "mark unread", unread, err, http.StatusOK)
	h.expectUnread(t, 1)

	reply, err := c.ReplyMailWithResponse(ctx, city, msg.Id, &genclient.ReplyMailParams{XGCRequest: contractCSRF},
		genclient.ReplyMailJSONRequestBody{From: ptr(box), Body: ptr("reply")})
	expectStatus(t, "reply mail", reply, err, http.StatusCreated)
	replyMsg := mustJSON(t, "reply mail", reply.JSON201, reply)
	threadID := msg.Id
	if msg.ThreadId != nil && *msg.ThreadId != "" {
		threadID = *msg.ThreadId
	}
	thread, err := c.GetV0CityByCityNameMailThreadByIdWithResponse(ctx, city, threadID, nil)
	expectStatus(t, "mail thread", thread, err, http.StatusOK)
	if !strings.Contains(string(thread.Body), replyMsg.Id) {
		t.Fatalf("thread %s missing reply %s: %s", threadID, replyMsg.Id, thread.Body)
	}

	archived, err := c.PostV0CityByCityNameMailByIdArchiveWithResponse(ctx, city, msg.Id,
		&genclient.PostV0CityByCityNameMailByIdArchiveParams{XGCRequest: contractCSRF})
	expectStatus(t, "archive mail", archived, err, http.StatusOK)
	deleted, err := c.DeleteV0CityByCityNameMailByIdWithResponse(ctx, city, replyMsg.Id,
		&genclient.DeleteV0CityByCityNameMailByIdParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete mail", deleted, err, http.StatusOK)
	missing, err := c.GetV0CityByCityNameMailByIdWithResponse(ctx, city, "hq-nosuchmail", nil)
	expectStatus(t, "get missing mail", missing, err, http.StatusNotFound)
}

func (h *contractHarness) expectUnread(t *testing.T, want int64) {
	t.Helper()
	count, err := h.client.GetV0CityByCityNameMailCountWithResponse(h.ctx, contractCityName,
		&genclient.GetV0CityByCityNameMailCountParams{Agent: ptr(h.mailbox)})
	expectStatus(t, "mail count", count, err, http.StatusOK)
	if got := mustJSON(t, "mail count", count.JSON200, count).Unread; got != want {
		t.Fatalf("unread = %d, want %d: %s", got, want, contractBody(count))
	}
}

// contractConfigFamily covers providers, agent/provider patches, packs,
// services, waits, and the city-level suspend toggle. It runs last among
// the mutating families because suspending the city pauses reconciliation.
func contractConfigFamily(t *testing.T, h *contractHarness) {
	c, ctx, city := h.client, h.ctx, contractCityName

	providers, err := c.GetV0CityByCityNameProvidersWithResponse(ctx, city)
	expectStatus(t, "list providers", providers, err, http.StatusOK)
	public, err := c.GetV0CityByCityNameProvidersPublicWithResponse(ctx, city)
	expectStatus(t, "list public providers", public, err, http.StatusOK)
	claude, err := c.GetV0CityByCityNameProviderByNameWithResponse(ctx, city, "claude")
	expectStatus(t, "get provider", claude, err, http.StatusOK)

	created, err := c.CreateProviderWithResponse(ctx, city, &genclient.CreateProviderParams{XGCRequest: contractCSRF},
		genclient.CreateProviderJSONRequestBody{Name: "contract-cli", Command: ptr("true"), DisplayName: ptr("Contract CLI")})
	expectStatus(t, "create provider", created, err, http.StatusCreated)
	h.expectProviderDisplayName(t, "contract-cli", "Contract CLI")
	updated, err := c.PatchV0CityByCityNameProviderByNameWithResponse(ctx, city, "contract-cli",
		&genclient.PatchV0CityByCityNameProviderByNameParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameProviderByNameJSONRequestBody{DisplayName: ptr("Contract CLI 2")})
	expectStatus(t, "patch provider", updated, err, http.StatusOK)
	h.expectProviderDisplayName(t, "contract-cli", "Contract CLI 2")

	// Provider patches overlay the provider.
	pput, err := c.PutV0CityByCityNamePatchesProvidersWithResponse(ctx, city,
		&genclient.PutV0CityByCityNamePatchesProvidersParams{XGCRequest: contractCSRF},
		genclient.PutV0CityByCityNamePatchesProvidersJSONRequestBody{Name: ptr("contract-cli"), PromptMode: ptr("none")})
	expectStatus(t, "put provider patch", pput, err, http.StatusOK)
	plist, err := c.GetV0CityByCityNamePatchesProvidersWithResponse(ctx, city)
	expectStatus(t, "list provider patches", plist, err, http.StatusOK)
	// KNOWN BUG (filed in the PR): patch reads come from the composed config,
	// which clears [patches] after applying them.
	pget, err := c.GetV0CityByCityNamePatchesProviderByNameWithResponse(ctx, city, "contract-cli")
	expectKnownBug(t, "get provider patch", pget, err, http.StatusNotFound, http.StatusOK)
	pdel, err := c.DeleteV0CityByCityNamePatchesProviderByNameWithResponse(ctx, city, "contract-cli",
		&genclient.DeleteV0CityByCityNamePatchesProviderByNameParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete provider patch", pdel, err, http.StatusOK)

	removed, err := c.DeleteV0CityByCityNameProviderByNameWithResponse(ctx, city, "contract-cli",
		&genclient.DeleteV0CityByCityNameProviderByNameParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete provider", removed, err, http.StatusOK)
	gone, err := c.GetV0CityByCityNameProviderByNameWithResponse(ctx, city, "contract-cli")
	expectStatus(t, "get deleted provider", gone, err, http.StatusNotFound)

	// Agent patches overlay the agent: suspension through a patch must show
	// on the agent read.
	aput, err := c.PutV0CityByCityNamePatchesAgentsWithResponse(ctx, city,
		&genclient.PutV0CityByCityNamePatchesAgentsParams{XGCRequest: contractCSRF},
		genclient.PutV0CityByCityNamePatchesAgentsJSONRequestBody{Name: ptr(contractAgent), Env: &map[string]string{"CONTRACT": "1"}})
	expectStatus(t, "put agent patch", aput, err, http.StatusOK)
	alist, err := c.GetV0CityByCityNamePatchesAgentsWithResponse(ctx, city)
	expectStatus(t, "list agent patches", alist, err, http.StatusOK)
	aget, err := c.GetV0CityByCityNamePatchesAgentByBaseWithResponse(ctx, city, contractAgent)
	expectKnownBug(t, "get agent patch", aget, err, http.StatusNotFound, http.StatusOK)
	adel, err := c.DeleteV0CityByCityNamePatchesAgentByBaseWithResponse(ctx, city, contractAgent,
		&genclient.DeleteV0CityByCityNamePatchesAgentByBaseParams{XGCRequest: contractCSRF})
	expectStatus(t, "delete agent patch", adel, err, http.StatusOK)

	qput, err := c.PutV0CityByCityNamePatchesAgentsWithResponse(ctx, city,
		&genclient.PutV0CityByCityNamePatchesAgentsParams{XGCRequest: contractCSRF},
		genclient.PutV0CityByCityNamePatchesAgentsJSONRequestBody{Dir: ptr(contractRig), Name: ptr(contractRigAgent), Env: &map[string]string{"CONTRACT": "1"}})
	// KNOWN BUG (filed in the PR): a patch for a rig-scoped convention agent
	// is rejected at reload ("not found in merged config") and surfaces as a
	// 500; the config is rolled back, so the reads below see no patch.
	expectKnownBug(t, "put qualified agent patch", qput, err, http.StatusInternalServerError, http.StatusOK)
	qget, err := c.GetV0CityByCityNamePatchesAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent)
	expectKnownBug(t, "get qualified agent patch", qget, err, http.StatusNotFound, http.StatusOK)
	qdel, err := c.DeleteV0CityByCityNamePatchesAgentByDirByBaseWithResponse(ctx, city, contractRig, contractRigAgent,
		&genclient.DeleteV0CityByCityNamePatchesAgentByDirByBaseParams{XGCRequest: contractCSRF})
	expectKnownBug(t, "delete qualified agent patch", qdel, err, http.StatusNotFound, http.StatusOK)

	packs, err := c.GetV0CityByCityNamePacksWithResponse(ctx, city)
	expectStatus(t, "list packs", packs, err, http.StatusOK)
	nopack, err := c.DeleteV0CityByCityNamePacksByNameWithResponse(ctx, city, "no-such-pack",
		&genclient.DeleteV0CityByCityNamePacksByNameParams{XGCRequest: contractCSRF})
	expectStatus(t, "remove unknown pack", nopack, err, http.StatusNotFound)

	services, err := c.GetV0CityByCityNameServicesWithResponse(ctx, city)
	expectStatus(t, "list services", services, err, http.StatusOK)
	nosvc, err := c.GetV0CityByCityNameServiceByNameWithResponse(ctx, city, "no-such-service")
	expectStatus(t, "get unknown service", nosvc, err, http.StatusNotFound)
	nosvcRestart, err := c.PostV0CityByCityNameServiceByNameRestartWithResponse(ctx, city, "no-such-service",
		&genclient.PostV0CityByCityNameServiceByNameRestartParams{XGCRequest: contractCSRF})
	expectStatus(t, "restart unknown service", nosvcRestart, err, http.StatusNotFound)

	waits, err := c.GetV0CityByCityNameWaitsWithResponse(ctx, city, nil)
	expectStatus(t, "list waits", waits, err, http.StatusOK)
	nowait, err := c.GetV0CityByCityNameWaitByIdWithResponse(ctx, city, "hq-nosuchwait")
	expectStatus(t, "get unknown wait", nowait, err, http.StatusNotFound)

	// City suspend/resume through PATCH, checked on the city read.
	suspended, err := c.PatchV0CityByCityNameWithResponse(ctx, city, &genclient.PatchV0CityByCityNameParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameJSONRequestBody{Suspended: ptr(true)})
	expectStatus(t, "suspend city", suspended, err, http.StatusOK)
	h.expectCitySuspended(t, true)
	resumed, err := c.PatchV0CityByCityNameWithResponse(ctx, city, &genclient.PatchV0CityByCityNameParams{XGCRequest: contractCSRF},
		genclient.PatchV0CityByCityNameJSONRequestBody{Suspended: ptr(false)})
	expectStatus(t, "resume city", resumed, err, http.StatusOK)
	h.expectCitySuspended(t, false)
}

func (h *contractHarness) expectProviderDisplayName(t *testing.T, name, want string) {
	t.Helper()
	h.readAfterWrite(t, "provider "+name+" display_name="+want, func() (bool, string) {
		got, err := h.client.GetV0CityByCityNameProviderByNameWithResponse(h.ctx, contractCityName, name)
		expectStatus(t, "get provider "+name, got, err, http.StatusOK)
		p := mustJSON(t, "get provider "+name, got.JSON200, got)
		return p.DisplayName != nil && *p.DisplayName == want, contractBody(got)
	})
}

func (h *contractHarness) expectCitySuspended(t *testing.T, want bool) {
	t.Helper()
	h.readAfterWrite(t, "city suspended", func() (bool, string) {
		got, err := h.client.GetV0CityByCityNameWithResponse(h.ctx, contractCityName)
		expectStatus(t, "get city", got, err, http.StatusOK)
		return mustJSON(t, "get city", got.JSON200, got).Suspended == want, contractBody(got)
	})
}
