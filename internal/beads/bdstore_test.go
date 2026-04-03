package beads_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeRunner returns a CommandRunner that returns canned output for specific
// commands, or an error if the command is unrecognized.
func fakeRunner(responses map[string]struct {
	out []byte
	err error
},
) beads.CommandRunner {
	return func(_, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if resp, ok := responses[key]; ok {
			return resp.out, resp.err
		}
		return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
	}
}

// --- Create ---

func TestBdStoreCreate(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd create --json Build a widget -t task`: {
			out: []byte(`{"id":"bd-abc-123","title":"Build a widget","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z","owner":""}`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	b, err := s.Create(beads.Bead{Title: "Build a widget"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "bd-abc-123" {
		t.Errorf("ID = %q, want %q", b.ID, "bd-abc-123")
	}
	if b.Title != "Build a widget" {
		t.Errorf("Title = %q, want %q", b.Title, "Build a widget")
	}
	if b.Status != "open" {
		t.Errorf("Status = %q, want %q", b.Status, "open")
	}
	if b.Type != "task" {
		t.Errorf("Type = %q, want %q", b.Type, "task")
	}
}

func TestBdStoreCreateDefaultsTypeToTask(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// Should pass -t task when Type is empty.
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "-t task") {
		t.Errorf("args = %q, want to contain '-t task'", args)
	}
}

func TestBdStoreCreatePreservesExplicitType(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"bug","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test", Type: "bug"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "-t bug") {
		t.Errorf("args = %q, want to contain '-t bug'", args)
	}
}

func TestBdStoreCreatePassesDeps(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{
		Title: "test",
		Needs: []string{"bd-1", "validates:bd-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--deps bd-1,validates:bd-2") {
		t.Errorf("args = %q, want combined deps flag", args)
	}
}

func TestBdStoreCreatePassesPriority(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z","priority":1}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	priority := 1
	created, err := s.Create(beads.Bead{Title: "test", Priority: &priority})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--priority 1") {
		t.Fatalf("args = %q, want priority flag", args)
	}
	if created.Priority == nil || *created.Priority != 1 {
		t.Fatalf("created.Priority = %v, want 1", created.Priority)
	}
}

func TestBdStoreCreateError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd create") {
		t.Errorf("error = %q, want to contain 'bd create'", err)
	}
}

func TestBdStoreCreateBadJSON(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{not json`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parsing JSON") {
		t.Errorf("error = %q, want to contain 'parsing JSON'", err)
	}
}

func TestBdStoreCreatePassesAssigneeAndFromMetadata(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-msg-1","title":"Update reminder","status":"open","issue_type":"message","created_at":"2025-01-15T10:30:00Z","assignee":"corp/lawrence","metadata":{"from":"priya"}}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	b, err := s.Create(beads.Bead{
		Title:       "Update reminder",
		Type:        "message",
		Assignee:    "corp/lawrence",
		From:        "priya",
		Description: "Friendly nudge",
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--assignee corp/lawrence") {
		t.Fatalf("args = %q, want assignee flag", args)
	}
	if !strings.Contains(args, `"from":"priya"`) {
		t.Fatalf("args = %q, want sender metadata", args)
	}
	if b.Assignee != "corp/lawrence" {
		t.Fatalf("Assignee = %q, want corp/lawrence", b.Assignee)
	}
	if b.From != "priya" {
		t.Fatalf("From = %q, want priya", b.From)
	}
}

// --- Get ---

