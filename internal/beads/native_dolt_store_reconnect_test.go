package beads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

// These tests exercise the native read-path reconnect: a read against the
// initial (dead) handle fails with a transient connection error, the injected
// reopen hook hands back a fresh (healthy) handle, and the retry succeeds. The
// reopen hook stands in for the store factory's real hook, which re-resolves the
// current managed Dolt port and re-opens against the live server.

func healthySearchStorage(issues ...*beadslib.Issue) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return issues, nil
		},
	}
}

func deadSearchStorage(err error) *nativeDoltStorageSpy {
	return &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, err
		},
	}
}

// storeWithReopen builds a test NativeDoltStore starting on dead and swapping to
// fresh via the reopen hook; reopens counts hook invocations.
func storeWithReopen(dead beadslib.Storage, fresh beadslib.Storage, reopens *int32) *NativeDoltStore {
	store := newNativeDoltStoreForTest(dead)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(reopens, 1)
		return fresh, nil
	}
	return store
}

func TestNativeDoltStoreGetReconnectsAndInstallsFreshStorage(t *testing.T) {
	fresh := healthySearchStorage(&beadslib.Issue{
		ID: "gc-existing", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	fresh.createIssue = func(_ context.Context, issue *beadslib.Issue, _ string) error {
		issue.ID = "gc-created"
		return nil
	}
	errDeadCreate := errors.New("create reached dead storage")
	dead := deadSearchStorage(errors.New("begin read tx: dial tcp 127.0.0.1:58216: i/o timeout"))
	dead.createIssue = func(context.Context, *beadslib.Issue, string) error {
		return errDeadCreate
	}
	store := newNativeDoltStoreForTest(dead)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		return fresh, nil
	}

	got, err := store.Get("gc-existing")
	if err != nil {
		t.Fatalf("Get after transient conn error: %v", err)
	}
	if got.ID != "gc-existing" {
		t.Fatalf("Get.ID = %q, want gc-existing", got.ID)
	}

	created, err := store.Create(Bead{Title: "created after reconnect", Type: "task"})
	if err != nil {
		t.Fatalf("Create after reconnect: %v", err)
	}
	if created.ID != "gc-created" {
		t.Fatalf("Create.ID = %q, want gc-created", created.ID)
	}
}

func TestNativeDoltStoreListReconnectsAfterTransientConnError(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-2", Title: "recovered list", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("[mysql] i/o timeout")), healthy, &reopens)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after transient conn error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-2" {
		t.Fatalf("List = %#v, want [gc-2]", got)
	}
	if n := atomic.LoadInt32(&reopens); n == 0 {
		t.Fatalf("expected the reopen hook to fire; got %d", n)
	}
}

func TestNativeDoltStoreHostedReopenProjectsCredentialCommandAgain(t *testing.T) {
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/ambient/poison")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	selectedCommand := "/selected/credential-provider"
	var openCalls int
	var projectedCommands []string
	var resolvedTokens []string
	nativeDoltOpenBestAvailable = func(ctx context.Context, _ string) (beadslib.Storage, error) {
		openCalls++
		command := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")
		projectedCommands = append(projectedCommands, command)
		if command != selectedCommand {
			return nil, errors.New("unexpected credential command projection")
		}
		var token string
		switch openCalls {
		case 1:
			token = "token-1"
		case 2:
			token = "token-2"
		default:
			return nil, errors.New("unexpected extra native open")
		}
		resolvedTokens = append(resolvedTokens, token)
		if openCalls == 1 {
			return &nativeDoltStorageSpy{
				getConfig: func(context.Context, string) (string, error) { return "gcg", nil },
				searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
					return nil, errors.New("expired hosted credential: invalid connection")
				},
			}, nil
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("credential-refresh reopen context has no deadline")
		}
		return healthySearchStorage(&beadslib.Issue{
			ID: "gcg-1", Title: token, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
		}), nil
	}

	root := t.TempDir()
	reopen := func(ctx context.Context) (NativeStorage, error) {
		return OpenNativeStorageAtWithoutAmbientEnvWithCredentialCommand(ctx, root, selectedCommand)
	}
	store, err := OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand(
		context.Background(), root, selectedCommand, WithNativeReopen(reopen))
	if err != nil {
		t.Fatalf("initial hosted open: %v", err)
	}
	t.Cleanup(func() { _ = store.CloseStore() })

	got, err := store.Get("gcg-1")
	if err != nil {
		t.Fatalf("Get after hosted credential expiry: %v", err)
	}
	if got.ID != "gcg-1" {
		t.Fatalf("Get ID = %q, want gcg-1", got.ID)
	}
	if got.Title != "token-2" {
		t.Fatalf("Get title = %q, want the credential resolved by the reopen", got.Title)
	}
	if openCalls != 2 {
		t.Fatalf("native opens = %d, want initial open plus one bounded reopen", openCalls)
	}
	if want := []string{selectedCommand, selectedCommand}; !slices.Equal(projectedCommands, want) {
		t.Fatalf("projected credential commands = %q, want %q", projectedCommands, want)
	}
	if want := []string{"token-1", "token-2"}; !slices.Equal(resolvedTokens, want) {
		t.Fatalf("resolved credentials = %q, want %q", resolvedTokens, want)
	}
	if got := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND"); got != "/ambient/poison" {
		t.Fatalf("ambient credential command after reopen = %q, want restored", got)
	}
}

func TestNativeDoltStoreReadDoesNotRetryNonTransientError(t *testing.T) {
	var reopens int32
	store := storeWithReopen(deadSearchStorage(errors.New("syntax error near 'FROM'")), healthySearchStorage(), &reopens)

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "syntax error") {
		t.Fatalf("Get error = %v, want the non-transient syntax error", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("non-transient error must not reconnect; got %d reopens", n)
	}
}

func TestNativeDoltStoreReadWithoutReopenHookDoesNotReconnect(t *testing.T) {
	// No reopen hook injected -> reconnect disabled, transient error returns as-is.
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))

	if _, err := store.Get("gc-1"); err == nil || !errContains(err, "invalid connection") {
		t.Fatalf("Get error = %v, want the transient error returned as-is (fail fast)", err)
	}
}

func TestNativeDoltStoreReconnectReopenErrorIsTerminalWhenNonTransient(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		return nil, errors.New("permission denied resolving managed dolt port")
	}
	_, err := store.Get("gc-1")
	if err == nil || !errContains(err, "reconnect after transient read error") {
		t.Fatalf("Get error = %v, want a wrapped reconnect failure", err)
	}
	if !errContains(err, "permission denied") {
		t.Fatalf("Get error = %v, want the reopen cause preserved", err)
	}
}

