package api

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// splitSessionState returns a fake State whose sessions class is RELOCATED to a
// store distinct from the work store, plus the session it seeded there.
//
// This is the shape of a converged split city: session beads minted after
// cutover exist only in the sessions binding, so a handler that resolves them
// through CityBeadStore() reads a store that never held the row. The work store
// is left deliberately empty of session beads — the assertion each test makes is
// "the handler found it", and only a sessions-class read can.
func splitSessionState(t *testing.T) (*fakeState, session.Info) {
	t.Helper()
	fs := newSessionFakeState(t)
	relocated := beads.NewMemStore()
	fs.sessionsBeadStore = relocated

	mgr := session.NewManagerWithOptions(relocated, fs.sp)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template: "myrig/worker",
		Title:    "Relocated",
		Command:  "test-agent",
		WorkDir:  t.TempDir(),
		Provider: "test-agent",
		Resume:   session.ProviderResume{},
		Hints:    runtime.Config{},
	})
	if err != nil {
		t.Fatalf("create session in relocated store: %v", err)
	}
	return fs, info
}

// TestResolveAgentSessionSubjectsReadsSessionsClass covers
// resolveAgentSessionSubjects, the worker-operation watch path. Reading the work
// store leaves session_id empty, so every worker.operation event for the agent
// is matched on session_name alone and the id-keyed signals are dropped.
func TestResolveAgentSessionSubjectsReadsSessionsClass(t *testing.T) {
	fs, info := splitSessionState(t)
	srv := New(fs)

	// The watch resolves by the agent's derived session name, so point the
	// relocated bead at it.
	sessionName := agentSessionName(fs.CityName(), "myrig/worker", fs.cfg.Workspace.SessionTemplate)
	if err := fs.sessionsBeadStore.SetMetadata(info.ID, "session_name", sessionName); err != nil {
		t.Fatalf("stamp session_name: %v", err)
	}

	gotName, gotID := srv.resolveAgentSessionSubjects("myrig/worker", fs.cfg)
	if gotName != sessionName {
		t.Fatalf("session name = %q, want %q", gotName, sessionName)
	}
	if gotID != info.ID {
		t.Fatalf("session id = %q, want %q (resolved through the work store, which never held the bead)", gotID, info.ID)
	}
}

// TestBeadListAssigneeTermsReadsSessionsClass covers beadListAssigneeTerms. The
// ?assignee filter is a WORK query, but the identifier it accepts is resolved
// against SESSION beads: read from the work store the resolve misses and the
// filter degrades to the raw term, so a list filtered by session id or alias
// silently returns nothing.
func TestBeadListAssigneeTermsReadsSessionsClass(t *testing.T) {
	fs, info := splitSessionState(t)
	srv := New(fs)

	terms := srv.beadListAssigneeTerms(context.Background(), info.ID)
	if len(terms) < 2 {
		t.Fatalf("assignee terms = %v; want the session's expanded identity set, not the bare term", terms)
	}
	if !slices.Contains(terms, info.SessionName) {
		t.Fatalf("assignee terms = %v; want them to include session_name %q", terms, info.SessionName)
	}
}

// TestNormalizeRawBeadAssigneeReadsSessionsClass covers normalizeRawBeadAssignee.
// This one is a WRITE path as well as a read: its materialize arm creates a
// session bead. Routed at the work store on a converged city that create is a
// stranded infrastructure bead the sessions binding never sees.
//
// The assignee is the CONFIGURED NAMED SESSION, not a live bead ID: that is the
// only input that reaches the create. A bead-ID assignee resolves on the first,
// non-materializing pass, so the materialize arm — the sole arm that Creates —
// never runs and the work-store assertion below cannot discriminate.
func TestNormalizeRawBeadAssigneeReadsSessionsClass(t *testing.T) {
	fs, _ := splitSessionState(t)
	srv := New(fs)

	const named = "myrig/worker"
	got, err := srv.normalizeRawBeadAssignee(context.Background(), named)
	if err != nil {
		t.Fatalf("normalizeRawBeadAssignee: %v", err)
	}
	if got == "" {
		t.Fatal("normalizeRawBeadAssignee returned empty; the named session was not resolved")
	}

	// Nothing may have been minted in the work store on the way.
	work, err := fs.cityBeadStore.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list work store: %v", err)
	}
	if len(work) != 0 {
		t.Fatalf("STRANDED WRITE: work store holds %d bead(s) after an assignee normalize; session-class writes must not land there: %v", len(work), work)
	}

	// And the materialized bead is in the sessions binding. This also proves the
	// create actually fired, so the assertion above is not passing for the
	// trivial reason that nothing was written at all.
	if _, err := session.ResolveSessionID(fs.sessionsBeadStore, named); err != nil {
		t.Fatalf("named session was not materialized into the SESSIONS store: %v", err)
	}
}

// TestExtmsgSessionSelectorsReadSessionsClass covers the two extmsg selector
// resolvers. Both feed delivery authorization, so a miss is a publish rejected
// on a conversation the session legitimately owns.
func TestExtmsgSessionSelectorsReadSessionsClass(t *testing.T) {
	fs, info := splitSessionState(t)
	srv := New(fs)

	// The handle source is stamped on the RELOCATED bead only, so the returned
	// value names which store answered. Asserting non-empty cannot: on a miss
	// extmsgSessionHandleForSelector falls back to extmsgHandleLabel(selector),
	// which is non-empty for every non-empty selector.
	const wantHandle = "relocated-alias"
	if err := fs.sessionsBeadStore.SetMetadata(info.ID, "alias", wantHandle); err != nil {
		t.Fatalf("stamp alias: %v", err)
	}

	resolve := srv.extmsgResolveSessionSelector()
	if resolve == nil {
		t.Fatal("extmsgResolveSessionSelector returned nil")
	}
	id, err := resolve(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("resolve selector: %v", err)
	}
	if id != info.ID {
		t.Fatalf("resolved selector = %q, want %q", id, info.ID)
	}

	if handle := srv.extmsgSessionHandleForSelector(info.ID); handle != wantHandle {
		t.Fatalf("extmsgSessionHandleForSelector = %q, want %q (the fallback label %q means the store that answered never held the bead)",
			handle, wantHandle, extmsgHandleLabel(info.ID))
	}
}