func TestBdStoreGet(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json bd-abc-123`: {
			out: []byte(`[{"id":"bd-abc-123","title":"Build a widget","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z","assignee":"alice"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	b, err := s.Get("bd-abc-123")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "bd-abc-123" {
		t.Errorf("ID = %q, want %q", b.ID, "bd-abc-123")
	}
	if b.Assignee != "alice" {
		t.Errorf("Assignee = %q, want %q", b.Assignee, "alice")
	}
}

func TestBdIssueToBeadFallsBackToMetadataFrom(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json bd-msg-1`: {
			out: []byte(`[{"id":"bd-msg-1","title":"Update reminder","status":"open","issue_type":"message","created_at":"2025-01-15T10:30:00Z","assignee":"corp/lawrence","metadata":{"from":"priya"}}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	b, err := s.Get("bd-msg-1")
	if err != nil {
		t.Fatal(err)
	}
	if b.From != "priya" {
		t.Fatalf("From = %q, want priya", b.From)
	}
}

func TestBdStoreGetNotFound(t *testing.T) {
	// Real "not found" scenario: bd show returns an empty JSON array.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("nonexistent-999")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestBdStoreGetCLIError(t *testing.T) {
	// CLI error should NOT be wrapped as ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("nonexistent-999")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Errorf("CLI error should not be ErrNotFound, got %v", err)
	}
}

func TestBdStoreGetCLINotFound(t *testing.T) {
	// bd CLI "not found" error should be wrapped as ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: Error fetching x: no issue found matching \"x\"")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestBdStoreGetBadJSON(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`not json`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("bd-abc-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "parsing JSON") {
		t.Errorf("error = %q, want to contain 'parsing JSON'", err)
	}
}

func TestBdStoreGetEmptyArray(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Get("bd-abc-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// --- Close ---

func TestBdStoreClose(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd close --json bd-abc-123`: {
			out: []byte(`[{"id":"bd-abc-123","title":"test","status":"closed","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	if err := s.Close("bd-abc-123"); err != nil {
		t.Fatal(err)
	}
}

func TestBdStoreCloseNotFound(t *testing.T) {
	// Generic CLI error without "not found" should NOT be ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.Close("nonexistent-999")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Errorf("generic CLI error should not be ErrNotFound, got %v", err)
	}
}

func TestBdStoreCloseCLINotFound(t *testing.T) {
	// bd CLI "issue not found" should be wrapped as ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		// close returns "not found", Get also returns "not found" → truly not found.
		return nil, fmt.Errorf("exit status 1: Error closing x: issue not found: x")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.Close("x")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestBdStoreUpdateCLINotFound(t *testing.T) {
	// bd CLI "not found" from update should be wrapped as ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: Error resolving x: no issue found matching \"x\"")
	}
	s := beads.NewBdStore("/city", runner)
	desc := "whatever"
	err := s.Update("x", beads.UpdateOpts{Description: &desc})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestBdStoreUpdateEmptyOpts(t *testing.T) {
	// Update with no fields should be a no-op (no bd call).
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("runner should not be called for empty update")
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	if err := s.Update("bd-abc-123", beads.UpdateOpts{}); err != nil {
		t.Errorf("empty Update should succeed, got %v", err)
	}
}

func TestBdStoreUpdatePassesPriority(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-42","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	priority := 0
	if err := s.Update("bd-42", beads.UpdateOpts{Priority: &priority}); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--priority 0") {
		t.Fatalf("args = %q, want priority flag", args)
	}
}

func TestBdStoreCloseCLIError(t *testing.T) {
	// CLI error should NOT be wrapped as ErrNotFound.
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("connection refused")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.Close("bd-abc-123")
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, beads.ErrNotFound) {
		t.Errorf("CLI error should not be ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error should contain original message, got %v", err)
	}
}

// --- List ---

func TestBdStoreList(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --limit 0 --include-infra`: {
			out: []byte(`[{"id":"bd-aaa","title":"first","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"},{"id":"bd-bbb","title":"second","status":"closed","issue_type":"bug","created_at":"2025-01-15T10:31:00Z"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("List() returned %d beads, want 2", len(got))
	}
	if got[0].ID != "bd-aaa" {
		t.Errorf("got[0].ID = %q, want %q", got[0].ID, "bd-aaa")
	}
	if got[1].Status != "closed" {
		t.Errorf("got[1].Status = %q, want %q", got[1].Status, "closed")
	}
}

func TestBdStoreListEmpty(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --limit 0 --include-infra`: {out: []byte(`[]`)},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("List() returned %d beads, want 0", len(got))
	}
}

func TestBdStoreListError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.List()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd list") {
		t.Errorf("error = %q, want to contain 'bd list'", err)
	}
}

func TestBdStoreListIncludesInfra(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	if _, err := s.List(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(gotArgs, " "), "--include-infra") {
		t.Fatalf("args = %q, want --include-infra", strings.Join(gotArgs, " "))
	}
}

// --- Ready ---

func TestBdStoreReady(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd ready --json --limit 0`: {
			out: []byte(`[{"id":"bd-aaa","title":"ready one","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("Ready() returned %d beads, want 1", len(got))
	}
	if got[0].Title != "ready one" {
		t.Errorf("got[0].Title = %q, want %q", got[0].Title, "ready one")
	}
}

func TestBdStoreReadyEmpty(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd ready --json --limit 0`: {out: []byte(`[]`)},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("Ready() returned %d beads, want 0", len(got))
	}
}

func TestBdStoreReadyError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Ready()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd ready") {
		t.Errorf("error = %q, want to contain 'bd ready'", err)
	}
}

// --- Status mapping ---

func TestBdStoreStatusMapping(t *testing.T) {
	tests := []struct {
		bdStatus   string
		wantStatus string
	}{
		{"open", "open"},
		{"in_progress", "in_progress"},
		{"blocked", "open"},
		{"review", "open"},
		{"testing", "open"},
		{"closed", "closed"},
	}
	for _, tt := range tests {
		t.Run(tt.bdStatus, func(t *testing.T) {
			runner := fakeRunner(map[string]struct {
				out []byte
				err error
			}{
				`bd show --json bd-x`: {
					out: []byte(fmt.Sprintf(`[{"id":"bd-x","title":"test","status":%q,"issue_type":"task","created_at":"2025-01-15T10:30:00Z"}]`, tt.bdStatus)),
				},
			})
			s := beads.NewBdStore("/city", runner)
			b, err := s.Get("bd-x")
			if err != nil {
				t.Fatal(err)
			}
			if b.Status != tt.wantStatus {
				t.Errorf("status %q → %q, want %q", tt.bdStatus, b.Status, tt.wantStatus)
			}
		})
	}
}

// --- Init ---

func TestBdStoreInit(t *testing.T) {
	var gotDir, gotName string
	var gotArgs []string
	runner := func(dir, name string, args ...string) ([]byte, error) {
		gotDir = dir
		gotName = name
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/my/city", runner)
	if err := s.Init("bright-lights", "", ""); err != nil {
		t.Fatal(err)
	}
	if gotDir != "/my/city" {
		t.Errorf("dir = %q, want %q", gotDir, "/my/city")
	}
	if gotName != "bd" {
		t.Errorf("name = %q, want %q", gotName, "bd")
	}
	wantArgs := "init --server -p bright-lights --skip-hooks"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

func TestBdStoreInitWithServerHost(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/my/city", runner)
	if err := s.Init("gc", "dolt.gc.svc.cluster.local", "3307"); err != nil {
		t.Fatal(err)
	}
	wantArgs := "init --server -p gc --skip-hooks --server-host dolt.gc.svc.cluster.local --server-port 3307"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

func TestBdStoreInitError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte("init failed"), fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.Init("test", "", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd init") {
		t.Errorf("error = %q, want to contain 'bd init'", err)
	}
}

// --- ConfigSet ---

func TestBdStoreConfigSet(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	if err := s.ConfigSet("issue_prefix", "bl"); err != nil {
		t.Fatal(err)
	}
	wantArgs := "config set issue_prefix bl"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

func TestBdStoreConfigSetError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte("config failed"), fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.ConfigSet("issue_prefix", "bl")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd config set") {
		t.Errorf("error = %q, want to contain 'bd config set'", err)
	}
}

// --- Purge ---

func TestBdStorePurge(t *testing.T) {
	var gotArgs []string
	var gotDir string
	var gotEnv []string
	s := beads.NewBdStore("/city", nil)
	s.SetPurgeRunner(func(dir string, env []string, args ...string) ([]byte, error) {
		gotDir = dir
		gotArgs = args
		gotEnv = env
		return []byte(`{"purged_count": 5}`), nil
	})
	result, err := s.Purge("/city/rigs/fe/.beads", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Purged != 5 {
		t.Errorf("Purged = %d, want 5", result.Purged)
	}
	// Verify args include purge --json (no --allow-stale: bd purge
	// does not support that flag).
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "purge") || !strings.Contains(args, "--json") {
		t.Errorf("args = %q, want purge --json", args)
	}
	// Should NOT contain --dry-run.
	if strings.Contains(args, "--dry-run") {
		t.Errorf("args = %q, should not contain --dry-run", args)
	}
	// Dir should be parent of beads dir.
	if gotDir != "/city/rigs/fe" {
		t.Errorf("dir = %q, want %q", gotDir, "/city/rigs/fe")
	}
	// Env should contain BEADS_DIR.
	foundBeadsDir := false
	for _, e := range gotEnv {
		if e == "BEADS_DIR=/city/rigs/fe/.beads" {
			foundBeadsDir = true
		}
	}
	if !foundBeadsDir {
		t.Errorf("env missing BEADS_DIR; got %v", gotEnv)
	}
}

func TestBdStorePurgeDryRun(t *testing.T) {
	var gotArgs []string
	s := beads.NewBdStore("/city", nil)
	s.SetPurgeRunner(func(_ string, _ []string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"purged_count": 0}`), nil
	})
	_, err := s.Purge("/city/.beads", true)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--dry-run") {
		t.Errorf("args = %q, want --dry-run", args)
	}
}

func TestBdStorePurgeError(t *testing.T) {
	s := beads.NewBdStore("/city", nil)
	s.SetPurgeRunner(func(_ string, _ []string, _ ...string) ([]byte, error) {
		return []byte("purge failed"), fmt.Errorf("exit status 1")
	})
	_, err := s.Purge("/city/.beads", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd purge") {
		t.Errorf("error = %q, want to contain 'bd purge'", err)
	}
}

func TestBdStorePurgeBadJSON(t *testing.T) {
	s := beads.NewBdStore("/city", nil)
	s.SetPurgeRunner(func(_ string, _ []string, _ ...string) ([]byte, error) {
		return []byte("not json"), nil
	})
	_, err := s.Purge("/city/.beads", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unexpected output") {
		t.Errorf("error = %q, want to contain 'unexpected output'", err)
	}
}

func TestBdStorePurgeMissingCount(t *testing.T) {
	s := beads.NewBdStore("/city", nil)
	s.SetPurgeRunner(func(_ string, _ []string, _ ...string) ([]byte, error) {
		return []byte(`{"other_field": true}`), nil
	})
	result, err := s.Purge("/city/.beads", false)
	if err != nil {
		t.Fatal(err)
	}
	// Missing purged_count should return 0 (not an error).
	if result.Purged != 0 {
		t.Errorf("Purged = %d, want 0 (missing field)", result.Purged)
	}
}

// --- Create with labels and parent ---

func TestBdStoreCreateWithLabels(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"convoy","created_at":"2025-01-15T10:30:00Z","labels":["owned"]}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test", Type: "convoy", Labels: []string{"owned"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--labels owned") {
		t.Errorf("args = %q, want to contain '--labels owned'", args)
	}
}

func TestBdStoreCreateWithParentID(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test", ParentID: "bd-parent-1"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--parent bd-parent-1") {
		t.Errorf("args = %q, want to contain '--parent bd-parent-1'", args)
	}
}

func TestBdStoreCreateNoLabelsNoParent(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`{"id":"bd-x","title":"test","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z"}`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if strings.Contains(args, "--labels") {
		t.Errorf("args = %q, should not contain --labels when Labels is nil", args)
	}
	if strings.Contains(args, "--parent") {
		t.Errorf("args = %q, should not contain --parent when ParentID is empty", args)
	}
}

// --- Update with labels ---

func TestBdStoreUpdateWithLabels(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	err := s.Update("bd-42", beads.UpdateOpts{Labels: []string{"pool:hw/polecat", "urgent"}})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--add-label pool:hw/polecat") {
		t.Errorf("args = %q, want --add-label pool:hw/polecat", args)
	}
	if !strings.Contains(args, "--add-label urgent") {
		t.Errorf("args = %q, want --add-label urgent", args)
	}
}

func TestBdStoreUpdateNoLabels(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	desc := "updated"
	err := s.Update("bd-42", beads.UpdateOpts{Description: &desc})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if strings.Contains(args, "--add-label") {
		t.Errorf("args = %q, should not contain --add-label when Labels is nil", args)
	}
}

// --- SetMetadata ---

func TestBdStoreSetMetadata(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	err := s.SetMetadata("bd-42", "merge_strategy", "mr")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "update --json bd-42 --set-metadata merge_strategy=mr"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

func TestBdStoreSetMetadataError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.SetMetadata("bd-42", "key", "value")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "setting metadata") {
		t.Errorf("error = %q, want to contain 'setting metadata'", err)
	}
}

// --- ListByLabel ---

func TestBdStoreListByLabel(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --label=order-run:digest --all --include-infra --limit 5`: {
			out: []byte(`[{"id":"bd-aaa","title":"digest wisp","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z","labels":["order-run:digest"]}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.ListByLabel("order-run:digest", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ListByLabel returned %d beads, want 1", len(got))
	}
	if got[0].ID != "bd-aaa" {
		t.Errorf("got[0].ID = %q, want %q", got[0].ID, "bd-aaa")
	}
	if len(got[0].Labels) != 1 || got[0].Labels[0] != "order-run:digest" {
		t.Errorf("got[0].Labels = %v, want [order-run:digest]", got[0].Labels)
	}
}

func TestBdStoreListByLabelEmpty(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd list --json --label=order-run:none --all --include-infra --limit 1`: {out: []byte(`[]`)},
	})
	s := beads.NewBdStore("/city", runner)
	got, err := s.ListByLabel("order-run:none", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("ListByLabel returned %d beads, want 0", len(got))
	}
}

func TestBdStoreListByLabelError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.ListByLabel("order-run:digest", 1)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bd list") {
		t.Errorf("error = %q, want to contain 'bd list'", err)
	}
}

func TestBdStoreListByLabelZeroLimit(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	_, err := s.ListByLabel("order-run:digest", 0)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--limit 0") {
		t.Errorf("args = %q, want --limit 0 for unlimited", args)
	}
	if !strings.Contains(args, "--include-infra") {
		t.Errorf("args = %q, want --include-infra", args)
	}
}

func TestBdStoreListByAssigneeIncludesInfra(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	if _, err := s.ListByAssignee("mayor", "open", 0); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--assignee=mayor") {
		t.Fatalf("args = %q, want --assignee=mayor", args)
	}
	if !strings.Contains(args, "--status=open") {
		t.Fatalf("args = %q, want --status=open", args)
	}
	if !strings.Contains(args, "--include-infra") {
		t.Fatalf("args = %q, want --include-infra", args)
	}
}