func TestIsNativeDoltTransientReadError(t *testing.T) {
	transient := []string{
		"begin read tx: invalid connection",
		"[mysql] i/o timeout",
		"dial tcp 127.0.0.1:3307: connect: connection refused",
		"write: broken pipe",
		"unexpected EOF",
		"use of closed network connection",
		"bad connection",
		"read: connection reset by peer",
	}
	for _, msg := range transient {
		if !isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = false, want true", msg)
		}
	}
	permanent := []string{
		"issue gc-1 not found",
		"syntax error",
		"no rows in result set",
	}
	for _, msg := range permanent {
		if isNativeDoltTransientReadError(errors.New(msg)) {
			t.Errorf("isNativeDoltTransientReadError(%q) = true, want false", msg)
		}
	}
	if isNativeDoltTransientReadError(nil) {
		t.Errorf("isNativeDoltTransientReadError(nil) = true, want false")
	}
}

func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(err.Error(), sub)
}

func nativeDoltStoreClosedForTest(s *NativeDoltStore) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

func nativeDoltStoreStateForTest(s *NativeDoltStore) (beadslib.Storage, NativeReopenFunc) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storage, s.reopen
}

func TestNativeDoltStoreCloseStoreWinsInFlightReconnect(t *testing.T) {
	var oldCloseCalls atomic.Int32
	var freshCloseCalls atomic.Int32
	var freshReadCalls atomic.Int32
	old := deadSearchStorage(errors.New("invalid connection"))
	old.close = func() error {
		oldCloseCalls.Add(1)
		return nil
	}
	fresh := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			freshReadCalls.Add(1)
			return nil, nil
		},
		close: func() error {
			freshCloseCalls.Add(1)
			return nil
		},
	}

	reopenStarted := make(chan struct{})
	releaseReopen := make(chan struct{})
	var once sync.Once
	store := newNativeDoltStoreForTest(old)
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		once.Do(func() { close(reopenStarted) })
		<-releaseReopen
		return fresh, nil
	}

	getDone := make(chan error, 1)
	go func() { _, err := store.Get("gc-1"); getDone <- err }()

	select {
	case <-reopenStarted:
	case <-time.After(time.Second):
		t.Fatal("reopen hook did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- store.CloseStore() }()

	deadline := time.Now().Add(time.Second)
	for !nativeDoltStoreClosedForTest(store) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !nativeDoltStoreClosedForTest(store) {
		t.Fatal("CloseStore did not latch the store closed")
	}
	if storage, _, release, err := store.acquireStorageGen(); !errors.Is(err, ErrStoreClosed) {
		if release != nil {
			release()
		}
		t.Fatalf("acquireStorageGen after close latch = (%T, %v), want ErrStoreClosed", storage, err)
	}
	if storage, release, err := store.acquireStorage(); !errors.Is(err, ErrStoreClosed) {
		if release != nil {
			release()
		}
		t.Fatalf("acquireStorage after close latch = (%T, %v), want ErrStoreClosed", storage, err)
	}

	close(releaseReopen)
	select {
	case err := <-getDone:
		if !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("Get racing CloseStore = %v, want ErrStoreClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Get did not return after reopen was released")
	}
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("CloseStore: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseStore did not return after reopen was released")
	}

	storage, reopen := nativeDoltStoreStateForTest(store)
	if storage != nil || reopen != nil {
		t.Fatalf("closed store state = (storage=%T, reopen=%v), want both nil", storage, reopen != nil)
	}
	deadline = time.Now().Add(time.Second)
	for freshCloseCalls.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := oldCloseCalls.Load(); got != 1 {
		t.Fatalf("old storage close calls = %d, want 1", got)
	}
	if got := freshCloseCalls.Load(); got != 1 {
		t.Fatalf("fresh storage close calls = %d, want 1", got)
	}
	if got := freshReadCalls.Load(); got != 0 {
		t.Fatalf("fresh storage read calls = %d, want 0", got)
	}
	if _, err := store.Get("gc-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after CloseStore = %v, want ErrStoreClosed", err)
	}
}

func TestNativeDoltStoreReadRetrySharesOneWallClockBudget(t *testing.T) {
	const budget = 100 * time.Millisecond
	firstReadDeadline := make(chan time.Time, 1)
	reopenDeadline := make(chan time.Time, 1)
	dead := &nativeDoltStorageSpy{
		searchIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			deadline, _ := ctx.Deadline()
			firstReadDeadline <- deadline
			time.Sleep(10 * time.Millisecond)
			return nil, errors.New("invalid connection")
		},
	}
	stillDead := deadSearchStorage(errors.New("invalid connection"))
	store := newNativeDoltStoreForTest(dead)
	store.readRetryBudgetOverride = budget
	store.reopen = func(ctx context.Context) (beadslib.Storage, error) {
		deadline, _ := ctx.Deadline()
		reopenDeadline <- deadline
		time.Sleep(10 * time.Millisecond)
		return stillDead, nil
	}

	started := time.Now()
	_, err := store.Get("gc-1")
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get error = %v, want context deadline exceeded", err)
	}
	if !isNativeDoltTransientReadError(err) {
		t.Fatalf("Get error = %v, want the last transient cause preserved", err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 400*time.Millisecond {
		t.Fatalf("Get elapsed = %s, want one %s wall-clock budget", elapsed, budget)
	}
	read := <-firstReadDeadline
	var reopen time.Time
	select {
	case reopen = <-reopenDeadline:
	case <-time.After(time.Second):
		t.Fatal("reopen did not receive the shared retry context")
	}
	if !read.Equal(reopen) {
		t.Fatalf("read deadline = %s, reopen deadline = %s; want one shared deadline", read, reopen)
	}
}

func TestNativeDoltStoreReadRetryBudgetBoundsReconnectGateWait(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.readRetryBudgetOverride = 40 * time.Millisecond
	var reopens atomic.Int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		reopens.Add(1)
		return healthySearchStorage(), nil
	}

	gate, err := store.acquireReconnectGate(context.Background())
	if err != nil {
		t.Fatalf("acquire reconnect gate: %v", err)
	}
	defer store.releaseReconnectGate(gate)

	started := time.Now()
	_, err = store.Get("gc-1")
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get waiting for reconnect gate = %v, want context deadline exceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("Get waiting for reconnect gate took %s, want <= 200ms", elapsed)
	}
	if got := reopens.Load(); got != 0 {
		t.Fatalf("reopen calls while reconnect gate held = %d, want 0", got)
	}
}

