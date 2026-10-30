package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

type claimRoleSpy struct {
	beadslib.Storage
	requests []issueops.ClaimRequest
	result   issueops.ClaimResult
	err      error

	// getResult/getErr configure IssueReader().Get, which Claim consults to
	// tell a genuinely missing id apart from a wisp id whenever Claim's own err
	// resolves to issueops.ErrNotFound: nil getErr with a non-nil getResult
	// simulates a row the reader finds (a wisp when it carries Ephemeral or
	// NoHistory), and a non-nil getErr (normally issueops.ErrNotFound)
	// simulates a row no plane holds.
	getResult *issueops.IssueDetails
	getErr    error

	// lifecycleErr, updateResult and updateErr configure IssueLifecycle()
	// and its Update, which Claim dials (Claim:true) for a wisp the claimer
	// refused (S24-G); nilLifecycle makes IssueLifecycle hand out no role and
	// no error. updates records every Update request, so a test can assert
	// none was dialed.
	lifecycleErr error
	nilLifecycle bool
	updates      []issueops.UpdateRequest
	updateResult issueops.UpdateResult
	updateErr    error
}

func (s *claimRoleSpy) IssueLifecycle() (issueops.Lifecycle, error) {
	if s.lifecycleErr != nil {
		return nil, s.lifecycleErr
	}
	if s.nilLifecycle {
		return nil, nil
	}
	return claimLifecycleSpy{spy: s}, nil
}

// claimLifecycleSpy is the lifecycle role claimRoleSpy hands out. Only Update
// is exercised; the embedded nil Lifecycle makes any other method panic.
type claimLifecycleSpy struct {
	issueops.Lifecycle
	spy *claimRoleSpy
}

func (l claimLifecycleSpy) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	l.spy.updates = append(l.spy.updates, req)
	return l.spy.updateResult, l.spy.updateErr
}

// claimRemoteSpy is claimRoleSpy behind a remote (bd serve) handle: it answers
// ServerContext the way the beads http store does, with the capability tokens
// of the server's cached handshake. Production Claim never reads
// ServerContext, so the remote variants run the local path; they pin that
// absence, and a capability gate on the wisp claim would fail them.
type claimRemoteSpy struct {
	*claimRoleSpy
	capabilities []string
}

func (s claimRemoteSpy) ServerContext() (serverURL, bdVersion string, capabilities []string) {
	return "https://beads.example.test", "test", append([]string(nil), s.capabilities...)
}

// bdServeHandshake is what a bd serve handshake advertises for the operations a
// claim reaches. No token announces that updateIssue accepts claim (the beads
// client's own doc: "no capability token announces the member"), so a remote
// store's wisp claim must not wait for one.
var bdServeHandshake = []string{"issues.claim", "issues.get", "issues.update"}

func (s *claimRoleSpy) IssueClaimer() (issueops.Claimer, error) { return s, nil }

func (s *claimRoleSpy) Claim(_ context.Context, req issueops.ClaimRequest) (issueops.ClaimResult, error) {
	s.requests = append(s.requests, req)
	return s.result, s.err
}

// IssueReader lets claimRoleSpy double as the reader role Claim's wisp
// disambiguation consults. Only Get is exercised by any test in this file;
// Ready and List are implemented to satisfy issueops.Reader and fail loudly
// if a future test reaches them unconfigured.
func (s *claimRoleSpy) IssueReader() (issueops.Reader, error) { return s, nil }

func (s *claimRoleSpy) Get(_ context.Context, _ issueops.GetRequest) (*issueops.IssueDetails, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.getResult, nil
}

func (s *claimRoleSpy) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("claimRoleSpy.Ready not configured")
}

func (s *claimRoleSpy) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	return issueops.IssuePage{}, errors.New("claimRoleSpy.List not configured")
}

