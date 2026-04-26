package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

type noBroadAPISessionRetireStore struct {
	*beads.MemStore
	t *testing.T
}

func (s *noBroadAPISessionRetireStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == session.LabelSession && len(query.Metadata) == 0 {
		s.t.Fatalf("continuity retirement used broad session label scan: %+v", query)
	}
	return s.MemStore.List(query)
}

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Materialization contract
// - Wake, Suspend, and Pin
// - Close and Retirement Semantics

func TestPhase0HandleSessionSuspend_MaterializesReservedNamedIntoSuspendedState(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/worker/suspend"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("suspend status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("len(session beads) = %d, want 1 canonical bead", len(all))
	}
	if got := all[0].Metadata["state"]; got != "suspended" {
		t.Fatalf("state = %q, want suspended", got)
	}
	if got := all[0].Metadata[apiNamedSessionMetadataKey]; got != "true" {
		t.Fatalf("configured_named_session = %q, want true", got)
	}
}

func TestPhase0HandleSessionClose_AllowsConfiguredAlwaysNamedSession(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""
	fs.cfg.NamedSessions[0].Mode = "always"
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget: %v", err)
	}
	if !ok {
		t.Fatal("expected named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession: %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/"+id+"/close"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("status = %q, want closed", bead.Status)
	}
}

func TestPhase0HandleSessionClose_ClearsBeadScopedWakeAndHoldOverrides(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadataBatch(id, map[string]string{
		"pin_awake":    "true",
		"held_until":   "9999-12-31T23:59:59Z",
		"sleep_intent": "user-hold",
		"state":        "suspended",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(overrides): %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/"+id+"/close"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("status = %q, want closed", bead.Status)
	}
	for _, field := range []string{"pin_awake", "held_until", "sleep_intent"} {
		if got := bead.Metadata[field]; got != "" {
			t.Fatalf("%s = %q after close, want cleared with the terminal bead", field, got)
		}
	}
}

func TestPhase0HandleSessionWake_ClosedBeadIDDoesNotCreateSuccessor(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.Close(id); err != nil {
		t.Fatalf("Close(%s): %v", id, err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/"+id+"/wake"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("wake closed bead ID status = %d, want rejection; body: %s", rec.Code, rec.Body.String())
	}
	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("closed bead-ID wake materialized %d successor session(s), want 0", len(all))
	}
}

func TestPhase0HandleSessionWake_ClosingBeadIDDoesNotWakeOrMaterialize(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadata(id, "state", "closing"); err != nil {
		t.Fatalf("SetMetadata(state=closing): %v", err)
	}

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/"+id+"/wake"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("wake closing bead ID status = %d, want rejection; body: %s", rec.Code, rec.Body.String())
	}
	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 || all[0].ID != id {
		t.Fatalf("closing bead-ID wake materialized or replaced sessions; open beads=%v, want only original %s", all, id)
	}
	if got := all[0].Metadata["state"]; got != "closing" {
		t.Fatalf("state after rejected wake = %q, want closing", got)
	}
}

func TestPhase0HandleSessionWake_NamedIdentityAfterTerminalCloseUsesFreshCanonicalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	oldID := phase0MaterializeCityScopedNamedWorker(t, srv, fs)

	closeRec := httptest.NewRecorder()
	h.ServeHTTP(closeRec, newPostRequest(cityURL(fs, "/session/"+oldID+"/close"), nil))
	if closeRec.Code != http.StatusOK {
		t.Fatalf("close status = %d, want %d; body: %s", closeRec.Code, http.StatusOK, closeRec.Body.String())
	}

	wakeRec := httptest.NewRecorder()
	h.ServeHTTP(wakeRec, newPostRequest(cityURL(fs, "/session/worker/wake"), nil))
	if wakeRec.Code != http.StatusOK {
		t.Fatalf("wake named identity after close status = %d, want %d; body: %s", wakeRec.Code, http.StatusOK, wakeRec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel(session): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("open session bead count = %d, want 1 fresh successor", len(all))
	}
	if all[0].ID == oldID {
		t.Fatalf("named-identity wake reused closed bead %s, want fresh canonical successor", oldID)
	}
	if got := all[0].Metadata["configured_named_identity"]; got != "worker" {
		t.Fatalf("successor configured_named_identity = %q, want worker", got)
	}
}