func TestBdStoreListByMetadataIncludesInfra(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/city", runner)
	if _, err := s.ListByMetadata(map[string]string{"alias": "mayor"}, 0); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(gotArgs, " ")
	if !strings.Contains(args, "--metadata-field alias=mayor") {
		t.Fatalf("args = %q, want metadata filter", args)
	}
	if !strings.Contains(args, "--include-infra") {
		t.Fatalf("args = %q, want --include-infra", args)
	}
}

// --- Verify working directory is passed ---

func TestBdStorePassesDir(t *testing.T) {
	var gotDir string
	runner := func(dir, _ string, _ ...string) ([]byte, error) {
		gotDir = dir
		return []byte(`[]`), nil
	}
	s := beads.NewBdStore("/my/city", runner)
	_, _ = s.List()
	if gotDir != "/my/city" {
		t.Errorf("dir = %q, want %q", gotDir, "/my/city")
	}
}

// --- DepAdd ---

func TestBdStoreDepAdd(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	err := s.DepAdd("bd-42", "bd-41", "blocks")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "dep add bd-42 bd-41 --type blocks"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

func TestBdStoreDepAddError(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)
	err := s.DepAdd("bd-42", "bd-41", "blocks")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "adding dep") {
		t.Errorf("error = %q, want 'adding dep'", err)
	}
}