// TestClaimDialsTheRoleWithTheAssigneeAsActor pins the one detail that makes
// Claim different from ReleaseIfCurrent: there is no separate service actor.
// issueops.ClaimRequest.Actor IS the assignee — "I am actor X, I claim issue Y
// for myself" — so the store's own s.actor ("native-test") must never appear
// on the wire here, unlike Release's Actor/ExpectedAssignee split.
func TestClaimDialsTheRoleWithTheAssigneeAsActor(t *testing.T) {
	issue := &beadslib.Issue{ID: "gc-1", Title: "do the thing", Status: beadslib.StatusInProgress, Assignee: "worker-1"}
	spy := &claimRoleSpy{result: issueops.ClaimResult{Issue: issue, Changed: true}}
	store := newNativeDoltStoreForTest(spy)

	bead, claimed, err := store.Claim("gc-1", "worker-1")
	if err != nil || !claimed {
		t.Fatalf("Claim = (%v, %v, %v), want (bead, true, nil)", bead, claimed, err)
	}
	if bead.ID != "gc-1" || bead.Assignee != "worker-1" {
		t.Errorf("bead = %+v, want the claimed row back", bead)
	}
	if len(spy.requests) != 1 {
		t.Fatalf("Claim called %d times, want 1", len(spy.requests))
	}
	req := spy.requests[0]
	if req.Actor != "worker-1" {
		t.Errorf("request.Actor = %q, want the caller's assignee (worker-1), not the store's own actor", req.Actor)
	}
	if req.IssueID != "gc-1" {
		t.Errorf("request.IssueID = %q, want gc-1", req.IssueID)
	}
}

// TestClaimReportsAConflictAsNotClaimedNotAnError pins the (bool, error) idiom
// mutation-list item "map ErrAlreadyClaimed to an error" must catch: a losing
// claim attempt is a value (ok=false, err=nil), matching SQLiteStore.Claim and
// BdStore.Claim, never an error every caller would otherwise have to unwrap.
func TestClaimReportsAConflictAsNotClaimedNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a foreign holder", issueops.ErrAlreadyClaimed},
		{"an ineligible status", issueops.ErrNotClaimable},
		{"a wire-reconstructed conflict wrapping the foreign-holder sentinel", &issueops.ClaimConflictError{Err: issueops.ErrAlreadyClaimed}},
		{"a wire-reconstructed conflict wrapping the ineligible-status sentinel", &issueops.ClaimConflictError{Err: issueops.ErrNotClaimable}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &claimRoleSpy{err: tc.err}
			store := newNativeDoltStoreForTest(spy)

			bead, claimed, err := store.Claim("gc-1", "worker-1")
			if err != nil {
				t.Fatalf("error = %v, want nil: a lost claim is a value here", err)
			}
			if claimed {
				t.Fatal("claimed = true on a conflict")
			}
			if bead.ID != "" || bead.Assignee != "" {
				t.Errorf("bead = %+v, want the zero value on a conflict", bead)
			}
		})
	}
}

// TestClaimMapsAMissingIDToErrNotFound pins the deliberate divergence from
// ReleaseIfCurrent: a missing bead is a real, wrapped error here, not folded
// into the (false, nil) conflict idiom — matching SQLiteStore.Claim and
// BdStore.Claim, both of which already surface a missing id as an error.
//
// A genuinely missing id has no row for the reader to find either, so
// Claim's wisp-disambiguation Get (see the Claim doc comment) also misses, and
// the original issueops.ErrNotFound passes through unchanged rather than being
// reclassified as a wisp refusal.
func TestClaimMapsAMissingIDToErrNotFound(t *testing.T) {
	spy := &claimRoleSpy{err: issueops.ErrNotFound, getErr: issueops.ErrNotFound}
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gc-missing", "worker-1")
	if claimed {
		t.Fatal("claimed = true on a not-found row")
	}
	if err == nil {
		t.Fatal("error = nil, want a wrapped ErrNotFound: a missing bead is not a conflict")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap beads.ErrNotFound", err)
	}
	if errors.Is(err, ErrWispNotClaimable) {
		t.Errorf("error = %v, want it NOT to claim this missing id is a wisp", err)
	}
}

