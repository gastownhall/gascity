package beads

import (
	"context"
	"errors"
	"fmt"
	"io"
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
			name: "a bare io.EOF is connection-level even though no substring matches",
			err:  fmt.Errorf("reading greeting: %w", io.EOF),
			want: nativeReadTransient,
			why:  "the sentinel rung is what the substring table could not see",
		},
		{
			name:    "ECONNREFUSED is connection-level",
			err:     fmt.Errorf("dial: %w", syscall.ECONNREFUSED),
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
// behavior: before it, an ErrCircuitOpen with no transient substring was handed
// straight to the caller, so a breaker that was about to re-arm read as a failed
// read. The retry is bounded by the read's own budget, so a breaker that stays
// open costs the budget and (on a proxied handle) demotes.
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
		start := time.Now()
		if _, err := bare.Get("gc-1"); !errors.Is(err, beadslib.ErrCircuitOpen) {
			t.Fatalf("Get err = %v, want the circuit error returned immediately", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Get took %s; a hook-less handle must not spend a cooldown", elapsed)
		}
	})
}

// TestFlagOffNativeReadIsByteIdenticalForTheTwoLaneGatedRungs is council
// B-F1 / C-F1's pin, and it is a pin on the lane this PR promises not to touch.
//
// The flag-off lane is every existing direct/hosted managed-Dolt city, and its
// read path is ordinary List/Get/Ready. Two of P2-08's rungs changed it:
//
//   - An open library breaker returned instantly on main (its text matches none
//     of the nine transient substrings). Classified as nativeReadCircuitOpen it
//     slept a 5s cooldown and retried inside a 90s budget — ~18 passes, ~90s of
//     hang — and then returned a different error string.
//   - An Error 1213 on a READ returned instantly on main (the matcher was
//     consulted on the write path only). Classified as nativeReadTransient every
//     pass called the reopen hook, which on a hosted city re-resolves the managed
//     port with recovery enabled and can restart a healthy city's Dolt server.
//
// Each row therefore asserts three things together, because any one alone
// passes on the broken code: the error is returned VERBATIM (not wrapped in a
// budget message), the reopen hook was never called, and the read did not spend
// wall time. The reopen hook is INSTALLED in every row — the `reopen == nil`
// escape protects only bare test handles, and all three production direct/hosted
// open sites pass one.
func TestFlagOffNativeReadIsByteIdenticalForTheTwoLaneGatedRungs(t *testing.T) {
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