func TestPhase0HandleSessionWake_NamedIdentitySkipsContinuityIneligibleHistoricalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	historicalID := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadataBatch(historicalID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "false",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(historical): %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/worker/wake"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake named identity with historical bead status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode wake response: %v", err)
	}
	freshID := resp["id"]
	if freshID == "" {
		t.Fatalf("wake response missing id: %#v", resp)
	}
	if freshID == historicalID {
		t.Fatalf("named-identity wake reused continuity-ineligible bead %s, want fresh canonical bead", historicalID)
	}
	fresh, err := fs.cityBeadStore.Get(freshID)
	if err != nil {
		t.Fatalf("Get(fresh %s): %v", freshID, err)
	}
	if got := fresh.Metadata["configured_named_identity"]; got != "worker" {
		t.Fatalf("fresh configured_named_identity = %q, want worker", got)
	}
	historical, err := fs.cityBeadStore.Get(historicalID)
	if err != nil {
		t.Fatalf("Get(historical %s): %v", historicalID, err)
	}
	if historical.Status == "closed" {
		t.Fatalf("historical continuity-ineligible bead %s was closed; want non-terminal history", historicalID)
	}
	if historical.Metadata["state"] != "archived" {
		t.Fatalf("historical state = %q, want archived", historical.Metadata["state"])
	}
	if historical.Metadata["state_reason"] != "continuity-ineligible-replacement" {
		t.Fatalf("historical state_reason = %q, want continuity-ineligible-replacement", historical.Metadata["state_reason"])
	}
	if historical.Metadata["retired_named_identity"] != "worker" {
		t.Fatalf("historical retired_named_identity = %q, want worker", historical.Metadata["retired_named_identity"])
	}
	if historical.Metadata["archived_at"] == "" || historical.Metadata["synced_at"] == "" {
		t.Fatalf("historical archive timestamps missing: archived_at=%q synced_at=%q", historical.Metadata["archived_at"], historical.Metadata["synced_at"])
	}
	if historical.Metadata["alias"] != "" || historical.Metadata["session_name"] != "" || historical.Metadata["session_name_explicit"] != "" {
		t.Fatalf("historical identifiers still assigned after replacement: alias=%q session_name=%q explicit=%q", historical.Metadata["alias"], historical.Metadata["session_name"], historical.Metadata["session_name_explicit"])
	}

	oldIDWake := httptest.NewRecorder()
	h.ServeHTTP(oldIDWake, newPostRequest(cityURL(fs, "/session/"+historicalID+"/wake"), nil))
	if oldIDWake.Code == http.StatusOK {
		t.Fatalf("wake archived historical bead ID status = %d, want rejection; body: %s", oldIDWake.Code, oldIDWake.Body.String())
	}
}

func TestPhase0RetireContinuityIneligibleNamedSessionIdentifiersDoesNotRestampRetiredHistory(t *testing.T) {
	fs := newSessionFakeState(t)
	store := &noBroadAPISessionRetireStore{MemStore: beads.NewMemStore(), t: t}
	fs.cityBeadStore = store
	srv := New(fs)
	archivedAt := "2026-03-01T12:00:00Z"
	historical, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"state":                     "archived",
			"state_reason":              "duplicate-repair",
			"archived_at":               archivedAt,
			"synced_at":                 archivedAt,
			"continuity_eligible":       "false",
			"configured_named_session":  "true",
			"configured_named_identity": "worker",
			"alias":                     "",
			"session_name":              "",
		},
	})
	if err != nil {
		t.Fatalf("Create(historical): %v", err)
	}

	retired, err := srv.retireContinuityIneligibleNamedSessionIdentifiers(store, apiNamedSessionSpec{Identity: "worker"})
	if err != nil {
		t.Fatalf("retireContinuityIneligibleNamedSessionIdentifiers: %v", err)
	}
	if len(retired) != 1 || retired[0].ID != historical.ID {
		t.Fatalf("retired = %#v, want existing historical bead %s returned for reassignment", retired, historical.ID)
	}
	updated, err := fs.cityBeadStore.Get(historical.ID)
	if err != nil {
		t.Fatalf("Get(historical): %v", err)
	}
	if updated.Metadata["archived_at"] != archivedAt || updated.Metadata["synced_at"] != archivedAt {
		t.Fatalf("archive timestamps restamped: archived_at=%q synced_at=%q, want %q", updated.Metadata["archived_at"], updated.Metadata["synced_at"], archivedAt)
	}
	if updated.Metadata["state_reason"] != "duplicate-repair" {
		t.Fatalf("state_reason = %q, want duplicate-repair", updated.Metadata["state_reason"])
	}
}