// TestClaimKeepsErrNotFoundWhenTheReaderFindsAnOrdinaryRow pins the window
// between Claim's two reads: the claimer missed id, but by the time the reader
// looks an ordinary issue holds it (created or promoted in between). That row
// carries no wisp-plane marker, so it is not evidence of a wisp, and Claim
// keeps the claimer's ErrNotFound rather than naming a wisp refusal that a
// retry would contradict.
func TestClaimKeepsErrNotFoundWhenTheReaderFindsAnOrdinaryRow(t *testing.T) {
	spy := &claimRoleSpy{
		err:       issueops.ErrNotFound,
		getResult: &issueops.IssueDetails{Issue: beadslib.Issue{ID: "gc-1"}},
	}
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gc-1", "worker-1")
	if claimed {
		t.Fatal("claimed = true on a not-found row")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want it to wrap beads.ErrNotFound", err)
	}
	if errors.Is(err, ErrWispNotClaimable) {
		t.Errorf("error = %v, want it NOT to name an unmarked row a wisp", err)
	}
}

// TestClaimSurfacesATransportFailure pins that anything other than the two
// named conflict sentinels (or a not-found) travels as a real error, exactly
// like ReleaseIfCurrent's equivalent guard — a transport failure folded into
// "not claimed" would make a caller believe the row was simply contested
// rather than unreachable.
func TestClaimSurfacesATransportFailure(t *testing.T) {
	spy := &claimRoleSpy{err: errors.New("dial tcp: connection refused")}
	store := newNativeDoltStoreForTest(spy)

	if _, claimed, err := store.Claim("gc-1", "worker-1"); err == nil || claimed {
		t.Fatalf("Claim = (claimed=%v, err=%v), want a surfaced transport error", claimed, err)
	}
}

// TestClaimRejectsAnEmptyAssignee pins that Claim never dials the role with a
// blank Actor — issueops.ClaimRequest.Actor becomes the issue's assignee, and
// an empty one would claim a bead for nobody.
func TestClaimRejectsAnEmptyAssignee(t *testing.T) {
	spy := &claimRoleSpy{}
	store := newNativeDoltStoreForTest(spy)

	if _, claimed, err := store.Claim("gc-1", "   "); err == nil || claimed {
		t.Fatalf("Claim with a blank assignee = (claimed=%v, err=%v), want a refusal", claimed, err)
	}
	if len(spy.requests) != 0 {
		t.Fatalf("a claim was dialed with no assignee: %+v", spy.requests)
	}
}

// claimBlockingSpy's Claim blocks until the test closes proceed, after
// signaling the test (by closing inClaim) that it has entered the role call —
// so a test can arrange a pending writer to arrive while Claim still holds
// its own acquireStorage read lock, then let Claim's claimer.Claim return
// ErrNotFound and fall into the wisp-disambiguation read.
//
// It overrides IssueClaimer and IssueReader (not just Claim) because
// claimRoleSpy's own IssueClaimer/IssueReader return the embedded
// *claimRoleSpy, which would bypass this type's blocking Claim entirely.
type claimBlockingSpy struct {
	claimRoleSpy
	inClaim chan struct{}
	proceed chan struct{}
}

func (s *claimBlockingSpy) IssueClaimer() (issueops.Claimer, error) { return s, nil }
func (s *claimBlockingSpy) IssueReader() (issueops.Reader, error)   { return s, nil }

func (s *claimBlockingSpy) Claim(_ context.Context, _ issueops.ClaimRequest) (issueops.ClaimResult, error) {
	close(s.inClaim)
	<-s.proceed
	return issueops.ClaimResult{}, issueops.ErrNotFound
}