// TestExtmsgNotifyMembersMaterializesIntoSessionsClass covers the extmsg member
// fan-out, the PR's #1 write site.
//
// A member that names a configured session with no live bead is materialized on
// first receive: resolveSessionIDMaterializingNamedWithContext ->
// materializeNamedSessionWithContext -> handle.Create. Through the work store on
// a converged split city every cold-wake mints a `type=session` bead there — the
// sessions binding never sees it and the boot containment re-check names it.
//
// The work store is not asserted empty here: the extmsg services are backed by
// it in this fixture, so their own conversation beads live there legitimately.
// The discriminating assertion is which store the SESSION bead landed in.
func TestExtmsgNotifyMembersMaterializesIntoSessionsClass(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.sessionsBeadStore = beads.NewMemStore()

	srv := New(fs)
	t.Cleanup(srv.waitForBackground)

	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services

	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-1",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller:         caller,
		Conversation:   ref,
		SessionID:      "myrig/worker",
		BackfillPolicy: extmsg.MembershipBackfillSinceJoin,
		Owner:          extmsg.MembershipOwnerManual,
		Now:            time.Now().UTC(),
	}); err != nil {
		t.Fatalf("EnsureMembership(peer): %v", err)
	}

	// Nothing is excluded, so the fan-out must materialize the named member.
	srv.extmsgNotifyMembers(context.Background(), ref, "Alice", "human", "hello peers", "", "")
	srv.waitForBackground()

	if id, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil {
		t.Fatalf("STRANDED WRITE: session bead %q minted in the WORK store; session-class creates must land in the sessions binding", id)
	}
	if _, err := session.ResolveSessionID(fs.sessionsBeadStore, "myrig/worker"); err != nil {
		t.Fatalf("named member was not materialized into the SESSIONS store: %v", err)
	}
}

// TestMailRecipientResolutionReadsSessionsClass covers the two mail recipient
// resolvers. Both map a human-typed recipient onto a session's mailbox address,
// so reading the work store sends mail to an address no session answers.
func TestMailRecipientResolutionReadsSessionsClass(t *testing.T) {
	fs, info := splitSessionState(t)
	srv := New(fs)

	if _, err := srv.resolveMailSendRecipientWithContext(context.Background(), info.ID); err != nil {
		t.Fatalf("resolveMailSendRecipientWithContext: %v", err)
	}

	recipients := srv.resolveMailQueryRecipientsWithContext(context.Background(), info.SessionName)
	if len(recipients) == 0 {
		t.Fatal("resolveMailQueryRecipientsWithContext returned no recipients")
	}
}

// TestResolveAgentTranscriptReadsSessionKeyFromSessionsClass covers the
// session-catalog leg of resolveAgentTranscript. session_key is the provider's
// own conversation identifier and is what DiscoverTranscript uses to pick the
// right transcript file: read from the work store the catalog misses, the key
// stays empty, and GET /agent/{name}/output falls back to workdir-only
// discovery — which on a shared workdir serves ANOTHER agent's transcript.
func TestResolveAgentTranscriptReadsSessionKeyFromSessionsClass(t *testing.T) {
	fs, info := splitSessionState(t)
	srv := New(fs)

	sessionName := agentSessionName(fs.CityName(), "myrig/worker", fs.cfg.Workspace.SessionTemplate)
	if err := fs.sessionsBeadStore.SetMetadata(info.ID, "session_name", sessionName); err != nil {
		t.Fatalf("stamp session_name: %v", err)
	}
	const wantKey = "relocated-session-key"
	if err := fs.sessionsBeadStore.SetMetadata(info.ID, "session_key", wantKey); err != nil {
		t.Fatalf("stamp session_key: %v", err)
	}

	agentCfg, ok := findAgent(fs.cfg, "myrig/worker")
	if !ok {
		t.Fatal("fixture agent myrig/worker not found in config")
	}
	state, err := srv.resolveAgentTranscript("myrig/worker", agentCfg)
	if err != nil {
		t.Fatalf("resolveAgentTranscript: %v", err)
	}
	if state.sessionID != info.ID {
		t.Fatalf("session id = %q, want %q", state.sessionID, info.ID)
	}
	if state.sessionKey != wantKey {
		t.Fatalf("session key = %q, want %q (read from the work store, which never held the bead)", state.sessionKey, wantKey)
	}
}

// TestSessionClassRoutingIsIdentityOnSingleStoreCity is the byte-identity proof
// for a city that relocates nothing: every accessor the fixes moved to resolves
// to the exact store value CityBeadStore() returns, so each substitution is a
// no-op there. A regression that made SessionsBeadStore() diverge at the default
// backend would fail here rather than in production.
func TestSessionClassRoutingIsIdentityOnSingleStoreCity(t *testing.T) {
	fs := newSessionFakeState(t)
	if fs.sessionsBeadStore != nil {
		t.Fatal("fixture must not relocate the sessions class")
	}
	if got := fs.SessionsBeadStore().Store; got != fs.CityBeadStore() {
		t.Fatal("default backend: SessionsBeadStore().Store must be the identical value CityBeadStore() returns")
	}
}