func TestNativeDoltStoreNilReadReturnsStoreClosed(t *testing.T) {
	var store *NativeDoltStore
	if _, err := store.Get("gc-1"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("nil store Get = %v, want ErrStoreClosed", err)
	}
}

// TestNativeDoltStoreConcurrentReadersReopenOnce pins single-flight: many
// readers racing a dead handle trigger exactly one reopen; the losers discard
// and retry against the installed handle.
func TestNativeDoltStoreConcurrentReadersReopenOnce(t *testing.T) {
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "recovered", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	var reopens int32
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("invalid connection")))
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return healthy, nil
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.Get("gc-1")
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Fatalf("reader %d: %v", i, e)
		}
	}
	if got := atomic.LoadInt32(&reopens); got != 1 {
		t.Fatalf("reopen called %d times, want exactly 1 (single-flight)", got)
	}
}

// TestClassifyNativeDoltReadErrorOrder pins the classification ORDER, which is
// the whole content of the table: every rung below is reachable by an error that
// a LATER rung would also claim, so a reordering silently changes what the read
// path does with it.
//
// Every row is driven on BOTH lanes. Rungs 2 and 3 are proxied-lane only
// (council B-F1 / C-F1), so wantDirect says what a direct or hosted handle —
// the lane the rollout flag is off for — makes of the same error.
func TestClassifyNativeDoltReadErrorOrder(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want nativeReadDisposition
		// wantDirect, when set, is the DIRECT lane's disposition where it
		// differs from the proxied one. Unset means both lanes agree.
		wantDirect *nativeReadDisposition
		// verdict, when set, is the endpoint fact the class must name.
		verdict ProxiedVerdict
		why     string
	}{
		{
			name: "an indeterminate commit wrapping a deadlock is NOT a serialization retry",
			// The dangerous shape: rung 2's text signature ("Error 1213") is
			// present, so a table that looked at text first would replay a
			// write whose outcome nobody knows.
			err:     fmt.Errorf("committing bead update: %w: Error 1213 (40001): deadlock found", beadslib.ErrCommitIndeterminate),
			want:    nativeReadNonReplayable,
			verdict: ProxiedVerdictWriteIndeterminate,
			why:     "ErrCommitIndeterminate must outrank every retryable signature",
		},
		{
			name:       "a bare serialization conflict is transient on the proxied lane and nothing on the direct one",
			err:        errors.New("Error 1213 (40001): Deadlock found when trying to get lock"),
			want:       nativeReadTransient,
			wantDirect: disposition(nativeReadUnclassified),
			verdict:    ProxiedVerdictNone,
			why:        "main consulted this matcher on the WRITE path only; a 1213 on a read returned at once",
		},
		{
			name: "an open circuit with no transient signature is a cooldown, not a return",
			// This is the rung the text table could not express at all: the
			// message says nothing a substring search recognizes, so before the
			// classifier it was handed straight back to the caller.
			err:        fmt.Errorf("reading beads: %w", beadslib.ErrCircuitOpen),
			want:       nativeReadCircuitOpen,
			wantDirect: disposition(nativeReadUnclassified),
			verdict:    ProxiedVerdictCircuitOpen,
		},
		{
			name:    "MySQL 1049 is terminal and names database_gone",
			err:     errors.New("begin read tx: Error 1049 (42000): Unknown database 'beads'"),
			want:    nativeReadTerminal,
			verdict: ProxiedVerdictDatabaseGone,
		},
		{
			name:    "MySQL 1045 is terminal and names access_denied",
			err:     errors.New("Error 1045 (28000): Access denied for user 'root'@'127.0.0.1'"),
			want:    nativeReadTerminal,
			verdict: ProxiedVerdictAccessDenied,
		},
		{
			name:       "a bare io.EOF is connection-level on the proxied lane and nothing on the direct one",
			err:        fmt.Errorf("reading greeting: %w", io.EOF),
			want:       nativeReadTransient,
			wantDirect: disposition(nativeReadUnclassified),
			why:        "the sentinel rung is what the substring table could not see, and seeing more is a widening",
		},
		{
			name: "a name that does not resolve is connection-level on the proxied lane and nothing on the direct one",
			// Council C-F5 / pr2 D-F2's reproduction. *net.DNSError satisfies
			// net.Error, so IsConnectionLevel claims it; nothing in the nine
			// substrings does, so on main a misconfigured BEADS_DOLT_SERVER_HOST
			// was an instant "no such host" rather than a 90s uncancellable
			// reconnect loop against a name that will not resolve either.
			err: fmt.Errorf("dialing managed dolt: %w", &net.DNSError{
				Err: "no such host", Name: "beads-dolt.invalid", IsNotFound: true,
			}),
			want:       nativeReadTransient,
			wantDirect: disposition(nativeReadUnclassified),
		},
		{
			name: "an unreachable host is connection-level on the proxied lane and nothing on the direct one",
			err:  fmt.Errorf("dial: %w", syscall.EHOSTUNREACH),
			// "no route to host" is in no substring the table carries.
			want:       nativeReadTransient,
			wantDirect: disposition(nativeReadUnclassified),
		},
		{
			name: "ECONNREFUSED is connection-level on BOTH lanes, through the text table",
			err:  fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
			// The direct lane reaches the same answer at rung 7 — "connection
			// refused" is one of the nine substrings — which is what makes this
			// row the control for the three above: gating rung 6 took nothing
			// away that main already had.
			want:    nativeReadTransient,
			verdict: ProxiedVerdictNone,
		},
		{
			name: "the text table still backstops a driver string no sentinel carries",
			err:  errors.New("begin read tx: invalid connection"),
			want: nativeReadTransient,
		},
		{
			name: "a context deadline is NOT an endpoint state",
			err:  fmt.Errorf("read: %w", context.DeadlineExceeded),
			want: nativeReadUnclassified,
			why:  "IsIndeterminate guards the connection-level rung; our own clock says nothing about the proxy",
		},
		{
			name: "ErrNotFound is not a connection problem",
			err:  fmt.Errorf("bead %q: %w", "gc-1", ErrNotFound),
			want: nativeReadUnclassified,
		},
		{
			name: "a nil error classifies as nothing",
			err:  nil,
			want: nativeReadUnclassified,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyNativeDoltReadError(tc.err, proxiedNativeLane)
			if got.disposition != tc.want {
				t.Fatalf("disposition = %d, want %d (%s)", got.disposition, tc.want, tc.why)
			}
			if got.verdict != tc.verdict {
				t.Errorf("verdict = %q, want %q", got.verdict, tc.verdict)
			}
			if tc.want == nativeReadCircuitOpen && got.cooldown != nativeReadCircuitCooldown {
				t.Errorf("cooldown = %s, want %s", got.cooldown, nativeReadCircuitCooldown)
			}

			wantDirect := tc.want
			if tc.wantDirect != nil {
				wantDirect = *tc.wantDirect
			}
			direct := classifyNativeDoltReadError(tc.err, directNativeLane)
			if direct.disposition != wantDirect {
				t.Fatalf("direct-lane disposition = %d, want %d: the rollout flag is off for that lane and its read path must not change (%s)",
					direct.disposition, wantDirect, tc.why)
			}
		})
	}
}