// TestClaimDoesNotDeadlockWhenAPendingWriterArrivesWhileItHoldsItsReadLock
// pins that Claim's wisp-disambiguation read never re-takes s.mu. Claim holds
// s.mu.RLock (via acquireStorage) across its whole body; a read through s.Get
// would re-take s.mu through withReadRetry/acquireStorageGen — a second,
// nested RLock request from the SAME goroutine. Go's sync.RWMutex gives a
// blocked Lock() writer priority over new readers once one is waiting, so a
// writer (a reconnect swap, or here a plain s.mu.Lock()/Unlock() standing in
// for one) arriving while Claim holds its outer RLock would wedge that nested
// RLock behind the writer, which itself could never proceed because Claim's
// outer RLock is never released. nativeClaimTargetIsWisp reads off the
// storage handle and ctx Claim already has, so neither side can block the
// other.
func TestClaimDoesNotDeadlockWhenAPendingWriterArrivesWhileItHoldsItsReadLock(t *testing.T) {
	spy := &claimBlockingSpy{inClaim: make(chan struct{}), proceed: make(chan struct{})}
	spy.getErr = issueops.ErrNotFound
	store := newNativeDoltStoreForTest(spy)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = store.Claim("gc-1", "worker-1")
	}()

	<-spy.inClaim // Claim is inside claimer.Claim, still holding its RLock.

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		store.mu.Lock()   // the pending writer: a reconnect swap or CloseStore.
		store.mu.Unlock() //nolint:staticcheck // SA2001: the empty critical section IS the test — acquiring and releasing s.mu.Lock() is what stands in for a pending writer; there is nothing to protect because nothing is shared here.
	}()
	time.Sleep(200 * time.Millisecond) // let the writer queue behind Claim's RLock.
	close(spy.proceed)                 // let claimer.Claim return ErrNotFound.

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("DEADLOCK: Claim's wisp-disambiguation read blocked behind a pending writer while Claim still held its own RLock")
	}
	select {
	case <-writerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the pending writer never got in either: Claim never released its RLock")
	}
}

// claimReconnectSpy pairs with a store's reopen hook so a transient error from
// the disambiguation read's IssueReader().Get call exercises the real
// reconnect path. It overrides Close so a reconnect's closeStorageQuietly(old)
// (called on a detached goroutine against this value) does not nil-panic
// through claimRoleSpy's embedded, unset beadslib.Storage field.
type claimReconnectSpy struct{ *claimRoleSpy }

func (claimReconnectSpy) Close() error { return nil }

// TestClaimDoesNotSelfDeadlockWhenTheDisambiguationReadNeedsAReconnect pins
// the companion case with no other goroutine involved: a transient failure on
// a disambiguation read through s.Get(id) would run withReadRetry's
// reconnect, which takes s.mu.Lock() — a write-lock request from the SAME
// goroutine that still holds Claim's own s.mu.RLock via acquireStorage. That
// can never be granted: a goroutine cannot upgrade its own read lock to a
// write lock, and nothing else can release the RLock it holds. Claim never
// calls s.Get/withReadRetry for this read (see Claim's doc comment and
// nativeClaimTargetIsWisp), so a transient error here keeps the claimer's
// ErrNotFound instead of reaching reconnect.
func TestClaimDoesNotSelfDeadlockWhenTheDisambiguationReadNeedsAReconnect(t *testing.T) {
	spy := claimReconnectSpy{&claimRoleSpy{
		err:    issueops.ErrNotFound,
		getErr: errors.New("invalid connection"), // the withReadRetry transient signature.
	}}
	store := newNativeDoltStoreForTest(spy)
	store.reopen = func(context.Context) (beadslib.Storage, error) { return spy, nil }
	store.readRetryBudgetOverride = 2 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, claimed, err := store.Claim("gc-1", "worker-1")
		if claimed {
			t.Error("claimed = true on a not-found row")
		}
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("error = %v, want a wrapped ErrNotFound, not a manufactured wisp refusal", err)
		}
		if errors.Is(err, ErrWispNotClaimable) {
			t.Errorf("error = %v, want it NOT to claim this transient-read id is a wisp", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("SELF-DEADLOCK: Claim's disambiguation read reached a reconnect that took s.mu.Lock() under Claim's own s.mu.RLock")
	}
}