func TestPhase0HandleSessionWake_ContinuityEligibleArchivedBeadRequestsStart(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadataBatch(id, map[string]string{
		"state":               "archived",
		"continuity_eligible": "true",
		"archived_at":         time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetMetadataBatch(archived): %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/"+id+"/wake"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake archived continuity-eligible bead status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	updated, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := updated.Metadata["state"]; got != "creating" {
		t.Fatalf("state = %q, want creating", got)
	}
	if got := updated.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
}

func TestPhase0HandleSessionWake_NamedIdentityReassignsHistoricalStateToFreshCanonicalBead(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)
	historicalID := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	if err := fs.cityBeadStore.SetMetadataBatch(historicalID, map[string]string{
		"state":               "archived",
		"continuity_eligible": "false",
	}); err != nil {
		t.Fatalf("SetMetadataBatch(historical): %v", err)
	}
	work, err := fs.cityBeadStore.Create(beads.Bead{
		Title:    "historical named work",
		Assignee: historicalID,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}
	wait, err := fs.cityBeadStore.Create(beads.Bead{
		Title:  "historical named wait",
		Type:   session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel, "session:" + historicalID},
		Metadata: map[string]string{
			"session_id": historicalID,
			"state":      "open",
			"nudge_id":   "nudge-historical",
		},
	})
	if err != nil {
		t.Fatalf("Create(wait): %v", err)
	}
	fabric := extmsg.NewServices(fs.cityBeadStore)
	ref := extmsg.ConversationRef{
		ScopeID:        "test-city",
		Provider:       "discord",
		AccountID:      "acct",
		ConversationID: "thread-historical",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := fabric.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    historicalID,
	}); err != nil {
		t.Fatalf("Bind(historical): %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/session/worker/wake"), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("wake named identity with historical state status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode wake response: %v", err)
	}
	freshID := resp["id"]
	if freshID == "" || freshID == historicalID {
		t.Fatalf("fresh id = %q, want new canonical bead distinct from %s", freshID, historicalID)
	}
	updatedWork, err := fs.cityBeadStore.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work): %v", err)
	}
	if updatedWork.Assignee != freshID {
		t.Fatalf("work assignee = %q, want fresh canonical bead %q", updatedWork.Assignee, freshID)
	}
	updatedWait, err := fs.cityBeadStore.Get(wait.ID)
	if err != nil {
		t.Fatalf("Get(wait): %v", err)
	}
	if updatedWait.Metadata["session_id"] != freshID {
		t.Fatalf("wait session_id = %q, want fresh canonical bead %q", updatedWait.Metadata["session_id"], freshID)
	}
	if updatedWait.Status != "closed" || updatedWait.Metadata["state"] != "canceled" {
		t.Fatalf("wait status/state = %q/%q, want closed/canceled after wake cleanup", updatedWait.Status, updatedWait.Metadata["state"])
	}
	if nudges, err := session.WaitNudgeIDs(fs.cityBeadStore, historicalID); err != nil {
		t.Fatalf("WaitNudgeIDs(historical): %v", err)
	} else if len(nudges) != 0 {
		t.Fatalf("historical wait nudges = %#v, want none after reassignment", nudges)
	}
	gotBinding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation(after reassignment): %v", err)
	}
	if gotBinding == nil || gotBinding.SessionID != freshID {
		t.Fatalf("binding after reassignment = %#v, want fresh canonical bead %s", gotBinding, freshID)
	}
}

func TestPhase0HandleSessionWake_RejectsTemplateTokenOnSessionSurface(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	rec := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/session/template:worker/wake"), nil)
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("wake status = %d, want non-200 session-targeting rejection; body: %s", rec.Code, rec.Body.String())
	}

	all, err := fs.cityBeadStore.ListByLabel(session.LabelSession, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("len(session beads) = %d, want 0", len(all))
	}
}

func TestPhase0ProviderCompatibility_CreateWritesManualOrigin(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	bead, err := fs.cityBeadStore.Get(success.Session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", success.Session.ID, err)
	}
	if got := bead.Metadata["session_origin"]; got != "manual" {
		t.Fatalf("session_origin = %q, want manual", got)
	}
}

func TestPhase0AgentCompatibility_CreateWritesEphemeralOrigin(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	req := newPostRequest("/v0/sessions", strings.NewReader(`{"kind":"agent","name":"myrig/worker"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	var resp sessionResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bead, err := fs.cityBeadStore.Get(resp.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", resp.ID, err)
	}
	if got := bead.Metadata["session_origin"]; got != "ephemeral" {
		t.Fatalf("session_origin = %q, want ephemeral", got)
	}
}

func TestPhase0NamedCompatibility_MaterializeWritesNamedOrigin(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	id := phase0MaterializeCityScopedNamedWorker(t, srv, fs)
	bead, err := fs.cityBeadStore.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got := bead.Metadata["session_origin"]; got != "named" {
		t.Fatalf("session_origin = %q, want named", got)
	}
}

func phase0MaterializeCityScopedNamedWorker(t *testing.T, srv *Server, fs *fakeState) string {
	t.Helper()
	fs.cfg.Agents[0].Dir = ""
	fs.cfg.NamedSessions[0].Dir = ""

	spec, ok, err := srv.findNamedSessionSpecForTarget(fs.cityBeadStore, "worker")
	if err != nil {
		t.Fatalf("findNamedSessionSpecForTarget(worker): %v", err)
	}
	if !ok {
		t.Fatal("expected city-scoped named session spec for worker")
	}
	id, err := srv.materializeNamedSession(fs.cityBeadStore, spec)
	if err != nil {
		t.Fatalf("materializeNamedSession(worker): %v", err)
	}
	return id
}