// disposition returns a pointer to d, for the table's optional direct-lane
// column: nativeReadUnclassified is the zero value, so "unset" and "explicitly
// unclassified" would otherwise be the same thing.
func disposition(d nativeReadDisposition) *nativeReadDisposition { return &d }

// TestNativeDoltReadTerminalEndpointFactStopsImmediately is the behavioral half
// of rungs 4 and 5: a database that is not there must not cost the caller the
// whole retry budget, and on a proxied handle it must arrive as a verdict the
// wrapper can demote on.
func TestNativeDoltReadTerminalEndpointFactStopsImmediately(t *testing.T) {
	unknownDB := errors.New("begin read tx: Error 1049 (42000): Unknown database 'beads'")

	t.Run("direct handle returns the driver error untouched", func(t *testing.T) {
		var reopens int32
		store := newNativeDoltStoreForTest(deadSearchStorage(unknownDB))
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return healthySearchStorage(), nil
		}
		store.readRetryBudgetOverride = 2 * time.Second

		start := time.Now()
		_, err := store.Get("gc-1")
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Get took %s; a terminal endpoint fact must not spend the budget", elapsed)
		}
		if !errors.Is(err, unknownDB) {
			t.Fatalf("Get err = %v, want the driver error", err)
		}
		if _, ok := ProxiedVerdictOf(err); ok {
			t.Fatalf("a DIRECT handle rendered a proxied verdict: %v", err)
		}
		if n := atomic.LoadInt32(&reopens); n != 0 {
			t.Fatalf("reopens = %d, want 0 — a fresh pool asks the same question", n)
		}
	})

	t.Run("proxied handle names database_gone", func(t *testing.T) {
		store := newNativeDoltStoreForTest(deadSearchStorage(unknownDB))
		store.proxiedReadVerdicts = true
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			return nil, errors.New("reopen must not be reached")
		}

		_, err := store.Get("gc-1")
		verdict, ok := ProxiedVerdictOf(err)
		if !ok {
			t.Fatalf("Get err = %v, want a *ProxiedVerdictError", err)
		}
		if verdict.Verdict != ProxiedVerdictDatabaseGone {
			t.Errorf("verdict = %q, want %q", verdict.Verdict, ProxiedVerdictDatabaseGone)
		}
		if !verdict.Terminal() {
			t.Error("database_gone must be terminal: the database is not coming back inside this handle's life")
		}
		if !errors.Is(err, unknownDB) {
			t.Error("the driver error must survive as the cause")
		}
	})
}

// TestNativeDoltReadReturnsTypedVerdictFromReopen is the trap the plan's §1
// names, in executable form: the reopen hook is where the proxied escalation
// ladder runs, and the verdict it produces has to reach the caller on the FIRST
// pass. Before the propagation rule, a typed refusal was wrapped, re-classified
// as non-transient and returned — which happened to work — but a TRANSIENT-
// looking verdict cause would have been looped on until the budget expired and
// surfaced as nativeReadRetryBudgetError, which carries no verdict at all.
func TestNativeDoltReadReturnsTypedVerdictFromReopen(t *testing.T) {
	skew := NewSchemaSkewVerdictError(ProxiedSkewLaneMain, ProxiedSkewDirAhead, "database main=67, binary pins 66")
	var reopens int32
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("begin read tx: i/o timeout")))
	store.proxiedReadVerdicts = true
	store.readRetryBudgetOverride = 3 * time.Second
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		// The shape P2-12's hook produces: a re-admission that refused, with a
		// cause whose own text ("connection refused") is transient, so nothing
		// but the verdict can stop the loop.
		return nil, fmt.Errorf("re-admitting the proxy: %w",
			NewSchemaSkewVerdictError(skew.Lane, skew.Dir, skew.Detail))
	}

	start := time.Now()
	_, err := store.Get("gc-1")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Get took %s; the verdict must end the read on the first pass", elapsed)
	}
	if n := atomic.LoadInt32(&reopens); n != 1 {
		t.Fatalf("reopens = %d, want exactly 1", n)
	}
	verdict, ok := ProxiedVerdictOf(err)
	if !ok {
		t.Fatalf("Get err = %v, want a *ProxiedVerdictError recoverable through the reconnect wrap", err)
	}
	if verdict.Verdict != ProxiedVerdictSchemaSkew || verdict.Lane != ProxiedSkewLaneMain || verdict.Dir != ProxiedSkewDirAhead {
		t.Errorf("verdict = %s/%s/%s, want schema_skew/main/ahead", verdict.Verdict, verdict.Lane, verdict.Dir)
	}
	if strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("the verdict was buried under a budget error: %v", err)
	}
}