// TestGetAloneStillReconnectsOnATransientFailureWithoutHanging is the control
// for the above: it pins that the ordinary s.Get path (not nested inside
// Claim, so no outer RLock is held) still recovers through withReadRetry's
// reconnect exactly as before the Claim fix, which touched only Claim's own
// disambiguation read and nothing in Get/withReadRetry/reconnect itself.
func TestGetAloneStillReconnectsOnATransientFailureWithoutHanging(t *testing.T) {
	spy := claimReconnectSpy{&claimRoleSpy{getErr: errors.New("invalid connection")}}
	store := newNativeDoltStoreForTest(spy)
	store.reopen = func(context.Context) (beadslib.Storage, error) { return spy, nil }
	store.readRetryBudgetOverride = 2 * time.Second

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = store.Get("gc-1")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Get alone hung too: this would mean the regression was in withReadRetry/reconnect, not Claim")
	}
}

// wispClaimSpy seeds a claimRoleSpy whose claimer refuses id as not found and
// whose reader resolves it to an open wisp at row version 7, the version a
// wisp claim must not be fenced on.
func wispClaimSpy(marker string) *claimRoleSpy {
	row := beadslib.Issue{ID: "gc-wisp-1", Status: beadslib.StatusOpen, RowVersion: 7}
	switch marker {
	case "ephemeral":
		row.Ephemeral = true
	case "no_history":
		row.NoHistory = true
	}
	claimed := row
	claimed.Status = beadslib.StatusInProgress
	claimed.Assignee = "worker-1"
	claimed.RowVersion = 8
	return &claimRoleSpy{
		err:          issueops.ErrNotFound,
		getResult:    &issueops.IssueDetails{Issue: row},
		updateResult: issueops.UpdateResult{Issue: &claimed, Changed: true},
	}
}

// assertWispClaimDial pins the one Update a wisp claim may dial: the bare
// claim `bd update <id> --claim` sends, Claim:true for the assignee as actor
// and nothing else. In particular it carries no ExpectedVersion fence on the
// row version the disambiguating read observed: the update's claim re-checks
// holder and status inside its own write transaction, and the beads http
// client sends only a request with no fence, guard, patch or force through
// its claim-only fallback for a server that predates claim on updateIssue.
func assertWispClaimDial(t *testing.T, spy *claimRoleSpy) {
	t.Helper()
	if len(spy.updates) != 1 {
		t.Fatalf("Update dialed %d times, want exactly 1", len(spy.updates))
	}
	req := spy.updates[0]
	if req.ExpectedVersion != nil {
		t.Errorf("Update ExpectedVersion = %d, want no fence on the read's row version", *req.ExpectedVersion)
	}
	if want := (issueops.UpdateRequest{Actor: "worker-1", IssueID: "gc-wisp-1", Claim: true}); !reflect.DeepEqual(req, want) {
		t.Errorf("Update request = %+v, want only the bare claim %+v", req, want)
	}
}

// TestClaimClaimsAWispThroughTheUpdateOperationOnALocalStore pins S24-G for a
// local store: issueops.Claimer refuses a wisp id (its CAS addresses the issue
// table only), but every local leg's Lifecycle.Update{Claim:true} resolves the
// id on both planes, so Claim claims the wisp there instead of refusing it,
// which is what BdStore.Claim (`bd update <id> --claim`) already does.
func TestClaimClaimsAWispThroughTheUpdateOperationOnALocalStore(t *testing.T) {
	for _, marker := range []string{"ephemeral", "no_history"} {
		t.Run(marker, func(t *testing.T) {
			spy := wispClaimSpy(marker)
			store := newNativeDoltStoreForTest(spy)

			bead, claimed, err := store.Claim("gc-wisp-1", "worker-1")
			if err != nil || !claimed {
				t.Fatalf("Claim(wisp) = (%+v, %v, %v), want (bead, true, nil)", bead, claimed, err)
			}
			if bead.ID != "gc-wisp-1" || bead.Assignee != "worker-1" || bead.Status != "in_progress" {
				t.Errorf("bead = %+v, want the claimed wisp back", bead)
			}
			assertWispClaimDial(t, spy)
		})
	}
}

