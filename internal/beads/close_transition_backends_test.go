package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
	_ "modernc.org/sqlite"
)

type bdCloseTransitionFixture struct {
	mu               sync.Mutex
	version          string
	versionErr       error
	closeErr         error
	status           string
	reason           string
	session          string
	ambientSession   string
	forceOtherWinner bool
	revision         int64
	versionCalls     int
	snapshotCalls    int
	identityCalls    int
	closeCalls       int
	closeArgs        []string
}

func (f *bdCloseTransitionFixture) runner(_ string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %q", name, args)
	}
	if len(args) == 2 && args[1] == "--help" {
		switch args[0] {
		case "update", "close", "assign", "delete":
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
	}
	if f.revision == 0 {
		f.revision = 11
	}
	switch args[0] {
	case "version":
		f.versionCalls++
		if f.versionErr != nil {
			return nil, f.versionErr
		}
		version := f.version
		if version == "" {
			version = "1.1.0"
		}
		return []byte("bd version " + version + "\n"), nil
	case "show", "query":
		f.snapshotCalls++
		return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":%q,"issue_type":"task","metadata":{"existing":"kept"},"close_reason":%q,"revision":%d}]`, f.status, f.reason, f.revision)), nil
	case "dep":
		return []byte(`[]`), nil
	case "sql":
		f.identityCalls++
		return []byte(fmt.Sprintf(`[{"id":"bd-42","status":%q,"close_reason":%q,"closed_by_session":%q}]`, f.status, f.reason, f.session)), nil
	case "close":
		f.closeCalls++
		f.closeArgs = slices.Clone(args)
		reason := argValue(args, "--reason")
		if reason == "" {
			reason = "Closed"
		}
		session := argValue(args, "--session")
		if f.forceOtherWinner {
			f.reason = "ordinary close won"
			f.session = "ordinary-session"
			f.status = "closed"
			f.revision++
		}
		if expected, ok := revisionArg(args); ok && expected != f.revision {
			return conditionalPreconditionBody(expected, f.revision), errors.New("exit status 9")
		}
		if !f.forceOtherWinner && f.status != "closed" {
			f.reason = reason
			if session != "" {
				f.session = session
			} else {
				f.session = f.ambientSession
			}
			f.revision++
		}
		f.status = "closed"
		return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":"closed","issue_type":"task","close_reason":%q,"revision":%d}]`, f.reason, f.revision)), f.closeErr
	default:
		return nil, fmt.Errorf("unexpected bd args %q", args)
	}
}

func TestBdStoreCloseTransitionRequiresStatusPredicateClose(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		versionErr error
	}{
		{name: "bd 1.0.4", version: "1.0.4"},
		{name: "minimum prerelease", version: "1.1.0-rc.1"},
		{name: "missing patch", version: "1.1"},
		{name: "extra numeric component", version: "1.1.0.1"},
		{name: "leading zero", version: "1.01.0"},
		{name: "unparseable version", version: "not-a-version"},
		{name: "version probe failure", versionErr: errors.New("version unavailable")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := &bdCloseTransitionFixture{
				version:    tt.version,
				versionErr: tt.versionErr,
				status:     "open",
			}
			store := NewBdStore("/city", fixture.runner)

			for attempt := 0; attempt < 2; attempt++ {
				_, err := store.CloseWithReasonIfOpen("bd-42", "must not be written")
				if !errors.Is(err, ErrCloseTransitionUnsupported) {
					t.Fatalf("CloseWithReasonIfOpen attempt %d error = %v, want ErrCloseTransitionUnsupported", attempt+1, err)
				}
			}
			if fixture.versionCalls != 1 {
				t.Fatalf("version calls = %d, want 1", fixture.versionCalls)
			}
			if fixture.closeCalls != 0 {
				t.Fatalf("close calls = %d, want 0", fixture.closeCalls)
			}
		})
	}
}

func TestBdStoreCloseTransitionAcceptsStableSemverAtMinimumOrNewer(t *testing.T) {
	for _, version := range []string{"1.1.0", "1.1.0+build.5", "1.1.1", "2.0.0"} {
		t.Run(version, func(t *testing.T) {
			fixture := &bdCloseTransitionFixture{version: version}
			store := NewBdStore("/city", fixture.runner)

			if err := store.requireCloseTransitionSupport(); err != nil {
				t.Fatalf("requireCloseTransitionSupport(%q): %v", version, err)
			}
			if fixture.versionCalls != 1 {
				t.Fatalf("version calls = %d, want 1", fixture.versionCalls)
			}
		})
	}
}

func argValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestBdStoreCloseTransitionPreservesSessionAttribution(t *testing.T) {
	fixture := &bdCloseTransitionFixture{status: "open", ambientSession: "claude-session-id"}
	store := NewBdStore("/city", fixture.runner)

	transition, err := store.CloseWithReasonIfOpen("bd-42", "all children closed")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want true")
	}
	if transition.Before.Status != "open" || transition.After.Status != "closed" {
		t.Fatalf("transition status = %q -> %q, want open -> closed", transition.Before.Status, transition.After.Status)
	}
	if got := transition.After.Metadata["close_reason"]; got != "all children closed" {
		t.Fatalf("After close_reason = %q, want all children closed", got)
	}
	if slices.Contains(fixture.closeArgs, "--session") {
		t.Fatalf("bd close args = %q, must preserve bd/CLAUDE_SESSION_ID attribution by omitting --session", fixture.closeArgs)
	}
	if fixture.session != fixture.ambientSession {
		t.Fatalf("closed_by_session = %q, want ambient attribution %q", fixture.session, fixture.ambientSession)
	}
	if fixture.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", fixture.closeCalls)
	}
}

func TestCachingStoreCloseEmitsForBdDefaultCloseReason(t *testing.T) {
	fixture := &bdCloseTransitionFixture{status: "open"}
	var notifications []struct {
		eventType string
		payload   json.RawMessage
	}
	store := NewCachingStoreForTest(NewBdStore("/city", fixture.runner), func(eventType, _ string, payload json.RawMessage) {
		notifications = append(notifications, struct {
			eventType string
			payload   json.RawMessage
		}{eventType: eventType, payload: slices.Clone(payload)})
	})

	if err := store.Close("bd-42"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(notifications) != 1 || notifications[0].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want one bead.closed", notifications)
	}
	var closed Bead
	if err := json.Unmarshal(notifications[0].payload, &closed); err != nil {
		t.Fatalf("decode bead.closed payload: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["close_reason"] != "Closed" {
		t.Fatalf("bead.closed payload = %#v, want closed with bd default close_reason", closed)
	}
}

func TestBdStoreCloseTransitionReturnsOtherWinner(t *testing.T) {
	fixture := &bdCloseTransitionFixture{status: "open", forceOtherWinner: true}
	store := NewBdStore("/city", fixture.runner)

	transition, err := store.CloseWithReasonIfOpen("bd-42", "autoclose reason")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if transition.Transitioned {
		t.Fatal("Transitioned = true, want false for another winner")
	}
	if got := transition.After.Metadata["close_reason"]; got != "ordinary close won" {
		t.Fatalf("After close_reason = %q, want ordinary close won", got)
	}
}

func TestBdStoreCloseTransitionDoesNotClaimAmbiguousCommittedClose(t *testing.T) {
	closeErr := errors.New("connection reset after close commit")
	fixture := &bdCloseTransitionFixture{status: "open", closeErr: closeErr}
	store := NewBdStore("/city", fixture.runner)

	transition, err := store.CloseWithReasonIfOpen("bd-42", "durably committed reason")
	if !errors.Is(err, closeErr) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want %v", err, closeErr)
	}
	if transition.Transitioned || !transition.AuthoritativeClosed("bd-42") {
		t.Fatalf("transition = %#v, want authoritative state without unprovable ownership", transition)
	}
	if got := transition.After.Metadata["close_reason"]; got != "durably committed reason" {
		t.Fatalf("After close_reason = %q, want durably committed reason", got)
	}
	if fixture.status != "closed" || fixture.reason != "durably committed reason" {
		t.Fatalf("durable fixture = status %q reason %q, want committed close", fixture.status, fixture.reason)
	}
	if fixture.snapshotCalls < 2 || fixture.identityCalls < 3 {
		t.Fatalf("verification reads = %d snapshots/%d identities, want authoritative snapshot and confirmation before returning error", fixture.snapshotCalls, fixture.identityCalls)
	}
}