// TestNativeDoltProxiedReadBudgetExhaustionIsANonTerminalVerdict is the other
// half of the same trap. A read that cannot reach bd's proxy at all spends its
// budget and returns nativeReadRetryBudgetError — an untyped error. The wrapper
// demotes on errors.As(*ProxiedVerdictError), so without this the handle would
// stay "native" and every subsequent read would pay the budget again.
//
// budget_exhausted is NON-terminal on purpose: running out of clock is a fact
// about us, so the next open re-admits rather than being permanently demoted.
func TestNativeDoltProxiedReadBudgetExhaustionIsANonTerminalVerdict(t *testing.T) {
	store := newNativeDoltStoreForTest(deadSearchStorage(errors.New("dial tcp 127.0.0.1:45123: connect: connection refused")))
	store.proxiedReadVerdicts = true
	store.readRetryBudgetOverride = 150 * time.Millisecond
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		return nil, errors.New("dial tcp 127.0.0.1:45123: connect: connection refused")
	}

	_, err := store.Get("gc-1")
	verdict, ok := ProxiedVerdictOf(err)
	if !ok {
		t.Fatalf("Get err = %v, want a *ProxiedVerdictError", err)
	}
	if verdict.Verdict != ProxiedVerdictBudgetExhausted {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, ProxiedVerdictBudgetExhausted)
	}
	if verdict.Terminal() {
		t.Error("budget_exhausted must be non-terminal: the clock says nothing about the endpoint")
	}
	if !strings.Contains(err.Error(), "budget exhausted") {
		t.Errorf("the original budget error must survive as the cause: %v", err)
	}

	t.Run("a direct handle keeps the untyped budget error", func(t *testing.T) {
		direct := newNativeDoltStoreForTest(deadSearchStorage(errors.New("dial tcp: connection refused")))
		direct.readRetryBudgetOverride = 150 * time.Millisecond
		direct.reopen = func(context.Context) (beadslib.Storage, error) {
			return nil, errors.New("dial tcp: connection refused")
		}
		_, err := direct.Get("gc-1")
		if _, ok := ProxiedVerdictOf(err); ok {
			t.Fatalf("a direct handle rendered a verdict: %v", err)
		}
		if !strings.Contains(err.Error(), "budget exhausted") {
			t.Errorf("err = %v, want the pre-existing budget error", err)
		}
	})
}

// TestNativeDoltOpenCircuitWaitsTheCooldownInsteadOfReturning pins rung 3's
// behavior: an ErrCircuitOpen with no transient substring used to be handed
// straight to the caller, so a breaker that was about to re-arm read as a failed
// read. The retry is bounded by the read's own budget, so a breaker that stays
// open costs the budget and demotes.
//
// Every store here is a PROXIED handle, and that is the point of the rung after
// council B-F1: rung 3 is lane-gated, because on the DIRECT lane the immediate
// return IS the contract main has and this PR promises not to change it. The
// direct-lane half of the same rung is
// TestFlagOffNativeReadIsByteIdenticalForTheLaneGatedRungs; the two are
// deliberately mirror images.
func TestNativeDoltOpenCircuitWaitsTheCooldownInsteadOfReturning(t *testing.T) {
	var reads, reopens int32
	healthy := healthySearchStorage(&beadslib.Issue{
		ID: "gc-1", Title: "after the breaker re-armed", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2,
	})
	flaky := &nativeDoltStorageSpy{
		searchIssues: func(ctx context.Context, q string, f beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if atomic.AddInt32(&reads, 1) == 1 {
				return nil, fmt.Errorf("beads read: %w", beadslib.ErrCircuitOpen)
			}
			return healthy.searchIssues(ctx, q, f)
		},
	}
	store := newNativeDoltStoreForTest(flaky)
	store.proxiedReadVerdicts = true
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		atomic.AddInt32(&reopens, 1)
		return nil, errors.New("reopen must not be reached for an open circuit")
	}
	// Shrink the cooldown wait to the budget so the test does not sit out five
	// real seconds; the read is expected to succeed on its second pass, which
	// happens after the cooldown select returns.
	store.readRetryBudgetOverride = 50 * time.Millisecond

	_, err := store.Get("gc-1")
	// The budget is shorter than the cooldown, so this read ends at the budget —
	// what matters is that it did NOT return the circuit error directly and did
	// NOT reconnect.
	if errors.Is(err, beadslib.ErrCircuitOpen) && !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("Get returned the circuit error without waiting: %v", err)
	}
	if n := atomic.LoadInt32(&reopens); n != 0 {
		t.Fatalf("reopens = %d, want 0 — an open circuit never reached a socket", n)
	}

	t.Run("a cooldown inside the budget lets the retry through", func(t *testing.T) {
		// The production cooldown is 5s, which is a real five seconds per run;
		// shrink it so the LOOP is what the test exercises rather than the clock.
		previous := nativeReadCircuitCooldown
		nativeReadCircuitCooldown = 20 * time.Millisecond
		t.Cleanup(func() { nativeReadCircuitCooldown = previous })

		atomic.StoreInt32(&reads, 0)
		second := newNativeDoltStoreForTest(flaky)
		second.proxiedReadVerdicts = true
		second.reopen = func(context.Context) (beadslib.Storage, error) {
			return nil, errors.New("reopen must not be reached for an open circuit")
		}
		second.readRetryBudgetOverride = 5 * time.Second

		if _, err := second.Get("gc-1"); err != nil {
			t.Fatalf("Get after the breaker re-armed: %v", err)
		}
		if n := atomic.LoadInt32(&reads); n != 2 {
			t.Fatalf("reads = %d, want 2 — the circuit error then the retry", n)
		}
	})

	t.Run("a handle with no reopen hook keeps fail-fast", func(t *testing.T) {
		bare := newNativeDoltStoreForTest(deadSearchStorage(fmt.Errorf("beads read: %w", beadslib.ErrCircuitOpen)))
		bare.proxiedReadVerdicts = true
		start := time.Now()
		if _, err := bare.Get("gc-1"); !errors.Is(err, beadslib.ErrCircuitOpen) {
			t.Fatalf("Get err = %v, want the circuit error returned immediately", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Get took %s; a hook-less handle must not spend a cooldown", elapsed)
		}
	})
}