// TestClaimClaimsAWispThroughTheUpdateOperationOnARemoteStore pins S24-G over
// http: a remote store gets the same bare claim a local one does, whatever its
// cached handshake says. No capability token announces claim on updateIssue,
// so Claim never gates on one: a server that serves the claim claims the wisp
// here, and one that predates it refuses the wisp by name
// (TestClaimMapsAByNameUpdateRefusalToErrWispNotClaimable).
func TestClaimClaimsAWispThroughTheUpdateOperationOnARemoteStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
	}{
		{"a bd serve handshake", bdServeHandshake},
		{"no handshake cached", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := wispClaimSpy("ephemeral")
			store := newNativeDoltStoreForTest(claimRemoteSpy{claimRoleSpy: spy, capabilities: tc.caps})

			bead, claimed, err := store.Claim("gc-wisp-1", "worker-1")
			if err != nil || !claimed {
				t.Fatalf("Claim(wisp) = (%+v, %v, %v), want (bead, true, nil)", bead, claimed, err)
			}
			if bead.ID != "gc-wisp-1" || bead.Assignee != "worker-1" || bead.Status != "in_progress" {
				t.Errorf("bead = %+v, want the claimed wisp back", bead)
			}
			assertWispClaimDial(t, spy)
		})
	}
}

// TestClaimRefusesAWispByNameWhenTheStoreHasNoLifecycleRole pins the G8 named
// refusal for a store, local or remote, that hands out no lifecycle role: it
// cannot claim the wisp through the update operation, so Claim refuses it as
// ErrWispNotClaimable (never ErrNotFound, which would send the class route
// looking for the wisp in the graph store), keeps the role's error, when there
// is one, as the cause, and dials no Update at all.
func TestClaimRefusesAWispByNameWhenTheStoreHasNoLifecycleRole(t *testing.T) {
	for _, tc := range []struct {
		name         string
		remote       bool
		lifecycleErr error
	}{
		{"local, role refused", false, &beadslib.ErrUnsupported{Op: "IssueLifecycle", Backend: "dolt"}},
		{"remote, role refused", true, &beadslib.ErrUnsupported{Op: "IssueLifecycle", Backend: "http"}},
		{"local, no role", false, nil},
		{"remote, no role", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := wispClaimSpy("ephemeral")
			spy.lifecycleErr = tc.lifecycleErr
			spy.nilLifecycle = tc.lifecycleErr == nil
			var storage beadslib.Storage = spy
			if tc.remote {
				storage = claimRemoteSpy{claimRoleSpy: spy, capabilities: bdServeHandshake}
			}
			store := newNativeDoltStoreForTest(storage)

			_, claimed, err := store.Claim("gc-wisp-1", "worker-1")
			if claimed {
				t.Fatal("claimed = true on a wisp the store cannot claim")
			}
			if !errors.Is(err, ErrWispNotClaimable) {
				t.Fatalf("error = %v, want it to wrap beads.ErrWispNotClaimable", err)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("error = %v, want it NOT to read as ErrNotFound", err)
			}
			if tc.lifecycleErr != nil && !errors.Is(err, tc.lifecycleErr) {
				t.Errorf("error = %v, want the lifecycle role's own error kept as the cause", err)
			}
			if len(spy.updates) != 0 {
				t.Errorf("Update dialed %d times, want none without a lifecycle role", len(spy.updates))
			}
		})
	}
}