// --- DepRemove ---

func TestBdStoreDepRemove(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return nil, nil
	}
	s := beads.NewBdStore("/city", runner)
	err := s.DepRemove("bd-42", "bd-41")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "dep remove bd-42 bd-41"
	if strings.Join(gotArgs, " ") != wantArgs {
		t.Errorf("args = %q, want %q", strings.Join(gotArgs, " "), wantArgs)
	}
}

// --- DepList ---

func TestBdStoreDepListDown(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd dep list bd-42 --json`: {
			out: []byte(`[{"id":"bd-41","title":"blocker","status":"open","issue_type":"task","created_at":"2026-03-06T10:00:00Z","dependency_type":"blocks"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	deps, err := s.DepList("bd-42", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("DepList = %d deps, want 1", len(deps))
	}
	if deps[0].IssueID != "bd-42" {
		t.Errorf("IssueID = %q, want %q", deps[0].IssueID, "bd-42")
	}
	if deps[0].DependsOnID != "bd-41" {
		t.Errorf("DependsOnID = %q, want %q", deps[0].DependsOnID, "bd-41")
	}
	if deps[0].Type != "blocks" {
		t.Errorf("Type = %q, want %q", deps[0].Type, "blocks")
	}
}

func TestBdStoreDepListUp(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd dep list bd-41 --json --direction=up`: {
			out: []byte(`[{"id":"bd-42","title":"dependent","status":"open","issue_type":"task","created_at":"2026-03-06T10:00:00Z","dependency_type":"blocks"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	deps, err := s.DepList("bd-41", "up")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("DepList = %d deps, want 1", len(deps))
	}
	// "up" on bd-41: bd-42 depends on bd-41.
	if deps[0].IssueID != "bd-42" {
		t.Errorf("IssueID = %q, want %q", deps[0].IssueID, "bd-42")
	}
	if deps[0].DependsOnID != "bd-41" {
		t.Errorf("DependsOnID = %q, want %q", deps[0].DependsOnID, "bd-41")
	}
}

func TestBdStoreDepListEmpty(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd dep list bd-42 --json`: {out: []byte(`[]`)},
	})
	s := beads.NewBdStore("/city", runner)
	deps, err := s.DepList("bd-42", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 0 {
		t.Errorf("DepList = %d deps, want 0", len(deps))
	}
}

func TestExecCommandRunnerWithEnvOverridesInheritedValues(t *testing.T) {
	t.Setenv("GC_CITY_PATH", "/wrong")
	t.Setenv("GC_DOLT_PORT", "9999")

	dir := t.TempDir()
	runner := beads.ExecCommandRunnerWithEnv(map[string]string{
		"GC_CITY_PATH": "/city",
		"GC_DOLT_PORT": "31364",
	})

	out, err := runner(dir, "sh", "-c", `printf '%s\n%s\n' "$GC_CITY_PATH" "$GC_DOLT_PORT"`)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q, want 2 lines", string(out))
	}
	if lines[0] != "/city" {
		t.Fatalf("GC_CITY_PATH = %q, want %q", lines[0], "/city")
	}
	if lines[1] != "31364" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", lines[1], "31364")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("runner should preserve working dir usability: %v", err)
	}
}

func TestBdStoreApplyGraphPlan(t *testing.T) {
	dir := t.TempDir()
	var capturedPlan beads.GraphApplyPlan
	runner := func(cmdDir, name string, args ...string) ([]byte, error) {
		if cmdDir != dir {
			t.Fatalf("runner dir = %q, want %q", cmdDir, dir)
		}
		if name != "bd" {
			t.Fatalf("runner name = %q, want bd", name)
		}
		if len(args) != 4 || args[0] != "create" || args[1] != "--graph" || args[3] != "--json" {
			t.Fatalf("args = %q", args)
		}
		data, err := os.ReadFile(args[2])
		if err != nil {
			t.Fatalf("reading plan file: %v", err)
		}
		if err := json.Unmarshal(data, &capturedPlan); err != nil {
			t.Fatalf("unmarshal plan file: %v", err)
		}
		return []byte(`{"ids":{"mol.root":"bd-1","mol.step":"bd-2"}}`), nil
	}

	s := beads.NewBdStore(dir, runner)
	result, err := s.ApplyGraphPlan(t.Context(), &beads.GraphApplyPlan{
		CommitMessage: "gc: instantiate mol",
		Nodes: []beads.GraphApplyNode{
			{Key: "mol.root", Title: "Root"},
			{Key: "mol.step", Title: "Step"},
		},
		Edges: []beads.GraphApplyEdge{
			{FromKey: "mol.step", ToKey: "mol.root", Type: "parent-child"},
		},
	})
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if got := result.IDs["mol.step"]; got != "bd-2" {
		t.Fatalf("result ID = %q, want bd-2", got)
	}
	if got := capturedPlan.CommitMessage; got != "gc: instantiate mol" {
		t.Fatalf("captured commit message = %q", got)
	}
	if len(capturedPlan.Nodes) != 2 || len(capturedPlan.Edges) != 1 {
		t.Fatalf("captured plan = %+v", capturedPlan)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".gc", "tmp", "graph-apply-*.json")); len(matches) != 0 {
		t.Fatalf("temp graph apply files were not cleaned up: %v", matches)
	}
}