// TestFlagOffNativeReadIsByteIdenticalForTheLaneGatedRungs is council
// B-F1 / C-F1 / C-F5's pin, and it is a pin on the lane this PR promises not to
// touch.
//
// The flag-off lane is every existing direct/hosted managed-Dolt city, and its
// read path is ordinary List/Get/Ready. Three of P2-08's rungs changed it:
//
//   - An open library breaker returned instantly on main (its text matches none
//     of the nine transient substrings). Classified as nativeReadCircuitOpen it
//     slept a 5s cooldown and retried inside a 90s budget — ~18 passes, ~90s of
//     hang — and then returned a different error string.
//   - An Error 1213 on a READ returned instantly on main (the matcher was
//     consulted on the write path only). Classified as nativeReadTransient every
//     pass called the reopen hook, which on a hosted city re-resolves the managed
//     port with recovery enabled and can restart a healthy city's Dolt server.
//   - A name that does not resolve returned instantly on main. Rung 6's
//     proxyendpoint.IsConnectionLevel claims ANY net.Error, and *net.DNSError is
//     one, so a misconfigured BEADS_DOLT_SERVER_HOST became the same 90s
//     reconnect loop — against a name the reopen hook will re-resolve to the
//     same failure. Council C-F5 filed this, the first fix round gated rungs 2
//     and 3 only, and the round's tally counted it closed; pr2 D-F2 re-found it
//     with exactly the reproduction this row now runs.
//
// Each row therefore asserts three things together, because any one alone
// passes on the broken code: the error is returned VERBATIM (not wrapped in a
// budget message), the reopen hook was never called, and the read did not spend
// wall time. The reopen hook is INSTALLED in every row — the `reopen == nil`
// escape protects only bare test handles, and all three production direct/hosted
// open sites pass one.
func TestFlagOffNativeReadIsByteIdenticalForTheLaneGatedRungs(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// proxiedReopens is what the SAME error must still cost on the proxied
		// lane, so a row cannot be satisfied by disabling the rung outright.
		proxiedReopens bool
	}{
		{
			name: "an open circuit breaker",
			err:  fmt.Errorf("read issue: %w", beadslib.ErrCircuitOpen),
			// Rung 3's remedy is a cooldown on the SAME handle: nothing reached
			// a socket, so there is nothing to reconnect.
			proxiedReopens: false,
		},
		{
			name:           "a serialization conflict on a read",
			err:            errors.New("begin read tx: Error 1213 (40001): Deadlock found when trying to get lock"),
			proxiedReopens: true,
		},
		{
			name: "a host name that does not resolve",
			err: fmt.Errorf("dialing managed dolt: %w", &net.DNSError{
				Err: "no such host", Name: "beads-dolt.invalid", IsNotFound: true,
			}),
			proxiedReopens: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name+" on the direct lane", func(t *testing.T) {
			var reopens int32
			store := newNativeDoltStoreForTest(deadSearchStorage(tc.err))
			store.reopen = func(context.Context) (beadslib.Storage, error) {
				atomic.AddInt32(&reopens, 1)
				return healthySearchStorage(), nil
			}
			// Short, so a regression reads as a budget spent rather than as a
			// 90-second test.
			store.readRetryBudgetOverride = 400 * time.Millisecond

			start := time.Now()
			_, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
			elapsed := time.Since(start)

			if !errors.Is(err, tc.err) {
				t.Fatalf("List err = %v, want the storage error itself", err)
			}
			if strings.Contains(err.Error(), "retry budget exhausted") {
				t.Fatalf("the direct lane spent its budget and rewrote the error: %v", err)
			}
			if _, typed := ProxiedVerdictOf(err); typed {
				t.Fatalf("a DIRECT handle rendered a proxied verdict: %v", err)
			}
			if n := atomic.LoadInt32(&reopens); n != 0 {
				t.Fatalf("the direct lane called the reopen hook %d time(s); on a hosted city that hook "+
					"re-resolves the managed port with recovery enabled and can restart a healthy server", n)
			}
			if elapsed > 100*time.Millisecond {
				t.Fatalf("the direct lane spent %s on a read that returned immediately on main", elapsed)
			}
		})

		t.Run(tc.name+" on the proxied lane", func(t *testing.T) {
			var reopens int32
			store := newNativeDoltStoreForTest(deadSearchStorage(tc.err))
			store.proxiedReadVerdicts = true
			store.reopen = func(context.Context) (beadslib.Storage, error) {
				atomic.AddInt32(&reopens, 1)
				return deadSearchStorage(tc.err), nil
			}
			store.readRetryBudgetOverride = 300 * time.Millisecond
			// The production cooldown is five seconds, which is the budget's
			// business rather than this test's.
			restore := nativeReadCircuitCooldown
			nativeReadCircuitCooldown = 20 * time.Millisecond
			t.Cleanup(func() { nativeReadCircuitCooldown = restore })

			_, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth})
			if err == nil {
				t.Fatal("the proxied lane returned no error for a permanently failing read")
			}
			verdict, typed := ProxiedVerdictOf(err)
			if !typed {
				t.Fatalf("the proxied lane returned an untyped error the wrapper cannot demote on: %v", err)
			}
			if verdict.Terminal() {
				t.Errorf("a spent budget says nothing about the endpoint, so it must be non-terminal: %v", verdict)
			}
			if got := atomic.LoadInt32(&reopens) > 0; got != tc.proxiedReopens {
				t.Fatalf("the proxied lane reopened = %v, want %v: the rung is gated on the lane, not disabled", got, tc.proxiedReopens)
			}
		})
	}
}

// TestStaleMarkSetDuringAReconnectSurvivesIt is council A-F4.
//
// The stale mark is the ONLY mechanism that can see the root-move hazard
// (design U22): bd's proxy root is recreated at the same path, root_id is
// path-derived so the new proxy has the same root identity, and the old pooled
// socket keeps serving the MOVED database without an error. The guard tick
// notices from the record and marks the pool; the reader re-points.
//
// Clearing the mark unconditionally once the reconnect returned lost exactly
// the case the mechanism is for. The reconnect sits inside the reopen hook —
// a re-admission, the ladder's sleeps, a library open — for long enough that a
// second tick can land inside it. With a flag, that tick's mark was swallowed
// (already true), the reader installed the FIRST replacement's storage and
// cleared the flag, and the handle then served generation B's socket while the
// pin said C. No later tick re-marks it: checkGeneration compares the record
// against pin C and reports Held.
//
// The reconnect below marks the pool again from inside the hook, which is the
// tick landing mid-reconnect, deterministically and with no goroutine.
func TestStaleMarkSetDuringAReconnectSurvivesIt(t *testing.T) {
	storage := healthySearchStorage()
	store := newNativeDoltStoreForTest(storage)

	var reopens int32
	store.reopen = func(context.Context) (beadslib.Storage, error) {
		n := atomic.AddInt32(&reopens, 1)
		if n == 1 {
			// The second generation arrives while the first re-point is still
			// in flight. This is the tick's move, verbatim: adoptPin then
			// markPoolStale.
			if !store.markPoolStale() {
				t.Error("markPoolStale refused a handle with a reopen hook")
			}
		}
		return storage, nil
	}

	if !store.markPoolStale() {
		t.Fatal("markPoolStale refused a handle with a reopen hook")
	}
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("List: %v", err)
	}

	// Two marks were set and two re-points are owed, so the read loop must have
	// gone round twice: once for the tick's mark, once for the one that landed
	// during it.
	if got := atomic.LoadInt32(&reopens); got != 2 {
		t.Fatalf("the read re-pointed %d time(s), want 2: a mark set DURING the reconnect was swallowed, "+
			"so this handle serves the previous generation's socket under the new generation's pin", got)
	}
	if marked, owed := store.poolStaleOwed(); owed {
		t.Fatalf("mark %d is still owed after both re-points: the read loop would spin", marked)
	}

	// And the steady state still clears: one mark, one re-point, no residue.
	if !store.markPoolStale() {
		t.Fatal("markPoolStale refused a handle with a reopen hook")
	}
	if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
		t.Fatalf("List after a lone mark: %v", err)
	}
	if got := atomic.LoadInt32(&reopens); got != 3 {
		t.Fatalf("a lone mark cost %d re-points in total, want 3", got)
	}
	if marked, owed := store.poolStaleOwed(); owed {
		t.Fatalf("mark %d is still owed after a lone mark's re-point", marked)
	}
}