// TestClaimMapsAByNameUpdateRefusalToErrWispNotClaimable pins the refusal a
// remote store reaches when its server predates claim on updateIssue: the
// beads client retries the bare claim through its claim-only fallback, whose
// claim operation excludes the wisp plane, so it refuses the wisp by name
// (*ErrUnsupported, the W-ClaimRequest.Wisp ledger row). Claim reports that as
// ErrWispNotClaimable, never a bare error the hook would fail on and never
// ErrNotFound, and keeps the refusal as the cause.
func TestClaimMapsAByNameUpdateRefusalToErrWispNotClaimable(t *testing.T) {
	spy := wispClaimSpy("ephemeral")
	spy.updateResult = issueops.UpdateResult{}
	refusal := &beadslib.ErrUnsupported{Op: "updateIssue", Backend: "http"}
	spy.updateErr = fmt.Errorf("claiming a wisp is not supported by this bd serve: %w", refusal)
	store := newNativeDoltStoreForTest(claimRemoteSpy{claimRoleSpy: spy, capabilities: bdServeHandshake})

	_, claimed, err := store.Claim("gc-wisp-1", "worker-1")
	if claimed || !errors.Is(err, ErrWispNotClaimable) || errors.Is(err, ErrNotFound) {
		t.Fatalf("Claim(wisp) = (claimed=%v, err=%v), want the named ErrWispNotClaimable refusal", claimed, err)
	}
	if !errors.Is(err, refusal) {
		t.Errorf("error = %v, want the update's refusal kept as the cause", err)
	}
	assertWispClaimDial(t, spy)
}

// TestClaimReportsALostWispClaimAsNotClaimed pins CAS on the wisp path: the
// update's claim re-checks holder and status inside its write transaction, so
// a foreign holder or an ineligible status is a lost claim, (false, nil),
// exactly as the claimer's conflicts are.
func TestClaimReportsALostWispClaimAsNotClaimed(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a foreign holder", &issueops.ClaimConflictError{Err: issueops.ErrAlreadyClaimed}},
		{"an ineligible status", issueops.ErrNotClaimable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := wispClaimSpy("ephemeral")
			spy.updateResult = issueops.UpdateResult{}
			spy.updateErr = tc.err
			store := newNativeDoltStoreForTest(spy)

			bead, claimed, err := store.Claim("gc-wisp-1", "worker-1")
			if err != nil || claimed || bead.ID != "" {
				t.Fatalf("Claim(wisp) = (%+v, %v, %v), want (zero, false, nil)", bead, claimed, err)
			}
			assertWispClaimDial(t, spy)
		})
	}
}

// TestClaimOfAMissingIDNeverDialsTheWispClaim pins the class-route boundary:
// an id no plane holds stays ErrNotFound (the error that opens the hook's
// claim-time class route) and the update operation is never dialed, on a
// remote store or a local one.
func TestClaimOfAMissingIDNeverDialsTheWispClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote bool
	}{{"local", false}, {"remote", true}} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &claimRoleSpy{err: issueops.ErrNotFound, getErr: issueops.ErrNotFound}
			var storage beadslib.Storage = spy
			if tc.remote {
				storage = claimRemoteSpy{claimRoleSpy: spy, capabilities: bdServeHandshake}
			}
			store := newNativeDoltStoreForTest(storage)

			_, claimed, err := store.Claim("gc-missing", "worker-1")
			if claimed || !errors.Is(err, ErrNotFound) || errors.Is(err, ErrWispNotClaimable) {
				t.Fatalf("Claim(missing) = (claimed=%v, err=%v), want a bare ErrNotFound", claimed, err)
			}
			if len(spy.updates) != 0 {
				t.Errorf("Update dialed %d times for a missing id, want none", len(spy.updates))
			}
		})
	}
}

// TestClaimOfAWispThatVanishesBeforeTheClaimIsNotFound pins the window after
// the disambiguating read: the wisp was deleted before the update ran, so the
// update's not-found is the answer, as ErrNotFound.
func TestClaimOfAWispThatVanishesBeforeTheClaimIsNotFound(t *testing.T) {
	spy := wispClaimSpy("ephemeral")
	spy.updateResult = issueops.UpdateResult{}
	spy.updateErr = fmt.Errorf("%w: issue gc-wisp-1", issueops.ErrNotFound)
	store := newNativeDoltStoreForTest(spy)

	_, claimed, err := store.Claim("gc-wisp-1", "worker-1")
	if claimed || !errors.Is(err, ErrNotFound) {
		t.Fatalf("Claim(vanished wisp) = (claimed=%v, err=%v), want ErrNotFound", claimed, err)
	}
}