func TestCloseIdentityQueriesPreferCanonicalWispOnDuplicateID(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	for _, statement := range []string{
		`CREATE TABLE issues (id TEXT PRIMARY KEY, status TEXT, close_reason TEXT, closed_by_session TEXT)`,
		`CREATE TABLE wisps (id TEXT PRIMARY KEY, status TEXT, close_reason TEXT, closed_by_session TEXT)`,
		`INSERT INTO issues VALUES ('duplicate-id', 'open', 'issue reason', 'issue-session')`,
		`INSERT INTO wisps VALUES ('duplicate-id', 'closed', 'wisp reason', 'wisp-session')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed duplicate identity rows: %v", err)
		}
	}

	want := nativeCloseIdentity{
		ID:              "duplicate-id",
		Status:          "closed",
		CloseReason:     "wisp reason",
		ClosedBySession: "wisp-session",
	}
	t.Run("bd sql", func(t *testing.T) {
		var got nativeCloseIdentity
		if err := db.QueryRow(bdCloseIdentitySQL(want.ID)).Scan(
			&got.ID,
			&got.Status,
			&got.CloseReason,
			&got.ClosedBySession,
		); err != nil {
			t.Fatalf("query bd close identity: %v", err)
		}
		if got != want {
			t.Fatalf("close identity = %#v, want canonical wisp %#v", got, want)
		}
	})
	t.Run("native sql", func(t *testing.T) {
		got, err := queryNativeCloseIdentity(context.Background(), db, want.ID)
		if err != nil {
			t.Fatalf("query native close identity: %v", err)
		}
		if got != want {
			t.Fatalf("close identity = %#v, want canonical wisp %#v", got, want)
		}
	})
}

func TestBdStoreCloseTransitionUsesCanonicalWispSnapshotOnDuplicateID(t *testing.T) {
	issueStatus := "closed"
	wispStatus := "open"
	wispReason := ""
	wispSession := ""
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command %s %q", name, args)
		}
		if len(args) == 2 && args[1] == "--help" {
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
		switch args[0] {
		case "version":
			return []byte("bd version 1.1.0\n"), nil
		case "show":
			return []byte(fmt.Sprintf(`[{"id":"duplicate-id","title":"stale issue","status":%q,"issue_type":"task","close_reason":"stale issue reason"}]`, issueStatus)), nil
		case "query":
			return []byte(fmt.Sprintf(`[{"id":"duplicate-id","title":"canonical wisp","status":%q,"issue_type":"task","no_history":true,"close_reason":%q}]`, wispStatus, wispReason)), nil
		case "dep":
			return []byte(`[]`), nil
		case "sql":
			return []byte(fmt.Sprintf(`[{"id":"duplicate-id","status":%q,"close_reason":%q,"closed_by_session":%q}]`, wispStatus, wispReason, wispSession)), nil
		case "close":
			if wispStatus != "closed" {
				wispStatus = "closed"
				wispReason = argValue(args, "--reason")
				wispSession = argValue(args, "--session")
			}
			return []byte(fmt.Sprintf(`[{"id":"duplicate-id","title":"canonical wisp","status":"closed","issue_type":"task","no_history":true,"close_reason":%q}]`, wispReason)), nil
		default:
			return nil, fmt.Errorf("unexpected bd args %q", args)
		}
	}

	store := NewBdStore("/city", runner)
	transition, err := store.CloseWithReasonIfOpen("duplicate-id", "canonical close reason")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want true for the canonical wisp close")
	}
	if transition.Before.Title != "canonical wisp" || transition.Before.Status != "open" || !transition.Before.NoHistory {
		t.Fatalf("Before = %#v, want the open canonical no-history wisp", transition.Before)
	}
	if transition.After.Title != "canonical wisp" || transition.After.Status != "closed" || !transition.After.NoHistory {
		t.Fatalf("After = %#v, want the closed canonical no-history wisp", transition.After)
	}
	if got := transition.After.Metadata["close_reason"]; got != "canonical close reason" {
		t.Fatalf("After close_reason = %q, want canonical close reason", got)
	}
	if issueStatus != "closed" {
		t.Fatalf("stale issue status = %q, want unchanged closed", issueStatus)
	}
}

func TestBdStoreCloseTransitionRejectsRecloseDuringSnapshot(t *testing.T) {
	status := "open"
	reason := ""
	session := ""
	snapshotCalls := 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
		switch args[0] {
		case "version":
			return []byte("bd version 1.1.0\n"), nil
		case "query":
			snapshotCalls++
			if snapshotCalls == 2 {
				reason = "replacement close"
				session = "other-session"
			}
			return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":%q,"issue_type":"task","close_reason":%q}]`, status, reason)), nil
		case "dep":
			return []byte(`[]`), nil
		case "sql":
			return []byte(fmt.Sprintf(`[{"id":"bd-42","status":%q,"close_reason":%q,"closed_by_session":%q}]`, status, reason, session)), nil
		case "close":
			status = "closed"
			reason = argValue(args, "--reason")
			session = argValue(args, "--session")
			return []byte(`[{"id":"bd-42","title":"target","status":"closed","issue_type":"task"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd args %q", args)
		}
	}
	store := NewBdStore("/city", runner)

	if _, err := store.CloseWithReasonIfOpen("bd-42", "first close"); err == nil {
		t.Fatal("CloseWithReasonIfOpen returned nil after identity changed during snapshot")
	} else if !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want identity-changed diagnostic", err)
	}
}

func TestBdStoreCloseTransitionFailsClosedWhenIdentityReadUnsupported(t *testing.T) {
	closeCalls := 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
		switch args[0] {
		case "version":
			return []byte("bd version 1.1.0\n"), nil
		case "show", "query":
			return []byte(`[{"id":"bd-42","title":"target","status":"open","issue_type":"task"}]`), nil
		case "dep":
			return []byte(`[]`), nil
		case "sql":
			return nil, errors.New(`unknown command "sql" for "bd"`)
		case "close":
			closeCalls++
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected bd args %q", args)
		}
	}
	store := NewBdStore("/city", runner)

	_, err := store.CloseWithReasonIfOpen("bd-42", "must not be written")
	if !errors.Is(err, ErrCloseTransitionUnsupported) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want ErrCloseTransitionUnsupported", err)
	}
	if closeCalls != 0 {
		t.Fatalf("close calls = %d, want 0", closeCalls)
	}
}

func TestBdStoreCloseIdentityFallsBackWhenEmbeddedBdRequiresCGO(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, ".beads", "metadata.json"),
		[]byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"demo"}`),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile metadata: %v", err)
	}

	var calls []string
	runner := func(callDir, name string, args ...string) ([]byte, error) {
		calls = append(calls, callDir+": "+name+" "+strings.Join(args, " "))
		switch name {
		case "bd":
			return nil, errors.New("opening database: embedded Dolt requires a CGO build (rebuild with CGO_ENABLED=1)")
		case "dolt":
			return []byte(`{"rows":[{"id":"bd-42","status":"open","close_reason":"","closed_by_session":""}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected command %s", name)
		}
	}
	store := NewBdStore(dir, runner)

	identity, err := store.readCloseIdentity("bd-42")
	if err != nil {
		t.Fatalf("readCloseIdentity: %v", err)
	}
	if identity.ID != "bd-42" || identity.Status != "open" {
		t.Fatalf("identity = %#v, want bd-42/open", identity)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %#v, want bd then direct dolt fallback", calls)
	}
	if !strings.HasPrefix(calls[0], dir+": bd sql --json ") {
		t.Fatalf("first call = %q, want bd sql identity read", calls[0])
	}
	wantDoltPrefix := filepath.Join(dir, ".beads", "embeddeddolt", "demo") + ": dolt sql -r json -q "
	if !strings.HasPrefix(calls[1], wantDoltPrefix) {
		t.Fatalf("second call = %q, want %q prefix", calls[1], wantDoltPrefix)
	}
}

func TestCachingStoreOrdinaryCloseFallsBackWhenBdTransitionIdentityReadUnsupported(t *testing.T) {
	status := "open"
	closeCalls := 0
	runner := func(_ string, _ string, args ...string) ([]byte, error) {
		if len(args) == 2 && args[1] == "--help" {
			return []byte("Flags:\n      --if-revision int\n"), nil
		}
		switch args[0] {
		case "version":
			return []byte("bd version 1.1.0\n"), nil
		case "show", "query":
			return []byte(fmt.Sprintf(`[{"id":"bd-42","title":"target","status":%q,"issue_type":"task"}]`, status)), nil
		case "dep":
			return []byte(`[]`), nil
		case "sql":
			return nil, errors.New(`unknown command "sql" for "bd"`)
		case "close":
			closeCalls++
			status = "closed"
			return []byte(`[{"id":"bd-42","title":"target","status":"closed","issue_type":"task"}]`), nil
		default:
			return nil, fmt.Errorf("unexpected bd args %q", args)
		}
	}
	cache := NewCachingStoreForTest(NewBdStore("/city", runner), nil)

	if err := cache.Close("bd-42"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closeCalls != 1 {
		t.Fatalf("legacy close calls = %d, want 1", closeCalls)
	}
	got, err := cache.Get("bd-42")
	if err != nil {
		t.Fatalf("Get after Close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status after Close = %q, want closed", got.Status)
	}
}

type nativeCloseTransitionStorage struct {
	*nativeDoltMemStorage
	mu      sync.Mutex
	reason  string
	session string
}

type nativeLegacyCloseSnapshotTrapStorage struct {
	*nativeCloseTransitionStorage
	snapshotCalls int
}

func (s *nativeLegacyCloseSnapshotTrapStorage) GetIssuesByIDs(context.Context, []string) ([]*beadslib.Issue, error) {
	s.snapshotCalls++
	return nil, errors.New("legacy out-of-transaction close snapshot used")
}

func newNativeCloseTransitionStorage() *nativeCloseTransitionStorage {
	return &nativeCloseTransitionStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
}

func (s *nativeCloseTransitionStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeCloseTransitionStorage) CloseIssue(_ context.Context, id, reason, _ string, session string) error {
	transition, err := s.store.CloseWithReasonIfOpen(id, reason)
	if err != nil {
		return err
	}
	if transition.Transitioned {
		s.mu.Lock()
		s.reason = reason
		s.session = session
		s.mu.Unlock()
	}
	return nil
}

func (s *nativeCloseTransitionStorage) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	issue, err := s.nativeDoltMemStorage.GetIssue(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	issue.CloseReason = s.reason
	issue.ClosedBySession = s.session
	s.mu.Unlock()
	return issue, nil
}

func TestNativeDoltStoreCloseTransitionReturnsDurableWinner(t *testing.T) {
	storage := newNativeCloseTransitionStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "target", Metadata: StringMap{"existing": "kept"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := store.CloseWithReasonIfOpen(created.ID, "native winner")
	if err != nil {
		t.Fatalf("first CloseWithReasonIfOpen: %v", err)
	}
	if !first.Transitioned {
		t.Fatal("first Transitioned = false, want true")
	}
	if got := first.After.Metadata["close_reason"]; got != "native winner" {
		t.Fatalf("first After close_reason = %q, want native winner", got)
	}
	if first.After.UpdatedAt.IsZero() {
		t.Fatal("first After UpdatedAt is zero, want the durable native timestamp")
	}

	repeat, err := store.CloseWithReasonIfOpen(created.ID, "must lose")
	if err != nil {
		t.Fatalf("repeat CloseWithReasonIfOpen: %v", err)
	}
	if repeat.Transitioned {
		t.Fatal("repeat Transitioned = true, want false")
	}
	if got := repeat.After.Metadata["close_reason"]; got != "native winner" {
		t.Fatalf("repeat After close_reason = %q, want native winner", got)
	}
}

func TestNativeDoltStoreCloseTransitionUsesOnlyTransactionSnapshots(t *testing.T) {
	storage := &nativeLegacyCloseSnapshotTrapStorage{nativeCloseTransitionStorage: newNativeCloseTransitionStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	transition, err := store.CloseWithReasonIfOpen(created.ID, "transaction close")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned || transition.After.Status != "closed" {
		t.Fatalf("transition = %#v, want committed transaction close", transition)
	}
	if storage.snapshotCalls != 0 {
		t.Fatalf("legacy snapshot calls = %d, want 0", storage.snapshotCalls)
	}
}

func TestNativeDoltStoreCloseTransitionDoesNotRequireIdentityReader(t *testing.T) {
	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	transition, err := store.CloseWithReasonIfOpen(created.ID, "transaction-owned close")
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if !transition.Transitioned {
		t.Fatal("Transitioned = false, want transaction-owned close")
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
}