// TestStaleMarkSurvivesTheABAInterleaving is council pr2 D-F6.
//
// A-F4 made the mark an epoch but cleared it with CompareAndSwap(seen, 0), so
// the counter returned to zero and the swap tested for ABA rather than for a
// watermark. Two interleavings lost a mark that way, and each row below drives
// one of them deterministically, in the order the race would, through the real
// repinStalePool and reconnect:
//
//  1. Two readers both load mark 1. R2 re-points and clears. Tick T2 marks
//     (the counter is 1 AGAIN). R1, parked on the reconnect gate the whole
//     time, finds another reader already reconnected — and its CAS(1, 0)
//     succeeds, erasing T2's mark.
//  2. A transient-error reconnect's reopen is in flight when the tick marks.
//     A reader loads that mark, finds the transient reconnect already
//     installed, and clears a mark the installed storage never saw.
//
// Either way the handle serves the previous generation's socket under the new
// pin, and checkGeneration compares the record against the new pin and reports
// Held, so no later tick re-marks it. The assertion is the caller-visible one:
// the next read must re-point.
func TestStaleMarkSurvivesTheABAInterleaving(t *testing.T) {
	read := func(t *testing.T, store *NativeDoltStore) {
		t.Helper()
		if _, err := store.List(ListQuery{AllowScan: true, TierMode: TierBoth}); err != nil {
			t.Fatalf("List: %v", err)
		}
	}

	t.Run("two readers and a tick between them", func(t *testing.T) {
		storage := healthySearchStorage()
		store := newNativeDoltStoreForTest(storage)
		var reopens int32
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return storage, nil
		}

		// Tick T1 marks; readers R1 and R2 both observe generation g and mark 1.
		if !store.markPoolStale() {
			t.Fatal("markPoolStale refused a handle with a reopen hook")
		}
		_, r1Gen, release, err := store.acquireStorageGen()
		if err != nil {
			t.Fatal(err)
		}
		release()
		r1Mark, owed := store.poolStaleOwed()
		if !owed {
			t.Fatal("a fresh mark was not owed")
		}
		// R2 wins the gate and re-points.
		read(t, store)
		if got := atomic.LoadInt32(&reopens); got != 1 {
			t.Fatalf("R2 re-pointed %d time(s), want 1", got)
		}
		// Tick T2 lands while R1 is still parked.
		if !store.markPoolStale() {
			t.Fatal("markPoolStale refused a handle with a reopen hook")
		}
		// R1 wakes: another reader already reconnected.
		if err := store.repinStalePool(context.Background(), r1Gen, r1Mark); err != nil {
			t.Fatalf("R1's repin: %v", err)
		}
		if got := atomic.LoadInt32(&reopens); got != 1 {
			t.Fatalf("R1 re-pointed on a generation another reader had already replaced (%d reopens)", got)
		}
		// T2's mark must still be owed, and the next read must honor it.
		read(t, store)
		if got := atomic.LoadInt32(&reopens); got != 2 {
			t.Fatalf("the read after T2's mark re-pointed %d time(s) in total, want 2: R1 erased the mark T2 set "+
				"while it was parked, so this handle serves the previous generation's socket under T2's pin", got)
		}
		if marked, owed := store.poolStaleOwed(); owed {
			t.Fatalf("mark %d is still owed after it was honored: the read loop would spin", marked)
		}
	})

	t.Run("a transient reconnect in flight when the tick marks", func(t *testing.T) {
		storage := healthySearchStorage()
		store := newNativeDoltStoreForTest(storage)
		var reopens int32
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			if atomic.AddInt32(&reopens, 1) == 1 {
				// The tick marks while this (transient-error) reopen is in
				// flight: whatever it re-admitted, it began before the mark.
				if !store.markPoolStale() {
					t.Error("markPoolStale refused a handle with a reopen hook")
				}
			}
			return storage, nil
		}

		_, gen, release, err := store.acquireStorageGen()
		if err != nil {
			t.Fatal(err)
		}
		release()
		// The transient-error path's reconnect, exactly as withReadRetry calls it.
		if err := store.reconnect(context.Background(), gen); err != nil {
			t.Fatalf("reconnect: %v", err)
		}
		// A reader that loaded the mark set during it, and observed the old
		// generation, now finds that reconnect already installed.
		mark, owed := store.poolStaleOwed()
		if !owed {
			t.Fatal("a mark set during a reconnect's reopen was treated as covered by that reopen")
		}
		if err := store.repinStalePool(context.Background(), gen, mark); err != nil {
			t.Fatalf("repin: %v", err)
		}
		read(t, store)
		if got := atomic.LoadInt32(&reopens); got != 2 {
			t.Fatalf("the read after the mark re-pointed %d time(s) in total, want 2: the mark was cleared "+
				"on the strength of a reopen that began before it", got)
		}
	})
}

// TestNativeDoltStorePingGoesThroughTheReadPath is council B-F2.
//
// P2-09 deliberately re-points ProxiedStore.Ping at the native leaf so doctor's
// per-scope health check costs zero forks. That made Ping the lane's
// most-repeated read — and it was the one read that reached acquireStorage
// directly instead of going through withReadRetry, so it sat outside both
// mechanisms the proxied lane depends on:
//
//  1. It did not honor poolStale. The guard tick's re-pin is adoptPin plus
//     markPoolStale, and the property the root-move row asserts is that no read
//     is served from the old generation before the mark is honored. On the H7
//     shape the old socket is still alive and serving the MOVED database, so an
//     unhardened Ping answered cleanly and doctor reported the scope healthy
//     after the tick already knew the generation had changed.
//  2. Its failures could never be classified, so a Ping against a dead proxy
//     returned a raw driver error, ProxiedStore.Ping's classifyReadError found
//     no verdict, and a handle every other read would have demoted stayed
//     "native".
//
// The hardening is PROXIED-LANE ONLY, and that is the second half of the
// finding rather than a detail (council pr2 D-F1). poolStale is only ever
// marked by the proxied guard tick, and proxiedReadVerdict is a no-op off the
// proxied lane, so neither reason above applies to a direct or hosted handle —
// while the cost does: rung 7's substring table matches "dial tcp" and
// "connection refused" on BOTH lanes, so an unconditionally routed Ping turned
// every flag-off ping failure into reconnect-and-retry through the managed-Dolt
// restart hook, on an uncancellable 90s budget, underneath three poll loops
// built on Ping failing fast. The direct rows below therefore assert what those
// callers see — the verbatim error, one upstream call, zero reopens, no wall
// time — because any one of those alone passes on the broken code.
//
// beadslib.Storage.GetStatistics returns a type from beads' internal package,
// so no in-process fixture can implement it (the constraint G3 recorded as
// P2-09 deviation 5); the rows that need a failing upstream read substitute
// Ping's single upstream call through the store's pingUpstream seam and drive
// the real Ping.
func TestNativeDoltStorePingGoesThroughTheReadPath(t *testing.T) {
	t.Run("a stale mark is honored before the ping is served", func(t *testing.T) {
		store := newNativeDoltStoreForTest(healthySearchStorage())
		store.proxiedReadVerdicts = true
		var reopens int32
		refused := errors.New("the re-admission refused this generation")
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return nil, refused
		}

		// The guard tick saw the generation move.
		if !store.markPoolStale() {
			t.Fatal("markPoolStale refused a handle with a reopen hook")
		}
		err := store.Ping()
		if got := atomic.LoadInt32(&reopens); got != 1 {
			t.Fatalf("Ping re-pointed the pool %d time(s), want 1: it was served from the OLD generation, "+
				"which on a moved root is a clean answer about the wrong database", got)
		}
		if !errors.Is(err, refused) {
			t.Fatalf("Ping err = %v, want the re-pin's refusal", err)
		}
	})

	t.Run("a proxied ping that spends its budget is a verdict", func(t *testing.T) {
		store := newNativeDoltStoreForTest(healthySearchStorage())
		store.proxiedReadVerdicts = true
		store.readRetryBudgetOverride = 150 * time.Millisecond
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			return nil, errors.New("dial tcp 127.0.0.1:44561: i/o timeout")
		}
		if !store.markPoolStale() {
			t.Fatal("markPoolStale refused a handle with a reopen hook")
		}

		verdict, ok := ProxiedVerdictOf(store.Ping())
		if !ok {
			t.Fatal("a ping that spent its whole budget produced no verdict, so the wrapper cannot demote on it")
		}
		if verdict.Verdict != ProxiedVerdictBudgetExhausted {
			t.Errorf("verdict = %q, want %q", verdict.Verdict, ProxiedVerdictBudgetExhausted)
		}
		if verdict.Terminal() {
			t.Error("a spent budget says nothing about the endpoint, so it must be non-terminal")
		}
	})

	// The two rows below are mirror images over one error, so a direct row
	// cannot be satisfied by disabling Ping's hardening outright.
	refusedDial := errors.New("begin read tx: dial tcp 127.0.0.1:3307: connect: connection refused")

	t.Run("a direct ping returns the refused dial the way main does", func(t *testing.T) {
		var pings, reopens int32
		store := newNativeDoltStoreForTest(healthySearchStorage())
		store.pingUpstream = func(context.Context, beadslib.Storage) error {
			atomic.AddInt32(&pings, 1)
			return refusedDial
		}
		// Short, so a regression reads as a spent budget rather than as a
		// 90-second test. Production has no override on this lane at all.
		store.readRetryBudgetOverride = 400 * time.Millisecond
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return healthySearchStorage(), nil
		}

		start := time.Now()
		err := store.Ping()
		elapsed := time.Since(start)

		if !errors.Is(err, refusedDial) {
			t.Fatalf("Ping err = %v, want the upstream error itself", err)
		}
		if strings.Contains(err.Error(), "retry budget exhausted") {
			t.Fatalf("the direct lane spent its budget and rewrote the error: %v", err)
		}
		if _, typed := ProxiedVerdictOf(err); typed {
			t.Fatalf("a DIRECT handle rendered a proxied verdict from a ping: %v", err)
		}
		if n := atomic.LoadInt32(&pings); n != 1 {
			t.Fatalf("the direct lane made %d upstream ping call(s), want 1: waitForRigStoreAccessible and "+
				"waitForBeadsScopeReadyAfterRecovery poll Ping every 250ms and check their deadline AFTER it", n)
		}
		if n := atomic.LoadInt32(&reopens); n != 0 {
			t.Fatalf("a direct ping called the reopen hook %d time(s); on a hosted city that hook re-resolves "+
				"the managed port with recovery enabled and can restart a healthy server", n)
		}
		if elapsed > 100*time.Millisecond {
			t.Fatalf("a direct ping spent %s; on main it returned on the first pass, and gc doctor's "+
				"--check-timeout is built on that", elapsed)
		}
	})

	t.Run("a proxied ping still retries the same refused dial", func(t *testing.T) {
		var pings, reopens int32
		store := newNativeDoltStoreForTest(healthySearchStorage())
		store.proxiedReadVerdicts = true
		store.pingUpstream = func(context.Context, beadslib.Storage) error {
			atomic.AddInt32(&pings, 1)
			return refusedDial
		}
		store.readRetryBudgetOverride = 300 * time.Millisecond
		store.reopen = func(context.Context) (beadslib.Storage, error) {
			atomic.AddInt32(&reopens, 1)
			return healthySearchStorage(), nil
		}

		err := store.Ping()
		verdict, typed := ProxiedVerdictOf(err)
		if !typed {
			t.Fatalf("the proxied lane returned an untyped ping error the wrapper cannot demote on: %v", err)
		}
		if verdict.Verdict != ProxiedVerdictBudgetExhausted {
			t.Errorf("verdict = %q, want %q", verdict.Verdict, ProxiedVerdictBudgetExhausted)
		}
		if n := atomic.LoadInt32(&pings); n < 2 {
			t.Fatalf("the proxied lane made %d upstream ping call(s), want more than one: the gate is on the "+
				"lane, not on the hardening", n)
		}
		if n := atomic.LoadInt32(&reopens); n == 0 {
			t.Fatal("the proxied lane never reconnected for a refused dial")
		}
	})
}
